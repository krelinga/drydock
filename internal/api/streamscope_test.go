package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// streamAs is another server on env's log whose requests carry cred.
func (env *streamEnv) streamAs(t *testing.T, cred auth.Credential) string {
	t.Helper()
	h := env.routes.Handlers()["events.stream"]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, WithCredential(r, cred)) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// expectHidden reads one frame and wants it to be want's id alone: no data,
// so EventSource dispatches nothing, but the stream's position moves.
func (s *sseReader) expectHidden(want events.Event) {
	s.t.Helper()
	f, ok := s.next()
	if !ok {
		s.t.Fatalf("stream ended; want the id-only frame for %d", want.ID)
	}
	if f.id != itoa(want.ID) || f.data != "" || f.name != "" {
		s.t.Fatalf("got %+v; want id %d alone (a %s is not this credential's to see)", f, want.ID, want.Kind)
	}
}

// TestTokenStreamCarriesOnlyItsKinds is vscode-extension §4.1: a bearer
// credential's stream carries only kinds within its scopes, on the
// Last-Event-ID replay and live alike. A session kind (secret.*,
// api_token.*) and a kind missing from events.Scopes (the filter fails
// closed) are sent as their id alone, so the position still advances: a
// reconnect from the last of them replays nothing and does not resync. The
// controls: the operator's stream over the same log receives every one of
// them, and the token's stream receives the read kinds between them (it was
// live). Dropping the filter on the replay alone fails the replay half.
func TestTokenStreamCarriesOnlyItsKinds(t *testing.T) {
	env := newStreamEnv(t) // the operator's
	reader := tokenWith(auth.ScopeRead, auth.ScopeLifecycle)
	tokURL := env.streamAs(t, reader)

	before := env.emit(t, string(events.KindWorkspaceState))
	backlog := []events.Event{
		env.emit(t, string(events.KindWorkspaceState)),
		env.emit(t, string(events.KindSecretCreated)),
		env.emit(t, string(events.KindAPITokenCreated)),
		env.emit(t, "unregistered.kind"),
		env.emit(t, string(events.KindWorkspaceStep)),
	}
	hidden := map[int64]bool{backlog[1].ID: true, backlog[2].ID: true, backlog[3].ID: true}

	// The replay.
	tok := env.connectURL(t, tokURL, itoa(before.ID))
	op := env.connect(t, itoa(before.ID))
	for _, ev := range backlog {
		if hidden[ev.ID] {
			tok.expectHidden(ev)
		} else {
			tok.expectEvent(ev)
		}
		op.expectEvent(ev)
	}
	tok.expectComment("connected")
	op.expectComment("connected")

	// Live.
	live := []events.Event{
		env.emit(t, string(events.KindSecretRotated)),
		env.emit(t, string(events.KindWorkspaceState)),
		env.emit(t, string(events.KindAPITokenRevoked)),
		env.emit(t, "another.unregistered"),
	}
	tok.expectHidden(live[0])
	tok.expectEvent(live[1])
	tok.expectHidden(live[2])
	tok.expectHidden(live[3])
	for _, ev := range live {
		op.expectEvent(ev)
	}

	// The position moved past the hidden ones: from the last, nothing to
	// replay, no resync.
	again := env.connectURL(t, tokURL, itoa(live[3].ID))
	again.expectComment("connected")
}

// TestTokenWithoutReadSeesNoKind: a credential without read sees no kind at
// all, read ones included (the stream route needs read, so this is the
// filter's own floor). The control: the same events reach a read token.
func TestTokenWithoutReadSeesNoKind(t *testing.T) {
	c := tokenWith(auth.ScopeLifecycle)
	r := tokenWith(auth.ScopeRead)
	for _, k := range []events.Kind{events.KindWorkspaceState, events.KindSessionStatus} {
		if MaySee(c, k) {
			t.Errorf("a token without read may see %s", k)
		}
		if !MaySee(r, k) {
			t.Errorf("control: a read token may not see %s", k)
		}
	}
	for k := range events.Scopes {
		if !MaySee(operator, k) {
			t.Errorf("the operator may not see %s", k)
		}
		if want := events.Scopes[k] == events.ScopeRead; MaySee(r, k) != want {
			t.Errorf("a read token sees %s: %v; want %v", k, !want, want)
		}
	}
}

// TestDetailViewEventsAreScoped: the workspace detail view's events are
// filtered as the stream is, so no path shows a token a session kind. The
// control: the operator's view of the same workspace carries them.
func TestDetailViewEventsAreScoped(t *testing.T) {
	evs := []events.Event{{ID: 3, Kind: events.KindSecretGrants}, {ID: 2, Kind: events.KindWorkspaceState}, {ID: 1, Kind: "unregistered.kind"}}
	read := func(c auth.Credential) string {
		g := gateAs(c)
		mux := Build(MuxAPI, g, WorkspaceRoutes{Provisioner: &stubProvisioner{}, Workspaces: stubReader{views: []workspace.View{{ID: "W1"}}}, Events: fixedEvents(evs)}.Handlers())
		rec := call(mux, "GET", "/api/workspaces/W1", "")
		if rec.Code != 200 {
			t.Fatalf("read = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	tok := read(tokenWith(auth.ScopeRead))
	if strings.Contains(tok, "secret.grants") || strings.Contains(tok, "unregistered.kind") || !strings.Contains(tok, "workspace.state") {
		t.Errorf("a token's view = %s", tok)
	}
	if op := read(operator); !strings.Contains(op, "secret.grants") || !strings.Contains(op, "unregistered.kind") {
		t.Errorf("control: the operator's view = %s", op)
	}
}

type fixedEvents []events.Event

func (f fixedEvents) Feed(context.Context, string, int) ([]events.Event, error) {
	return append([]events.Event(nil), f...), nil
}

// TestRevokedTokenLosesItsStream is Alive generalised to credentials (§4.2):
// with the real token store, a token's stream pings while it lives, and the
// first heartbeat after its revocation ends it. The same for expiry.
func TestRevokedTokenLosesItsStream(t *testing.T) {
	for _, end := range []string{"revoked", "expired"} {
		t.Run(end, func(t *testing.T) {
			b := newBearerRig(t)
			_, meta := b.mint(t, auth.ScopeRead)
			routes := EventRoutes{Log: b.log, Clock: b.clock, Heartbeat: 20 * time.Second, Alive: b.svc.Alive}
			h := routes.Handlers()["events.stream"]
			cred := auth.TokenCredential(meta)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, WithCredential(r, cred)) }))
			t.Cleanup(srv.Close)
			env := &streamEnv{log: b.log, clock: b.clock, url: srv.URL}
			s := env.connect(t, "")
			s.expectComment("connected")
			beat := func(d time.Duration) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for b.clock.Waiting() == 0 {
					if time.Now().After(deadline) {
						t.Fatal("the stream never armed its heartbeat")
					}
					time.Sleep(time.Millisecond)
				}
				b.clock.Advance(d)
			}
			beat(20 * time.Second)
			s.expectComment("ping") // control: alive, it pings
			if end == "revoked" {
				if _, err := b.svc.Tokens.Revoke(context.Background(), meta.ID); err != nil {
					t.Fatal(err)
				}
				beat(20 * time.Second)
			} else {
				beat(auth.DefaultTokenLifetime)
			}
			for {
				f, ok := s.next()
				if !ok {
					break
				}
				if f.comment == "ping" {
					t.Fatalf("the stream pinged after the token was %s", end)
				}
			}
		})
	}
}
