//go:build linux

package subproc

import (
	"context"
	"errors"
	"os"

	"github.com/krelinga/drydock/internal/pty"
)

// PTYRunner starts a program on a pseudo-terminal Drydock owns.
//
// It is Exec's Start with one difference, and every rule of this package
// still holds: the program is resolved by the Resolver (so a test's fake
// binary is what runs), argv is a list with no shell, a non-nil Env replaces
// the environment rather than adding to it, and cancelling the context sends
// SIGTERM, never SIGKILL. The terminal itself is internal/pty's: this file
// composes the two rather than growing a second PTY implementation, because
// the session supervisor (design §8) and the login handshake (§7.2) both need
// a terminal *and* a substitutable, argv-checked child, and only the
// composition gives both.
//
// Why a terminal at all: Claude Code behaves differently on one. An untrusted
// workspace hangs on `Trust <dir>? [y/N]` on a PTY and exits 1 with a message
// when redirected (Spike 02, re-measured on 2.1.289), and `devcontainer exec`
// allocates a terminal in the container only when its own stdin and stdout
// are one (measured on CLI 0.89.0) — so a pipe would test a process Drydock
// never runs.
type PTYRunner interface {
	// StartPTY launches c with a fresh terminal of cols × rows as its
	// stdin, stdout, stderr and controlling terminal, and returns the
	// process and the terminal's master side. The caller reads the master
	// until it reports the end (pty.IsEOF) and closes it. c's Stdin,
	// Stdout and Stderr must be nil: the terminal is all three.
	StartPTY(ctx context.Context, c Cmd, cols, rows int) (Process, *os.File, error)
}

// StartPTY is the production PTYRunner.
func (e Exec) StartPTY(ctx context.Context, c Cmd, cols, rows int) (Process, *os.File, error) {
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		return nil, nil, errors.New("subproc: a PTY is the child's stdio; Stdin, Stdout and Stderr must be nil")
	}
	cmd, err := e.command(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	master, err := pty.Start(cmd, cols, rows)
	if err != nil {
		return nil, nil, err
	}
	return &process{cmd: cmd, ctx: ctx}, master, nil
}
