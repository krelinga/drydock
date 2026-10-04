package workspace

import (
	"bytes"
	"context"
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

	// Controls: a failed workspace frees both its repo and its slot.
	f.store.Move(ctx, first.ID, Failed, "")
	if _, err := f.store.Create(ctx, 1, "main"); err != nil {
		t.Errorf("repo 1 after its workspace failed: %v", err)
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
