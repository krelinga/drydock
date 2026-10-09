package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github/githubtest"
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

func (e *env) busy() bool {
	e.cat.mu.Lock()
	defer e.cat.mu.Unlock()
	return e.cat.running
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

// TestATriggeredRefreshCompletes is the positive control for the shutdown
// tests below: a triggered refresh, with nothing shutting down, lists,
// writes the cache and says so on the stream.
func TestATriggeredRefreshCompletes(t *testing.T) {
	e := newEnv(t)
	if got := e.cat.trigger(); got != started {
		t.Fatalf("a Trigger before Shutdown: %v; want started", got)
	}
	e.cat.triggers.Wait()
	if n := len(e.list(t)); n != 5 {
		t.Errorf("the triggered refresh cached %d repositories; want 5", n)
	}
	if got := e.eventKinds(t); len(got) != 1 || got[0] != KindRefreshed {
		t.Errorf("events %v; want one %s", got, KindRefreshed)
	}
}

// TestTriggersDuringARefreshQueueOneMore: Triggers while a refresh runs —
// triggered or Run's — start nothing then, and together queue exactly one
// refresh after it, which emits an event of its own.
func TestTriggersDuringARefreshQueueOneMore(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	release := make(chan struct{})
	entered := h.arm("/app/installations", func(*http.Request) { <-release })
	if got := e.cat.trigger(); got != started {
		t.Fatalf("control: the first Trigger: %v; want started", got)
	}
	<-entered
	for i := 0; i < 2; i++ {
		if got := e.cat.trigger(); got != queued {
			t.Errorf("a Trigger during a triggered refresh: %v; want queued", got)
		}
	}
	close(release)
	refreshed(t, sub, 2)
	e.cat.triggers.Wait()
	if n := e.fake.Count("GET /app/installations"); n != 2 {
		t.Errorf("%d refreshes ran; want the first and one queued after it", n)
	}

	// Run's refresh queues one the same way.
	release = make(chan struct{})
	entered = h.arm("/app/installations", func(*http.Request) { <-release })
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { e.cat.Run(ctx, nil); close(ran) }()
	<-entered
	if got := e.cat.trigger(); got != queued {
		t.Errorf("a Trigger during Run's refresh: %v; want queued", got)
	}
	close(release)
	refreshed(t, sub, 2)
	e.cat.triggers.Wait()
	cancel()
	<-ran
	if n := e.fake.Count("GET /app/installations"); n != 4 {
		t.Errorf("%d refreshes ran; want Run's and one queued after it, so 4", n)
	}
	if e.busy() {
		t.Error("a refresh is still running after every one ended")
	}
}

// TestATriggerAsARefreshEndsIsNotLost: a Trigger after the running refresh
// has emitted its event but before it lets go of running would, joining it,
// be answered by an event the client saw before it asked, and its button
// would never settle. It queues a refresh, which emits its own.
func TestATriggerAsARefreshEndsIsNotLost(t *testing.T) {
	e := newEnv(t)
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	var once sync.Once
	var late triggered = -1
	e.cat.ending = func() {
		once.Do(func() {
			if got := e.eventKinds(t); len(got) != 1 {
				t.Errorf("control: events %v as the first refresh ends; want its one", got)
			}
			late = e.cat.trigger()
		})
	}
	if got := e.cat.trigger(); got != started {
		t.Fatalf("control: the first Trigger: %v; want started", got)
	}
	refreshed(t, sub, 2)
	e.cat.triggers.Wait()
	if late != queued {
		t.Errorf("a Trigger as the refresh ended: %v; want queued", late)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{})
	go func() { e.cat.Run(ctx, nil); close(ran) }()
	<-entered

	e.fake.Mu.Lock()
	e.fake.Installations[0].Repos = append(e.fake.Installations[0].Repos,
		githubtest.Repo{ID: 6, FullName: "krelinga/added", DefaultBranch: "main", PushedAt: t0})
	e.fake.Mu.Unlock()
	if got := e.cat.trigger(); got != queued {
		t.Errorf("a Trigger during Run's refresh: %v; want queued", got)
	}
	close(release)
	counts := refreshed(t, sub, 2)
	e.cat.triggers.Wait()
	if counts[0] != 5 {
		t.Errorf("control: the refresh that listed before the repository was added counted %d; want 5", counts[0])
	}
	if counts[1] != 6 {
		t.Errorf("the refresh a Trigger asked for counted %d; want 6, with the repository added before it", counts[1])
	}
	if _, ok := e.list(t)["krelinga/added"]; !ok {
		t.Error("the refresh a Trigger asked for does not cache the repository added before it")
	}
	cancel()
	<-ran
}

// TestShutdownEndsATriggeredRefresh: Shutdown cancels a triggered refresh and
// does not return until it has ended, so nothing it does — a GitHub call, a
// database write — comes after, and the refresh a Trigger queued meanwhile
// never starts. The listing it is in ends a moment after its context does,
// the way a slow step would; were Shutdown to return without waiting, the
// listing would hold until the test has looked.
func TestShutdownEndsATriggeredRefresh(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	var log logged
	e.cat.Logf = log.logf
	shutdown, looked := make(chan struct{}), make(chan struct{})
	entered := h.arm("/app/installations", func(req *http.Request) {
		<-req.Context().Done()
		select {
		case <-shutdown:
			<-looked
		case <-time.After(200 * time.Millisecond):
		}
	})
	if got := e.cat.trigger(); got != started {
		t.Fatalf("control: Trigger: %v; want started", got)
	}
	<-entered
	if got := e.cat.trigger(); got != queued {
		t.Errorf("control: a Trigger during the refresh: %v; want queued", got)
	}
	go func() { e.cat.Shutdown(time.Minute); close(shutdown) }()
	select {
	case <-shutdown:
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not end a triggered refresh")
	}
	if e.busy() {
		t.Error("Shutdown returned with the triggered refresh still running")
	}
	close(looked)
	e.cat.triggers.Wait()
	if n := e.fake.Count(""); n != 0 {
		t.Errorf("GitHub saw %d requests from refreshes Shutdown ended", n)
	}
	if n := len(e.list(t)); n != 0 {
		t.Errorf("a refresh Shutdown ended cached %d repositories", n)
	}
	if got := e.eventKinds(t); len(got) != 0 {
		t.Errorf("a refresh Shutdown ended wrote events %v", got)
	}
	if got := log.get(); len(got) != 0 {
		t.Errorf("a Shutdown that waited the refresh out logged %q", got)
	}

	// After Shutdown a Trigger starts nothing, and the catalog stays idle.
	if got := e.cat.trigger(); got != refused {
		t.Errorf("a Trigger after Shutdown: %v; want refused", got)
	}
	if e.busy() {
		t.Error("a Trigger after Shutdown left a refresh running")
	}
}

// TestShutdownSaysWhenItsWaitRunsOut: a refresh that outlasts Shutdown's
// bound — which is the database closing under it — is written to the
// service log, once. TestShutdownEndsATriggeredRefresh is the control: a
// wait that does not run out logs nothing.
func TestShutdownSaysWhenItsWaitRunsOut(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	var log logged
	e.cat.Logf = log.logf
	release := make(chan struct{})
	entered := h.arm("/app/installations", func(*http.Request) { <-release })
	e.cat.trigger()
	<-entered
	e.cat.Shutdown(time.Millisecond)
	got := log.get()
	close(release)
	e.cat.triggers.Wait()
	if len(got) != 1 || !strings.Contains(got[0], "did not stop within 1ms") {
		t.Errorf("logged %q; want one line saying the refresh did not stop", got)
	}
}
