//go:build linux

package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/identity"
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
// tells the expiry watch, the watch stores a live login and emits
// auth.identity — and the supervisor resumes on that event, by itself, to
// serving. Nothing signals the supervisor directly.
//
// The positive control is a handshake that does not succeed: a wrong code
// leaves the login at the prompt, the watch is not told, no auth.identity is
// written, and the supervisor stays parked with remote-control never run.
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
	go r.m.Watch(ctx)

	cfgDir := filepath.Join(t.TempDir(), "claude-config")
	os.MkdirAll(cfgDir, 0o700)
	lm := &login.Manager{Launcher: &logintest.Launcher{Fake: lf, ConfigDir: cfgDir}, Events: r.log,
		Clock: sys.RealClock{}, Identity: w, Settle: 2 * time.Second}
	defer lm.Shutdown(10 * time.Second)
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
	if st, _ := r.row(); st != AwaitingLogin || r.invocations() != 0 || identityEvents() != before {
		t.Fatalf("control: after a wrong code the supervisor is %s with %d remote-control runs and %d new auth.identity",
			st, r.invocations(), identityEvents()-before)
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
