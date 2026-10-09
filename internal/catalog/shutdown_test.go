package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/sys"
)

// holdingTransport holds the next request it is armed for — the first whose
// path starts with prefix — signalling entered, and hold decides when it
// ends. A request whose context has ended by then fails with that error.
type holdingTransport struct {
	mu      sync.Mutex
	prefix  string
	entered chan struct{}
	hold    func(req *http.Request)
}

func (h *holdingTransport) arm(prefix string, hold func(*http.Request)) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prefix, h.entered, h.hold = prefix, make(chan struct{}), hold
	return h.entered
}

func (h *holdingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	held := h.prefix != "" && strings.HasPrefix(req.URL.Path, h.prefix)
	var entered chan struct{}
	var hold func(*http.Request)
	if held {
		h.prefix, entered, hold = "", h.entered, h.hold
	}
	h.mu.Unlock()
	if held {
		close(entered)
		hold(req)
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func holding(e *env) *holdingTransport {
	h := &holdingTransport{}
	e.cat.GitHub.HTTP = &http.Client{Transport: h}
	return h
}

func (e *env) eventKinds(t *testing.T) []string {
	t.Helper()
	evs, err := e.log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

// logged collects what the catalog writes to the service log.
type logged struct {
	mu    sync.Mutex
	lines []string
}

func (l *logged) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logged) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// refreshed waits for n more repo.refreshed events on sub, and returns each
// one's repository count.
func refreshed(t *testing.T, sub *events.Sub, n int) []int {
	t.Helper()
	var counts []int
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatal("the subscription ended")
			}
			if ev.Kind != KindRefreshed {
				t.Fatalf("event %s; want %s", ev.Kind, KindRefreshed)
			}
			var d struct{ Count int }
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
			counts = append(counts, d.Count)
		case <-time.After(30 * time.Second):
			t.Fatalf("refresh %d of %d never ended", i+1, n)
		}
	}
	return counts
}

// trigger is Trigger with its ticket, through the worker, for a test that
// waits on the answer.
func (e *env) trigger(t *testing.T) life.Ticket {
	t.Helper()
	tk, err := e.cat.w.Trigger()
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	return tk
}

// TestATriggeredRefreshCompletes is the positive control for the shutdown
// tests below: a triggered refresh, with nothing shutting down, lists,
// writes the cache and says so on the stream.
func TestATriggeredRefreshCompletes(t *testing.T) {
	e := newEnv(t)
	e.cat.Trigger()
	if _, err := e.cat.w.Await(waitCtx(t), 1); err != nil {
		t.Fatalf("the triggered refresh: %v", err)
	}
	if n := len(e.list(t)); n != 5 {
		t.Errorf("the triggered refresh cached %d repositories; want 5", n)
	}
	if got := e.eventKinds(t); len(got) != 1 || got[0] != KindRefreshed {
		t.Errorf("events %v; want one %s", got, KindRefreshed)
	}
}

// TestTriggersDuringARefreshQueueOneMore: Triggers while a refresh runs —
// triggered or the periodic one — start nothing then, and together get
// exactly one refresh after it, which emits an event of its own.
func TestTriggersDuringARefreshQueueOneMore(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	release := make(chan struct{})
	entered := h.arm("/app/installations", func(*http.Request) { <-release })
	e.trigger(t)
	<-entered
	var last life.Ticket
	for i := 0; i < 2; i++ {
		last = e.trigger(t)
	}
	if n := e.fake.Count("GET /app/installations"); n != 0 {
		t.Errorf("control: %d listings reached GitHub while the first was held", n)
	}
	close(release)
	refreshed(t, sub, 2)
	if _, err := e.cat.w.Await(waitCtx(t), last); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Count("GET /app/installations"); n != 2 {
		t.Errorf("%d refreshes ran; want the first and one queued after it", n)
	}

	// The periodic refresh gets one more the same way.
	release = make(chan struct{})
	entered = h.arm("/app/installations", func(*http.Request) { <-release })
	waitTimer(t, e)
	e.clock.Advance(DefaultInterval)
	<-entered
	last = e.trigger(t)
	close(release)
	refreshed(t, sub, 2)
	if _, err := e.cat.w.Await(waitCtx(t), last); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Count("GET /app/installations"); n != 4 {
		t.Errorf("%d refreshes ran; want the periodic one and one queued after it, so 4", n)
	}
}

// waitTimer waits until the worker has set its periodic timer, so an
// Advance lands after it.
func waitTimer(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.clock.Waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker never set its periodic timer")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestATriggerAsARefreshEndsIsNotLost: a Trigger after the running refresh
// has emitted its event but before the worker records what it answered
// would, answered by it, be settled by an event the client saw before it
// asked, and its button would never settle. It gets a refresh of its own.
func TestATriggerAsARefreshEndsIsNotLost(t *testing.T) {
	e := newEnv(t)
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	var once sync.Once
	late := make(chan life.Ticket, 1)
	e.cat.mu.Lock()
	e.cat.ending = func() {
		once.Do(func() {
			if got := e.eventKinds(t); len(got) != 1 {
				t.Errorf("control: events %v as the first refresh ends; want its one", got)
			}
			late <- e.trigger(t)
		})
	}
	e.cat.mu.Unlock()
	e.trigger(t)
	refreshed(t, sub, 2)
	if _, err := e.cat.w.Await(waitCtx(t), <-late); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Count("GET /app/installations"); n != 2 {
		t.Errorf("%d refreshes ran; want 2", n)
	}
}

// TestATriggerAfterTheListingListsAgain: a Trigger during a periodic refresh
// whose listing has already returned — the operator just added a repository
// to the installation and pressed Refresh — gets a refresh that lists it,
// not the older one's result.
func TestATriggerAfterTheListingListsAgain(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	release := make(chan struct{})
	// The probes come after the listing: holding the first holds a refresh
	// that has already listed.
	entered := h.arm("/repos/", func(*http.Request) { <-release })
	waitTimer(t, e)
	e.clock.Advance(DefaultInterval)
	<-entered

	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = append(e.fake.Installations[0].Repos,
		githubtest.Repo{ID: 6, FullName: "krelinga/added", DefaultBranch: "main", PushedAt: t0})
	e.fake.Mu.Unlock()
	tk := e.trigger(t)
	close(release)
	counts := refreshed(t, sub, 2)
	res, err := e.cat.w.Await(waitCtx(t), tk)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0] != 5 {
		t.Errorf("control: the refresh that listed before the repository was added counted %d; want 5", counts[0])
	}
	if counts[1] != 6 || res.Count != 6 {
		t.Errorf("the refresh a Trigger asked for counted %d (answered %d); want 6, with the repository added before it", counts[1], res.Count)
	}
	if _, ok := e.list(t)["krelinga/added"]; !ok {
		t.Error("the refresh a Trigger asked for does not cache the repository added before it")
	}
}

// TestShutdownEndsATriggeredRefresh: stopping the catalog's group cancels a
// triggered refresh, and its Wait does not return until it has ended, so
// nothing it does — a GitHub call, a database write — comes after, and the
// refresh a Trigger asked for meanwhile never starts: its ticket is refused.
// The listing it is in ends a moment after its context does, the way a slow
// step would; were Wait to return without waiting, the listing would hold
// until the test has looked.
func TestShutdownEndsATriggeredRefresh(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	var log logged
	e.cat.Logf = log.logf
	stopped, looked := make(chan struct{}), make(chan struct{})
	entered := h.arm("/app/installations", func(req *http.Request) {
		<-req.Context().Done()
		select {
		case <-stopped:
			<-looked
		case <-time.After(200 * time.Millisecond):
		}
	})
	first := e.trigger(t)
	<-entered
	queued := e.trigger(t)
	var late []string
	go func() { late = e.group.Wait(nil); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the group's Wait did not end a triggered refresh")
	}
	if late != nil {
		t.Errorf("stragglers %v after a Wait with no deadline", late)
	}
	close(looked)
	// The running refresh answered its ticket, cut off; the one asked for
	// during it never began, and is refused.
	if _, err := e.cat.w.Await(waitCtx(t), first); !errors.Is(err, context.Canceled) {
		t.Errorf("the refresh shutdown cut off answered %v; want its context's end", err)
	}
	if _, err := e.cat.w.Await(waitCtx(t), queued); !errors.Is(err, life.ErrStopping) {
		t.Errorf("a refresh asked for during the one shutdown cut off: %v; want ErrStopping", err)
	}
	if n := e.fake.Count(""); n != 0 {
		t.Errorf("GitHub saw %d requests from refreshes shutdown ended", n)
	}
	if n := len(e.list(t)); n != 0 {
		t.Errorf("a refresh shutdown ended cached %d repositories", n)
	}
	if got := e.eventKinds(t); len(got) != 0 {
		t.Errorf("a refresh shutdown ended wrote events %v", got)
	}
	if got := log.get(); len(got) != 0 {
		t.Errorf("a refresh shutdown ended logged %q", got)
	}

	// After shutdown a Trigger starts nothing, and Refresh says why.
	if _, err := e.cat.w.Trigger(); !errors.Is(err, life.ErrStopping) {
		t.Errorf("a Trigger after shutdown: %v; want ErrStopping", err)
	}
	if _, err := e.cat.Refresh(waitCtx(t)); !errors.Is(err, life.ErrStopping) {
		t.Errorf("a Refresh after shutdown: %v; want ErrStopping", err)
	}
	if n := e.fake.Count(""); n != 0 {
		t.Errorf("GitHub saw %d requests after shutdown", n)
	}
}

// TestShutdownNamesARefreshThatOutlivesItsWait: a refresh that outlasts the
// bound on the wait — which is the database closing under it — is named by
// the group's Wait, prefixed by the child Serve gives the catalog, for Serve
// to write to the service log. TestShutdownEndsATriggeredRefresh is the
// control: a wait that does not run out names nothing.
func TestShutdownNamesARefreshThatOutlivesItsWait(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	// A catalog started as Serve starts it, under a child named "catalog".
	root := life.NewGroup(context.Background())
	cat := &Catalog{DB: e.cat.DB, Events: e.cat.Events, Clock: e.clock, GitHub: e.cat.GitHub}
	if err := cat.start(root.Child("catalog"), false); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := h.arm("/app/installations", func(*http.Request) { <-release })
	if _, err := cat.w.Trigger(); err != nil {
		t.Fatal(err)
	}
	<-entered
	deadline, stop := sys.NewTimer(e.clock, time.Second)
	defer stop()
	waited := make(chan []string)
	go func() { waited <- root.Wait(deadline) }()
	e.clock.Advance(time.Second)
	got := <-waited
	close(release)
	if len(got) != 1 || got[0] != "catalog/refresh" {
		t.Errorf("Wait named %q; want the catalog's refresh", got)
	}
	if late := root.Wait(nil); late != nil {
		t.Errorf("control: once released, Wait named %q", late)
	}
}
