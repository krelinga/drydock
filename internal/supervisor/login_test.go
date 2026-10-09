//go:build linux

package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/login/logintest"
	"github.com/krelinga/drydock/internal/sys"
)

// volumeOfLogin is the shared volume as the watch would read it after a
// login fakeclaude ran: it holds a login exactly when fakeclaude has accepted
// a code — the moment the real `claude auth login` writes the credential.
// Before that it is the first-run state, no file at all.
type volumeOfLogin struct {
	t     *testing.T
	login *claudetest.Fake
	creds []byte
}

func (v volumeOfLogin) accepted() bool {
	for _, e := range claudetest.Kind(v.login.Events(v.t), claudetest.EventSubmission) {
		if e.Verdict == "accepted" {
			return true
		}
	}
	return false
}

func (v volumeOfLogin) Credentials(context.Context) ([]byte, error) {
	if !v.accepted() {
		return nil, nil
	}
	return v.creds, nil
}

func (v volumeOfLogin) AuthStatus(context.Context) ([]byte, error) {
	if !v.accepted() {
		return []byte(`{"loggedIn":false,"authMethod":"none"}`), nil
	}
	return []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"op@example.invalid","orgId":"org"}`), nil
}

// TestASuccessfulHandshakeResumesAWaitingSupervisor is the two Phase 5
// halves end to end, with fakeclaude on both sides: a running workspace's
// supervisor parks in awaiting_login while no one has signed in, the login
// handshake (internal/login, fakeclaude on its PTY) succeeds, the handshake
// tells the expiry watch, the watch stores a live login, emits auth.identity
// and calls its OnChange — and the supervisor resumes from that call to
// serving. Nothing the handshake does reaches the supervisor directly, and
// nothing follows the event log.
//
// The positive control is a handshake that does not succeed: a wrong code
// leaves the login at the prompt, the watch is not told, no auth.identity is
// written, OnChange resumes nothing, and the supervisor stays parked with
// remote-control never run.
func TestASuccessfulHandshakeResumesAWaitingSupervisor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRig(t)
	rc := r.claude(claudetest.Step{Mode: claudetest.RCServe})

	b := make([]byte, 12)
	rand.Read(b)
	good := "cnryGOOD" + hex.EncodeToString(b) + "#state"
	lf := claudetest.Install(t, claudetest.Script{Login: &claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(good)}})
	creds, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "credentials", "ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The fixtures' own clock, so ok.json is a month from expiry.
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC))
	w := &identity.Watch{DB: r.db.DB, Events: r.log, Clock: clock, Volume: "drydock-claude-config",
		Window: 72 * time.Hour, Source: volumeOfLogin{t: t, login: lf, creds: creds}}
	// As the server wires it, with the provisioner's door reduced to the job
	// body it runs: the watch calls OnChange after it stores a verdict that
	// changed, and on a live login every supervisor waiting on one is
	// resumed (internal/provision gives each Resume a supervisor job of its
	// own; internal/server's TestASignInResumesThroughProvisionJobs drives
	// that half). Set before the watch starts: its worker reads it.
	var resumes atomic.Int64
	w.OnChange = func(ctx context.Context, v identity.View) {
		if v.State == nil || !v.State.Live() {
			return
		}
		for _, id := range r.m.AwaitingLogin() {
			resumes.Add(1)
			if err := r.m.Resume(ctx, id); err != nil {
				t.Errorf("Resume(%s): %v", id, err)
			}
		}
	}
	startWatch(t, w)
	// As the server wires it: the supervisor reads the watch's stored verdict.
	r.m.Identity = func(ctx context.Context) (string, bool) {
		v, err := w.Read(ctx)
		if err != nil || v.State == nil {
			return "", false
		}
		return string(*v.State), true
	}
	if v, err := w.Check(ctx); err != nil || v.State == nil || *v.State != identity.Absent {
		t.Fatalf("first check: %+v %v; want absent", v, err)
	}

	cfgDir := filepath.Join(t.TempDir(), "claude-config")
	os.MkdirAll(cfgDir, 0o700)
	lm := &login.Manager{Launcher: &logintest.Launcher{Fake: lf, ConfigDir: cfgDir}, Events: r.log,
		Clock: sys.RealClock{}, Identity: w, Settle: 2 * time.Second}
	lg := life.NewGroup(ctx)
	if err := lm.Start(lg); err != nil {
		t.Fatal(err)
	}
	defer lg.Wait(time.After(10 * time.Second))
	sub := r.log.Subscribe()
	defer r.log.Cancel(sub)
	phase := func(want login.Phase) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case ev := <-sub.C:
				var d struct{ Login login.View }
				if ev.Kind == login.KindLogin && json.Unmarshal(ev.Data, &d) == nil && d.Login.Phase == want {
					return
				}
			case <-deadline:
				t.Fatalf("no %s; current %+v", want, lm.Current())
			}
		}
	}
	identityEvents := func() int {
		evs, _ := r.log.Since(ctx, 0)
		n := 0
		for _, e := range evs {
			if e.Kind == identity.KindIdentity {
				n++
			}
		}
		return n
	}

	r.start()
	r.waitState(AwaitingLogin, ReasonSignedOut)
	before := identityEvents()

	// Control: a wrong code. The login stays at the prompt; nothing is told.
	v, err := lm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	phase(login.AwaitingCode)
	if err := lm.Submit(ctx, v.ID, []byte("wrongcode#state")); err != nil {
		t.Fatal(err)
	}
	phase(login.InvalidCode)
	time.Sleep(300 * time.Millisecond)
	if st, _ := r.row(); st != AwaitingLogin || r.invocations() != 0 || identityEvents() != before || resumes.Load() != 0 {
		t.Fatalf("control: after a wrong code the supervisor is %s with %d remote-control runs, %d new auth.identity and %d resumes",
			st, r.invocations(), identityEvents()-before, resumes.Load())
	}

	// The right code: the handshake succeeds, the watch stores a live
	// login, auth.identity is written, and the supervisor resumes on it.
	if err := lm.Submit(ctx, v.ID, []byte(good)); err != nil {
		t.Fatal(err)
	}
	phase(login.Succeeded)
	r.waitFor(10*time.Second, "an auth.identity after the login", func() bool { return identityEvents() > before })
	r.waitState(Serving, "")
	if got, _ := w.Read(ctx); got.State == nil || *got.State != identity.OK || got.LoggedInAt == nil {
		t.Errorf("the watch after the login: %+v", got)
	}
	if n := len(claudetest.Kind(rc.Events(t), claudetest.EventStart)); n < 1 {
		t.Error("remote-control never ran after the login")
	}
	rc.NoViolations(t)
	lf.NoViolations(t)
}

// fixedVolume is the shared volume holding one credential file (nil: none),
// with auth status answering as Claude Code does for it: loggedIn:true for
// any live tokens, expired ones included (Spike 01), false otherwise.
type fixedVolume struct{ creds []byte }

func (v fixedVolume) Credentials(context.Context) ([]byte, error) { return v.creds, nil }

func (v fixedVolume) AuthStatus(context.Context) ([]byte, error) {
	if !live(v.creds) {
		return []byte(`{"loggedIn":false,"authMethod":"none"}`), nil
	}
	return []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"op@example.invalid","orgId":"org"}`), nil
}

func live(creds []byte) bool {
	var c struct {
		O struct{ AccessToken, RefreshToken string } `json:"claudeAiOauth"`
	}
	return json.Unmarshal(creds, &c) == nil && c.O.AccessToken != "" && c.O.RefreshToken != ""
}

// TestAnExpiredAccessTokenStillStarts is #52's review finding: `expired`
// means the credential file's expiresAt has passed, which dates the access
// token — the refresh token beside it is live, and Claude Code renews the
// access token from it as it starts (Spike 00). So the real watch, reading
// the expired.json fixture (expiresAt an hour before the clock, both tokens
// present), stores expired, and the supervisor starts a server under it
// rather than parking it in awaiting_login for a sign-in nothing needs.
//
// The controls are the two states that really are no login: the blanked
// tombstone and no file at all, read through the same watch, start nothing
// and spend nothing.
func TestAnExpiredAccessTokenStillStarts(t *testing.T) {
	read := func(name string) []byte {
		if name == "" {
			return nil
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "credentials", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		fixture string
		want    identity.State
		starts  bool
	}{
		{"expired.json", identity.Expired, true},
		{"blanked.json", identity.Blanked, false},
		{"", identity.Absent, false},
	} {
		t.Run(string(c.want), func(t *testing.T) {
			r := newRig(t)
			r.claude(claudetest.Step{Mode: claudetest.RCServe})
			// The fixtures' own clock: expired.json lapsed an hour before it.
			clock := sys.NewFakeClock(time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC))
			w := &identity.Watch{DB: r.db.DB, Events: r.log, Clock: clock, Volume: "drydock-claude-config",
				Window: 72 * time.Hour, Source: fixedVolume{creds: read(c.fixture)}}
			startWatch(t, w)
			v, err := w.Check(context.Background())
			if err != nil || v.State == nil || *v.State != c.want {
				t.Fatalf("the watch stored %v (%v); want %s", v.State, err, c.want)
			}
			r.m.Identity = func(ctx context.Context) (string, bool) {
				v, err := w.Read(ctx)
				if err != nil || v.State == nil {
					return "", false
				}
				return string(*v.State), true
			}
			r.start()
			if c.starts {
				r.waitState(Serving, ReasonServing)
				if r.invocations() != 1 {
					t.Errorf("%d starts; want 1", r.invocations())
				}
				return
			}
			r.waitState(AwaitingLogin, ReasonSignedOut)
			time.Sleep(200 * time.Millisecond)
			if n := r.invocations(); n != 0 {
				t.Errorf("%d starts under %s", n, c.want)
			}
			if _, n := r.row(); n != 0 {
				t.Errorf("restart_count %d under %s", n, c.want)
			}
		})
	}
}

// startWatch starts w as Serve does, boot check included, under a group
// stopped and waited for before the database closes (cleanups run
// last-registered first).
func startWatch(t *testing.T, w *identity.Watch) {
	t.Helper()
	g := life.NewGroup(context.Background())
	t.Cleanup(func() { g.Wait(nil) })
	if err := w.Start(g); err != nil {
		t.Fatal(err)
	}
}
