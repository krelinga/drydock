package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

type stubProvisioner struct {
	err     error
	created []string // "repo|branch"
	started []string
	acted   []string // "stop W1", "rebuild W1", "delete W1 confirm"
}

func (s *stubProvisioner) act(what string) error {
	if s.err != nil {
		return s.err
	}
	s.acted = append(s.acted, what)
	return nil
}

func (s *stubProvisioner) Stop(_ context.Context, id string) error    { return s.act("stop " + id) }
func (s *stubProvisioner) Rebuild(_ context.Context, id string) error { return s.act("rebuild " + id) }
func (s *stubProvisioner) Delete(_ context.Context, id, confirm string) error {
	return s.act("delete " + id + " " + confirm)
}

func (s *stubProvisioner) ApproveConfig(_ context.Context, id, hash, by string) error {
	return s.act("approve " + id + " " + hash + " " + by)
}
func (s *stubProvisioner) DeclineConfig(_ context.Context, id string) error {
	return s.act("decline " + id)
}

func (s *stubProvisioner) Create(_ context.Context, repo int64, branch string) (workspace.Workspace, error) {
	if s.err != nil {
		return workspace.Workspace{}, s.err
	}
	s.created = append(s.created, strings.Join([]string{itoa(repo), branch}, "|"))
	return workspace.Workspace{ID: "01JABCDEFGHJKMNPQRSTVWXYZ0", State: workspace.Pending}, nil
}

func (s *stubProvisioner) Start(_ context.Context, id string) error {
	if s.err != nil {
		return s.err
	}
	s.started = append(s.started, id)
	return nil
}

type stubReader struct {
	views []workspace.View
	cap   int
}

// CapacityOf is the store's own, over a store with this cap.
func (s stubReader) CapacityOf(vs []workspace.View) workspace.Capacity {
	return (&workspace.Store{Cap: s.cap}).CapacityOf(vs)
}

func (s stubReader) Views(context.Context) ([]workspace.View, error) { return s.views, nil }
func (s stubReader) View(_ context.Context, id string) (workspace.View, error) {
	for _, v := range s.views {
		if v.ID == id {
			return v, nil
		}
	}
	return workspace.View{}, workspace.ErrNotFound
}

type stubEvents struct{}

func (stubEvents) Feed(_ context.Context, id string, limit int) ([]events.Event, error) {
	if limit != 50 {
		return nil, errors.New("the detail carries 50 events")
	}
	return []events.Event{{ID: 7, WorkspaceID: id, Level: events.Info, Kind: workspace.KindStep,
		Message: "Up: started.", Data: json.RawMessage(`{"step":"up","status":"started"}`)}}, nil
}

func workspaceMux(p Provisioner, r WorkspaceReader) *http.ServeMux {
	return Build(MuxAPI, stubGate{session: true, origin: true, host: true},
		WorkspaceRoutes{Provisioner: p, Workspaces: r, Events: stubEvents{}}.Handlers())
}

func call(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an envelope: %s", rec.Body)
	}
	return e.Error.Code
}

// TestCreateMapsEachRefusalToItsCode: every refusal the contract names, each
// with its status and code — beside the control, the same request accepted
// with 202 and the new id.
func TestCreateMapsEachRefusalToItsCode(t *testing.T) {
	p := &stubProvisioner{}
	mux := workspaceMux(p, stubReader{})

	rec := call(mux, "POST", "/api/workspaces", `{"repository_id": 101, "branch": "dev"}`)
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"id":"01JABCDEFGHJKMNPQRSTVWXYZ0"}` {
		t.Fatalf("control: %d %s", rec.Code, rec.Body)
	}
	call(mux, "POST", "/api/workspaces", `{"repository_id": 101}`)
	if strings.Join(p.created, " ") != "101|dev 101|" {
		t.Errorf("created %v; an absent branch reaches the provisioner as empty, for it to default", p.created)
	}

	for _, body := range []string{``, `{`, `[]`, `{"branch":"main"}`, `{"repository_id":"101"}`,
		`{"repository_id":0}`, `{"repository_id":-4}`, `{"repository_id":1}{"repository_id":2}`} {
		if rec := call(mux, "POST", "/api/workspaces", body); rec.Code != 400 || errCode(t, rec) != CodeBadRequest {
			t.Errorf("body %q: %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{provision.ErrUnknownRepository, 404, CodeNotFound},
		{workspace.ErrInProgress, 409, CodeInProgress},
		{workspace.ErrAtCap, 409, CodeAtCapacity},
		{provision.ErrNotConfigured, 503, CodeAppNotConfigured},
		{provision.ErrBadBranch, 400, CodeBadRequest},
		{errors.New("disk on fire"), 500, CodeInternal},
	} {
		p.err = c.err
		rec := call(mux, "POST", "/api/workspaces", `{"repository_id": 101}`)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%v: %d %s; want %d %s", c.err, rec.Code, rec.Body, c.status, c.code)
		}
		if strings.Contains(rec.Body.String(), "disk on fire") {
			t.Error("an internal error's text reached the response")
		}
	}
}

func TestStartMapsEachRefusalToItsCode(t *testing.T) {
	p := &stubProvisioner{}
	mux := workspaceMux(p, stubReader{})
	if rec := call(mux, "POST", "/api/workspaces/W1/start", ``); rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != `{}` ||
		len(p.started) != 1 || p.started[0] != "W1" {
		t.Fatalf("control: %d %s %v", rec.Code, rec.Body, p.started)
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{workspace.ErrNotFound, 404, CodeNotFound},
		{workspace.ErrInProgress, 409, CodeInProgress},
		{workspace.ErrAtCap, 409, CodeAtCapacity},
		{provision.ErrNotConfigured, 503, CodeAppNotConfigured},
	} {
		p.err = c.err
		if rec := call(mux, "POST", "/api/workspaces/W1/start", ``); rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}

// TestAtCapacityNamesTheCap: the refusal's detail names the configured cap,
// so the UI's sentence can say the number (frontend §9), on every route that
// can refuse at the cap. The control is a store with no cap, whose refusal
// carries no detail rather than a made-up number.
func TestAtCapacityNamesTheCap(t *testing.T) {
	for _, c := range []struct {
		cap  int
		want string
	}{{7, "The cap is 7."}, {0, ""}} {
		mux := workspaceMux(&stubProvisioner{err: workspace.ErrAtCap}, stubReader{cap: c.cap})
		for _, r := range [][3]string{{"POST", "/api/workspaces", `{"repository_id":1}`},
			{"POST", "/api/workspaces/W1/start", ``}, {"POST", "/api/workspaces/W1/rebuild", ``}} {
			rec := call(mux, r[0], r[1], r[2])
			var e errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != 409 ||
				e.Error.Code != CodeAtCapacity || e.Error.Detail != c.want {
				t.Errorf("cap %d, %s %s: %d %s; want detail %q", c.cap, r[0], r[1], rec.Code, rec.Body, c.want)
			}
		}
	}
}

// TestLifecycleRoutesMapEachRefusalToItsCode: stop, rebuild and delete
// answer 202 {} and pass the id — and for delete the confirm, untrimmed and
// unfolded — and each refusal reaches the client as its own code. The
// confirm's mismatch is confirm_mismatch, not bad_request: the UI's answer to
// it is specific.
func TestLifecycleRoutesMapEachRefusalToItsCode(t *testing.T) {
	p := &stubProvisioner{}
	mux := workspaceMux(p, stubReader{})
	for _, c := range []struct{ method, path, want string }{
		{"POST", "/api/workspaces/W1/stop", "stop W1"},
		{"POST", "/api/workspaces/W1/rebuild", "rebuild W1"},
		{"DELETE", "/api/workspaces/W1?confirm=krelinga/alpha", "delete W1 krelinga/alpha"},
		{"DELETE", "/api/workspaces/W1?confirm=%20krelinga/Alpha", "delete W1  krelinga/Alpha"},
		{"DELETE", "/api/workspaces/W1", "delete W1 "},
		{"DELETE", "/api/workspaces/W1/config-approval", "decline W1"},
	} {
		p.acted = nil
		rec := call(mux, c.method, c.path, ``)
		if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != `{}` || len(p.acted) != 1 || p.acted[0] != c.want {
			t.Errorf("%s %s: %d %s %q; want 202 {} and %q", c.method, c.path, rec.Code, rec.Body, p.acted, c.want)
		}
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{workspace.ErrNotFound, 404, CodeNotFound},
		{workspace.ErrInProgress, 409, CodeInProgress},
		{workspace.ErrAtCap, 409, CodeAtCapacity},
		{provision.ErrNotConfigured, 503, CodeAppNotConfigured},
		{provision.ErrConfirmMismatch, 400, CodeConfirmMismatch},
		{provision.ErrShuttingDown, 503, CodeUnavailable},
		{workspace.ErrNoApproval, 409, CodeApprovalNotPending},
		{workspace.ErrApprovalStale, 409, CodeApprovalStale},
	} {
		p.err = c.err
		for _, r := range [][3]string{{"POST", "/api/workspaces/W1/stop", ``}, {"POST", "/api/workspaces/W1/rebuild", ``},
			{"DELETE", "/api/workspaces/W1?confirm=krelinga/alpha", ``},
			{"POST", "/api/workspaces/W1/config-approval", `{"hash":"sha256:ab"}`},
			{"DELETE", "/api/workspaces/W1/config-approval", ``}} {
			if rec := call(mux, r[0], r[1], r[2]); rec.Code != c.status || errCode(t, rec) != c.code {
				t.Errorf("%s %s with %v: %d %s; want %d %s", r[0], r[1], c.err, rec.Code, rec.Body, c.status, c.code)
			}
		}
	}
}

// TestApproveCarriesTheHashAndTheSession: the hash is passed through as
// sent, the approver is the session the gate attached (here none, so
// "unknown"), and a body without a hash is bad_request with nothing done —
// beside the control that a hash is accepted.
func TestApproveCarriesTheHashAndTheSession(t *testing.T) {
	p := &stubProvisioner{}
	mux := workspaceMux(p, stubReader{})
	rec := call(mux, "POST", "/api/workspaces/W1/config-approval", `{"hash":"sha256:ab"}`)
	if rec.Code != 202 || strings.TrimSpace(rec.Body.String()) != `{}` || len(p.acted) != 1 || p.acted[0] != "approve W1 sha256:ab unknown" {
		t.Errorf("control: %d %s %q", rec.Code, rec.Body, p.acted)
	}
	for _, body := range []string{``, `{}`, `{"hash":""}`, `{"hash":"x"} {}`, `not json`} {
		p.acted = nil
		rec := call(mux, "POST", "/api/workspaces/W1/config-approval", body)
		if rec.Code != 400 || errCode(t, rec) != CodeBadRequest || len(p.acted) != 0 {
			t.Errorf("body %q: %d %s, acted %q", body, rec.Code, rec.Body, p.acted)
		}
	}
}

// TestWorkspaceViewsHaveTheContractShape pins the JSON the frontend builds
// against: field names, null rather than "" for an unset detail and
// container, steps as an object keyed by step, and the detail's events in
// the stream's own shape.
func TestWorkspaceViewsHaveTheContractShape(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cid := "c0ffee"
	views := []workspace.View{
		{ID: "W2", RepositoryID: 102, FullName: "krelinga/b", Branch: "main", State: workspace.Running,
			ContainerID: &cid, CreatedAt: at,
			Steps: map[workspace.Step]workspace.StepOutcome{
				workspace.StepUp:            {Status: "done", At: at},
				workspace.StepSessionServer: {Status: "done", Detail: "Nothing to do yet.", At: at},
			},
			LastAction: &workspace.ActionOutcome{Action: "stop", Step: "container", Status: "failed",
				Detail: "docker could not stop it.", At: at}},
		{ID: "W1", RepositoryID: 101, FullName: "krelinga/a", Branch: "dev", State: workspace.Pending,
			CreatedAt: at, Steps: map[workspace.Step]workspace.StepOutcome{}},
	}
	mux := workspaceMux(&stubProvisioner{}, stubReader{views: views, cap: 3})

	rec := call(mux, "GET", "/api/workspaces", ``)
	want := `{"workspaces":[` +
		`{"id":"W2","repository_id":102,"full_name":"krelinga/b","branch":"main","state":"running","state_detail":null,` +
		`"container_id":"c0ffee","created_at":"2026-10-04T12:00:00Z","steps":{` +
		`"session_server":{"status":"done","detail":"Nothing to do yet.","at":"2026-10-04T12:00:00Z"},` +
		`"up":{"status":"done","at":"2026-10-04T12:00:00Z"}},` +
		`"last_action":{"action":"stop","step":"container","status":"failed","detail":"docker could not stop it.","at":"2026-10-04T12:00:00Z"},"environment_id":null,"supervisor":null,"session":null,"approval":null,"resources":null,"vscode":null},` +
		`{"id":"W1","repository_id":101,"full_name":"krelinga/a","branch":"dev","state":"pending","state_detail":null,` +
		`"container_id":null,"created_at":"2026-10-04T12:00:00Z","steps":{},"last_action":null,"environment_id":null,"supervisor":null,"session":null,"approval":null,"resources":null,"vscode":null}],` +
		`"capacity":{"cap":3,"occupied":2},"disk":null}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("GET /api/workspaces = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}

	rec = call(mux, "GET", "/api/workspaces/W1", ``)
	var detail map[string]json.RawMessage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &detail) != nil {
		t.Fatalf("GET /api/workspaces/W1 = %d %s", rec.Code, rec.Body)
	}
	if string(detail["id"]) != `"W1"` || string(detail["steps"]) != `{}` ||
		string(detail["events"]) != `[{"id":7,"workspace_id":"W1","level":"info","kind":"workspace.step","message":"Up: started.","data":{"step":"up","status":"started"},"at":"0001-01-01T00:00:00Z"}]` {
		t.Errorf("detail %s", rec.Body)
	}
	if rec := call(mux, "GET", "/api/workspaces/NOPE", ``); rec.Code != 404 || errCode(t, rec) != CodeNotFound {
		t.Errorf("unknown id: %d %s", rec.Code, rec.Body)
	}
}
