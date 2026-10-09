package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type recordingProber struct {
	asked []int
	seen  []preview.Observation
}

func (p *recordingProber) Probe(_ context.Context, ws string, port int, seen preview.Observation) preview.ProbeResult {
	p.seen = append(p.seen, seen)
	p.asked = append(p.asked, port)
	return preview.ProbeResult{Outcome: preview.ProbeAnswering, Message: "m"}
}

// portRoutes is the port routes over a real registry, behind an open gate:
// what is under test is each route's request shape and refusals (the gate's
// order is the meta-tests', which enumerate these routes with every other).
func portRoutes(t *testing.T, domain string) (http.Handler, *preview.Service, *recordingProber, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w1', 1, '/x', 'main', 'running')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	clock := sys.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	svc := &preview.Service{DB: db.DB, Clock: clock, Random: rand.Reader, Domain: domain, Events: events.New(db.DB, clock)}
	pr := &recordingProber{}
	return Build(MuxAPI, allOpen, PortRoutes{Registry: svc, Prober: pr}.Handlers()), svc, pr, db
}

func portCall(h http.Handler, method, path, body string) (int, Error, []byte) {
	r := httptest.NewRequest(method, "https://drydock.example.com"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var env errorEnvelope
	json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Error, rec.Body.Bytes()
}

func TestPortRoutesRequestsAndRefusals(t *testing.T) {
	h, svc, pr, db := portRoutes(t, testPreviewDomain)
	ctx := context.Background()

	// Add: the control, then every malformed body.
	if code, _, body := portCall(h, "POST", "/api/workspaces/w1/ports", `{"container_port":5173,"label":"vite"}`); code != http.StatusAccepted {
		t.Fatalf("add = %d %s", code, body)
	}
	ps, _ := svc.Ports(ctx, "w1", true)
	if len(ps) != 1 || ps[0].Enabled {
		t.Fatalf("after add: %+v", ps)
	}
	id := ps[0].ID
	for _, b := range []string{``, `{}`, `{"container_port":"5173"}`, `{"container_port":5173,"enabled":true}`, `{"container_port":1}{}`, `[1]`} {
		if code, e, _ := portCall(h, "POST", "/api/workspaces/w1/ports", b); code != http.StatusBadRequest || e.Code != CodeBadRequest {
			t.Errorf("add %q = %d %s", b, code, e.Code)
		}
	}
	for b, want := range map[string]string{
		`{"container_port":0}`:                                             CodeBadRequest,
		`{"container_port":5173}`:                                          CodePortExists,
		`{"container_port":80,"host_header":"evil.example"}`:               CodeBadRequest,
		`{"container_port":80,"upstream_scheme":"gopher"}`:                 CodeBadRequest,
		`{"container_port":80,"label":"` + strings.Repeat("x", 101) + `"}`: CodeBadRequest,
	} {
		if _, e, _ := portCall(h, "POST", "/api/workspaces/w1/ports", b); e.Code != want {
			t.Errorf("add %s = %q; want %q", b, e.Code, want)
		}
	}
	if code, e, _ := portCall(h, "POST", "/api/workspaces/nosuch/ports", `{"container_port":1}`); code != 404 || e.Code != CodeNotFound {
		t.Errorf("add to no workspace = %d %s", code, e.Code)
	}

	// Update: the control, then what is not a change.
	if code, _, body := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+id, `{"enabled":true,"host_header":"passthrough"}`); code != http.StatusAccepted {
		t.Fatalf("enable = %d %s", code, body)
	}
	if p, _ := svc.Port(ctx, "w1", id); !p.Enabled || p.HostHeader != preview.HostPassthrough {
		t.Errorf("after enabling: %+v", p)
	}
	for _, b := range []string{``, `{}`, `{"enabled":null}`, `{"label":null}`, `{"enabled":"yes"}`, `{"slug":"mine"}`, `{"enabled":true,"upstream":"10.0.0.1"}`} {
		if code, e, _ := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+id, b); code != http.StatusBadRequest || e.Code != CodeBadRequest {
			t.Errorf("update %q = %d %s", b, code, e.Code)
		}
	}
	if code, e, _ := portCall(h, "PATCH", "/api/workspaces/w1/ports/nosuch", `{"enabled":false}`); code != 404 || e.Code != CodeNotFound {
		t.Errorf("update of no port = %d %s", code, e.Code)
	}

	// Probe: the row's port, not anything in the path.
	code, _, body := portCall(h, "GET", "/api/workspaces/w1/ports/"+id+"/probe", "")
	if code != 200 || len(pr.asked) != 1 || pr.asked[0] != 5173 || !strings.Contains(string(body), `"outcome":"answering"`) {
		t.Errorf("probe = %d %s, asked %v", code, body, pr.asked)
	}

	// List: hidden only on request.
	portCall(h, "PATCH", "/api/workspaces/w1/ports/"+id, `{"hidden":true}`)
	var l PortList
	_, _, body = portCall(h, "GET", "/api/workspaces/w1/ports", "")
	json.Unmarshal(body, &l)
	if !l.Previews || len(l.Ports) != 0 {
		t.Errorf("the default list = %s", body)
	}
	_, _, body = portCall(h, "GET", "/api/workspaces/w1/ports?hidden=true", "")
	json.Unmarshal(body, &l)
	if len(l.Ports) != 1 || l.Ports[0].ID != id || !l.Ports[0].Hidden {
		t.Errorf("?hidden=true = %s", body)
	}

	// Retire, then everything about the row is gone but the slug.
	if code, _, _ := portCall(h, "DELETE", "/api/workspaces/w1/ports/"+id, ""); code != http.StatusAccepted {
		t.Fatalf("retire = %d", code)
	}
	for _, c := range [][3]string{{"DELETE", "/" + id, ""}, {"PATCH", "/" + id, `{"enabled":true}`}, {"GET", "/" + id + "/probe", ""}} {
		if code, e, _ := portCall(h, c[0], "/api/workspaces/w1/ports"+c[1], c[2]); code != 404 || e.Code != CodeNotFound {
			t.Errorf("%s of a retired port = %d %s", c[0], code, e.Code)
		}
	}
	if len(pr.asked) != 1 {
		t.Errorf("a retired port was probed: %v", pr.asked)
	}

	// A workspace being deleted takes no new port and no enable.
	db.Exec(`UPDATE workspace SET state = 'deleting'`)
	if code, e, _ := portCall(h, "POST", "/api/workspaces/w1/ports", `{"container_port":9}`); code != 409 || e.Code != CodeInProgress {
		t.Errorf("add on a deleting workspace = %d %s", code, e.Code)
	}

	// Without a scanner the rescan is declared, gated, 501.
	if code, e, _ := portCall(h, "POST", "/api/workspaces/w1/ports/rescan", ""); code != http.StatusNotImplemented || e.Code != CodeNotImplemented {
		t.Errorf("rescan with no scanner = %d %s", code, e.Code)
	}
}

type recordingScanner struct {
	discovery string
	asked     []string
	err       error
}

func (s *recordingScanner) Discovery(string) string { return s.discovery }

func (s *recordingScanner) Rescan(_ context.Context, ws string) error {
	s.asked = append(s.asked, ws)
	return s.err
}

// TestRescanRoute: POST …/ports/rescan asks the scanner about the path's
// workspace and answers 202 with nothing to apply; the scanner's refusals
// are the registry's codes, and a scanner stopping is 503 unavailable.
func TestRescanRoute(t *testing.T) {
	sc := &recordingScanner{}
	h := Build(MuxAPI, allOpen, PortRoutes{Registry: &preview.Service{}, Scanner: sc}.Handlers())
	code, _, body := portCall(h, "POST", "/api/workspaces/w1/ports/rescan", "")
	if code != http.StatusAccepted || strings.TrimSpace(string(body)) != "{}" || len(sc.asked) != 1 || sc.asked[0] != "w1" {
		t.Fatalf("rescan = %d %s, asked %v", code, body, sc.asked)
	}
	for err, want := range map[error]struct {
		code int
		name string
	}{
		preview.ErrNoWorkspace:       {404, CodeNotFound},
		preview.ErrWorkspaceDeleting: {409, CodeInProgress},
		life.ErrStopping:             {503, CodeUnavailable},
		life.ErrNotStarted:           {503, CodeUnavailable},
	} {
		sc.err = err
		if code, e, _ := portCall(h, "POST", "/api/workspaces/w1/ports/rescan", ""); code != want.code || e.Code != want.name {
			t.Errorf("%v: %d %s; want %v", err, code, e.Code, want)
		}
	}
}

// TestPortsWithNoPreviewDomain: listed, said so, and never enabled.
func TestPortsWithNoPreviewDomain(t *testing.T) {
	h, svc, _, _ := portRoutes(t, "")
	portCall(h, "POST", "/api/workspaces/w1/ports", `{"container_port":5173}`)
	ps, _ := svc.Ports(context.Background(), "w1", false)
	if len(ps) != 1 || ps[0].Host != nil {
		t.Fatalf("with no domain: %+v", ps)
	}
	var l PortList
	_, _, body := portCall(h, "GET", "/api/workspaces/w1/ports", "")
	json.Unmarshal(body, &l)
	if l.Previews {
		t.Errorf("the list says previews are on: %s", body)
	}
	if code, e, _ := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+ps[0].ID, `{"enabled":true}`); code != 503 || e.Code != CodePreviewsNotConfigured {
		t.Errorf("enable with no domain = %d %s", code, e.Code)
	}
	if code, _, _ := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+ps[0].ID, `{"label":"still editable"}`); code != http.StatusAccepted {
		t.Errorf("control: a relabel with no domain = %d", code)
	}
}

// TestPortRoutesDiagnosis is PF §13 step 6 at the routes: an enable of a port
// discovery sees listening on loopback only is 409 port_loopback with the row
// left off (the control on 0.0.0.0 enables); the probe hands the prober the
// row's observation, which is how the proxy's probe answers it without a dial;
// and the list carries discovery's state, null with no scanner.
func TestPortRoutesDiagnosis(t *testing.T) {
	h, svc, pr, db := portRoutes(t, testPreviewDomain)
	ctx := context.Background()
	for _, n := range []int{5173, 8080} {
		portCall(h, "POST", "/api/workspaces/w1/ports", fmt.Sprintf(`{"container_port":%d}`, n))
	}
	db.Exec(`UPDATE forwarded_port SET observed = 1, observed_state = 'listening', bind_addr = '127.0.0.1' WHERE container_port = 5173`)
	db.Exec(`UPDATE forwarded_port SET observed = 1, observed_state = 'listening', bind_addr = '0.0.0.0' WHERE container_port = 8080`)
	ps, _ := svc.Ports(ctx, "w1", true)
	loop, open := ps[0], ps[1]
	if code, e, _ := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+loop.ID, `{"enabled":true}`); code != http.StatusConflict || e.Code != CodePortLoopback {
		t.Errorf("enabling a loopback port = %d %s; want 409 %s", code, e.Code, CodePortLoopback)
	}
	if p, _ := svc.Port(ctx, "w1", loop.ID); p.Enabled {
		t.Error("the refused enable enabled it")
	}
	if code, _, body := portCall(h, "PATCH", "/api/workspaces/w1/ports/"+open.ID, `{"enabled":true}`); code != http.StatusAccepted {
		t.Errorf("control: enabling a port on 0.0.0.0 = %d %s", code, body)
	}
	portCall(h, "GET", "/api/workspaces/w1/ports/"+loop.ID+"/probe", "")
	if len(pr.seen) != 1 || !pr.seen[0].LoopbackOnly() || pr.seen[0].Bind != "127.0.0.1" {
		t.Errorf("the probe was handed %+v; want the row's loopback observation", pr.seen)
	}

	var l struct {
		Discovery *string `json:"discovery"`
	}
	_, _, body := portCall(h, "GET", "/api/workspaces/w1/ports", "")
	if json.Unmarshal(body, &l); l.Discovery != nil || !strings.Contains(string(body), `"discovery":null`) {
		t.Errorf("with no scanner the list says %s", body)
	}
	sh := Build(MuxAPI, allOpen, PortRoutes{Registry: svc, Scanner: &recordingScanner{discovery: "limited"}}.Handlers())
	_, _, body = portCall(sh, "GET", "/api/workspaces/w1/ports", "")
	if json.Unmarshal(body, &l); l.Discovery == nil || *l.Discovery != "limited" {
		t.Errorf("with a scanner holding changes back the list says %s", body)
	}
}
