package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

type stubResources struct {
	of   map[string]*workspace.Resources
	host *workspace.HostDisk
}

func (s stubResources) Of(id string) *workspace.Resources { return s.of[id] }
func (s stubResources) HostDisk() *workspace.HostDisk     { return s.host }

// TestViewsCarryTheSamplersMeasurements: with a sampler, each view carries
// its workspace's resources — null for one never measured, not zeros — and
// the list carries the host's disk; the shape is pinned, as the client reads
// it. The control is TestWorkspaceViewsHaveTheContractShape: no sampler,
// every field null.
func TestViewsCarryTheSamplersMeasurements(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	layer := uint64(300)
	res := stubResources{
		of: map[string]*workspace.Resources{"W2": {At: at,
			Memory: &workspace.MemorySample{Bytes: 1200, At: at},
			Disk:   &workspace.DiskSample{Bytes: 3400, DirectoryBytes: 3100, ContainerBytes: &layer, At: at, Stale: true},
		}},
		host: &workspace.HostDisk{UsedBytes: 91, TotalBytes: 100, LimitPercent: 90, Over: true, At: at},
	}
	views := []workspace.View{{ID: "W2", State: workspace.Running}, {ID: "W1", State: workspace.Stopped}}
	mux := Build(MuxAPI, stubGate{session: true, origin: true, host: true},
		WorkspaceRoutes{Provisioner: &stubProvisioner{}, Workspaces: stubReader{views: views},
			Events: stubEvents{}, Resources: res}.Handlers())

	rec := call(mux, "GET", "/api/workspaces", ``)
	var list struct {
		Workspaces []map[string]json.RawMessage
		Disk       json.RawMessage
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Workspaces) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := `{"at":"2026-10-08T12:00:00Z","memory":{"bytes":1200,"at":"2026-10-08T12:00:00Z","stale":false},` +
		`"disk":{"bytes":3400,"directory_bytes":3100,"container_bytes":300,"partial":false,"at":"2026-10-08T12:00:00Z","stale":true}}`
	if got := string(list.Workspaces[0]["resources"]); got != want {
		t.Errorf("resources\n got %s\nwant %s", got, want)
	}
	if got := string(list.Workspaces[1]["resources"]); got != "null" {
		t.Errorf("a workspace never measured: %s, want null", got)
	}
	if got := string(list.Disk); got != `{"used_bytes":91,"total_bytes":100,"limit_percent":90,"over":true,"at":"2026-10-08T12:00:00Z"}` {
		t.Errorf("disk %s", got)
	}

	rec = call(mux, "GET", "/api/workspaces/W2", ``)
	var detail map[string]json.RawMessage
	if json.Unmarshal(rec.Body.Bytes(), &detail) != nil || string(detail["resources"]) != want {
		t.Errorf("detail resources: %s", detail["resources"])
	}
}

// TestDiskFullIsItsOwnRefusal: the pre-flight's refusal is 507 disk_full on
// every route that can take disk, with the figures in the detail — never a
// 500, and never the generic sentence. The control is the same routes
// accepting with no error (TestStartMapsEachRefusalToItsCode and friends).
func TestDiskFullIsItsOwnRefusal(t *testing.T) {
	full := &provision.DiskFullError{UsedBytes: 93, TotalBytes: 100, LimitPercent: 90}
	mux := workspaceMux(&stubProvisioner{err: full}, stubReader{})
	for _, r := range [][3]string{{"POST", "/api/workspaces", `{"repository_id":1}`},
		{"POST", "/api/workspaces/W1/start", ``}, {"POST", "/api/workspaces/W1/rebuild", ``}} {
		rec := call(mux, r[0], r[1], r[2])
		var e errorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != 507 || e.Error.Code != CodeDiskFull ||
			e.Error.Detail != "The workspace disk is 93% full; Drydock refuses at 90%." ||
			!strings.Contains(e.Error.Message, "Delete a workspace") {
			t.Errorf("%s %s: %d %s", r[0], r[1], rec.Code, rec.Body)
		}
	}
}
