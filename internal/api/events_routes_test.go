package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type streamEnv struct {
	log   *events.Log
	clock *sys.FakeClock
	alive atomic.Bool
	url   string
	// serve is the handler without a server around it, so a panic in it
	// reaches the test rather than net/http's recover.
	serve http.HandlerFunc
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	env := &streamEnv{clock: sys.NewFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))}
	env.log = events.New(db.DB, env.clock)
	env.alive.Store(true)
	routes := EventRoutes{Log: env.log, Clock: env.clock, Heartbeat: 20 * time.Second,
		Alive: func(_ context.Context, id string) (bool, error) { return id == "s1" && env.alive.Load(), nil }}
	h := routes.Handlers()["events.stream"]
	// Stands in for the gate, which these tests are not about.
	env.serve = func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, auth.Session{ID: "s1"})))
	}
	srv := httptest.NewServer(env.serve)
	t.Cleanup(func() { env.log.Close(); srv.Close() })
	env.url = srv.URL
	return env
}

func (env *streamEnv) emit(t *testing.T, kind string) events.Event {
	t.Helper()
	e, err := env.log.Emit(context.Background(), "ws1", events.Info, kind, "m", map[string]string{"k": kind})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// frame is one SSE frame, or a comment.
type frame struct {
	id, name, data, comment string
}

type sseReader struct {
	t    *testing.T
	body io.ReadCloser
	r    *bufio.Reader
}

func (env *streamEnv) connect(t *testing.T, lastEventID string) *sseReader {
	t.Helper()
	return env.connectURL(t, env.url, lastEventID)
}

func (env *streamEnv) connectURL(t *testing.T, url, lastEventID string) *sseReader {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("connect: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &sseReader{t: t, body: resp.Body, r: bufio.NewReader(resp.Body)}
}

// next reads one frame, failing the test after five seconds. ok is false at
// the end of the stream.
func (s *sseReader) next() (f frame, ok bool) {
	s.t.Helper()
	type res struct {
		f  frame
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		var f frame
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				ch <- res{f, false}
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				ch <- res{f, true}
				return
			case strings.HasPrefix(line, ":"):
				f.comment = strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.name = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data = line[6:]
			}
		}
	}()
	select {
	case r := <-ch:
		return r.f, r.ok
	case <-time.After(5 * time.Second):
		s.t.Fatal("no frame within 5s")
		return frame{}, false
	}
}

func (s *sseReader) expectComment(want string) {
	s.t.Helper()
	if f, ok := s.next(); !ok || f.comment != want {
		s.t.Fatalf("got %+v (open=%v), want comment %q", f, ok, want)
	}
}

func (s *sseReader) expectEvent(want events.Event) {
	s.t.Helper()
	f, ok := s.next()
	if !ok {
		s.t.Fatalf("stream ended; want event %d", want.ID)
	}
	if f.name != "" {
		s.t.Errorf("event %d is named %q: EventSource drops named events without a listener", want.ID, f.name)
	}
	var got events.Event
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		s.t.Fatalf("data %q: %v", f.data, err)
	}
	if f.id != itoa(want.ID) || got.ID != want.ID || got.Kind != want.Kind || string(got.Data) != string(want.Data) {
		s.t.Fatalf("got id %s %+v, want %+v", f.id, got, want)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// The stream delivers an event while the response is still open: an event
// that arrived only when the body ended would be a page that updates on reload.
func TestStreamDeliversLive(t *testing.T) {
	env := newStreamEnv(t)
	s := env.connect(t, "")
	s.expectComment("connected")
	a := env.emit(t, "workspace.state")
	b := env.emit(t, "workspace.step")
	s.expectEvent(a)
	s.expectEvent(b)
}

// Reconnecting with Last-Event-ID replays exactly the gap, then carries on live.
func TestStreamReplaysTheGap(t *testing.T) {
	env := newStreamEnv(t)
	seen := env.emit(t, "one")
	missed1 := env.emit(t, "two")
	missed2 := env.emit(t, "three")

	s := env.connect(t, itoa(seen.ID))
	s.expectEvent(missed1)
	s.expectEvent(missed2)
	s.expectComment("connected")
	live := env.emit(t, "four")
	s.expectEvent(live)

	// Control: with no Last-Event-ID there is no replay at all — the client
	// is on its first load and fetches state instead.
	fresh := env.connect(t, "")
	fresh.expectComment("connected")
	next := env.emit(t, "five")
	fresh.expectEvent(next)
}

// A gap replay cannot close is answered with a named resync carrying the
// latest id, and no partial replay: half a gap applied looks like a whole one.
func TestStreamResync(t *testing.T) {
	env := newStreamEnv(t)
	env.log.ReplayWindow = 2
	var last events.Event
	for i := 0; i < 5; i++ {
		last = env.emit(t, "k")
	}
	for name, header := range map[string]string{
		"too far behind":   "1",
		"ahead of the log": "999",
		"not a number":     "abc",
	} {
		t.Run(name, func(t *testing.T) {
			s := env.connect(t, header)
			f, ok := s.next()
			if !ok || f.name != "resync" || f.id != itoa(last.ID) {
				t.Fatalf("got %+v, want a resync with id %d", f, last.ID)
			}
			s.expectComment("connected") // nothing replayed in between
		})
	}
	// Control: two behind with a window of two replays rather than resyncs.
	s := env.connect(t, itoa(last.ID-2))
	if f, _ := s.next(); f.name == "resync" {
		t.Fatal("control: a gap inside the window was answered with resync")
	}
}

// The heartbeat pings while the session lives, and ends the stream when it
// does not: a device signed out elsewhere must stop receiving events.
func TestStreamEndsWhenTheSessionDies(t *testing.T) {
	env := newStreamEnv(t)
	s := env.connect(t, "")
	s.expectComment("connected")

	beat := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for env.clock.Waiting() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the stream never armed its heartbeat")
			}
			time.Sleep(time.Millisecond)
		}
		env.clock.Advance(20 * time.Second)
	}
	beat()
	s.expectComment("ping") // control: the heartbeat runs, and the session check passes

	env.alive.Store(false)
	beat()
	if f, ok := s.next(); ok {
		t.Fatalf("the stream went on after its session died: %+v", f)
	}
}

// Close is what the server calls before shutting down; without it a stream
// holds Shutdown for its whole timeout.
func TestStreamEndsOnClose(t *testing.T) {
	env := newStreamEnv(t)
	s := env.connect(t, "")
	s.expectComment("connected")
	env.log.Close()
	if f, ok := s.next(); ok {
		t.Fatalf("the stream went on after Close: %+v", f)
	}
}

// A stream asked for after Close — between the server closing the log and
// shutting its listeners, which is when a reconnecting browser arrives — ends
// at once rather than panicking: its subscription is over before it starts,
// and the handler's deferred Cancel must not close it again. The handler is
// called directly, since net/http would recover the panic and hide it.
//
// The control is a stream opened before Close, which is live (it is sent an
// event) until Close ends it.
func TestAStreamAskedForAfterCloseEnds(t *testing.T) {
	env := newStreamEnv(t)
	s := env.connect(t, "")
	s.expectComment("connected")
	s.expectEvent(env.emit(t, "workspace.state"))
	env.log.Close()
	if f, ok := s.next(); ok {
		t.Fatalf("control: the stream went on after Close: %+v", f)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		env.serve(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/events", nil))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stream asked for after Close is still open")
	}
}

// A new EventSource cannot set Last-Event-ID, so the client's hard retry says
// where it was with ?last_event_id= — and gets the same replay. When both are
// present the header wins: an EventSource keeps its URL across its own
// reconnects, so the query is the older position.
func TestStreamResumesFromTheQuery(t *testing.T) {
	env := newStreamEnv(t)
	seen := env.emit(t, "one")
	missed := env.emit(t, "two")
	later := env.emit(t, "three")

	s := env.connectURL(t, env.url+"?last_event_id="+itoa(seen.ID), "")
	s.expectEvent(missed)
	s.expectEvent(later)
	s.expectComment("connected")

	both := env.connectURL(t, env.url+"?last_event_id="+itoa(seen.ID), itoa(missed.ID))
	both.expectEvent(later) // from the header's position, not the query's
	both.expectComment("connected")

	// The query is held to the header's rules: a bad one is a resync.
	bad := env.connectURL(t, env.url+"?last_event_id=abc", "")
	if f, ok := bad.next(); !ok || f.name != "resync" {
		t.Errorf("a malformed last_event_id: %+v; want resync", f)
	}
}
