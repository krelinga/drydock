package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

// signInSource is the shared volume as the watch reads it: no login until
// signIn, then the ok.json fixture, with auth status agreeing.
type signInSource struct {
	mu    sync.Mutex
	creds []byte
}

func (s *signInSource) signIn(creds []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = creds
}

func (s *signInSource) Credentials(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds, nil
}

func (s *signInSource) AuthStatus(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		return []byte(`{"loggedIn":false,"authMethod":"none"}`), nil
	}
	return []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"op@example.invalid","orgId":"org"}`), nil
}

// TestASignInResumesThroughProvisionJobs is R5 wired end to end in the real
// server: a running workspace's session server parks in awaiting_login while
// no one has signed in; the volume then holds a login, a check (POST
// /api/auth/claude/check, as Check now sends it) stores it — and the watch's
// OnChange, a direct call, has the provisioner launch a supervisor job for the
// workspace, which starts the server again: a workspace.job of kind
// supervisor follows the auth.identity, and the supervisor leaves
// awaiting_login. Nothing follows the event log to do it (TestOnlySSESubscribes).
//
// The control is the check before the sign-in: it finds the same absent
// volume, announces nothing new, and no supervisor job follows it.
func TestASignInResumesThroughProvisionJobs(t *testing.T) {
	srv, serve := appServer(t, t.TempDir())
	creds, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "credentials", "ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	src := &signInSource{}
	srv.Identity.Source = src
	r := serve()
	r.client.Timeout = 30 * time.Second
	ctx := context.Background()
	cookie := r.signIn(t)
	awaitCatalog(t, r, cookie)

	since := func(after int64) []events.Event {
		evs, err := srv.Events.Since(ctx, after)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("not within 30s: %s", what)
			}
		}
	}
	// supervisorJobs counts the workspace's supervisor jobs ended after mark.
	supervisorJobs := func(id string, mark int64) int {
		n := 0
		for _, e := range since(mark) {
			var d workspace.JobData
			if e.Kind == workspace.KindJob && e.WorkspaceID == id && json.Unmarshal(e.Data, &d) == nil && d.Kind == provision.JobSupervisor {
				n++
			}
		}
		return n
	}
	lastSupervisor := func(id string) workspace.SupervisorData {
		var d workspace.SupervisorData
		for _, e := range since(0) {
			if e.Kind == workspace.KindSupervisor && e.WorkspaceID == id {
				json.Unmarshal(e.Data, &d)
			}
		}
		return d
	}
	check := func() {
		t.Helper()
		resp := r.do(t, req{method: "POST", path: "/api/auth/claude/check", origin: uiOrigin, cookie: cookie})
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("check = %d", resp.StatusCode)
		}
	}
	answered := func(mark int64) bool {
		for _, e := range since(mark) {
			if e.Kind == "auth.identity" || e.Kind == "auth.identity_checked" {
				return true
			}
		}
		return false
	}

	waitFor("the boot check's verdict", func() bool {
		v, err := srv.Identity.Read(ctx)
		return err == nil && v.State != nil
	})
	resp := r.do(t, req{method: "POST", path: "/api/workspaces", origin: uiOrigin, cookie: cookie, body: `{"repository_id":1}`})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var id string
	waitFor("the workspace running, its create over, its server awaiting a login", func() bool {
		ws, err := srv.Workspaces.List(ctx)
		if err != nil || len(ws) != 1 || ws[0].State != workspace.Running {
			return false
		}
		id = ws[0].ID
		ended := false
		for _, e := range since(0) {
			var d workspace.JobData
			if e.Kind == workspace.KindJob && e.WorkspaceID == id && json.Unmarshal(e.Data, &d) == nil && d.Kind == provision.JobCreate {
				ended = true
			}
		}
		return ended && lastSupervisor(id).State == "awaiting_login"
	})

	// Control: a check with nobody signed in yet resumes nothing.
	mark, _ := srv.Events.Latest(ctx)
	check()
	waitFor("the control check's answer", func() bool { return answered(mark) })
	time.Sleep(300 * time.Millisecond)
	if n := supervisorJobs(id, mark); n != 0 {
		t.Fatalf("control: %d supervisor jobs after a check that found no login", n)
	}

	// The sign-in.
	src.signIn(creds)
	mark, _ = srv.Events.Latest(ctx)
	check()
	waitFor("a supervisor job after the sign-in", func() bool { return supervisorJobs(id, mark) == 1 })
	var identityAt, jobAt int64
	for _, e := range since(mark) {
		var d workspace.JobData
		switch {
		case e.Kind == "auth.identity" && identityAt == 0:
			identityAt = e.ID
		case e.Kind == workspace.KindJob && e.WorkspaceID == id && json.Unmarshal(e.Data, &d) == nil && d.Kind == provision.JobSupervisor:
			jobAt = e.ID
		}
	}
	if identityAt == 0 || jobAt < identityAt {
		t.Errorf("auth.identity at %d, the supervisor job's end at %d: want the job after the verdict", identityAt, jobAt)
	}
	if got := lastSupervisor(id); got.State == "awaiting_login" {
		t.Errorf("after the sign-in the session server is still %s (%s)", got.State, got.Reason)
	}
}
