package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
)

// Two devices writing one secret at once publish their events in the order
// their rows committed, each event carrying the metadata its own commit wrote
// (the events package's row rule). The commit order is measured by a trigger
// that logs every committed description of the row; the publish order is the
// stream's. Written as commit-then-emit, with the Meta read after the commit,
// the two orders part (and an event can carry the other device's description).
func TestConcurrentWritesPublishInCommitOrder(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "API_TOKEN", "v0")
	for _, q := range []string{
		`CREATE TABLE commit_trace (n INTEGER PRIMARY KEY AUTOINCREMENT, description TEXT)`,
		`CREATE TRIGGER commit_trace_secret AFTER UPDATE ON secret BEGIN
			INSERT INTO commit_trace (description) VALUES (NEW.description); END`,
	} {
		if _, err := e.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	const writers, each = 8, 50
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	got := make(chan []events.Event, 1)
	go func() {
		var out []events.Event
		for ev := range sub.C {
			if strings.HasPrefix(ev.Kind, "secret.") && ev.Kind != "secret.undeliverable" && ev.Kind != "secret.deliverable" {
				out = append(out, ev)
			}
			if len(out) == writers*each {
				break
			}
		}
		got <- out
	}()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				// Odd rounds change only the prose (secret.updated), even
				// ones the value too (secret.rotated): both kinds race.
				value := fmt.Sprintf("v-%d-%d", w, i)
				if i%2 == 1 {
					_, err := e.s.PutProse(ctx, "API_TOKEN", "reaches a scratch database", fmt.Sprintf("d-%d-%d", w, i))
					if err != nil {
						t.Error(err)
					}
					continue
				}
				if _, err := e.s.Put(ctx, "API_TOKEN", value, "reaches a scratch database", fmt.Sprintf("d-%d-%d", w, i)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	var published []events.Event
	select {
	case published = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the writes' events were not all published")
	}

	rows, err := e.db.QueryContext(ctx, `SELECT description FROM commit_trace ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	var committed []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		committed = append(committed, d)
	}
	rows.Close()

	var said []string
	var last int64
	for _, ev := range published {
		if ev.ID <= last {
			t.Errorf("event %d published after %d", ev.ID, last)
		}
		last = ev.ID
		var d struct {
			Secret Meta `json:"secret"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		said = append(said, d.Secret.Description)
	}
	// The positive control: every write committed and was announced.
	if len(committed) != writers*each || len(said) != writers*each {
		t.Fatalf("%d commits and %d events, want %d of each", len(committed), len(said), writers*each)
	}
	for i := range committed {
		if said[i] != committed[i] {
			t.Fatalf("event %d says %q, but commit %d wrote %q: publish order is not commit order\ncommitted %v\npublished %v",
				i, said[i], i, committed[i], committed, said)
		}
	}
}
