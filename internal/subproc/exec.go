package subproc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// DefaultWaitDelay is Exec's WaitDelay when it sets none: how long a
// cancelled subprocess is given to wind down after its SIGTERM. Shutdown's
// budget counts it (internal/server's workShutdownWait).
const DefaultWaitDelay = 5 * time.Second

// Exec is the production Runner: os/exec, with the program resolved by the
// Resolver rather than by exec.Command's own PATH lookup, so a test's fake is
// what runs.
type Exec struct {
	Resolver Resolver
	// WaitDelay bounds how long Run waits for pipes after the context ends;
	// a child that forked a grandchild holding stdout open would otherwise
	// hold Run forever.
	WaitDelay time.Duration
}

func (e Exec) command(ctx context.Context, c Cmd) (*exec.Cmd, error) {
	r := e.Resolver
	if r == nil {
		r = PathResolver{}
	}
	path, err := r.Resolve(c.Name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Name, err)
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Args[0] = c.Name // argv[0] as named, so a fake sees what the real one would
	cmd.Dir = c.Dir
	// nil Env would mean "inherit"; an empty one means "nothing". Cmd.Env's
	// contract is replace-not-add, so a nil there is passed through as
	// inherit only because that is what the caller asked for.
	cmd.Env = c.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	cmd.WaitDelay = e.WaitDelay
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	// SIGTERM, not SIGKILL, when the context ends: the devcontainer CLI and
	// git both clean up on TERM, and §8's supervisor rule is the same.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	return cmd, nil
}

// errGroupRunOnly is Start or StartPTY given a Cmd with Group: the group is
// signalled by Run alone, and dropping the flag would leave the children it
// asked to be stopped running.
var errGroupRunOnly = errors.New("subproc: Group is for Run only; Start and StartPTY would not signal the group")

// Run executes to completion. A non-zero exit is data in ExitCode, never Err.
func (e Exec) Run(ctx context.Context, c Cmd) Result {
	cmd, err := e.command(ctx, c)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	if !c.Group {
		return result(cmd.Run(), ctx)
	}
	// The group is the program's pid; signalling -pid reaches every member
	// still in it, the program's children included.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The SIGTERM starts the group's grace: WaitDelay, the same wind-down a
	// plain Run gets (shutdown's budget counts it once), for the program and
	// its children alike. Cancel runs before Wait returns, so grace is set
	// by the time Run reads it.
	var grace <-chan time.Time
	stop := func() {}
	cmd.Cancel = func() error {
		grace, stop = sys.NewTimer(sys.RealClock{}, cmd.WaitDelay)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	err = cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		reapGroup(cmd.Process.Pid, grace)
	}
	stop()
	return result(err, ctx)
}

// reapGroup waits for a cancelled group to empty and SIGKILLs what is left
// when grace fires — at once, when no grace was started (the program had
// exited before its context ended, so nothing was sent a SIGTERM). The
// SIGKILL is not sent the moment the program exits: a child handling its
// SIGTERM — a docker client cleaning up — must get to finish.
//
// A context that ends just after the program exited successfully, before
// Run reads ctx.Err(), takes this path too, with no grace: the group is
// SIGKILLed at once. For up that kills the docker run --sig-proxy=false
// client a successful up leaves attached, which is harmless — with no -i
// and no --rm, the container and its log are untouched by the client's end
// (measured: a cancel after the start left the container running), and the
// result is still the success the program reported.
//
// The group's id stays reserved while any member lives, so the signals reach
// another group only if this one emptied and its pid was reissued to a new
// group leader between two polls.
func reapGroup(pgid int, grace <-chan time.Time) {
	for grace != nil && syscall.Kill(-pgid, 0) == nil {
		tick, stop := sys.NewTimer(sys.RealClock{}, 20*time.Millisecond)
		select {
		case <-grace:
			grace = nil
		case <-tick:
		}
		stop()
	}
	// ESRCH — nothing left — is the usual answer.
	syscall.Kill(-pgid, syscall.SIGKILL)
}

// Start launches c and returns without waiting.
func (e Exec) Start(ctx context.Context, c Cmd) (Process, error) {
	if c.Group {
		return nil, errGroupRunOnly
	}
	cmd, err := e.command(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd, ctx: ctx}, nil
}

type process struct {
	cmd *exec.Cmd
	ctx context.Context
}

func (p *process) Pid() int { return p.cmd.Process.Pid }

func (p *process) Signal(s Signal) error {
	sig := map[Signal]syscall.Signal{SignalTerm: syscall.SIGTERM, SignalKill: syscall.SIGKILL, SignalInt: syscall.SIGINT}[s]
	return p.cmd.Process.Signal(sig)
}

func (p *process) Wait() Result { return result(p.cmd.Wait(), p.ctx) }

func result(err error, ctx context.Context) Result {
	if err == nil {
		return Result{}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ctx.Err() != nil {
			return Result{ExitCode: exit.ExitCode(), Err: ctx.Err()}
		}
		return Result{ExitCode: exit.ExitCode()}
	}
	return Result{ExitCode: -1, Err: err}
}
