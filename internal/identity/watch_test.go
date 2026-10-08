package identity

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// The credential fixtures hold absolute expiresAt values, so every clock here
// starts at the fixtures' own recorded_at — never the wall clock, or expiring
// would rot into expired two days after recording (the classifier's tests
// make the same choice for the same reason).

func fixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func recordedAt(t *testing.T) time.Time {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "test", "fixtures", "credentials", "ok.json.meta"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "recorded_at" {
			at, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			if err != nil {
				t.Fatal(err)
			}
			return at
		}
	}
	t.Fatal("ok.json.meta has no recorded_at")
	return time.Time{}
}

// fakeSource returns what it is told and counts what it was asked.
type fakeSource struct {
	mu                    sync.Mutex
	creds, status         []byte
	credsErr, statusErr   error
	credsCalls, authCalls int
}

func (f *fakeSource) set(creds, status []byte, credsErr, statusErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds, f.status, f.credsErr, f.statusErr = creds, status, credsErr, statusErr
}

func (f *fakeSource) Credentials(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credsCalls++
	return f.creds, f.credsErr
}

func (f *fakeSource) AuthStatus(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	return f.status, f.statusErr
}

type harness struct {
	w      *Watch
	src    *fakeSource
	clock  *sys.FakeClock
	log    *events.Log
	db     *sql.DB
	dir    string
	logged *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(recordedAt(t))
	log := events.New(db.DB, clock)
	src := &fakeSource{}
	var logged bytes.Buffer
	var mu sync.Mutex
	w := &Watch{DB: db.DB, Events: log, Clock: clock, Source: src, Volume: "drydock-claude-config",
		Window: 72 * time.Hour, Interval: 6 * time.Hour,
		Logf: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged.WriteString(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n")
		}}
	return &harness{w: w, src: src, clock: clock, log: log, db: db.DB, dir: dir, logged: &logged}
}

func (h *harness) events(t *testing.T) []events.Event {
	t.Helper()
	evs, err := h.log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (h *harness) kinds(t *testing.T) []string {
	var out []string
	for _, e := range h.events(t) {
		out = append(out, e.Kind)
	}
	return out
}

func stateIs(t *testing.T, v View, want State) {
	t.Helper()
	if v.State == nil {
		t.Fatalf("state = nil; want %s", want)
	}
	if *v.State != want {
		t.Fatalf("state = %s; want %s", *v.State, want)
	}
}

func (h *harness) rowState(t *testing.T) string {
	t.Helper()
	var s string
	if err := h.db.QueryRow(`SELECT state FROM claude_identity WHERE id = 1`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEveryFixtureGetsItsVerdict: the four auth-status recordings and the
// credential fixtures, through the watch, each stored and announced. The
// expired case is the classic one: `auth status` says loggedIn:true and the
// verdict is still expired, because the countdown comes from the file.
func TestEveryFixtureGetsItsVerdict(t *testing.T) {
	cases := []struct {
		name, creds, status string
		want                State
		email               bool
	}{
		{"ok", "ok.json", "valid.json", OK, true},
		{"a fresh login: access token eight hours out", "fresh-login.json", "valid.json", OK, true},
		{"expiring", "expiring.json", "valid.json", Expiring, true},
		{"expired despite loggedIn:true", "expired.json", "expired.json", Expired, true},
		{"blanked", "blanked.json", "blanked.json", Blanked, false},
		{"absent", "", "absent.json", Absent, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var creds []byte
			if c.creds != "" {
				creds = fixture(t, "credentials", c.creds)
			}
			h.src.set(creds, fixture(t, "authstatus", c.status), nil, nil)
			v, err := h.w.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			stateIs(t, v, c.want)
			if got := h.rowState(t); got != string(c.want) {
				t.Errorf("stored state = %s; want %s", got, c.want)
			}
			if c.email != (v.AccountEmail != nil) {
				t.Errorf("account_email = %v; want present=%v", v.AccountEmail, c.email)
			}
			if c.email && *v.AccountEmail != "fixture@example.invalid" {
				t.Errorf("account_email = %q", *v.AccountEmail)
			}
			if c.want.Live() != (v.ExpiresAt != nil) || c.want.Live() != (v.LoggedInAt != nil) {
				t.Errorf("expires_at %v, logged_in_at %v beside %s", v.ExpiresAt, v.LoggedInAt, c.want)
			}
			if v.LastCheckedAt == nil || !v.LastCheckedAt.Equal(h.clock.Now()) {
				t.Errorf("last_checked_at = %v; want %v", v.LastCheckedAt, h.clock.Now())
			}
			evs := h.events(t)
			if len(evs) != 1 || evs[0].Kind != KindIdentity {
				t.Fatalf("events = %v; want one %s", h.kinds(t), KindIdentity)
			}
			var data struct{ Identity View }
			if err := json.Unmarshal(evs[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			stateIs(t, data.Identity, c.want)
			// Absent never needs auth status, and is not asked it.
			if c.creds == "" && h.src.authCalls != 0 {
				t.Errorf("absent asked auth status %d times", h.src.authCalls)
			}
		})
	}
}

// TestBlankedIsNotAbsent is the trap §7.3 is about. The two auth-status
// recordings are the same bytes — loggedIn:false — so anything reading only
// them reports both alike. The file separates them. And the tombstone wins
// however the second read goes: loggedIn:true beside it, or a second read
// that fails outright, still yields blanked. The control is absent's own
// verdict, from the same auth-status bytes.
func TestBlankedIsNotAbsent(t *testing.T) {
	blankedStatus := fixture(t, "authstatus", "blanked.json")
	absentStatus := fixture(t, "authstatus", "absent.json")
	if !bytes.Equal(blankedStatus, absentStatus) {
		t.Fatal("the recordings differ; this test's premise is that auth status cannot tell them apart")
	}
	verdict := func(creds, status []byte, statusErr error) State {
		t.Helper()
		h := newHarness(t)
		h.src.set(creds, status, nil, statusErr)
		v, err := h.w.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return *v.State
	}
	blanked := fixture(t, "credentials", "blanked.json")
	if got := verdict(nil, absentStatus, nil); got != Absent {
		t.Fatalf("control: no file = %s; want absent", got)
	}
	if got := verdict(blanked, blankedStatus, nil); got != Blanked {
		t.Errorf("blanked file, loggedIn:false = %s; want blanked", got)
	}
	if got := verdict(blanked, fixture(t, "authstatus", "valid.json"), nil); got != Blanked {
		t.Errorf("blanked file, loggedIn:true = %s; want blanked", got)
	}
	if got := verdict(blanked, nil, &ReadError{Problem: ProblemImage, Detail: "no network"}); got != Blanked {
		t.Errorf("blanked file, auth status unreadable = %s; want blanked", got)
	}
}

// TestUnreadableInputKeepsTheStoredState: each way a check can fail keeps
// the last verdict, updates last_checked_at, says what went wrong — an event
// and check_error — and never writes ok. The control is the first check and
// the last: the same watch does change the stored state when it can read.
func TestUnreadableInputKeepsTheStoredState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.src.set(fixture(t, "credentials", "expiring.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	if v, err := h.w.Check(ctx); err != nil {
		t.Fatal(err)
	} else {
		stateIs(t, v, Expiring)
	}

	failures := []struct {
		name                string
		creds, status       []byte
		credsErr, statusErr error
		problem             Problem
	}{
		{"docker down", nil, nil, &ReadError{Problem: ProblemDocker, Detail: "x"}, nil, ProblemDocker},
		{"corrupt file", fixture(t, "credentials", "corrupt.json"), fixture(t, "authstatus", "valid.json"), nil, nil, ProblemCredentials},
		{"empty file", []byte{}, fixture(t, "authstatus", "valid.json"), nil, nil, ProblemCredentials},
		{"live file, auth status unreadable", fixture(t, "credentials", "ok.json"), nil, nil,
			&ReadError{Problem: ProblemImage, Detail: "no network"}, ProblemImage},
		{"live file, auth status garbage", fixture(t, "credentials", "ok.json"), []byte("{"), nil, nil, ProblemAuthStatus},
		{"live file, loggedIn:false", fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "absent.json"), nil, nil, ProblemDisagree},
	}
	for _, f := range failures {
		h.clock.Advance(time.Minute)
		before := len(h.events(t))
		h.src.set(f.creds, f.status, f.credsErr, f.statusErr)
		v, err := h.w.Check(ctx)
		var re *ReadError
		if !errors.As(err, &re) || re.Problem != f.problem {
			t.Errorf("%s: err = %v; want a %s ReadError", f.name, err, f.problem)
		}
		stateIs(t, v, Expiring)
		if got := h.rowState(t); got != "expiring" {
			t.Errorf("%s: stored state = %s; want expiring kept", f.name, got)
		}
		if v.LastCheckedAt == nil || !v.LastCheckedAt.Equal(h.clock.Now()) {
			t.Errorf("%s: last_checked_at = %v; want the failed check's time", f.name, v.LastCheckedAt)
		}
		if v.CheckError == nil || v.CheckError.Problem != f.problem || !strings.Contains(v.CheckError.Message, "kept") {
			t.Errorf("%s: check_error = %+v", f.name, v.CheckError)
		}
		evs := h.events(t)[before:]
		if len(evs) != 1 || evs[0].Kind != KindCheckFailed || evs[0].Level != events.Warn {
			t.Errorf("%s: events = %+v; want one %s", f.name, evs, KindCheckFailed)
		}
	}

	// Control: a readable check stores its verdict — a change, to blanked —
	// and clears check_error.
	h.clock.Advance(time.Minute)
	h.src.set(fixture(t, "credentials", "blanked.json"), nil, nil, nil)
	v, err := h.w.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stateIs(t, v, Blanked)
	if v.CheckError != nil {
		t.Errorf("check_error survived a good check: %+v", v.CheckError)
	}
}

// TestRecoveryIsAnnouncedEvenWithoutAChange: a failed check put check_error
// on the stream; a good check with the same verdict must take it off, or the
// UI keeps saying the check is failing. The control: a second good check,
// with nothing to announce, emits nothing.
func TestRecoveryIsAnnouncedEvenWithoutAChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	good := func() {
		h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
		if _, err := h.w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	good()
	h.src.set(nil, nil, &ReadError{Problem: ProblemDocker, Detail: "x"}, nil)
	h.w.Check(ctx)
	good()
	good()
	want := []string{KindIdentity, KindCheckFailed, KindIdentity}
	if got := h.kinds(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v; want %v", got, want)
	}
}

// TestFirstCheckFailingInventsNothing: with no row, a failure has no state to
// keep and writes none — the view's state is null, not absent.
func TestFirstCheckFailingInventsNothing(t *testing.T) {
	h := newHarness(t)
	h.src.set(nil, nil, &ReadError{Problem: ProblemDocker, Detail: "x"}, nil)
	v, err := h.w.Check(context.Background())
	if err == nil {
		t.Fatal("a failed read was not an error")
	}
	if v.State != nil {
		t.Errorf("state = %s after a check that read nothing; want null", *v.State)
	}
	var n int
	h.db.QueryRow(`SELECT count(*) FROM claude_identity`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows written by a failed first check", n)
	}
	// Control: the same watch, readable, does write.
	h.src.set(nil, nil, nil, nil)
	if v, err := h.w.Check(context.Background()); err != nil || v.State == nil || *v.State != Absent {
		t.Errorf("control: no file = %v, %v; want absent", v.State, err)
	}
}

// TestTheWindowIsConfiguration: the expiring fixture's login is two days out
// — expiring under the default three days, ok under one day.
func TestTheWindowIsConfiguration(t *testing.T) {
	for _, c := range []struct {
		window time.Duration
		want   State
	}{{72 * time.Hour, Expiring}, {24 * time.Hour, OK}} {
		h := newHarness(t)
		h.w.Window = c.window
		h.src.set(fixture(t, "credentials", "expiring.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
		v, err := h.w.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stateIs(t, v, c.want)
	}
}

// TestLoggedInAtIsWhenTheLoginWasFirstSeen: set by the first live verdict,
// kept across later ones (a refresh moves expires_at, not this), cleared by
// blanked, and set afresh by the next login.
func TestLoggedInAtIsWhenTheLoginWasFirstSeen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.src.set(nil, nil, nil, nil)
	h.w.Check(ctx)
	h.clock.Advance(time.Hour)
	first := h.clock.Now()
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	v, _ := h.w.Check(ctx)
	if v.LoggedInAt == nil || !v.LoggedInAt.Equal(first) {
		t.Fatalf("logged_in_at = %v; want %v", v.LoggedInAt, first)
	}
	h.clock.Advance(time.Hour)
	h.src.set(fixture(t, "credentials", "expiring.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	v, _ = h.w.Check(ctx)
	if v.LoggedInAt == nil || !v.LoggedInAt.Equal(first) {
		t.Errorf("logged_in_at moved on a later check: %v; want %v", v.LoggedInAt, first)
	}
	h.src.set(fixture(t, "credentials", "blanked.json"), nil, nil, nil)
	v, _ = h.w.Check(ctx)
	if v.LoggedInAt != nil || v.AccountEmail != nil || v.ExpiresAt != nil {
		t.Errorf("blanked kept login details: %+v", v)
	}
	h.clock.Advance(time.Hour)
	h.src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	v, _ = h.w.Check(ctx)
	if v.LoggedInAt == nil || !v.LoggedInAt.Equal(h.clock.Now()) {
		t.Errorf("a new login after blanked: logged_in_at = %v; want %v", v.LoggedInAt, h.clock.Now())
	}
}

// TestRunChecksAtBootAndOnTheInterval: Run checks at once, then again when
// the interval passes, and not before.
func TestRunChecksAtBootAndOnTheInterval(t *testing.T) {
	h := newHarness(t)
	h.src.set(nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Run(ctx); close(done) }()
	waitFor(t, func() bool { return h.clock.Waiting() == 1 })
	if n := h.calls(); n != 1 {
		t.Fatalf("checks at boot = %d; want 1", n)
	}
	h.clock.Advance(6*time.Hour - time.Second)
	if n := h.calls(); n != 1 {
		t.Fatalf("checked before the interval: %d", n)
	}
	h.clock.Advance(time.Second)
	waitFor(t, func() bool { return h.calls() == 2 && h.clock.Waiting() == 1 })
	cancel()
	<-done
}

func (h *harness) calls() int {
	h.src.mu.Lock()
	defer h.src.mu.Unlock()
	return h.src.credsCalls
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAFreshLoginIsNotExpiring is the real deployment's finding (2026-10-08):
// after a real sign-in the credential's expiresAt was about eight hours out,
// because it dates the access token, and the watch called the login
// "expiring" from the moment it was made. Through the watch, a fresh login —
// eight-hour access token, refreshTokenExpiresAt thirty days out — is ok:
// stored ok, announced at info with no expiry in its sentence, and the View
// carries both dates, each under its own name. The positive control is the
// expiring fixture — the same access token beside a login that really ends
// in two days — which is stored expiring, announced as a warning, and dated
// by the login, not by the access token.
func TestAFreshLoginIsNotExpiring(t *testing.T) {
	for _, c := range []struct {
		name, creds string
		want        State
		level       events.Level
	}{
		{"fresh login", "fresh-login.json", OK, events.Info},
		{"control: the login ends in two days", "expiring.json", Expiring, events.Warn},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			now := h.clock.Now()
			h.src.set(fixture(t, "credentials", c.creds), fixture(t, "authstatus", "valid.json"), nil, nil)
			v, err := h.w.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			stateIs(t, v, c.want)
			if got := h.rowState(t); got != string(c.want) {
				t.Errorf("stored state = %s; want %s", got, c.want)
			}
			if v.ExpiresAt == nil || v.ExpiresAt.Sub(now) != 8*time.Hour {
				t.Errorf("expires_at = %v; want the access token's, eight hours out", v.ExpiresAt)
			}
			if v.LoginExpiresAt == nil || v.LoginExpiresAt.Sub(now) <= 24*time.Hour {
				t.Fatalf("login_expires_at = %v; want the refresh token's, days out", v.LoginExpiresAt)
			}
			evs := h.events(t)
			if len(evs) != 1 {
				t.Fatalf("events = %v", h.kinds(t))
			}
			if evs[0].Level != c.level {
				t.Errorf("event level = %s; want %s", evs[0].Level, c.level)
			}
			if says := strings.Contains(evs[0].Message, "expires"); says != (c.want == Expiring) {
				t.Errorf("event message %q; want an expiry sentence only when expiring", evs[0].Message)
			}
			if c.want == Expiring && !strings.Contains(evs[0].Message, v.LoginExpiresAt.UTC().Format("2006-01-02 15:04")) {
				t.Errorf("expiring message %q is not dated by the login (%v)", evs[0].Message, v.LoginExpiresAt)
			}
		})
	}
}

// TestALapsedAccessTokenIsInformational: past its eight hours, with no
// session server running to refresh it, the access token's expiresAt is in
// the past while the login is fine. That is expired — live, an info event,
// no countdown — and never a warning. (#61 keeps it from blocking a start:
// supervisor's TestAnExpiredAccessTokenStillStarts.)
func TestALapsedAccessTokenIsInformational(t *testing.T) {
	h := newHarness(t)
	h.src.set(fixture(t, "credentials", "fresh-login.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	if v, err := h.w.Check(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		stateIs(t, v, OK) // control: the same file, before its access token lapses
	}
	h.clock.Advance(9 * time.Hour)
	v, err := h.w.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stateIs(t, v, Expired)
	if !Expired.Live() {
		t.Error("expired is not live; a start would be refused")
	}
	evs := h.events(t)
	if last := evs[len(evs)-1]; last.Level != events.Info || strings.Contains(last.Message, "Sign in") {
		t.Errorf("expired announced as %s %q; want info, asking nothing", last.Level, last.Message)
	}
}
