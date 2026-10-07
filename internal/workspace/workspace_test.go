package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type fixture struct {
	store  *Store
	events *events.Log
	dbPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drydock.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	log := events.New(db.DB, clock)
	for id := int64(1); id <= 20; id++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (?, 1, ?, 'main')`,
			id, fmt.Sprintf("krelinga/repo%d", id)); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{
		store: &Store{DB: db.DB, Events: log, Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}},
			Root: "/srv/drydock/ws", Cap: 3},
		events: log, dbPath: path,
	}
}

func (f *fixture) create(t *testing.T, repo int64) Workspace {
	t.Helper()
	w, err := f.store.Create(context.Background(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// stepEvents returns workspace id's workspace.step events as "step:status".
func (f *fixture) stepEvents(t *testing.T, id string) []string {
	t.Helper()
	all, err := f.events.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range all {
		if e.WorkspaceID != id || e.Kind != KindStep {
			continue
		}
		var d struct{ Step, Status string }
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d.Step+":"+d.Status)
	}
	return out
}

func TestStateMachine(t *testing.T) {
	// Deleting is a sink: the property that makes an interrupted delete
	// resumable rather than reversible.
	for _, to := range States {
		if CanMove(Deleting, to) {
			t.Errorf("Deleting → %s is allowed", to)
		}
	}
	// Every other state can be deleted: delete is always offered.
	for _, from := range States[:len(States)-1] {
		if !CanMove(from, Deleting) {
			t.Errorf("%s cannot be deleted", from)
		}
	}
	// Nothing returns to Pending, and nothing reaches Running except from a
	// build: there is no shortcut around the probe.
	for _, from := range States {
		if CanMove(from, Pending) {
			t.Errorf("%s → Pending is allowed", from)
		}
		if CanMove(from, Running) && from != Building {
			t.Errorf("%s → Running skips the build and its probe", from)
		}
	}
	// Controls: the moves the lifecycle needs are all there.
	for _, m := range [][2]State{
		{Pending, Cloning}, {Cloning, Building}, {Building, Running}, {Running, Stopped},
		{Stopped, Building}, {Running, Building}, {Failed, Cloning}, {Failed, Building},
	} {
		if !CanMove(m[0], m[1]) {
			t.Errorf("%s → %s is refused", m[0], m[1])
		}
	}
}

func TestMoveWritesAnEventAndRefusesIllegalMoves(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	if w.State != Pending || !strings.HasPrefix(w.HostPath, "/srv/drydock/ws/"+w.ID) {
		t.Fatalf("created %+v", w)
	}

	if _, err := f.store.Move(ctx, w.ID, Running, ""); !errors.As(err, &ErrIllegalMove{}) {
		t.Fatalf("Pending → Running: %v; want ErrIllegalMove", err)
	}
	if got, _ := f.store.Get(ctx, w.ID); got.State != Pending {
		t.Errorf("a refused move changed the state to %s", got.State)
	}

	before, _ := f.events.Latest(ctx)
	if _, err := f.store.Move(ctx, w.ID, Cloning, ""); err != nil {
		t.Fatal(err)
	}
	evs, _ := f.events.Since(ctx, before)
	if len(evs) != 1 || evs[0].Kind != KindState || string(evs[0].Data) != `{"from":"pending","state":"cloning"}` {
		t.Errorf("a move wrote %+v; want one workspace.state event", evs)
	}
}

// Two movers racing: exactly one wins, and the other is told what it lost to
// rather than overwriting it.
func TestConcurrentMovesHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	f.store.Move(ctx, w.ID, Cloning, "")
	f.store.Move(ctx, w.ID, Building, "")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, to := range []State{Running, Failed} {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = f.store.Move(ctx, w.ID, to, "") }()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	// Running → Failed is legal, so both may legitimately succeed in
	// sequence; what must never happen is a lost update, which shows as
	// the event log disagreeing with the row.
	got, _ := f.store.Get(ctx, w.ID)
	all, _ := f.events.Since(ctx, 0)
	var last string
	for _, e := range all {
		if e.Kind == KindState {
			var d struct{ State string }
			json.Unmarshal(e.Data, &d)
			last = d.State
		}
	}
	if won == 0 || last != string(got.State) {
		t.Errorf("%d winners; the row says %s, the last event says %s", won, got.State, last)
	}
}

// TestStateEventsArePublishedInCommitOrder is the race a post-merge review
// found (#12): a provisioning run's Move(running) and a delete's
// Move(deleting), which startDelete deliberately allows to overlap. When a
// move committed its row and then appended its event as two steps, the
// delete could read `running`, commit `deleting` and publish it in between,
// and the stream then ended on `running` while the row said `deleting` —
// every following client showed Stop on a workspace being deleted. Both
// moves are one events.Commit now, so the newest workspace.state, in the
// table and as published, is the row's state every time.
//
// Many rounds of several pairs at once, because the window it closes is a
// few microseconds wide. Mutation-checked: with Move's UPDATE committed
// before its event is appended, this fails within a few hundred rounds.
func TestStateEventsArePublishedInCommitOrder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.store.Cap = 0
	const pairs = 8
	rounds := 400
	if testing.Short() {
		rounds = 40
	}
	sub := f.events.Subscribe()
	defer f.events.Cancel(sub)
	lastPublished := map[string]string{}
	drain := func() {
		for {
			select {
			case e, ok := <-sub.C:
				if !ok {
					t.Fatal("the subscription was cut off; the test reads too slowly")
				}
				if e.Kind == KindState {
					var d struct{ State string }
					json.Unmarshal(e.Data, &d)
					lastPublished[e.WorkspaceID] = d.State
				}
			default:
				return
			}
		}
	}
	// A positive control on the reader: it sees the states published.
	positive := false
	for round := 0; round < rounds; round++ {
		ws := make([]Workspace, pairs)
		for i := range ws {
			ws[i] = f.create(t, int64(i+1))
			for _, to := range []State{Cloning, Building} {
				if _, err := f.store.Move(ctx, ws[i].ID, to, ""); err != nil {
					t.Fatal(err)
				}
			}
		}
		drain()
		var wg sync.WaitGroup
		for _, w := range ws {
			wg.Add(2)
			go func() { // the provisioning run finishing
				defer wg.Done()
				f.store.Move(ctx, w.ID, Running, "")
			}()
			go func() { // the delete, asked the moment the card says running
				defer wg.Done()
				for {
					got, err := f.store.Get(ctx, w.ID)
					if err != nil {
						t.Error(err)
						return
					}
					if got.State == Running {
						break
					}
				}
				if _, err := f.store.Move(ctx, w.ID, Deleting, ""); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		drain()
		for _, w := range ws {
			got, err := f.store.Get(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != Deleting {
				t.Fatalf("round %d: the row is %s; the delete's move did not land", round, got.State)
			}
			positive = positive || lastPublished[w.ID] != ""
			if lastPublished[w.ID] != string(got.State) {
				t.Fatalf("round %d: the row says %s, the newest published workspace.state says %q", round,
					got.State, lastPublished[w.ID])
			}
			if err := f.store.Remove(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
		}
		if t.Failed() {
			return
		}
	}
	if !positive {
		t.Fatal("no workspace.state was ever published: the check above compared nothing")
	}
}

func TestCreateRefusesADuplicateAndPastTheCap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t) // cap 3
	first := f.create(t, 1)
	if _, err := f.store.Create(ctx, 1, "main"); !errors.Is(err, ErrInProgress) {
		t.Errorf("a second workspace for repo 1: %v; want ErrInProgress", err)
	}
	f.create(t, 2)
	f.create(t, 3)
	if _, err := f.store.Create(ctx, 4, "main"); !errors.Is(err, ErrAtCap) {
		t.Errorf("a fourth with a cap of three: %v; want ErrAtCap", err)
	}

	// A failed workspace frees its slot but not its repository: it still
	// holds the clone, and its way back is start. Same for stopped and
	// deleting — one repository, one workspace, until the delete finishes.
	f.store.Move(ctx, first.ID, Failed, "")
	if _, err := f.store.Create(ctx, 1, "main"); !errors.Is(err, ErrInProgress) {
		t.Errorf("repo 1 while its workspace is failed: %v; want ErrInProgress", err)
	}
	f.store.Move(ctx, first.ID, Deleting, "")
	if _, err := f.store.Create(ctx, 1, "main"); !errors.Is(err, ErrInProgress) {
		t.Errorf("repo 1 while its workspace is deleting: %v; want ErrInProgress", err)
	}
	// Controls: the freed slot takes another repository, and once the row
	// is gone the repository takes a workspace again.
	if _, err := f.store.Create(ctx, 4, "main"); err != nil {
		t.Errorf("repo 4 in the slot the failed workspace freed: %v", err)
	}
	f.store.Remove(ctx, first.ID)
	f.store.Cap = 100
	if _, err := f.store.Create(ctx, 1, "main"); err != nil {
		t.Errorf("repo 1 after its workspace was deleted: %v", err)
	}
}

// Reaching running carries the container id in the event's data, so the
// reducer can show it without a refetch; the control is that no other move
// claims one.
func TestRunningCarriesTheContainerID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	f.store.Move(ctx, w.ID, Cloning, "")
	f.store.Move(ctx, w.ID, Building, "")
	f.store.SetContainer(ctx, w.ID, "c0ffee")
	f.store.Move(ctx, w.ID, Running, "")
	f.store.Move(ctx, w.ID, Stopped, "")
	evs, _ := f.events.ForWorkspace(ctx, w.ID, 10)
	got := map[string]string{}
	for _, ev := range evs {
		var d struct {
			State       string
			ContainerID string `json:"container_id"`
		}
		json.Unmarshal(ev.Data, &d)
		if ev.Kind == KindState {
			got[d.State] = d.ContainerID
		}
	}
	if got["running"] != "c0ffee" {
		t.Errorf("the move to running carries %q", got["running"])
	}
	if got["stopped"] != "" || got["building"] != "" {
		t.Errorf("other moves carry a container id: %v", got)
	}
}

// A double-tap: many creates for one repo at once land exactly one row.
func TestConcurrentCreatesLandOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.store.Cap = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, dup := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.store.Create(ctx, 7, "main")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrInProgress):
				dup++
			default:
				t.Errorf("a racing create failed with %v, not ErrInProgress", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || dup != 9 {
		t.Errorf("%d created, %d refused; want 1 and 9", ok, dup)
	}
}

func allSteps(fail Step, err error, ran *[]Step) map[Step]StepFunc {
	m := map[Step]StepFunc{}
	for _, st := range Steps {
		m[st] = func(context.Context, Workspace) error {
			*ran = append(*ran, st)
			if st == fail {
				return err
			}
			return nil
		}
	}
	return m
}

// Testing §8 "every step writes an event naming itself": the success path
// emits all eight in order, and a scripted failure at each step leaves an
// event naming that step, a Failed workspace saying which, and no later step
// run.
func TestEveryStepWritesAnEventNamingItself(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.store.Cap = 100

	var ran []Step
	ok := f.create(t, 1)
	if err := f.store.Provision(ctx, ok.ID, StepAllocate, allSteps("", nil, &ran)); err != nil {
		t.Fatalf("the success path: %v", err)
	}
	var want []string
	for _, st := range Steps {
		want = append(want, string(st)+":started", string(st)+":done")
	}
	if got := f.stepEvents(t, ok.ID); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("success path events:\n got %v\nwant %v", got, want)
	}
	if w, _ := f.store.Get(ctx, ok.ID); w.State != Running {
		t.Errorf("after every step the workspace is %s, want running", w.State)
	}

	for i, fail := range Steps {
		t.Run(string(fail), func(t *testing.T) {
			var ran []Step
			w := f.create(t, int64(2+i))
			err := f.store.Provision(ctx, w.ID, StepAllocate, allSteps(fail, errors.New("boom"), &ran))
			var se *StepError
			if !errors.As(err, &se) || se.Step != fail {
				t.Fatalf("Provision = %v; want a StepError for %s", err, fail)
			}
			if ran[len(ran)-1] != fail {
				t.Errorf("steps after the failure ran: %v", ran)
			}
			got := f.stepEvents(t, w.ID)
			if last := got[len(got)-1]; last != string(fail)+":failed" {
				t.Errorf("last step event %q; want %s:failed", last, fail)
			}
			after, _ := f.store.Get(ctx, w.ID)
			if fail == StepSessionServer {
				// The container is fine; the supervisor owns the rest.
				if after.State != Running {
					t.Errorf("a failed session server left the workspace %s, want running", after.State)
				}
				return
			}
			if after.State != Failed || !strings.Contains(after.StateDetail, label(fail)) {
				t.Errorf("workspace %s %q; want failed, naming %q", after.State, after.StateDetail, label(fail))
			}
		})
	}
}

// A start or a rebuild runs from the config onward: the clone survives both.
func TestProvisionFromAStoppedWorkspace(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	var ran []Step
	f.store.Provision(ctx, w.ID, StepAllocate, allSteps("", nil, &ran))
	f.store.Move(ctx, w.ID, Stopped, "")

	ran = nil
	steps := allSteps("", nil, &ran)
	delete(steps, StepClone) // not needed from here, so not required
	if err := f.store.Provision(ctx, w.ID, StepResolveConfig, steps); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 6 || ran[0] != StepResolveConfig {
		t.Errorf("a start ran %v", ran)
	}
	// Control: a missing function for a step that will run is refused up
	// front, before any step runs.
	f.store.Move(ctx, w.ID, Stopped, "")
	ran = nil
	delete(steps, StepUp)
	if err := f.store.Provision(ctx, w.ID, StepResolveConfig, steps); err == nil || len(ran) != 0 {
		t.Errorf("a plan missing a step: err %v, ran %v", err, ran)
	}
}

// A step's raw error can carry a token — the clone URL has one (§6 step 2) —
// so only a Public sentence reaches the event log; the raw text reaches only
// the caller. Swept over the database file's raw bytes (testing §4.2).
func TestStepErrorsAreNotWrittenDown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	const canary = "ghs_CanaryToken8Jx2Qv7Lw0"
	const marker = "MarkerSentenceVisible"

	w := f.create(t, 1)
	var ran []Step
	err := f.store.Provision(ctx, w.ID, StepAllocate,
		allSteps(StepClone, fmt.Errorf("git clone https://x-access-token:%s@github.com/k/r.git: exit 128", canary), &ran))
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("control: the caller should get the full error, got %v", err)
	}

	w2 := f.create(t, 2)
	f.store.Provision(ctx, w2.ID, StepAllocate,
		allSteps(StepUp, Public(marker, fmt.Errorf("token %s", canary)), &ran))

	f.store.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	var raw []byte
	for _, p := range []string{f.dbPath, f.dbPath + "-wal"} {
		b, _ := os.ReadFile(p)
		raw = append(raw, b...)
	}
	if !bytes.Contains(raw, []byte(marker)) {
		t.Fatal("control: the public sentence is not in the database, so the sweep proves nothing")
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Error("a step's raw error text reached the database")
	}
}

func TestRemoveOnlyFromDeleting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	if err := f.store.Remove(ctx, w.ID); err == nil {
		t.Error("a pending workspace's row was removed without going through Deleting")
	}
	f.store.Move(ctx, w.ID, Deleting, "")
	if err := f.store.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Remove: %v", err)
	}
}

// TestRemoveTakesTheSupervisorRowAndKeepsTheHistory: Remove takes the row's
// supervisor with it (its foreign key would refuse the delete otherwise) and
// leaves the event log, token_grant and secret_access — the history "which
// workspaces ever held this secret?" is asked of after the fact (§10.4).
func TestRemoveTakesTheSupervisorRowAndKeepsTheHistory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	db := f.store.DB
	for _, q := range []string{
		`INSERT INTO supervisor (id, workspace_id, state, capacity) VALUES ('s1', '` + w.ID + `', 'exited', 4)`,
		`INSERT INTO token_grant (id, workspace_id, repository_id, permissions) VALUES ('g1', '` + w.ID + `', 1, '{}')`,
		`INSERT INTO secret_access (secret_id, workspace_id, at) VALUES ('sec', '` + w.ID + `', 'now')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Remove(ctx, w.ID); err == nil {
		t.Fatal("removed outside deleting")
	}
	var n int
	db.QueryRowContext(ctx, `SELECT count(*) FROM supervisor`).Scan(&n)
	if n != 1 {
		t.Errorf("a refused Remove took the supervisor row")
	}
	f.store.Move(ctx, w.ID, Deleting, "")
	if err := f.store.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"supervisor": 0, "workspace": 0, "token_grant": 1, "secret_access": 1} {
		db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		if n != want {
			t.Errorf("%s has %d rows after Remove, want %d", table, n, want)
		}
	}
	var events int
	db.QueryRowContext(ctx, `SELECT count(*) FROM event WHERE workspace_id = ?`, w.ID).Scan(&events)
	if events < 3 {
		t.Errorf("the workspace's events went with it: %d left", events)
	}
}

// TestRemoveAnnouncesAReleasedRepository: when a workspace's removal takes
// a repository row the installation had dropped, the same commit writes a
// repo.removed event naming it, after workspace.gone — a repo.* event is
// what makes an open catalog refetch, and without one the UI kept showing
// the removed repository. The control is a removal whose repository is still
// installed: workspace.gone alone, and no repo.* event.
func TestRemoveAnnouncesAReleasedRepository(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	kept, released := f.create(t, 1), f.create(t, 2)
	if _, err := f.store.DB.ExecContext(ctx, `UPDATE repository SET removed_at = 'then' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	remove := func(w Workspace) []events.Event {
		t.Helper()
		before, _ := f.events.Since(ctx, 0)
		f.store.Move(ctx, w.ID, Deleting, "")
		if err := f.store.Remove(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		all, _ := f.events.Since(ctx, 0)
		var out []events.Event
		for _, e := range all[len(before):] {
			if e.Kind != KindState {
				out = append(out, e)
			}
		}
		return out
	}

	if got := remove(kept); len(got) != 1 || got[0].Kind != KindGone {
		t.Errorf("control: removing a workspace of an installed repository wrote %+v; want workspace.gone alone", got)
	}
	got := remove(released)
	if len(got) != 2 || got[0].Kind != KindGone || got[1].Kind != store.KindRepositoriesRemoved ||
		got[1].WorkspaceID != "" || string(got[1].Data) != `{"repository_ids":[2]}` {
		t.Fatalf("removing the released repository's last workspace wrote %+v", got)
	}
	if !strings.HasPrefix(got[1].Kind, "repo.") {
		t.Errorf("%s is not a repo.* kind, which is what the catalog refetches on", got[1].Kind)
	}
	var n int
	f.store.DB.QueryRowContext(ctx, `SELECT count(*) FROM repository WHERE id = 2`).Scan(&n)
	if n != 0 {
		t.Error("the event was written but the row was not removed")
	}
}

// TestAnnotateSetsTheDetailWithoutMoving: a delete that stops part-way stays
// deleting and says why; Annotate refuses a workspace not in the state named.
func TestAnnotateSetsTheDetailWithoutMoving(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	if err := f.store.Annotate(ctx, w.ID, Deleting, "stuck"); err == nil {
		t.Error("annotated a pending workspace as deleting")
	}
	f.store.Move(ctx, w.ID, Deleting, "")
	if err := f.store.Annotate(ctx, w.ID, Deleting, "The delete stopped part-way."); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.Get(ctx, w.ID)
	if got.State != Deleting || got.StateDetail != "The delete stopped part-way." {
		t.Errorf("%+v", got)
	}
	all, _ := f.events.Since(ctx, 0)
	last := all[len(all)-1]
	if last.Kind != KindState || !strings.Contains(string(last.Data), `"detail":"The delete stopped part-way."`) ||
		!strings.Contains(string(last.Data), `"state":"deleting"`) {
		t.Errorf("last event %s %s", last.Kind, last.Data)
	}
}

// TestClearDetailOnlyWhenThereIsOne: a retry's clear writes a state event
// with the same state and no detail, and only when there was a detail to
// clear in the state named — so a resume of a delete that never stuck, or a
// stop of a workspace with nothing to say, writes nothing.
func TestClearDetailOnlyWhenThereIsOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := f.create(t, 1)
	f.store.Move(ctx, w.ID, Deleting, "")
	count := func() int {
		all, _ := f.events.Since(ctx, 0)
		return len(all)
	}
	n := count()
	if cleared, err := f.store.ClearDetail(ctx, w.ID, Deleting); err != nil || cleared || count() != n {
		t.Errorf("no detail: cleared %v, err %v, %d events written", cleared, err, count()-n)
	}
	f.store.Annotate(ctx, w.ID, Deleting, "The delete stopped part-way.")
	if cleared, err := f.store.ClearDetail(ctx, w.ID, Running); err != nil || cleared {
		t.Errorf("the wrong state: cleared %v, err %v", cleared, err)
	}
	n = count()
	if cleared, err := f.store.ClearDetail(ctx, w.ID, Deleting); err != nil || !cleared {
		t.Fatalf("cleared %v, err %v", cleared, err)
	}
	all, _ := f.events.Since(ctx, 0)
	if len(all) != n+1 || all[n].Kind != KindState || string(all[n].Data) != `{"from":"deleting","state":"deleting"}` {
		t.Errorf("events after the clear: %+v", all[n:])
	}
	if got, _ := f.store.Get(ctx, w.ID); got.State != Deleting || got.StateDetail != "" {
		t.Errorf("%+v", got)
	}
}

// TestCapacityIsOccupyingCountedOneWay: the count GET /api/workspaces
// reports, the count a start checks (Occupied), and the boundary Create
// enforces are one rule — Occupying — over a workspace in every state. The
// count is checked against Occupying itself, and then against what Create
// does at exactly that cap and one above it, so a count by any other rule
// fails one of the three.
func TestCapacityIsOccupyingCountedOneWay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.store.Cap = 0
	path := map[State][]State{
		Pending: nil, Cloning: {Cloning}, Building: {Cloning, Building}, Running: {Cloning, Building, Running},
		Stopped: {Cloning, Building, Running, Stopped}, Failed: {Failed}, Deleting: {Deleting},
	}
	want := 0
	for i, s := range States {
		w := f.create(t, int64(i+1))
		for _, to := range path[s] {
			if _, err := f.store.Move(ctx, w.ID, to, ""); err != nil {
				t.Fatal(err)
			}
		}
		if Occupying(s) {
			want++
		}
	}
	if want != 4 {
		t.Fatalf("Occupying counts %d states; the test expects pending, cloning, building and running", want)
	}
	vs, err := f.store.Views(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.store.Cap = 9
	c := f.store.CapacityOf(vs)
	if c.Occupied != want || c.Cap == nil || *c.Cap != 9 {
		t.Errorf("CapacityOf = %+v; want %d of 9", c, want)
	}
	if n, err := f.store.Occupied(ctx); err != nil || n != want {
		t.Errorf("Occupied = %d, %v; want %d", n, err, want)
	}
	// The enforced boundary: at a cap of exactly the count a create is
	// refused, and one above it is let through.
	f.store.Cap = c.Occupied
	if _, err := f.store.Create(ctx, 18, "main"); !errors.Is(err, ErrAtCap) {
		t.Errorf("create at a cap of %d = %v; want ErrAtCap", c.Occupied, err)
	}
	f.store.Cap = c.Occupied + 1
	if _, err := f.store.Create(ctx, 18, "main"); err != nil {
		t.Errorf("create at a cap of %d = %v", c.Occupied+1, err)
	}
	if got := (&Store{}).CapacityOf(vs); got.Cap != nil {
		t.Errorf("no cap is reported as %d", *got.Cap)
	}
}

func TestNewID(t *testing.T) {
	zero, _ := NewID(time.UnixMilli(0), bytes.NewReader(make([]byte, 10)))
	if zero != "00000000000000000000000000" {
		t.Errorf("all-zero ULID = %s", zero)
	}
	max, _ := NewID(time.UnixMilli(1<<48-1), bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))
	if max != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Errorf("all-ones ULID = %s", max)
	}
	// The ULID spec's own example timestamp: 1469918176385 ms is 01ARYZ6S41.
	ex, _ := NewID(time.UnixMilli(1469918176385), bytes.NewReader(make([]byte, 10)))
	if !strings.HasPrefix(ex, "01ARYZ6S41") {
		t.Errorf("timestamp prefix = %s, want 01ARYZ6S41", ex[:10])
	}
	// Later ids sort after earlier ones.
	a, _ := NewID(time.UnixMilli(1000), bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))
	b, _ := NewID(time.UnixMilli(1001), bytes.NewReader(make([]byte, 10)))
	if a >= b {
		t.Errorf("%s does not sort before %s", a, b)
	}
}

// A Note is a success with a sentence: the step is done, its detail is the
// sentence, and the run goes on to running. The control is a real failure
// in the same position, which fails the workspace — so the Note's success
// is the Note's doing, not a harness that cannot fail.
func TestANoteIsDoneWithADetail(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var ran []Step
	w := f.create(t, 1)
	steps := allSteps("", nil, &ran)
	steps[StepCredentialVolume] = func(context.Context, Workspace) error { return Note("Nothing to do yet.") }
	if err := f.store.Provision(ctx, w.ID, StepAllocate, steps); err != nil {
		t.Fatal(err)
	}
	v, err := f.store.View(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != Running || v.StateDetail != nil {
		t.Errorf("state %s %v", v.State, v.StateDetail)
	}
	if got := v.Steps[StepCredentialVolume]; got.Status != "done" || got.Detail != "Nothing to do yet." {
		t.Errorf("the noted step: %+v", got)
	}
	if got := v.Steps[StepUp]; got.Status != "done" || got.Detail != "" || got.At.IsZero() {
		t.Errorf("a plain step: %+v", got)
	}
	if len(v.Steps) != len(Steps) || v.FullName != "krelinga/repo1" {
		t.Errorf("view %+v", v)
	}

	f.store.Env.Clock.(*sys.FakeClock).Advance(time.Millisecond) // ids order by their millisecond
	w2 := f.create(t, 2)
	steps[StepCredentialVolume] = func(context.Context, Workspace) error { return errors.New("Nothing to do yet.") }
	f.store.Provision(ctx, w2.ID, StepAllocate, steps)
	if v, _ := f.store.View(ctx, w2.ID); v.State != Failed || v.Steps[StepCredentialVolume].Status != "failed" {
		t.Errorf("control: a plain error is a failure: %s %+v", v.State, v.Steps[StepCredentialVolume])
	}
	vs, err := f.store.Views(ctx)
	if err != nil || len(vs) != 2 || vs[0].ID != w2.ID {
		t.Errorf("Views is not newest first: %v %+v", err, vs)
	}
	if _, err := f.store.View(ctx, "01JABCDEFGHJKMNPQRSTVWXYZ0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("View of no workspace: %v", err)
	}
}

// TestViewsReadOnlyLiveHistoryAndSkipAMalformedEvent is #39's review: the
// list read every step, action, supervisor and session event ever written,
// deleted workspaces' included, and one unreadable event made the whole list
// a 500. Now a deleted workspace's events are not read at all (its malformed
// event is never even seen, so nothing is logged for it), a live one's
// malformed event is skipped and logged, and the list still answers — with
// the well-formed step beside it, which is the positive control.
func TestViewsReadOnlyLiveHistoryAndSkipAMalformedEvent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var logged []string
	f.store.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	gone := f.create(t, 1)
	if _, err := f.store.Move(ctx, gone.ID, Deleting, ""); err != nil {
		t.Fatal(err)
	}
	live := f.create(t, 2)
	if err := f.store.stepEvent(ctx, live.ID, StepClone, "done", events.Info, ""); err != nil {
		t.Fatal(err)
	}
	// Malformed rows, as only a bug or a hand edit could write them: the
	// log's own Append refuses data that is not an object.
	for _, ws := range []string{gone.ID, live.ID} {
		for _, kind := range []string{KindStep, KindAction, KindSupervisor} {
			if _, err := f.store.DB.ExecContext(ctx,
				`INSERT INTO event (workspace_id, level, kind, message, data, at) VALUES (?, 'info', ?, 'x', ?, ?)`,
				ws, kind, `{"step": 7`, "2026-10-04T12:00:00Z"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.store.Remove(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	vs, err := f.store.Views(ctx)
	if err != nil {
		t.Fatalf("one malformed event failed the whole list: %v", err)
	}
	if len(vs) != 1 || vs[0].ID != live.ID {
		t.Fatalf("listed %+v; want only %s", vs, live.ID)
	}
	if got := vs[0].Steps[StepClone]; got.Status != "done" {
		t.Errorf("the well-formed step is %+v; want done", got)
	}
	if vs[0].LastAction != nil || vs[0].Supervisor != nil {
		t.Errorf("a malformed event was reported as %+v / %+v; want nothing known", vs[0].LastAction, vs[0].Supervisor)
	}
	// The malformed step has no readable name, so it is its own group: it
	// is skipped, and the clone step's group still reports its newest event.
	if len(logged) != 3 {
		t.Errorf("logged %d skips; want the live workspace's 3:\n%s", len(logged), strings.Join(logged, "\n"))
	}
	for _, l := range logged {
		if strings.Contains(l, gone.ID) {
			t.Errorf("a deleted workspace's history was read: %s", l)
		}
		if !strings.Contains(l, live.ID) {
			t.Errorf("a skip does not name its workspace: %s", l)
		}
	}
	if _, err := f.store.View(ctx, live.ID); err != nil {
		t.Errorf("the detail view failed on the same event: %v", err)
	}

	// The bound itself, read off the query the list runs: a second clone
	// event for the live workspace replaces the first in the result (newest
	// per step), and nothing of the deleted workspace's is selected.
	if err := f.store.stepEvent(ctx, live.ID, StepClone, "failed", events.Error, ""); err != nil {
		t.Fatal(err)
	}
	q, args := latestEvents()
	rows, err := f.store.DB.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	clones, n := 0, 0
	for rows.Next() {
		var ws, kind, at string
		var data sql.NullString
		if err := rows.Scan(&ws, &kind, &data, &at); err != nil {
			t.Fatal(err)
		}
		n++
		if ws == gone.ID {
			t.Errorf("the list's query read the deleted workspace's %s event", kind)
		}
		if kind == KindStep && strings.Contains(data.String, `"clone"`) {
			clones++
		}
	}
	if n == 0 || clones != 1 {
		t.Errorf("the query returned %d rows with %d clone step events; want rows, and the newest clone event alone", n, clones)
	}
}
