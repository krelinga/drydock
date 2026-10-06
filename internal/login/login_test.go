//go:build linux

package login_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/login/logintest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// recorder is the identity watch's LoggedIn, recorded.
type recorder struct {
	mu  sync.Mutex
	ats []time.Time
}

func (r *recorder) LoggedIn(_ context.Context, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ats = append(r.ats, at)
	return nil
}

func (r *recorder) calls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.ats...)
}

type env struct {
	root     string
	db       *store.DB
	log      *events.Log
	sub      *events.Sub
	fake     *claudetest.Fake
	launcher *logintest.Launcher
	m        *login.Manager
	ids      *recorder
	logged   *bytes.Buffer
	logMu    *sync.Mutex
}

// newEnv puts every piece of mutable state under one temp root — the
// database, the fake's script, state and config dir, the captured service
// log — so the sweep can read all of it (testing §4.2).
func newEnv(t *testing.T, script claudetest.Script, clock sys.Clock) *env {
	t.Helper()
	root := t.TempDir()
	if script.StateDir == "" {
		script.StateDir = filepath.Join(root, "fake-state")
	}
	fake := claudetest.Install(t, script)
	db, err := store.Open(context.Background(), filepath.Join(root, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := events.New(db.DB, clock)
	sub := log.Subscribe()
	t.Cleanup(func() { log.Cancel(sub) })
	cfg := filepath.Join(root, "claude-config")
	os.MkdirAll(cfg, 0o700)
	l := &logintest.Launcher{Fake: fake, ConfigDir: cfg}
	var buf bytes.Buffer
	var mu sync.Mutex
	ids := &recorder{}
	m := &login.Manager{Launcher: l, Events: log, Clock: clock, Identity: ids, Settle: 5 * time.Second,
		Logf: func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&buf, f+"\n", a...); mu.Unlock() }}
	t.Cleanup(func() { m.Shutdown(10 * time.Second) })
	return &env{root: root, db: db, log: log, sub: sub, fake: fake, launcher: l, m: m, ids: ids, logged: &buf, logMu: &mu}
}

// next waits for the auth.login event whose phase satisfies ok.
func (e *env) next(t *testing.T, ok func(login.View) bool) login.View {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-e.sub.C:
			if ev.Kind != login.KindLogin {
				continue
			}
			var d struct{ Login login.View }
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
			if ok(d.Login) {
				return d.Login
			}
		case <-deadline:
			t.Fatalf("no matching auth.login event; current: %+v", e.m.Current())
		}
	}
}

func phase(p login.Phase) func(login.View) bool {
	return func(v login.View) bool { return v.Phase == p }
}

// ended waits until the manager reports the login over.
func (e *env) ended(t *testing.T, id string) login.View {
	t.Helper()
	for i := 0; i < 400; i++ {
		if v := e.m.Current(); v != nil && v.ID == id && v.Phase.Ended() && contains(e.launcher.Removed(), id) {
			return *v
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("login %s never ended: %+v", id, e.m.Current())
	return login.View{}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func canary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	rand.Read(b)
	c := make([]byte, 12)
	rand.Read(c)
	return "cnryCODE" + hex.EncodeToString(b) + "#cnrySTATE" + hex.EncodeToString(c)
}

func accept(code string) claudetest.Script {
	return claudetest.Script{Login: &claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(code)}}
}

// TestHappyPath: the URL is scraped, shown, a code is typed — once, framed
// with one CR, on a PTY the size Drydock chose — the classifier's verdict is
// succeeded, the process is cleaned up, and the watch is told when.
func TestHappyPath(t *testing.T) {
	code := canary(t)
	e := newEnv(t, accept(code), sys.RealClock{})
	ctx := context.Background()

	v, err := e.m.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != login.Starting || !login.ValidID(v.ID) {
		t.Fatalf("begin: %+v", v)
	}
	aw := e.next(t, phase(login.AwaitingCode))
	if aw.URL == nil || aw.Deadline == nil {
		t.Fatalf("awaiting_code without URL or deadline: %+v", aw)
	}
	u, err := url.Parse(*aw.URL)
	if err != nil || u.Host != "claude.com" || u.Query().Get("state") == "" || strings.HasSuffix(u.Query().Get("state"), "Paste") {
		t.Fatalf("URL %q is not a usable authorize URL", *aw.URL)
	}
	if got := aw.Deadline.Sub(time.Now()); got < 4*time.Minute || got > 5*time.Minute+time.Second {
		t.Errorf("deadline in %v; want five minutes", got)
	}

	if err := e.m.Submit(ctx, v.ID, []byte("  "+code+"\n")); err != nil {
		t.Fatal(err)
	}
	e.next(t, phase(login.Submitting))
	ok := e.next(t, phase(login.Succeeded))
	if ok.EndedAt == nil || ok.Attempts != 1 {
		t.Errorf("succeeded: %+v", ok)
	}
	end := e.ended(t, v.ID)
	if end.Phase != login.Succeeded {
		t.Fatalf("ended %s", end.Phase)
	}
	waitFor(t, func() bool { return len(e.ids.calls()) == 1 })
	if !e.ids.calls()[0].Equal(*ok.EndedAt) {
		t.Errorf("LoggedIn at %v; want the verdict's %v", e.ids.calls()[0], *ok.EndedAt)
	}

	evs := e.fake.Events(t)
	subs := claudetest.Kind(evs, claudetest.EventSubmission)
	if len(subs) != 1 || subs[0].Verdict != "accepted" || subs[0].SHA256 != claudetest.CodeSHA256(code) || subs[0].Len != len(code) {
		t.Errorf("fakeclaude saw %+v; want the trimmed code once, accepted", subs)
	}
	st := claudetest.Kind(evs, claudetest.EventStart)
	if len(st) != 1 || !st[0].TTY || st[0].Width != login.DefaultCols || st[0].Height != login.DefaultRows {
		t.Errorf("start %+v; want a PTY %dx%d", st, login.DefaultCols, login.DefaultRows)
	}
	if got := st[0].Argv; strings.Join(got, " ") != "auth login --claudeai" {
		t.Errorf("argv %q", got)
	}
	e.fake.NoViolations(t)
	// A new login can start at once: nothing is left holding the slot.
	if _, err := e.m.Begin(ctx); err != nil {
		t.Errorf("a login after success: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// TestWrongCodeLoopsBack is Spike 01's result 3: a wrong code is
// invalid_code with the same URL and the deadline still running, and the
// next code reaches the same PTY. The control is the second code's success.
// Without the per-submission window, the first `Invalid code` would answer
// the second code, and the real verdict would be ignored. That only shows
// when bytes arrive between a code and its verdict, as a real PTY read can
// split them, so the replay is paced in small chunks.
func TestWrongCodeLoopsBack(t *testing.T) {
	good, bad := canary(t), canary(t)
	s := accept(good)
	s.Pace = claudetest.Pace{Chunk: 5, Delay: claudetest.Duration(2 * time.Millisecond)}
	e := newEnv(t, s, sys.RealClock{})
	ctx := context.Background()
	v, _ := e.m.Begin(ctx)
	aw := e.next(t, phase(login.AwaitingCode))

	if err := e.m.Submit(ctx, v.ID, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	inv := e.next(t, phase(login.InvalidCode))
	if *inv.URL != *aw.URL || !inv.Deadline.Equal(*aw.Deadline) {
		t.Errorf("a wrong code changed the URL or the deadline: %+v", inv)
	}
	// A second wrong code: still a loop.
	if err := e.m.Submit(ctx, v.ID, []byte(canary(t))); err != nil {
		t.Fatal(err)
	}
	e.next(t, phase(login.Submitting))
	e.next(t, phase(login.InvalidCode))

	if err := e.m.Submit(ctx, v.ID, []byte(good)); err != nil {
		t.Fatal(err)
	}
	ok := e.next(t, phase(login.Succeeded))
	if ok.Attempts != 3 {
		t.Errorf("attempts %d", ok.Attempts)
	}
	e.ended(t, v.ID)
	if n := len(e.launcher.Launched()); n != 1 {
		t.Errorf("%d launches; a wrong code must not restart the process", n)
	}
	verdicts := []string{}
	for _, s := range claudetest.Kind(e.fake.Events(t), claudetest.EventSubmission) {
		verdicts = append(verdicts, s.Verdict)
	}
	if strings.Join(verdicts, ",") != "rejected,rejected,accepted" {
		t.Errorf("verdicts %v", verdicts)
	}
	e.fake.NoViolations(t)
}

// TestShapeRefusedBeforeThePTY: a truncated copy never reaches the process.
func TestShapeRefusedBeforeThePTY(t *testing.T) {
	good := canary(t)
	e := newEnv(t, accept(good), sys.RealClock{})
	ctx := context.Background()
	v, _ := e.m.Begin(ctx)
	e.next(t, phase(login.AwaitingCode))
	half, _, _ := strings.Cut(good, "#")
	for _, bad := range []string{"", half, half + "#", "#x", "a#b#c", "a b#c", "ab\x03#cd"} {
		err := e.m.Submit(ctx, v.ID, []byte(bad))
		var ce *login.CodeError
		if !errors.As(err, &ce) {
			t.Errorf("%q: %v; want a CodeError", bad, err)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), half) {
			t.Errorf("the refusal quotes the code: %v", err)
		}
	}
	if subs := claudetest.Kind(e.fake.Events(t), claudetest.EventSubmission); len(subs) != 0 {
		t.Errorf("a refused shape reached the PTY: %+v", subs)
	}
	// Control: a well-shaped code does reach it.
	if err := e.m.Submit(ctx, v.ID, []byte(good)); err != nil {
		t.Fatal(err)
	}
	e.next(t, phase(login.Succeeded))
}

// TestDeadline: with an injected clock, five minutes at the prompt is
// timed_out, the process is killed and removed, and Drydock carries on — a
// new login starts. The control is that four minutes is still waiting.
func TestDeadline(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	e := newEnv(t, claudetest.Script{Login: &claudetest.Login{Mode: claudetest.LoginTimeout}}, clock)
	ctx := context.Background()
	v, _ := e.m.Begin(ctx)
	aw := e.next(t, phase(login.AwaitingCode))
	if !aw.Deadline.Equal(clock.Now().Add(login.DefaultDeadline)) {
		t.Errorf("deadline %v", aw.Deadline)
	}
	if err := e.m.Submit(ctx, v.ID, []byte(canary(t))); err != nil {
		t.Fatal(err)
	}
	e.next(t, phase(login.Submitting))
	p, err := e.launcher.Proc(v.ID)
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(4 * time.Minute)
	time.Sleep(200 * time.Millisecond)
	if cur := e.m.Current(); cur.Phase != login.Submitting {
		t.Fatalf("control: at four minutes the login is %s", cur.Phase)
	}
	clock.Advance(time.Minute)
	to := e.next(t, phase(login.TimedOut))
	if to.EndedAt == nil || to.Deadline != nil {
		t.Errorf("timed_out view: %+v", to)
	}
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the process outlived its deadline")
	}
	if !contains(e.launcher.Removed(), v.ID) {
		t.Error("the timed-out login was not removed")
	}
	if len(e.ids.calls()) != 0 {
		t.Error("a timeout told the watch about a login")
	}
	if _, err := e.m.Begin(ctx); err != nil {
		t.Errorf("a new login after a timeout: %v", err)
	}
}

// TestNoURLInTime: the start timeout covers a launch that never shows a URL.
func TestNoURLInTime(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	e := newEnv(t, accept("x#y"), clock)
	e.launcher.Hold = make(chan struct{}) // an image build that never ends
	v, _ := e.m.Begin(context.Background())
	waitFor(t, func() bool { return len(e.launcher.Launched()) == 1 })
	waitFor(t, func() bool { return clock.Waiting() > 0 })
	clock.Advance(login.DefaultStartTimeout)
	f := e.next(t, phase(login.Failed))
	if f.Problem == nil || *f.Problem != login.ProblemNoURL || f.ID != v.ID {
		t.Errorf("failed: %+v", f)
	}
}

// TestCancel: a cancel while waiting for the code, and one during start,
// each end cancelled with the process gone, and a new login starts after.
func TestCancel(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	ctx := context.Background()
	v, _ := e.m.Begin(ctx)
	e.next(t, phase(login.AwaitingCode))
	p, _ := e.launcher.Proc(v.ID)
	if err := e.m.Cancel(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	c := e.next(t, phase(login.Cancelled))
	if c.ID != v.ID {
		t.Fatal(c)
	}
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled login's process is still running")
	}
	if !contains(e.launcher.Removed(), v.ID) {
		t.Error("not removed")
	}
	if err := e.m.Cancel(ctx, v.ID); !errors.Is(err, login.ErrEnded) {
		t.Errorf("cancelling it again: %v; want ErrEnded", err)
	}
	if err := e.m.Submit(ctx, v.ID, []byte("a#b")); !errors.Is(err, login.ErrEnded) {
		t.Errorf("a code for it: %v; want ErrEnded", err)
	}

	// During start, before any process exists.
	e.launcher.Hold = make(chan struct{})
	v2, err := e.m.Begin(ctx)
	if err != nil {
		t.Fatalf("a new login after a cancel: %v", err)
	}
	waitFor(t, func() bool { return len(e.launcher.Launched()) == 2 })
	if err := e.m.Submit(ctx, v2.ID, []byte("a#b")); !errors.Is(err, login.ErrNotAwaiting) {
		t.Errorf("a code while starting: %v; want ErrNotAwaiting", err)
	}
	e.m.Cancel(ctx, v2.ID)
	if c := e.next(t, phase(login.Cancelled)); c.ID != v2.ID {
		t.Fatal(c)
	}
	if err := e.m.Cancel(ctx, "000000000000000000000000"); !errors.Is(err, login.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

// TestOneAtATime: twenty concurrent starts make exactly one login.
func TestOneAtATime(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, busy := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.m.Begin(context.Background())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, login.ErrInProgress):
				busy++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || busy != 19 {
		t.Errorf("%d started, %d refused; want 1 and 19", ok, busy)
	}
	e.next(t, phase(login.AwaitingCode))
	if n := len(e.launcher.Launched()); n != 1 {
		t.Errorf("%d processes launched", n)
	}
}

// TestProcessDiesMidLogin: the container dying at the prompt is failed —
// never succeeded, never left waiting for a deadline — and cleaned up.
func TestProcessDiesMidLogin(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	v, _ := e.m.Begin(context.Background())
	e.next(t, phase(login.AwaitingCode))
	p, _ := e.launcher.Proc(v.ID)
	syscall.Kill(p.Pid(), syscall.SIGKILL)
	f := e.next(t, phase(login.Failed))
	if f.Problem == nil || *f.Problem != login.ProblemExited {
		t.Errorf("%+v", f)
	}
	if !contains(e.launcher.Removed(), v.ID) {
		t.Error("not removed")
	}
	if len(e.ids.calls()) != 0 {
		t.Error("a dead process told the watch about a login")
	}
}

// TestLaunchFailureNamesItsProblem: a launcher's problem becomes the view's,
// with the launcher's own sentence when it has one.
func TestLaunchFailureNamesItsProblem(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	e.launcher.Fail = &login.LaunchError{Problem: login.ProblemVolumeOwner, Detail: "uid 4242", Message: "The volume belongs to uid 4242."}
	e.m.Begin(context.Background())
	f := e.next(t, phase(login.Failed))
	if f.Problem == nil || *f.Problem != login.ProblemVolumeOwner || f.Message != "The volume belongs to uid 4242." {
		t.Errorf("%+v", f)
	}
}

// TestShutdownEndsTheLogin: Drydock stopping mid-login fails it, saying so,
// and leaves nothing running.
func TestShutdownEndsTheLogin(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	v, _ := e.m.Begin(context.Background())
	e.next(t, phase(login.AwaitingCode))
	p, _ := e.launcher.Proc(v.ID)
	e.m.Shutdown(10 * time.Second)
	select {
	case <-p.Exited():
	default:
		t.Fatal("shutdown returned with the process running")
	}
	if cur := e.m.Current(); cur == nil || cur.Phase != login.Failed || *cur.Problem != login.ProblemShutdown {
		t.Errorf("after shutdown: %+v", cur)
	}
	if _, err := e.m.Begin(context.Background()); !errors.Is(err, login.ErrShutdown) {
		t.Errorf("begin after shutdown: %v", err)
	}
}

// TestCanarySweep is testing §4.2's sweep for the login code. Codes that are
// high-entropy canaries go in — one wrong, one right — and afterwards neither
// is in any file under the temp root (the database and its WAL, the fake's
// script, state and event log), the service log, any event, the view, or any
// process's argv. The positive controls: the fake confirms each code arrived
// on its stdin (by hash), and the same sweep finds a canary the test plants.
func TestCanarySweep(t *testing.T) {
	good, bad := canary(t), canary(t)
	e := newEnv(t, accept(good), sys.RealClock{})
	ctx := context.Background()
	v, _ := e.m.Begin(ctx)
	e.next(t, phase(login.AwaitingCode))
	for _, c := range []string{bad, good} {
		if err := e.m.Submit(ctx, v.ID, []byte(c)); err != nil {
			t.Fatal(err)
		}
		e.next(t, func(v login.View) bool { return v.Phase == login.InvalidCode || v.Phase == login.Succeeded })
	}
	// /proc/*/cmdline while the process is still settling, then after.
	sweepProc(t, good, bad)
	e.ended(t, v.ID)
	sweepProc(t, good, bad)

	// Control: both codes were really typed.
	subs := claudetest.Kind(e.fake.Events(t), claudetest.EventSubmission)
	if len(subs) != 2 || subs[0].SHA256 != claudetest.CodeSHA256(bad) || subs[1].SHA256 != claudetest.CodeSHA256(good) {
		t.Fatalf("control: fakeclaude saw %+v", subs)
	}

	e.db.DB.Exec(`PRAGMA wal_checkpoint(FULL)`)
	views, _ := json.Marshal(e.m.Current())
	e.logMu.Lock()
	logged := e.logged.String()
	e.logMu.Unlock()
	evs, err := e.log.Since(ctx, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events: %v %d", err, len(evs))
	}
	evJSON, _ := json.Marshal(evs)
	for _, c := range []string{good, bad} {
		for _, part := range codeParts(c) {
			if hits := sweepTree(t, e.root, part); len(hits) > 0 {
				t.Errorf("a code is in %v", hits)
			}
			for name, s := range map[string]string{"service log": logged, "events": string(evJSON), "view": string(views)} {
				if strings.Contains(s, part) {
					t.Errorf("a code is in the %s", name)
				}
			}
		}
	}
	// Control: the sweep finds what is there.
	plant := filepath.Join(e.root, "planted")
	os.WriteFile(plant, []byte("x"+good+"x"), 0o600)
	if hits := sweepTree(t, e.root, good); len(hits) != 1 {
		t.Errorf("control: the sweep found %v", hits)
	}
}

// codeParts is the code and each half: a half of a one-time code is still
// something no file should hold.
func codeParts(c string) []string {
	a, b, _ := strings.Cut(c, "#")
	return []string{c, a, b}
}

func sweepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte(needle)) {
			hits = append(hits, p)
		}
		return nil
	})
	return hits
}

// sweepProc reads every process's argv: the code must never be one.
func sweepProc(t *testing.T, codes ...string) {
	t.Helper()
	ents, _ := os.ReadDir("/proc")
	read := 0
	for _, d := range ents {
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		read++
		for _, c := range codes {
			for _, part := range codeParts(c) {
				if bytes.Contains(b, []byte(part)) {
					t.Errorf("a code is in /proc/%s/cmdline", d.Name())
				}
			}
		}
	}
	if read == 0 {
		t.Error("control: read no /proc/*/cmdline at all")
	}
}

// TestClassifierDecides: the success verdict is the classifier's reading of
// the stream, so the URL the view carries is exactly classify's — the
// pattern, applied per line, never joined to the next line's prompt.
func TestClassifierDecides(t *testing.T) {
	e := newEnv(t, accept("x#y"), sys.RealClock{})
	e.m.Begin(context.Background())
	aw := e.next(t, phase(login.AwaitingCode))
	want, err := classify.ClassifyLogin(claudetest.Transcript(t, claudetest.FixtureLoginPrompt))
	if err != nil {
		t.Fatal(err)
	}
	if *aw.URL != want.AuthorizeURL {
		t.Errorf("URL %q; want the classifier's %q", *aw.URL, want.AuthorizeURL)
	}
}
