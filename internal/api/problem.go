package api

import (
	"encoding/json"
	"net/http"

	"github.com/krelinga/drydock/internal/secrets"
)

// Error is the one error shape every route returns, from frontend §4.5 #7.
//
// The `Code` is the point. Design §12 pairs each failure mode with a specific
// sentence and the UI renders that sentence rather than a generic one; if the
// client had to recognise failures by matching on prose, the first reworded
// message would silently turn a precise error into an unknown one. So the code
// is stable and machine-readable, the message is for a human, and the detail is
// optional context.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type errorEnvelope struct {
	Error Error `json:"error"`
}

// Stable error codes. Add to this list rather than inventing a code at a call
// site: the UI switches on these, and an unrecognised one renders as a generic
// failure, which is the thing the envelope exists to avoid.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeForbiddenOrigin = "forbidden_origin"
	CodeForbiddenHost   = "forbidden_host"
	CodeNotFound        = "not_found"
	// CodeMethodNotAllowed is a declared path under a method it does not take.
	// Distinct from bad_request, which means the body or query was wrong: a
	// 405 is a client bug in which route it called, not in what it sent.
	CodeMethodNotAllowed = "method_not_allowed"
	CodeNotImplemented   = "not_implemented"
	CodeInProgress       = "in_progress"
	CodeBadRequest       = "bad_request"
	CodeBadPassword      = "bad_password"
	CodeLockedOut        = "locked_out"
	CodeNotConfigured    = "not_configured"
	// CodeAppNotConfigured: no GitHub App is configured, so there is no
	// repository list. Distinct from not_configured, which is the password.
	CodeAppNotConfigured = "app_not_configured"
	CodeInternal         = "internal"

	// The secret routes' refusals (design §10.1). Each is one sentence in
	// the UI, and the detail names the rule or the character — never the
	// value.
	//
	// CodeSecretsNotConfigured: no master key, so nothing can be stored.
	CodeSecretsNotConfigured = "secrets_not_configured"
	// CodeSecretNameInvalid: not an environment variable name.
	CodeSecretNameInvalid = secrets.CodeNameInvalid
	// CodeSecretNameReserved: a name on §10.1's reserved list; the detail
	// says why that one.
	CodeSecretNameReserved = secrets.CodeNameReserved
	// CodeSecretValueEmpty: indistinguishable from unset, so refused.
	CodeSecretValueEmpty = secrets.CodeValueEmpty
	// CodeSecretValueControl: a newline, carriage return, NUL, tab or other
	// control character — or bytes that are not UTF-8. The detail names
	// the character and its byte offset. A newline would forge a line in
	// GET-SECRETS (§10.3).
	CodeSecretValueControl = secrets.CodeValueControl
	// CodeSecretValueTooLong: over 32 KiB.
	CodeSecretValueTooLong = secrets.CodeValueTooLong
	// CodeSecretReachRequired: blank (or over-long) reach. The field is the
	// control (§10.4), not documentation.
	CodeSecretReachRequired = secrets.CodeReachRequired
	// CodeSecretDescriptionInvalid: over-long or not text.
	CodeSecretDescriptionInvalid = secrets.CodeDescriptionBad
	// CodeUnknownRepository: a grant names a repository id the catalog
	// does not have.
	CodeUnknownRepository = secrets.CodeUnknownRepo
)

// WriteError sends the envelope. Nothing else in the codebase should write an
// error body by hand.
//
// Detail is caller-supplied prose and is rendered by the UI as *text*, never as
// HTML (frontend §8). It must never carry a credential: redact-by-default
// (§13.5) applies here as much as to the event log, and an error string is one
// of the sinks the canary sweep deliberately covers.
func WriteError(w http.ResponseWriter, status int, code, message, detail string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// No CORS header, ever, on any response from any route — including the
	// error paths, which is where a reflexive Access-Control-Allow-Origin
	// tends to get added by someone debugging a fetch (§13.5).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: Error{
		Code:    code,
		Message: message,
		Detail:  detail,
	}})
}
