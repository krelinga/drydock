package container_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// This file bounds the two things in the tier that talk to the network and
// can wait forever: `devcontainer up` (the Feature from ghcr, the Claude Code
// download in the Feature's install.sh, apt, base images, buildx) and
// `docker pull`. CI run 37899159383 sat 9m50s in one `up` until Go's 10-minute
// package timeout panicked the whole package, with nothing in the log to say
// what the CLI was waiting on.
//
// So each is given a deadline well inside the package timeout (-timeout 20m
// in .github/actions/go-suite) and, when it passes, what the CLI printed and
// what was running at that moment goes to stderr *before* the child is
// stopped: the process tree names the fetch it was stuck in.
//
// There is deliberately no retry of `up`: a timeout that passes on the second
// try is the flake hidden, and the dump is what lets the next one be named.
const (
	// upLimit: the slowest `up` in a green run (a cold Feature build) takes
	// well under two minutes; the whole package takes under five. Three
	// times that, so a slow runner does not trip it, and a hang is cut off
	// with room left in the package timeout for the other tests' results.
	upLimit = 6 * time.Minute
	// pullLimit: a base image is a few tens of MB.
	pullLimit = 3 * time.Minute
	// dumpTail is how much of each stream is dumped.
	dumpTail = 16 << 10
)

// pullImage pulls up front so no test's timing depends on a cache, bounded,
// and once more when the pull itself failed or timed out: a pull is
// idempotent and asserts nothing.
func pullImage(t *testing.T, img string) {
	t.Helper()
	var out []byte
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), pullLimit)
		out, err = exec.CommandContext(ctx, "docker", "pull", "--quiet", img).CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "BOUND: docker pull %s attempt %d failed (timed out: %v): %v\n%s\n", img, attempt, timedOut, err, tailString(out))
	}
	t.Fatalf("docker pull %s: %v: %s", img, err, tailString(out))
}

// newServer is server.New with the provisioner's `devcontainer up` bounded.
func newServer(ctx context.Context, cfg config.Config) (*server.Server, error) {
	srv, err := server.New(ctx, cfg, sys.Production())
	if err == nil {
		srv.Provisioner.Containers.Run = boundedRunner{Inner: subproc.Exec{}}
	}
	return srv, err
}

// boundedRunner is a subproc.Runner that gives `devcontainer up` a deadline
// and dumps what it knows when the deadline passes. Everything else, and
// Start, is the inner runner's.
type boundedRunner struct {
	Inner subproc.Runner
	// Limit overrides upLimit (to prove the dump path).
	Limit time.Duration
	// Out is where a dump goes; nil is stderr.
	Out io.Writer
}

// Unwrap lets subproc.Underlying see the inner runner, so the Manager's
// docker guard still takes an Exec's Resolver through this wrapper.
func (b boundedRunner) Unwrap() subproc.Runner { return b.Inner }

func (b boundedRunner) Start(ctx context.Context, c subproc.Cmd) (subproc.Process, error) {
	return b.Inner.Start(ctx, c)
}

func (b boundedRunner) Run(ctx context.Context, c subproc.Cmd) subproc.Result {
	if c.Name != "devcontainer" || len(c.Args) == 0 || c.Args[0] != "up" {
		return b.Inner.Run(ctx, c)
	}
	limit := b.Limit
	if limit == 0 {
		limit = upLimit
	}
	var out, errb ring
	c.Stdout = tee(c.Stdout, &out)
	c.Stderr = tee(c.Stderr, &errb)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The dump comes first and the child is stopped after it, so the process
	// list shows what it was in.
	// finished is set under mu when the command has exited, and the timer
	// checks it under mu: an up that ends as the timer fires prints no dump.
	var mu sync.Mutex
	finished := false
	timer := time.AfterFunc(limit, func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		dumpHang(b.Out, limit, c, &out, &errb)
		cancel()
	})
	defer timer.Stop()
	res := b.Inner.Run(rctx, c)
	mu.Lock()
	finished = true
	mu.Unlock()
	return res
}

func dumpHang(dst io.Writer, limit time.Duration, c subproc.Cmd, stdout, stderr *ring) {
	var w strings.Builder
	fmt.Fprintf(&w, "\nBOUND: `devcontainer up` still running after %s; stopping it. Workspace folder: %s\n", limit, flagValue(c.Args, "--workspace-folder"))
	fmt.Fprintf(&w, "--- devcontainer stdout (tail) ---\n%s\n--- devcontainer stderr (tail) ---\n%s\n", stdout.String(), stderr.String())
	w.WriteString("--- processes (what it was waiting on) ---\n")
	w.WriteString(shell("ps", "-eo", "pid,ppid,etime,args", "--forest", "--width", "200"))
	w.WriteString("\n--- docker ps -a ---\n")
	w.WriteString(shell("docker", "ps", "-a", "--format", "{{.ID}} {{.Image}} {{.Status}} {{.Names}}"))
	w.WriteString("\n")
	if dst == nil {
		dst = os.Stderr
	}
	fmt.Fprint(dst, w.String())
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func shell(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s: %v\n%s", name, err, tailString(out))
	}
	return tailString(out)
}

func tailString(b []byte) string {
	if len(b) > dumpTail {
		b = b[len(b)-dumpTail:]
	}
	return strings.TrimSpace(string(b))
}

func tee(orig io.Writer, r *ring) io.Writer {
	if orig == nil {
		return r
	}
	return io.MultiWriter(orig, r)
}

// ring keeps the last dumpTail bytes written.
type ring struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b.Write(p)
	if r.b.Len() > 2*dumpTail {
		keep := append([]byte(nil), r.b.Bytes()[r.b.Len()-dumpTail:]...)
		r.b.Reset()
		r.b.Write(keep)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return tailString(r.b.Bytes())
}
