package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/login"
)

// IdentityWatch is what the Claude identity routes need from
// internal/identity.
type IdentityWatch interface {
	Read(ctx context.Context) (identity.View, error)
	Trigger()
}

// LoginManager is what the handshake routes need from internal/login.
type LoginManager interface {
	Current() *login.View
	Begin(ctx context.Context) (login.View, error)
	Submit(ctx context.Context, id string, code []byte) error
	Cancel(ctx context.Context, id string) error
}

// ClaudeIdentity is GET /api/auth/claude (design §5, frontend §4.5 #2).
//
// Two halves. `identity` is the stored verdict from the expiry watch (§7.3):
// the five-way state, the account and expiry beside a login, when it was last
// checked, and the last check's failure if it failed. `login` is the
// handshake (§7.2) in progress, or one that ended in the last few minutes, so
// a phone that discarded the page while the operator was in their browser
// picks it back up (frontend §2.4); null otherwise.
type ClaudeIdentity struct {
	Identity identity.View `json:"identity"`
	Login    *login.View   `json:"login"`
}

// ClaudeRoutes serves the Claude identity and the login handshake.
type ClaudeRoutes struct {
	Watch IdentityWatch
	Login LoginManager
}

// Handlers returns the map Build consumes, keyed by route Name.
func (cr ClaudeRoutes) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"claude.identity.read":  cr.read,
		"claude.identity.check": cr.check,
		"claude.login.begin":    cr.begin,
		"claude.login.code":     cr.code,
		"claude.login.cancel":   cr.cancel,
	}
}

func (cr ClaudeRoutes) read(w http.ResponseWriter, r *http.Request) {
	v, err := cr.Watch.Read(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the Claude login state.", "")
		return
	}
	var lv *login.View
	if cr.Login != nil {
		lv = cr.Login.Current()
	}
	writeJSON(w, http.StatusOK, ClaudeIdentity{Identity: v, Login: lv})
}

// check is the async shape: start a check — or join the one running — and
// answer 202. The verdict arrives as auth.identity, a failure as
// auth.identity_check_failed.
func (cr ClaudeRoutes) check(w http.ResponseWriter, r *http.Request) {
	cr.Watch.Trigger()
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// begin starts a login. 202 {login_id}; every phase after this one arrives as
// auth.login, and the starting phase is already on the stream.
func (cr ClaudeRoutes) begin(w http.ResponseWriter, r *http.Request) {
	v, err := cr.Login.Begin(r.Context())
	switch {
	case errors.Is(err, login.ErrInProgress):
		WriteError(w, http.StatusConflict, CodeInProgress, "A Claude login is already in progress.", "")
		return
	case errors.Is(err, login.ErrShutdown):
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "Drydock is shutting down.", "")
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not start a Claude login.", "")
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		LoginID string `json:"login_id"`
	}{v.ID})
}

// maxCodeBody bounds the code route's body. A code is under 200 bytes.
const maxCodeBody = 4 << 10

// code types the pasted code into the login's PTY. Redact by default: the
// body is read into a buffer this handler zeroes, the code is taken out of it
// without becoming a string when it can be, and no refusal — a malformed
// body, a bad shape, a login that has ended — says anything built from it.
// The JSON decoder's own errors can quote a character of their input, so
// none is passed on.
func (cr ClaudeRoutes) code(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("lid")
	if !login.ValidID(id) {
		WriteError(w, http.StatusNotFound, CodeNotFound, "No such login.", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCodeBody))
	defer wipe(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "The request body could not be read.", "")
		return
	}
	code, ok := codeFromBody(body)
	defer wipe(code)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, `The body must be {"code": "<the code Claude showed>"}.`, "")
		return
	}
	err = cr.Login.Submit(r.Context(), id, code)
	var ce *login.CodeError
	switch {
	case errors.As(err, &ce):
		WriteError(w, http.StatusBadRequest, CodeLoginCodeInvalid,
			"That does not look like a whole login code. Copy the whole code Claude shows, both sides of the #.", codeRule(ce.Err))
	case errors.Is(err, login.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "No such login.", "")
	case errors.Is(err, login.ErrEnded):
		WriteError(w, http.StatusConflict, CodeLoginEnded, "That login has ended. Start a new one.", "")
	case errors.Is(err, login.ErrNotAwaiting):
		WriteError(w, http.StatusConflict, CodeLoginNotAwaiting, "The login is not waiting for a code right now.", "")
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not submit the code.", "")
	default:
		// Typed into the PTY. The verdict arrives as auth.login.
		writeJSON(w, http.StatusAccepted, struct{}{})
	}
}

func (cr ClaudeRoutes) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("lid")
	if !login.ValidID(id) {
		WriteError(w, http.StatusNotFound, CodeNotFound, "No such login.", "")
		return
	}
	switch err := cr.Login.Cancel(r.Context(), id); {
	case errors.Is(err, login.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "No such login.", "")
	case errors.Is(err, login.ErrEnded):
		WriteError(w, http.StatusConflict, CodeLoginEnded, "That login has already ended.", "")
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not cancel the login.", "")
	default:
		writeJSON(w, http.StatusAccepted, struct{}{})
	}
}

// codeFromBody takes the code out of {"code": "..."}: strictly one object
// with exactly that one string field. The value is copied out of the body
// as bytes — a valid code is printable ASCII with no quote or backslash, so
// its JSON spelling is itself — and only a value with escapes goes through a
// string, which the shape check then refuses anyway.
func codeFromBody(body []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil || dec.More() {
		return nil, false
	}
	v, ok := raw["code"]
	if !ok || len(raw) != 1 || len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return nil, false
	}
	inner := v[1 : len(v)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return bytes.Clone(inner), true
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, false
	}
	return []byte(s), true
}

// codeRule names the shape rule a code broke, for the refusal's detail.
func codeRule(err error) string {
	switch {
	case errors.Is(err, classify.ErrCodeEmpty):
		return "empty"
	case errors.Is(err, classify.ErrCodeNoSeparator):
		return "no_separator"
	case errors.Is(err, classify.ErrCodeHalfMissing):
		return "half_missing"
	case errors.Is(err, classify.ErrCodeExtraHash):
		return "extra_hash"
	case errors.Is(err, classify.ErrCodeBadCharacter):
		return "bad_character"
	}
	return ""
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
