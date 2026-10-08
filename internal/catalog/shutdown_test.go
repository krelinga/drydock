package catalog

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// holdingTransport holds the next listing it is armed for: it signals
// entered, and then hold decides when the request ends.
type holdingTransport struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	hold    func(req *http.Request)
}

func (h *holdingTransport) arm(hold func(*http.Request)) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed, h.entered, h.hold = true, make(chan struct{}), hold
	return h.entered
}

func (h *holdingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	held := h.armed && req.URL.Path == "/app/installations"
	var entered chan struct{}
	var hold func(*http.Request)
	if held {
		h.armed, entered, hold = false, h.entered, h.hold
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

// TestATriggeredRefreshCompletes is the positive control for the shutdown
// tests below: a triggered refresh, with nothing shutting down, lists,
// writes the cache and says so on the stream.
func TestATriggeredRefreshCompletes(t *testing.T) {
	e := newEnv(t)
	if !e.cat.trigger() {
		t.Fatal("a Trigger before Shutdown started nothing")
	}
	e.cat.triggers.Wait()
	if n := len(e.list(t)); n != 5 {
		t.Errorf("the triggered refresh cached %d repositories; want 5", n)
	}
	if got := e.eventKinds(t); len(got) != 1 || got[0] != KindRefreshed {
		t.Errorf("events %v; want one %s", got, KindRefreshed)
	}
}

// TestConcurrentTriggersJoin: a Trigger while a refresh is running starts no
// second one — whether the running one was triggered or Run's — and the one
// running completes for both.
func TestConcurrentTriggersJoin(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	release := make(chan struct{})
	entered := h.arm(func(*http.Request) { <-release })
	if !e.cat.trigger() {
		t.Fatal("control: the first Trigger started nothing")
	}
	<-entered
	if e.cat.trigger() {
		t.Error("a Trigger during a triggered refresh started a second")
	}
	close(release)
	e.cat.triggers.Wait()
	if n := e.fake.Count("GET /app/installations"); n != 1 {
		t.Errorf("%d refreshes ran; want the second Trigger to join the first", n)
	}
	if n := len(e.list(t)); n != 5 {
		t.Errorf("the joined refresh cached %d repositories; want 5", n)
	}

	// Run's refresh is joined the same way.
	release = make(chan struct{})
	entered = h.arm(func(*http.Request) { <-release })
	sub := e.log.Subscribe()
	defer e.log.Cancel(sub)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { e.cat.Run(ctx, nil); close(ran) }()
	<-entered
	if e.cat.trigger() {
		t.Error("a Trigger during Run's refresh started a second")
	}
	close(release)
	if ev := <-sub.C; ev.Kind != KindRefreshed {
		t.Errorf("Run's refresh ended with %s", ev.Kind)
	}
	cancel()
	<-ran
	if n := e.fake.Count("GET /app/installations"); n != 2 {
		t.Errorf("%d refreshes ran; want Run's alone", n)
	}
}

// TestShutdownEndsATriggeredRefresh: Shutdown cancels a triggered refresh and
// does not return until it has ended, so nothing it does — a GitHub call, a
// database write — comes after. The listing it is in ends a moment after
// its context does, the way a slow step would; were Shutdown to return
// without waiting, the listing would hold until the test has looked.
func TestShutdownEndsATriggeredRefresh(t *testing.T) {
	e := newEnv(t)
	h := holding(e)
	shutdown, looked := make(chan struct{}), make(chan struct{})
	entered := h.arm(func(req *http.Request) {
		<-req.Context().Done()
		select {
		case <-shutdown:
			<-looked
		case <-time.After(200 * time.Millisecond):
		}
	})
	if !e.cat.trigger() {
		t.Fatal("control: Trigger started nothing")
	}
	<-entered
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
		t.Errorf("GitHub saw %d requests from a refresh Shutdown ended", n)
	}
	if n := len(e.list(t)); n != 0 {
		t.Errorf("a refresh Shutdown ended cached %d repositories", n)
	}
	if got := e.eventKinds(t); len(got) != 0 {
		t.Errorf("a refresh Shutdown ended wrote events %v", got)
	}

	// After Shutdown a Trigger starts nothing, and the catalog stays idle.
	if e.cat.trigger() {
		t.Error("a Trigger after Shutdown started a refresh")
	}
	if e.busy() {
		t.Error("a Trigger after Shutdown left a refresh running")
	}
}
