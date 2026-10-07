package container_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
)

// approveHostNetwork records, as an operator's approval would, the one
// host setting this tier's configurations carry for the test's own sake:
// runArgs --network=host, so a container reaches the fake GitHub on the
// host's loopback (design §6, "What a configuration may ask of the host").
// It is the subset step 3 computes for those configurations, hashed by the
// same function, so a run asks for nothing more — and a test that changes a
// configuration's host access is asked, as the lifecycle test's attack is.
func approveHostNetwork(t *testing.T, db *sql.DB, repos ...int64) {
	t.Helper()
	s := []container.HostSetting{{Field: "runArgs", Source: container.SourceRepository, Value: json.RawMessage(`["--network=host"]`)}}
	b, _ := json.Marshal(s)
	for _, r := range repos {
		if _, err := db.Exec(`INSERT INTO config_approval (repository_id, hash, settings, approved_by, approved_at)
			VALUES (?, ?, ?, 'test', ?)`, r, container.HashSettings(s), string(b), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
}
