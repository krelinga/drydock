//go:build linux

// Command fakeclaude stands in for `claude` (testing §6.4). It replays the
// recorded corpus onto its terminal, reads what it is sent, and appends what
// it saw to an event log the test reads back. See package claudetest for the
// script it obeys; build it with claudetest.Install.
//
// Three rules shape every line here:
//
//   - The bytes are the corpus's, read at run time. A transcript recorded on
//     a PTY is written with output post-processing OFF, so the master reads
//     exactly the file; a refusal recorded with output redirected is written
//     to stderr with the terminal's processing left ON, so on a PTY it gains
//     the `\r` a real one would. Each recording is reproduced as the context
//     it was recorded in produced it.
//   - Nothing fakeclaude has to say about the test goes to the terminal.
//     Violations go to the event log, never to the stream under test.
//   - A login code is never written anywhere. A submission is logged as its
//     SHA-256 and length, so the fake's own state survives the canary sweep.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/krelinga/drydock/internal/claudetest"
)

func main() { os.Exit(run(os.Args[1:])) }

type fake struct {
	s          claudetest.Script
	dir        string // transcripts/claude-<Version>
	invocation int
	tty        bool
	width      int
	height     int
}

func run(args []string) int {
	s, err := loadScript()
	if err != nil {
		return fail("%v", err)
	}
	// Refuse to pretend. A test written against another version's corpus is
	// testing a belief this binary does not hold.
	if s.Version != "" && s.Version != claudetest.Version {
		return fail("recorded against Claude Code %s; refusing to impersonate %s", claudetest.Version, s.Version)
	}
	f := &fake{s: s, dir: filepath.Join(s.Corpus, "transcripts", "claude-"+claudetest.Version)}
	if st, err := os.Stat(f.dir); err != nil || !st.IsDir() {
		return fail("no corpus for %s at %s", claudetest.Version, f.dir)
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fail("state dir: %v", err)
	}
	f.tty = isTerminal(0) && isTerminal(1)
	if f.tty {
		if ws, err := unix.IoctlGetWinsize(1, unix.TIOCGWINSZ); err == nil {
			f.width, f.height = int(ws.Col), int(ws.Row)
		}
	}

	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		os.Stdout.WriteString(claudetest.VersionLine)
		return 0
	}

	f.invocation, err = f.nextInvocation()
	if err != nil {
		return fail("%v", err)
	}
	switch {
	case len(args) >= 2 && args[0] == "auth" && args[1] == "login":
		return f.exit(f.login(args))
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		return f.exit(f.authStatus(args))
	case len(args) >= 1 && args[0] == "remote-control":
		return f.exit(f.remoteControl(args))
	}
	f.start(args, "")
	f.violation("nothing scripted for argv %q", args)
	return f.exit(fail("nothing scripted for %q", strings.Join(args, " ")))
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "fakeclaude: "+format+"\n", a...)
	return 2
}

func loadScript() (claudetest.Script, error) {
	path := os.Getenv(claudetest.ScriptEnv)
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return claudetest.Script{}, err
		}
		path = exe + ".json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return claudetest.Script{}, fmt.Errorf("no script: %w", err)
	}
	var s claudetest.Script
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("script %s: %w", path, err)
	}
	if s.Corpus == "" || s.StateDir == "" {
		return s, fmt.Errorf("script %s: corpus and state_dir are required", path)
	}
	return s, nil
}

// --- the event log ----------------------------------------------------------

// withLock runs fn holding an exclusive lock on the state directory, so
// concurrent invocations (a status poll beside a serving server) do not
// interleave their counters.
func (f *fake) withLock(fn func() error) error {
	l, err := os.OpenFile(filepath.Join(f.s.StateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := unix.Flock(int(l.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	return fn()
}

func (f *fake) nextInvocation() (int, error) {
	var n int
	err := f.withLock(func() error {
		p := filepath.Join(f.s.StateDir, "invocations")
		b, _ := os.ReadFile(p)
		fmt.Sscan(string(b), &n)
		n++
		return os.WriteFile(p, []byte(fmt.Sprint(n)), 0o600)
	})
	return n, err
}

func (f *fake) log(e claudetest.Event) {
	e.Invocation = f.invocation
	b, _ := json.Marshal(e)
	_ = f.withLock(func() error {
		w, err := os.OpenFile(filepath.Join(f.s.StateDir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer w.Close()
		_, err = w.Write(append(b, '\n'))
		return err
	})
}

func (f *fake) start(argv []string, mode string) {
	f.log(claudetest.Event{Kind: claudetest.EventStart, Argv: argv, Mode: mode, TTY: f.tty, Width: f.width, Height: f.height})
}

func (f *fake) violation(format string, a ...any) {
	f.log(claudetest.Event{Kind: claudetest.EventViolation, What: fmt.Sprintf(format, a...)})
}

func (f *fake) exit(code int) int {
	f.log(claudetest.Event{Kind: claudetest.EventExit, Code: code})
	return code
}

// --- the terminal -----------------------------------------------------------

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// rawTerminal puts the terminal where Claude Code's Ink UI puts it: no echo,
// no line editing, no signal keys, CR not translated — so a submitted code
// arrives as the bytes written, and is never echoed into the buffer the
// supervisor keeps (Spike 01 measured the real prompt does not echo). Output
// post-processing is turned off too, so a recorded transcript reaches the
// master byte for byte.
func rawTerminal() error {
	t, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	return unix.IoctlSetTermios(0, unix.TCSETS, t)
}

func (f *fake) requireTTY(mode string) bool {
	if f.tty {
		return true
	}
	f.violation("%s needs a PTY: the supervisor and the login handshake own one, and Claude Code's output differs on a pipe", mode)
	fmt.Fprintf(os.Stderr, "fakeclaude: %s needs a PTY\n", mode)
	return false
}

// read returns a transcript for Version.
func (f *fake) read(name string) []byte {
	return f.corpus(filepath.Join("transcripts", "claude-"+claudetest.Version, name))
}

// corpus reads a file under test/fixtures and refuses one whose bytes are not
// the ones pinned in claudetest.Pinned: a re-recorded fixture is a belief to
// re-derive, not a change to absorb silently.
func (f *fake) corpus(rel string) []byte {
	die := func(format string, a ...any) {
		f.violation("corpus: "+format, a...)
		fmt.Fprintf(os.Stderr, "fakeclaude: corpus: "+format+"\n", a...)
		os.Exit(f.exit(3))
	}
	b, err := os.ReadFile(filepath.Join(f.s.Corpus, rel))
	if err != nil {
		die("%v", err)
	}
	want, ok := claudetest.Pinned[rel]
	if !ok {
		die("%s is not pinned in claudetest.Pinned", rel)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
		die("%s is not the recording fakeclaude was derived from; re-derive the fake and update its pin", rel)
	}
	return b
}

// replay writes recorded bytes at the script's pace.
func (f *fake) replay(w *os.File, b []byte) {
	chunk := f.s.Pace.Chunk
	if chunk <= 0 {
		chunk = len(b)
	}
	for len(b) > 0 {
		n := min(chunk, len(b))
		if _, err := w.Write(b[:n]); err != nil {
			return
		}
		b = b[n:]
		if len(b) > 0 && f.s.Pace.Delay > 0 {
			time.Sleep(f.s.Pace.Delay.D())
		}
	}
}

// cut returns the prefix of whole that equals part, or exits: a synthetic
// mode built by splitting one recording at another's length is only sound
// while the corpus still has that shape.
func (f *fake) mustPrefix(whole, part []byte, what string) []byte {
	if !bytes.HasPrefix(whole, part) {
		f.violation("corpus: %s is no longer a prefix — re-derive the fake", what)
		fmt.Fprintf(os.Stderr, "fakeclaude: corpus: %s is no longer a prefix\n", what)
		os.Exit(f.exit(3))
	}
	return whole[len(part):]
}

// --- auth login ---------------------------------------------------------------

func (f *fake) login(argv []string) int {
	l := f.s.Login
	f.start(argv, "login")
	if l == nil {
		f.violation("auth login was run but the script has no login section")
		return fail("no login scripted")
	}
	if !f.requireTTY("auth login") {
		return 2
	}
	if err := rawTerminal(); err != nil {
		return fail("raw mode: %v", err)
	}

	// Two recordings, at 80 and 1000 columns; the nearer one is replayed.
	// They differ only in the per-run PKCE values, because 2.1.289 writes
	// the URL unbroken at every width.
	prompt := f.read(claudetest.FixtureLoginPrompt)
	if abs(f.width-80) < abs(f.width-1000) {
		prompt = f.read(claudetest.FixtureLoginPrompt80)
	}
	invalid := f.mustPrefix(f.read(claudetest.FixtureLoginInvalid), f.read(claudetest.FixtureLoginPrompt), "login-code-prompt of login-invalid-code")
	success := f.mustPrefix(f.read(claudetest.FixtureLoginSuccess), f.read(claudetest.FixtureLoginPrompt), "login-code-prompt of login-success-after-prompt")

	f.replay(os.Stdout, prompt)

	seen := map[string]bool{}
	var pending []byte
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexAny(pending, "\r\n")
			if i < 0 {
				break
			}
			sub, term := pending[:i], pending[i]
			pending = pending[i+1:]
			// The framing is the code and one carriage return — what a
			// terminal sends for Enter, and what the recording's
			// `tmux send-keys … Enter` sent. A line feed is not Enter to
			// a raw-mode reader; `\r\n` leaves a stray LF that would be
			// read as the start of the next submission.
			if term == '\n' {
				f.violation("submission framed with LF, not CR")
				if len(sub) == 0 {
					continue
				}
			}
			if len(sub) == 0 {
				f.violation("empty submission")
				continue
			}
			if bytes.ContainsFunc(sub, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
				f.violation("submission carries whitespace or control bytes (%d bytes)", len(sub))
			}
			sum := sha256.Sum256(sub)
			h := hex.EncodeToString(sum[:])
			if seen[h] && !l.AllowRepeat {
				f.violation("the same code was submitted more than once")
			}
			seen[h] = true

			if l.Mode == claudetest.LoginTimeout {
				f.log(claudetest.Event{Kind: claudetest.EventSubmission, SHA256: h, Len: len(sub), Verdict: "unanswered"})
				continue
			}
			time.Sleep(l.VerdictDelay.D())
			if l.AcceptSHA256 != "" && h == l.AcceptSHA256 {
				f.log(claudetest.Event{Kind: claudetest.EventSubmission, SHA256: h, Len: len(sub), Verdict: "accepted"})
				if len(pending) > 0 {
					f.violation("%d bytes arrived after the accepted code", len(pending))
				}
				f.replay(os.Stdout, success)
				return 0
			}
			f.log(claudetest.Event{Kind: claudetest.EventSubmission, SHA256: h, Len: len(sub), Verdict: "rejected"})
			// Spike 01: the process stays at the prompt, the same URL
			// stays valid, and the prompt is not re-printed.
			f.replay(os.Stdout, invalid)
		}
		if err != nil {
			if len(pending) > 0 {
				f.violation("%d bytes were never terminated by a carriage return", len(pending))
			}
			return 1
		}
	}
}

func abs(n int) int { return max(n, -n) }

// --- auth status ------------------------------------------------------------

func (f *fake) authStatus(argv []string) int {
	a := f.s.AuthStatus
	f.start(argv, "auth-status")
	if a == nil {
		f.violation("auth status was run but the script has no auth_status section")
		return fail("no auth status scripted")
	}
	if !slices.Contains(argv[2:], "--json") {
		f.violation("auth status without --json: only the JSON form is recorded")
		return fail("only `auth status --json` is recorded")
	}
	shape := a.Shape
	if shape == "from-config" {
		var err error
		if shape, err = shapeFromConfig(); err != nil {
			f.violation("auth status: %v", err)
			return fail("%v", err)
		}
	}
	switch shape {
	case "absent", "valid", "expired", "blanked":
	default:
		f.violation("auth status shape %q is not one of the four recorded", shape)
		return fail("unrecorded shape %q", shape)
	}
	os.Stdout.Write(f.corpus(filepath.Join("authstatus", shape+".json")))
	return 0
}

// shapeFromConfig answers as 2.1.289 was measured to (Spike 01's
// auth-status.sh): the file decides presence and blanking; expiry does not
// move loggedIn, so an expired credential answers "valid".
func shapeFromConfig() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		return "", errors.New("from-config needs CLAUDE_CONFIG_DIR")
	}
	b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	var c struct {
		OAuth *struct {
			AccessToken  *string `json:"accessToken"`
			RefreshToken *string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.OAuth == nil || c.OAuth.AccessToken == nil || c.OAuth.RefreshToken == nil {
		return "", errors.New("a credential file the corpus has no auth status recording for")
	}
	if *c.OAuth.AccessToken == "" && *c.OAuth.RefreshToken == "" {
		return "blanked", nil
	}
	return "valid", nil
}

// --- remote-control -----------------------------------------------------------

type stepState struct {
	Index   int   `json:"index"`
	Count   int   `json:"count"`
	Started int64 `json:"started_unix_nano"`
}

// step advances the script: the current step ends after Times invocations or
// once For has elapsed since its first, and the last step repeats.
func (f *fake) step() (claudetest.Step, error) {
	steps := f.s.RemoteControl
	if len(steps) == 0 {
		return claudetest.Step{}, errors.New("remote-control was run but the script has no remote_control steps")
	}
	var cur claudetest.Step
	err := f.withLock(func() error {
		p := filepath.Join(f.s.StateDir, "rc-step.json")
		var st stepState
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &st)
		}
		now := time.Now()
		for st.Index < len(steps)-1 {
			s := steps[st.Index]
			done := (s.Times > 0 && st.Count >= s.Times) ||
				(s.For > 0 && st.Started != 0 && now.Sub(time.Unix(0, st.Started)) >= s.For.D())
			if !done {
				break
			}
			st = stepState{Index: st.Index + 1}
		}
		if st.Started == 0 {
			st.Started = now.UnixNano()
		}
		st.Count++
		cur = steps[st.Index]
		b, _ := json.Marshal(st)
		return os.WriteFile(p, b, 0o600)
	})
	return cur, err
}

// badCommandLine is the one refusal fakeclaude derives from argv rather than
// the script: the real binary refuses this combination whatever its config,
// and the supervisor's argv is what a test of it is about.
func badCommandLine(argv []string) bool {
	single := slices.ContainsFunc(argv, func(a string) bool {
		return a == "-c" || a == "--continue" || a == "--session-id" || strings.HasPrefix(a, "--session-id=")
	})
	server := slices.ContainsFunc(argv, func(a string) bool {
		for _, f := range []string{"--spawn", "--capacity", "--create-session-in-dir"} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
		return false
	})
	return single && server
}

func (f *fake) remoteControl(argv []string) int {
	s, err := f.step()
	if err != nil {
		f.start(argv, "")
		f.violation("%v", err)
		return fail("%v", err)
	}
	if badCommandLine(argv) {
		s = claudetest.Step{Mode: claudetest.RCRefuseBadCommandLine}
	}
	f.start(argv, string(s.Mode))

	switch s.Mode {
	case claudetest.RCRefuseWaitRegistration:
		return f.refuse(claudetest.FixtureRefuseWait, s)
	case claudetest.RCRefuseNotTrusted:
		return f.refuse(claudetest.FixtureRefuseTrust, s)
	case claudetest.RCRefuseNoOrganization:
		return f.refuse(claudetest.FixtureRefuseOrg, s)
	case claudetest.RCRefuseBadCommandLine:
		return f.refuse(claudetest.FixtureRefuseCmdline, s)
	case claudetest.RCHangTrust:
		// Measured on 2.1.289: the PTY hangs, a redirect refuses.
		if !f.tty {
			return f.refuse(claudetest.FixtureRefuseTrust, s)
		}
		return f.hang(claudetest.FixtureHangTrust)
	case claudetest.RCHangRemoteDialog:
		if !f.requireTTY(string(s.Mode)) {
			return 2
		}
		return f.hang(claudetest.FixtureHangDialog)
	case claudetest.RCCrash:
		if !f.requireTTY(string(s.Mode)) {
			return 2
		}
		if err := rawTerminal(); err != nil {
			return fail("raw mode: %v", err)
		}
		f.replay(os.Stdout, f.read(claudetest.FixtureCrash))
		time.Sleep(s.ExitAfter.D())
		return 1
	case claudetest.RCServe, claudetest.RCServeDelayedSession, claudetest.RCServeModelOutputID:
		if !f.requireTTY(string(s.Mode)) {
			return 2
		}
		return f.serve(s)
	}
	f.violation("unknown remote-control mode %q", s.Mode)
	return fail("unknown mode %q", s.Mode)
}

// refuse writes a refusal recorded with output redirected. On a PTY the
// terminal's own output processing is left alone, so its `\n` arrives as
// `\r\n`, as the real process's would.
func (f *fake) refuse(name string, s claudetest.Step) int {
	f.replay(os.Stderr, f.read(name))
	time.Sleep(s.ExitAfter.D())
	return 1
}

// hang writes a gate's prompt and then waits for ever. Anything typed at it
// is a violation: Drydock must never answer a gate, because the fix for each
// is a key in .claude.json the Feature writes, and a `y` typed into
// `Enable Remote Control?` would hide the missing key until the next rebuild.
func (f *fake) hang(name string) int {
	if err := rawTerminal(); err != nil {
		return fail("raw mode: %v", err)
	}
	f.replay(os.Stdout, f.read(name))
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			f.violation("%d bytes typed into a gate prompt", n)
		}
		if err != nil {
			// The master closed. A real hang would get SIGHUP; block
			// rather than exit, so nothing here looks like a verdict.
			// (A sleep, not `select {}`, which the runtime would call
			// a deadlock and exit 2.)
			for {
				time.Sleep(time.Hour)
			}
		}
	}
}

func (f *fake) serve(s claudetest.Step) int {
	if err := rawTerminal(); err != nil {
		return fail("raw mode: %v", err)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)

	// Keystrokes reach a serving remote-control as commands — space shows
	// a QR code, `w` toggles the spawn mode — so Drydock must write
	// nothing to it.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				f.violation("%d bytes written to a serving remote-control", n)
			}
			if err != nil {
				return
			}
		}
	}()

	running := f.read(claudetest.FixtureServeRunning)
	shutdown := f.mustPrefix(f.read(claudetest.FixtureServe), running, "session-url-osc8 of env-status-block")

	// pause sleeps, or reports that SIGTERM arrived first.
	pause := func(d time.Duration) bool {
		select {
		case <-term:
			return false
		case <-time.After(d):
			return true
		}
	}

	switch s.Mode {
	case claudetest.RCServe:
		// Hold the session back by splitting the recording at the
		// repaint that announces it.
		i := bytes.Index(running, []byte("Session started"))
		if i < 0 {
			f.violation("corpus: no `Session started` in session-url-osc8")
			return 3
		}
		i = bytes.LastIndexByte(running[:i], '\n') + 1
		f.replay(os.Stdout, running[:i])
		if !pause(s.SessionDelay.D()) {
			return 0
		}
		f.replay(os.Stdout, running[i:])
	case claudetest.RCServeDelayedSession:
		later := f.mustPrefix(f.read(claudetest.FixtureDelayedSession), running, "session-url-osc8 of session-ids-delayed")
		f.replay(os.Stdout, running)
		if !pause(s.SessionDelay.D()) {
			return 0
		}
		f.replay(os.Stdout, later)
		shutdown = nil // the recording's shutdown belongs to the one-session run
	case claudetest.RCServeModelOutputID:
		f.replay(os.Stdout, running)
		if !pause(s.SessionDelay.D()) {
			return 0
		}
		f.replay(os.Stdout, f.read(claudetest.FixtureModelOutputID))
		shutdown = nil
	}

	<-term
	// SIGTERM is the clean stop: the recorded shutdown section ends with
	// `Environment preserved`, and the next start is accepted at once
	// (Spike 02). That is why the supervisor sends it first.
	f.replay(os.Stdout, shutdown)
	return 0
}
