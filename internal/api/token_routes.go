package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/krelinga/drydock/internal/auth"
)

// TokenRoutes are the API token routes (vscode-extension §4.2): the three
// /api/tokens routes, the operator's session's alone, and DELETE
// /api/auth/token, a token revoking itself.
type TokenRoutes struct {
	Tokens *auth.Tokens
}

// Handlers returns the map Build consumes, keyed by route Name.
func (t TokenRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"tokens.list":       t.list,
		"tokens.create":     t.create,
		"tokens.revoke":     t.revoke,
		"auth.token.delete": t.revokeSelf,
	}
}

// TokenList is GET /api/tokens: every token's metadata, newest first.
type TokenList struct {
	Tokens []auth.Token `json:"tokens"`
}

func (t TokenRoutes) list(w http.ResponseWriter, r *http.Request) {
	toks, err := t.Tokens.List(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not list the API tokens.", "")
		return
	}
	writeJSON(w, http.StatusOK, TokenList{Tokens: toks})
}

// MintedToken is POST /api/tokens' 201: the token's metadata and, in Token,
// the token itself — the one response in the API that carries a credential,
// which is exactly what minting is. It is written to no event and no log;
// api_token.created carries the metadata alone.
type MintedToken struct {
	auth.Token
	Value string `json:"token"`
}

func (t TokenRoutes) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label         string       `json:"label"`
		Scopes        []auth.Scope `json:"scopes"`
		ExpiresInDays *int         `json:"expires_in_days"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "Send a JSON body with a label, scopes and expires_in_days.", "")
		return
	}
	var lifetime time.Duration // zero: the default
	if body.ExpiresInDays != nil {
		days := *body.ExpiresInDays
		if days < 1 || days > int(auth.MaxTokenLifetime/(24*time.Hour)) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "A token lasts from one day to a year.", "")
			return
		}
		lifetime = time.Duration(days) * 24 * time.Hour
	}
	value, tok, err := t.Tokens.Create(r.Context(), body.Label, body.Scopes, lifetime)
	switch {
	case errors.Is(err, auth.ErrTokenLabel):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "A token needs a label of 1 to 100 characters, with no control characters.", "")
		return
	case errors.Is(err, auth.ErrTokenScopes):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "A token needs one or more of read, lifecycle and approve.", "")
		return
	case errors.Is(err, auth.ErrTokenLifetime):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "A token lasts from one day to a year.", "")
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the token.", "")
		return
	}
	writeJSON(w, http.StatusCreated, MintedToken{Token: tok, Value: value})
}

// revoke is DELETE /api/tokens/{id}: 202, settled by api_token.revoked. A
// token already revoked, or none by that id, is 404.
func (t TokenRoutes) revoke(w http.ResponseWriter, r *http.Request) {
	if _, err := t.Tokens.Revoke(r.Context(), r.PathValue("id")); errors.Is(err, auth.ErrNoToken) {
		WriteError(w, http.StatusNotFound, CodeNotFound, "There is no such live token.", "")
		return
	} else if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not revoke the token.", "")
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// revokeSelf is DELETE /api/auth/token: the calling token revokes itself, so
// a client's Sign Out ends the credential rather than forgetting it. Scope
// self, which only a token holds. 204, like a session's sign-out: the caller
// has nothing left to follow the stream with.
func (t TokenRoutes) revokeSelf(w http.ResponseWriter, r *http.Request) {
	c, ok := CredentialFrom(r.Context())
	if !ok || c.Kind != auth.CredentialToken {
		WriteError(w, http.StatusForbidden, CodeForbiddenScope, "This credential may not do that.", "")
		return
	}
	if _, err := t.Tokens.Revoke(r.Context(), c.ID); err != nil && !errors.Is(err, auth.ErrNoToken) {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not revoke the token.", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// APIVersion is GET /api/meta's `api`: bumped when a route, event kind or
// view field a client relies on changes incompatibly (vscode-extension
// §9.1). Additive changes do not bump it.
const APIVersion = 1

// MetaRoutes is GET /api/meta, the handshake (vscode-extension §9.1).
type MetaRoutes struct {
	// Version is the release this server was built from ("dev" locally).
	Version string
	// API is the answer's `api`; zero answers APIVersion. A field, so a
	// test can play a newer server without a build-time constant.
	API int
}

// Handlers returns the map Build consumes, keyed by route Name.
func (m MetaRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{"meta.read": m.read}
}

// Meta is GET /api/meta's answer.
type Meta struct {
	Version    string         `json:"version"`
	API        int            `json:"api"`
	Credential CredentialView `json:"credential"`
}

// CredentialView is what the caller's credential is and may do.
type CredentialView struct {
	Kind      auth.CredentialKind `json:"kind"`
	Label     string              `json:"label"`
	Scopes    []auth.Scope        `json:"scopes"`
	ExpiresAt time.Time           `json:"expires_at"`
}

func (m MetaRoutes) read(w http.ResponseWriter, r *http.Request) {
	c, ok := CredentialFrom(r.Context())
	if !ok {
		WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, "Sign in to continue.", "")
		return
	}
	api := m.API
	if api == 0 {
		api = APIVersion
	}
	version := m.Version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, http.StatusOK, Meta{Version: version, API: api,
		Credential: CredentialView{Kind: c.Kind, Label: c.Label, Scopes: c.Scopes, ExpiresAt: c.ExpiresAt}})
}
