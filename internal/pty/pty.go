//go:build linux

// Package pty opens a pseudo-terminal and starts a child on it.
//
// Two things in Drydock own a terminal rather than a pipe: the login handshake
// (design §7.2) and the session supervisor's `claude remote-control` (§8).
// Claude Code's output differs between the two — an untrusted workspace HANGS
// on `Trust <dir>? [y/N]` on a terminal and exits 1 with a message when
// redirected (Spike 02, re-measured on 2.1.289) — so a test that drives either
// over a pipe is testing a process Drydock never runs.
//
// This is the whole of what that needs, on golang.org/x/sys/unix, which the
// module already carried: open `/dev/ptmx`, unlock and name the slave, set the
// window size, and start the child as a session leader with the slave as its
// controlling terminal. A dependency such as github.com/creack/pty would buy
// portability to other kernels, and Drydock runs on one Linux server.
package pty

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Open returns a new master/slave pair sized cols × rows.
//
// The master is opened through os.OpenFile, so it is registered with Go's
// poller: a Read blocked on it is released by Close rather than holding a
// thread forever.
func Open(cols, rows int) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*os.File, *os.File, error) {
		master.Close()
		return nil, nil, e
	}
	if err := ctl(master, func(fd uintptr) error { return unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0) }); err != nil {
		return fail(fmt.Errorf("pty: unlock: %w", err))
	}
	var n int
	if err := ctl(master, func(fd uintptr) (e error) { n, e = unix.IoctlGetInt(int(fd), unix.TIOCGPTN); return e }); err != nil {
		return fail(fmt.Errorf("pty: slave number: %w", err))
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(fmt.Errorf("pty: open slave: %w", err))
	}
	if err := SetSize(master, cols, rows); err != nil {
		slave.Close()
		return fail(err)
	}
	return master, slave, nil
}

// SetSize sets the window size. Either side of the pair will do.
func SetSize(f *os.File, cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 0xffff || rows > 0xffff {
		return fmt.Errorf("pty: size %dx%d out of range", cols, rows)
	}
	ws := &unix.Winsize{Col: uint16(cols), Row: uint16(rows)}
	return ctl(f, func(fd uintptr) error { return unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws) })
}

// Size reports the window size of a terminal.
func Size(f *os.File) (cols, rows int, err error) {
	var ws *unix.Winsize
	err = ctl(f, func(fd uintptr) (e error) { ws, e = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ); return e })
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// Start runs cmd with the slave as its stdin, stdout, stderr and controlling
// terminal, and returns the master. The parent's copy of the slave is closed
// before returning, so a Read on the master ends (with EIO, which IsEOF
// recognizes) once the child and anything it forked have exited.
//
// cmd.Stdin, Stdout and Stderr must be unset; cmd.SysProcAttr is replaced.
func Start(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, errors.New("pty: Start owns the child's stdio")
	}
	master, slave, err := Open(cols, rows)
	if err != nil {
		return nil, err
	}
	defer slave.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Setsid makes the child a session leader; Setctty then makes fd 0 —
	// an index into the child's descriptors, not the parent's — its
	// controlling terminal, so it gets SIGHUP when the master closes and
	// isatty(3) is true for the right reason.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

// IsEOF reports whether err from reading a master means the slave side has
// closed. Linux reports that as EIO rather than io.EOF.
func IsEOF(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, fs.ErrClosed) || errors.Is(err, io.EOF)
}

func ctl(f *os.File, op func(fd uintptr) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = op(fd) }); err != nil {
		return err
	}
	return opErr
}
