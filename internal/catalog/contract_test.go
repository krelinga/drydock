package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// TestContractRefresh is the catalog end to end against githubtest.NewBackend:
// the fake on every run, the real dev App in the live job. It is the check
// that the probe's reading of GitHub's answers — a directory listing, a 404 —
// gives the right badge on real repositories.
func TestContractRefresh(t *testing.T) {
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := &Catalog{DB: db.DB, GitHub: b.Client, Clock: sys.RealClock{}, Events: events.New(db.DB, sys.RealClock{})}

	res, err := cat.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh (live=%v): %v", b.Live, err)
	}
	if res.Count != 2 {
		t.Errorf("refreshed %d repositories; want the two testbeds", res.Count)
	}
	v, err := cat.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{githubtest.TestbedA: true, githubtest.TestbedB: false}
	for _, r := range v.Repos {
		has, ok := want[r.FullName]
		if !ok {
			t.Errorf("unexpected repository %s", r.FullName)
			continue
		}
		if r.HasDevcontainer == nil || *r.HasDevcontainer != has {
			t.Errorf("%s: has_devcontainer %v; want %v", r.FullName, r.HasDevcontainer, has)
		}
		if r.PushedAt == nil {
			t.Errorf("%s: no pushed_at", r.FullName)
		}
		delete(want, r.FullName)
	}
	if len(want) != 0 {
		t.Errorf("missing from the list: %v", want)
	}
	if len(v.Installations) != 1 || v.Installations[0].Account != githubtest.TestbedAccount {
		t.Errorf("installations %+v", v.Installations)
	}
}
