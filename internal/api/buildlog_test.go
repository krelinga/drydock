package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

type stubBuildLogs map[string]provision.BuildLog

func (s stubBuildLogs) BuildLog(_ context.Context, id string) (provision.BuildLog, bool, error) {
	b, ok := s[id]
	if ok && b.Lines == nil {
		return provision.BuildLog{At: b.At}, true, provision.ErrLogWithheld
	}
	return b, ok, nil
}

// TestBuildLogRoute: a held log is served with its time; a workspace with
// none says held:false with no lines (not an empty log); an unknown
// workspace is 404.
func TestBuildLogRoute(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	logs := stubBuildLogs{"W1": {Lines: []string{"Step 1/2", "error: [redacted]"}, At: at}, "W3": {At: at}}
	views := []workspace.View{{ID: "W1"}, {ID: "W2"}, {ID: "W3"}}
	mux := Build(MuxAPI, stubGate{session: true, origin: true, host: true},
		WorkspaceRoutes{Provisioner: &stubProvisioner{}, Workspaces: stubReader{views: views},
			Events: stubEvents{}, BuildLogs: logs}.Handlers())
	for path, want := range map[string]string{
		"/api/workspaces/W1/build-log": `{"lines":["Step 1/2","error: [redacted]"],"at":"2026-10-08T12:00:00Z","held":true,"withheld":false}`,
		"/api/workspaces/W2/build-log": `{"lines":[],"at":null,"held":false,"withheld":false}`,
		"/api/workspaces/W3/build-log": `{"lines":[],"at":"2026-10-08T12:00:00Z","held":true,"withheld":true}`,
	} {
		if rec := call(mux, "GET", path, ``); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := call(mux, "GET", "/api/workspaces/NOPE/build-log", ``); rec.Code != 404 || errCode(t, rec) != CodeNotFound {
		t.Errorf("unknown: %d %s", rec.Code, rec.Body)
	}
}
