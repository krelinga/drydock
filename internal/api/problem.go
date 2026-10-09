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
	// CodeAtCapacity: the concurrent-container cap is reached (§6 step 1).
	// A 409 like in_progress, but a different sentence and a different
	// action — stop something — so a different code.
	CodeAtCapacity = "at_capacity"
	// CodeDiskFull: a create, start or rebuild refused by the disk
	// pre-flight (design §12, *Disk full*); 507, with the figures in detail.
	CodeDiskFull   = "disk_full"
	CodeBadRequest = "bad_request"
	// CodeConfirmMismatch: a workspace delete whose ?confirm= is missing or
	// is not the repository's full name exactly (frontend §6.5). A 400 of
	// its own rather than bad_request, because the UI's answer is specific:
	// the typed name did not match, type it again.
	CodeConfirmMismatch = "confirm_mismatch"
	// CodeApprovalNotPending: a host-access approval (design §6) for a
	// workspace that is not stopped waiting for one — already approved,
	// declined, or moved on.
	CodeApprovalNotPending = "approval_not_pending"
	// CodeApprovalStale: the hash approved is not the request the workspace
	// is waiting on. The configuration changed after the operator was shown
	// it; nothing was approved, and the page shows the new request.
	CodeApprovalStale = "approval_stale"
	CodeBadPassword   = "bad_password"
	CodeLockedOut     = "locked_out"
	CodeNotConfigured = "not_configured"
	// CodeAppNotConfigured: no GitHub App is configured, so there is no
	// repository list. Distinct from not_configured, which is the password.
	CodeAppNotConfigured = "app_not_configured"
	CodeInternal         = "internal"

	// The login handshake's refusals (design §7.2). None carries the code:
	// the detail names the rule a malformed one broke, never a character of
	// it.
	//
	// CodeLoginCodeInvalid: the pasted code is not `<code>#<state>` in
	// printable ASCII — most likely half a copy. Refused before the PTY.
	// The detail is the rule: empty, no_separator, half_missing,
	// extra_hash or bad_character.
	CodeLoginCodeInvalid = "login_code_invalid"
	// CodeLoginNotAwaiting: the login is starting, or is checking a code
	// already; a code is taken only at the prompt.
	CodeLoginNotAwaiting = "login_not_awaiting_code"
	// CodeLoginEnded: the login has already ended — timed out, cancelled,
	// failed or succeeded. Start a new one.
	CodeLoginEnded = "login_ended"
	// CodeUnavailable: Drydock is shutting down.
	CodeUnavailable = "unavailable"

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
	// CodeSecretValueRequired: a PUT with no `value` for a name that has no
	// secret. Absent means "keep the stored value", and there is none.
	CodeSecretValueRequired = secrets.CodeValueRequired
	// CodeSecretValueEmpty: a `value` sent as "". Indistinguishable from
	// unset, so refused — and never read as absent.
	CodeSecretValueEmpty = secrets.CodeValueEmpty
	// CodeSecretValueControl: a newline, carriage return, NUL, tab or other
	// control character — or bytes that are not UTF-8. The detail names
	// the character and its byte offset. A newline would forge a line in
	// GET-SECRETS (§10.3).
	CodeSecretValueControl = secrets.CodeValueControl
	// CodeSecretValueTooLong: over 32 KiB.
	CodeSecretValueTooLong = secrets.CodeValueTooLong
	// CodeSecretReachRequired: a blank reach. The field is the control
	// (§10.4), not documentation.
	CodeSecretReachRequired = secrets.CodeReachRequired
	// CodeSecretReachTooLong: a reach over 2000 bytes.
	CodeSecretReachTooLong = secrets.CodeReachTooLong
	// CodeSecretDescriptionTooLong: a description over 4000 bytes.
	CodeSecretDescriptionTooLong = secrets.CodeDescriptionTooLong
	// CodeSecretDescriptionInvalid: a description that is not UTF-8. A JSON
	// body cannot carry one, so the API does not send this today.
	CodeSecretDescriptionInvalid = secrets.CodeDescriptionBad
	// CodeSecretExists: a create — a PUT with If-None-Match: * — for a name
	// already stored. 412, and nothing written: a create never replaces a
	// value that can never be shown again (frontend §6.4).
	CodeSecretExists = "secret_exists"
	// CodeUnknownRepository: a grant names a repository id the catalog
	// does not have.
	CodeUnknownRepository = secrets.CodeUnknownRepo
	// CodePortExists: a port added by hand that the workspace already lists
	// live (PF §6).
	CodePortExists = "port_exists"
	// CodeTooManyPorts: a workspace already lists preview.MaxPorts ports.
	CodeTooManyPorts = "too_many_ports"
	// CodePreviewsNotConfigured: an enable with no preview domain.
	CodePreviewsNotConfigured = "previews_not_configured"
	// CodePortLoopback: an enable of a port discovery sees listening on
	// loopback only, which no preview can reach (PF §11, §13 step 6).
	CodePortLoopback = "port_loopback"
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
