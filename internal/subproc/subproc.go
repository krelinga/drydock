// Package subproc is the boundary between Drydock and every program it shells
// out to: the `devcontainer` CLI, `claude`, `git`, `docker`.
//
// It exists as a seam for a reason worth stating precisely, because it is the
// one place where the obvious testing approach is actively worse. A Go
// interface mocking the `devcontainer` CLI tests Drydock's *belief* about that
// CLI. A fake binary on PATH tests the argv Drydock actually builds, the pipes
// it actually wires, the exit code it actually reads, and the JSON it actually
// parses — and **the argv is a security surface**, because `--mount` and
// `--additional-features` are assembled from workspace data (testing §5.3).
//
// So resolution goes through PATH, or through an injected Resolver, and the
// component tier substitutes a real executable rather than a mock.
//
// # Rules and details
//
// Exec is the real runner: no shell anywhere, Env replaces rather than
// inherits, a non-zero exit is data, and cancelling sends SIGTERM. StartPTY is
// the same rules on a terminal (internal/pty composed, not reimplemented): the
// supervisor's and the login handshake's way to start a child that must see a
// PTY.
package subproc

import (
	"context"
	"io"
	"os/exec"
)

// Cmd is one invocation, described as data so a test can assert on it before
// anything runs.
type Cmd struct {
	// Name is the program as named, not as resolved: "devcontainer", not
	// "/usr/local/bin/devcontainer". Resolution is the Resolver's job, which
	// is what makes substitution possible.
	Name string
	// Args is argv[1:]. Never a shell string — there is no shell in this
	// package, deliberately, so a workspace name containing a semicolon is
	// an argument and not a command.
	Args []string
	// Dir is the working directory, empty for the parent's.
	Dir string
	// Env, when non-nil, *replaces* the environment rather than adding to
	// it. That direction is deliberate: design §13.5 requires the App key
	// and the secrets master key never to reach a child's environment, and
	// inheriting-by-default is how a key reaches a child nobody considered.
	Env []string
	// Stdin, Stdout, Stderr are optional; nil means discard.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Resolver turns a program name into a path. The production implementation is
// exec.LookPath; a test supplies one pointing at its fakes' directory, which
// is cheaper and more explicit than mutating the process's PATH.
type Resolver interface {
	Resolve(name string) (string, error)
}

// PathResolver resolves through the process's PATH.
type PathResolver struct{}

func (PathResolver) Resolve(name string) (string, error) { return exec.LookPath(name) }

// FixedResolver maps names to absolute paths, for a test that has built fake
// binaries into a temp directory.
type FixedResolver map[string]string

func (f FixedResolver) Resolve(name string) (string, error) {
	if p, ok := f[name]; ok {
		return p, nil
	}
	return "", exec.ErrNotFound
}

// Result is what a finished invocation reports.
//
// ExitCode is carried explicitly rather than left inside an error because of
// Spike 02: all four `remote-control` startup refusals exit 1, so the exit code
// is not a discriminator and the *message* has to be classified. A caller that
// only sees `err != nil` cannot do that, and the natural wrong implementation —
// branching on the exit code — is the one that crash-loops against a config
// error or gives up on a wait.
type Result struct {
	ExitCode int
	// Err is set for failures to *start or signal* the process (not found,
	// permission denied, context cancelled) — never for a non-zero exit,
	// which is data.
	Err error
}

// Unwrapper is implemented by a Runner that wraps another (to bound, trace or
// record it) and says which. Underlying follows the chain, so a caller that
// needs the real runner's configuration, an Exec's Resolver, still finds it.
type Unwrapper interface {
	Unwrap() Runner
}

// Underlying returns r with every wrapper removed.
func Underlying(r Runner) Runner {
	for {
		u, ok := r.(Unwrapper)
		if !ok {
			return r
		}
		inner := u.Unwrap()
		if inner == nil {
			return r
		}
		r = inner
	}
}

// Runner starts programs. One method, two shapes: Run waits, Start does not.
type Runner interface {
	// Run executes to completion.
	Run(ctx context.Context, c Cmd) Result
	// Start launches and returns a Process for something long-lived — the
	// session supervisor's `remote-control` server, or a login handshake on
	// a PTY.
	Start(ctx context.Context, c Cmd) (Process, error)
}

// Process is a running child.
type Process interface {
	// Pid is recorded in the supervisor row for observability only. It is
	// never used to find the process again: design §6's rule is that Docker
	// is the truth and the database is a cache, and PF §8 extends it to
	// PIDs — a dead container's PID can be reused by a host process, so a
	// scan re-resolves rather than trusting a remembered number.
	Pid() int
	// Signal sends a signal. The supervisor sends SIGTERM and escalates to
	// SIGKILL only on timeout: a clean stop deregisters the folder and lets
	// the next start in immediately, while a SIGKILL of a session-less
	// server costs a 409 for one to three minutes (Spike 02).
	Signal(sig Signal) error
	// Wait blocks until exit.
	Wait() Result
}

// Signal is the small set Drydock actually sends, named rather than passed as
// an os.Signal so the SIGTERM-then-SIGKILL rule is legible at call sites.
type Signal uint8

const (
	SignalTerm Signal = iota
	SignalKill
	SignalInt
)
