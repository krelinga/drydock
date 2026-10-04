package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// DefaultHeartbeat is the gap between `: ping` comments (frontend §4.5 #1).
// Long enough to cost nothing, short enough that a dead-but-open connection
// is noticed, and that a revoked session's stream ends within one beat.
const DefaultHeartbeat = 20 * time.Second

// EventRoutes serves GET /api/events, the stream every browser keeps open.
//
// The wire format, and why:
//
//   - Every event carries `id:`, so the browser sends Last-Event-ID when it
//     reconnects and the gap is replayed through the same reducer as live
//     events (frontend §2.3, §4.3).
//   - Domain events are *unnamed* — no `event:` line — with the kind inside the
//     JSON. EventSource silently drops a named event nobody registered a
//     listener for, so naming them would make a kind the client does not know
//     yet vanish rather than reach the reducer's unknown-kind branch.
//   - `resync` is the one named event, so it can never be mistaken for data:
//     it means "replay cannot close your gap; refetch". It carries the latest
//     id, so the next reconnect resumes from there rather than resyncing again.
type EventRoutes struct {
	Log   *events.Log
	Clock sys.Clock
	// Alive re-checks the stream's session on every heartbeat, without
	// sliding its idle window. A stream outlives the request that opened it,
	// and signing a device out must end that device's stream too.
	Alive     func(ctx context.Context, sessionID string) (bool, error)
	Heartbeat time.Duration
}

// Handlers returns the map Build consumes, keyed by route Name.
func (e EventRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{"events.stream": e.stream}
}

func (e EventRoutes) stream(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFrom(r.Context())
	if !ok { // the gate guarantees a session; refuse rather than stream to nobody
		WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, "Sign in to continue.", "")
		return
	}
	beat := e.Heartbeat
	if beat <= 0 {
		beat = DefaultHeartbeat
	}

	// Subscribe before reading the backlog, so an event written in between is
	// in the channel rather than lost; the overlap is skipped by id below.
	sub := e.Log.Subscribe()
	defer e.Log.Cancel(sub)

	var backlog []events.Event
	resync := false
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		last, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
		if err != nil || last < 0 {
			resync = true // not an id this server issued
		} else if backlog, err = e.Log.Since(r.Context(), last); errors.Is(err, events.ErrResync) {
			resync = true
		} else if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the event log.", "")
			return
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	var sent int64
	if resync {
		latest, err := e.Log.Latest(r.Context())
		if err != nil {
			return
		}
		if writeFrame(w, latest, "resync", []byte(`{}`)) != nil {
			return
		}
		sent = latest
	}
	for _, ev := range backlog {
		if err := writeEvent(w, ev); err != nil {
			return
		}
		sent = ev.ID
	}
	// A comment opens the stream for the browser even when there is nothing
	// to say yet, and pushes the headers through any buffer on the way.
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	// One timer for the whole loop, re-armed only when it fires: re-arming
	// per event would let a busy stream go without a session check forever.
	tick := e.Clock.After(beat)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return // cut off or shutting down; the client reconnects and replays
			}
			if ev.ID <= sent {
				continue
			}
			if writeEvent(w, ev) != nil || rc.Flush() != nil {
				return
			}
			sent = ev.ID
		case <-tick:
			tick = e.Clock.After(beat)
			// Ending the response is how a revoked session finds out: the
			// browser reconnects, gets a 401, and its EventSource closes
			// (frontend §2.3), which the client turns into "signed out".
			if alive, err := e.Alive(r.Context(), sess.ID); err != nil || !alive {
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func writeEvent(w io.Writer, ev events.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return writeFrame(w, ev.ID, "", b)
}

// writeFrame writes one SSE frame. data must be a single line, which
// json.Marshal guarantees: a newline in it would end the frame early, and the
// remainder would be parsed as fields the server never meant to send.
func writeFrame(w io.Writer, id int64, name string, data []byte) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %d\n", id)
	if name != "" {
		fmt.Fprintf(&b, "event: %s\n", name)
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	_, err := io.WriteString(w, b.String())
	return err
}
