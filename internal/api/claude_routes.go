package api

import (
	"context"
	"net/http"

	"github.com/krelinga/drydock/internal/identity"
)

// IdentityWatch is what the Claude identity routes need from
// internal/identity.
type IdentityWatch interface {
	Read(ctx context.Context) (identity.View, error)
	Trigger()
}

// ClaudeIdentity is GET /api/auth/claude (design §5, frontend §4.5 #2).
//
// Two halves. `identity` is the stored verdict from the expiry watch (§7.3):
// the five-way state, the account and expiry beside a login, when it was last
// checked, and the last check's failure if it failed. `login` is the
// in-flight login handshake (§7.2) so a phone that discarded the page while
// the operator was in their browser can pick it back up (frontend §2.4).
// The handshake is not built yet, so `login` is always null — the key is
// present, so a client written now already handles the shape it will have.
type ClaudeIdentity struct {
	Identity identity.View `json:"identity"`
	// Login is the in-flight handshake: login_id, phase, the authorize URL,
	// the deadline. Null when none is in flight — always, until §7.2 lands.
	Login *struct{} `json:"login"`
}

// ClaudeRoutes serves the identity half of the Claude routes. The two login
// routes stay nil (501) until the handshake lands.
type ClaudeRoutes struct {
	Watch IdentityWatch
}

// Handlers returns the map Build consumes, keyed by route Name.
func (cr ClaudeRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"claude.identity.read":  cr.read,
		"claude.identity.check": cr.check,
	}
}

func (cr ClaudeRoutes) read(w http.ResponseWriter, r *http.Request) {
	v, err := cr.Watch.Read(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the Claude login state.", "")
		return
	}
	writeJSON(w, http.StatusOK, ClaudeIdentity{Identity: v})
}

// check is the async shape: start a check — or join the one running — and
// answer 202. The verdict arrives as auth.identity, a failure as
// auth.identity_check_failed.
func (cr ClaudeRoutes) check(w http.ResponseWriter, r *http.Request) {
	cr.Watch.Trigger()
	writeJSON(w, http.StatusAccepted, struct{}{})
}
