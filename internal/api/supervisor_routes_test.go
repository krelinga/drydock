package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

type stubRestarter struct {
	err  error
	asks []string
}

func (s *stubRestarter) RestartSupervisor(_ context.Context, id string) error {
	s.asks = append(s.asks, id)
	return s.err
}

func TestSupervisorRestartRoute(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{nil, http.StatusAccepted, ""},
		{workspace.ErrInProgress, http.StatusConflict, CodeInProgress},
		{workspace.ErrNotFound, http.StatusNotFound, CodeNotFound},
		{provision.ErrNoSupervisor, http.StatusServiceUnavailable, CodeNotConfigured},
	} {
		st := &stubRestarter{err: tc.err}
		h := SupervisorRoutes{Provisioner: st, Workspaces: stubReader{}}.Handlers()["workspaces.supervisor"]
		req := httptest.NewRequest("POST", "/api/workspaces/W1/supervisor", nil)
		req.SetPathValue("id", "W1")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != tc.code {
			t.Errorf("%v: %d, want %d", tc.err, rec.Code, tc.code)
		}
		if tc.want != "" {
			var env struct{ Error struct{ Code string } }
			json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != tc.want {
				t.Errorf("%v: code %q, want %q", tc.err, env.Error.Code, tc.want)
			}
		}
		if len(st.asks) != 1 || st.asks[0] != "W1" {
			t.Errorf("asked %v", st.asks)
		}
	}
}

func TestSupervisorLogsRoute(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	var asked int
	logs := func(id string, n int) ([]LogLine, bool, bool) {
		asked = n
		if id != "W1" {
			return nil, false, false
		}
		return []LogLine{{N: 7, At: at, Text: "Capacity: 1/4"}}, true, true
	}
	h := SupervisorRoutes{Workspaces: stubReader{views: []workspace.View{{ID: "W1"}, {ID: "W2"}}}, Logs: logs}.Handlers()["workspaces.logs"]
	get := func(id, q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/workspaces/"+id+"/logs"+q, nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	rec := get("W1", "")
	if rec.Code != 200 || asked != 200 {
		t.Fatalf("%d, tail %d", rec.Code, asked)
	}
	if got, want := rec.Body.String(), `{"lines":[{"n":7,"at":"2026-10-06T00:00:00Z","text":"Capacity: 1/4"}],"truncated":true,"held":true}`+"\n"; got != want {
		t.Errorf("body %s want %s", got, want)
	}
	if rec := get("W2", "?tail=5"); rec.Code != 200 || rec.Body.String() != `{"lines":[],"truncated":false,"held":false}`+"\n" || asked != 5 {
		t.Errorf("a workspace with no log: %d %s (tail %d)", rec.Code, rec.Body.String(), asked)
	}
	for _, q := range []string{"?tail=0", "?tail=x", "?tail=5001"} {
		if rec := get("W1", q); rec.Code != 400 {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
	if rec := get("W9", ""); rec.Code != 404 {
		t.Errorf("unknown workspace: %d", rec.Code)
	}
}
