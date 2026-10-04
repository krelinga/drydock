package subproc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

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
		cmd.WaitDelay = 5 * time.Second
	}
	// SIGTERM, not SIGKILL, when the context ends: the devcontainer CLI and
	// git both clean up on TERM, and §8's supervisor rule is the same.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	return cmd, nil
}

// Run executes to completion. A non-zero exit is data in ExitCode, never Err.
func (e Exec) Run(ctx context.Context, c Cmd) Result {
	cmd, err := e.command(ctx, c)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	return result(cmd.Run(), ctx)
}

// Start launches c and returns without waiting.
func (e Exec) Start(ctx context.Context, c Cmd) (Process, error) {
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
