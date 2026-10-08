package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 2. Claude identity (design §7.3, Spike 01)

// IdentityState is the five-way verdict stored in claude_identity.state. It is
// a stored column rather than a derived value because a state the prose
// requires and the schema cannot hold gets inferred differently by every
// reader — the banner and the card would have disagreed.
type IdentityState uint8

const (
	IdentityOK IdentityState = iota
	// IdentityExpiring: the **login** — the refresh token — expires within
	// the window, by the `refreshTokenExpiresAt` Claude Code itself records
	// and itself warns from. Never the access token's `expiresAt`, which a
	// real login sets about eight hours out (measured 2026-10-08): a window
	// on that would call every login expiring from the moment it is made.
	IdentityExpiring
	// IdentityExpired: the access token's `expiresAt` has passed. Not a
	// fault — the live refresh token beside it renews it the next time
	// Claude Code runs (Spike 00) — and informational only.
	IdentityExpired
	// IdentityBlanked: a dead login rewrites the credential in place with
	// empty token strings, killing every container on the shared volume at
	// once (Spike 00, re-confirmed live on 2.1.289). "Signed out. Sign in
	// again." — not a countdown.
	IdentityBlanked
	// IdentityAbsent: nobody has ever signed in. The expected first-run
	// state, and a different sentence.
	IdentityAbsent
)

// Identity is the verdict plus the two dates the file carries.
type Identity struct {
	State        IdentityState
	AccountEmail string
	// ExpiresAt is the access token's expiry: hours, renewed by every
	// refresh. It decides IdentityExpired and nothing else.
	ExpiresAt time.Time
	// LoginExpiresAt is the refresh token's expiry, `refreshTokenExpiresAt`,
	// and the zero time when the file carries none (a file written before
	// Claude Code recorded it). It decides IdentityExpiring.
	LoginExpiresAt time.Time
}

// ExpiringWindow is §7.3's warning window, and Claude Code's own: 2.1.289's
// `oauth-expiry` notice ("Your login expires in N days · run /login to
// renew") fires when refreshTokenExpiresAt is at most three days away
// (`3*86400000` in the shipped binary). The boundary is inclusive — exactly
// three days out is already Expiring.
const ExpiringWindow = 72 * time.Hour

// ClassifyIdentity needs **two** inputs, which is the finding rather than an
// inconvenience (Spike 01, result 8):
//
//   - `claude auth status --json` reports `loggedIn:true` for a credential that
//     expired an hour ago, so it cannot supply the expiry;
//   - it reports `loggedIn:false` for a blanked credential *and* for a missing
//     one, so it cannot tell the worst failure in the system from a routine
//     first run.
//
// The verdict comes from the JSON, the expiries from the file, and whether the
// file exists-with-empty-tokens or does not exist at all is the only thing that
// separates blanked from absent. Neither input alone is sufficient, and a
// classifier that trusts `auth status` reports a healthy login for an expired
// one.
//
// credentialsJSON is nil when the file is absent. A non-nil empty slice is a
// file that exists and is empty — not absence, and an error.
//
// The rules, in the order they are applied. The file is read first: the two
// verdicts that matter most — Blanked and Absent — need nothing else, and
// `auth status` is the less stable input (2.1.289 already changed its shape).
// A broken second read must never hide the tombstone.
//
//  1. credentialsJSON == nil → IdentityAbsent, whatever authStatusJSON holds —
//     `loggedIn:true`, garbage, or nothing. The shared volume has no
//     credential to share; a `loggedIn:true` there can only come from
//     something like CLAUDE_CODE_OAUTH_TOKEN, which cannot drive Remote
//     Control.
//  2. credentialsJSON that does not parse, or that lacks a `claudeAiOauth`
//     object with string `accessToken` and `refreshToken` → an error. Never
//     IdentityAbsent, never IdentityOK.
//  3. Both token strings empty → IdentityBlanked, whatever `expiresAt` holds
//     and whatever authStatusJSON holds — `loggedIn:true`, garbage, or
//     nothing. The tokens are what authenticate; the tombstone's
//     `expiresAt: 0` is incidental. A blanked file is unambiguous evidence
//     that every container on the volume is dead, and no reading of the other
//     input may mask it.
//  4. Exactly one token empty (partial blanking) → an error. Claude Code has
//     not been observed writing that shape; Blanked would tell the operator
//     that workspaces are dead which may still run, and OK would hide a
//     credential that cannot renew. Neither guess is safe.
//  5. Live tokens with `expiresAt` missing, null, or <= 0 → an error (a
//     string or fractional `expiresAt` already failed rule 2's parse).
//     There is no expiry to judge, and 0 is the tombstone's sentinel, not a
//     time. `refreshTokenExpiresAt` may be missing or null — a file written
//     before Claude Code recorded it, which only means there is no countdown
//     — but present it must be a positive integer: a string or a fraction
//     fails rule 2's parse, and <= 0 is an error here.
//  6. Only now, because the remaining verdicts use it, authStatusJSON must
//     parse as an object carrying a boolean `loggedIn`. Anything else — nil,
//     empty, truncated, `loggedIn` missing or not a boolean — is an error.
//     Unknown keys are ignored (2.1.289 added `projectsDirectory` and
//     `configDirectory`).
//  7. Live tokens with `loggedIn:false` → an error. The file claims a usable
//     credential and Claude Code itself disowns it (a different config dir, a
//     login racing the poll); no verdict is honest. The converse is not an
//     error: `loggedIn:true` beside an expired file is rule 8, by design.
//  8. Otherwise, from the file alone, and in this order:
//     a. IdentityExpiring when `refreshTokenExpiresAt` is present, at most
//     the window away and not yet past, and `expiresAt` is not more than
//     the window beyond it. That is Claude Code's own rule for its
//     "Your login expires" notice, clause for clause, so Drydock warns
//     exactly when an interactive Claude Code would — the headless
//     session servers never show that notice to anyone. A login that
//     needs a human outranks an access token that does not.
//     b. expiresAt <= now → IdentityExpired (`loggedIn:true`
//     notwithstanding). Informational: the refresh token renews it.
//     c. Otherwise IdentityOK — however soon `expiresAt` is. The access
//     token lives about eight hours, so its expiry is never a countdown.
//
// A `refreshTokenExpiresAt` already past is not a verdict of its own, as it
// is not in Claude Code: the next refresh either succeeds (the date was
// Claude Code's 30-day default rather than the server's) or is rejected and
// blanks the file, which rule 3 reports.
//
// ExpiresAt and LoginExpiresAt are the file's `expiresAt` and
// `refreshTokenExpiresAt` (epoch milliseconds) in UTC, and are set only for
// rule 8's three verdicts; for Blanked and Absent they are the zero time,
// never 1970. AccountEmail is `auth status`'s `email` when present, and
// likewise set only under rule 8 — an email beside "Signed out" or "No one has
// signed in" would name an account that is not the one on the volume.
func ClassifyIdentity(authStatusJSON, credentialsJSON []byte, now time.Time) (Identity, error) {
	return ClassifyIdentityWithin(authStatusJSON, credentialsJSON, now, ExpiringWindow)
}

// ClassifyIdentityWithin is ClassifyIdentity with rule 8a's warning window as
// a parameter: the watch takes it from configuration (§7.3), and every other
// rule is the same code. A window <= 0 is an error rather than a classifier
// that can never say expiring.
func ClassifyIdentityWithin(authStatusJSON, credentialsJSON []byte, now time.Time, window time.Duration) (Identity, error) {
	if window <= 0 {
		return Identity{}, fmt.Errorf("classify identity: expiring window %s must be positive", window)
	}
	if credentialsJSON == nil {
		return Identity{State: IdentityAbsent}, nil
	}

	var creds struct {
		OAuth *struct {
			AccessToken           *string `json:"accessToken"`
			RefreshToken          *string `json:"refreshToken"`
			ExpiresAt             *int64  `json:"expiresAt"`
			RefreshTokenExpiresAt *int64  `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(credentialsJSON, &creds); err != nil {
		return Identity{}, fmt.Errorf("classify identity: .credentials.json: %w", err)
	}
	o := creds.OAuth
	if o == nil || o.AccessToken == nil || o.RefreshToken == nil {
		return Identity{}, errors.New("classify identity: .credentials.json: no claudeAiOauth with accessToken and refreshToken")
	}

	access, refresh := *o.AccessToken != "", *o.RefreshToken != ""
	switch {
	case !access && !refresh:
		return Identity{State: IdentityBlanked}, nil
	case !access || !refresh:
		return Identity{}, fmt.Errorf("classify identity: .credentials.json: partially blanked (accessToken empty: %t, refreshToken empty: %t)", !access, !refresh)
	}

	if o.ExpiresAt == nil {
		return Identity{}, errors.New("classify identity: .credentials.json: live tokens without expiresAt")
	}
	ms := *o.ExpiresAt
	if ms <= 0 {
		return Identity{}, fmt.Errorf("classify identity: .credentials.json: live tokens with unusable expiresAt %d", ms)
	}
	var loginExpires time.Time
	if rt := o.RefreshTokenExpiresAt; rt != nil {
		if *rt <= 0 {
			return Identity{}, fmt.Errorf("classify identity: .credentials.json: live tokens with unusable refreshTokenExpiresAt %d", *rt)
		}
		loginExpires = time.UnixMilli(*rt).UTC()
	}

	// Rule 6: only the live-token verdicts need auth status.
	var status struct {
		LoggedIn *bool  `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(authStatusJSON, &status); err != nil {
		return Identity{}, fmt.Errorf("classify identity: auth status --json: %w", err)
	}
	if status.LoggedIn == nil {
		return Identity{}, errors.New("classify identity: auth status --json: no boolean loggedIn")
	}
	if !*status.LoggedIn {
		return Identity{}, errors.New("classify identity: credential file holds live tokens but auth status reports loggedIn:false")
	}

	id := Identity{AccountEmail: status.Email, ExpiresAt: time.UnixMilli(ms).UTC(), LoginExpiresAt: loginExpires}
	switch {
	case loginExpiring(id, now, window):
		id.State = IdentityExpiring
	case !id.ExpiresAt.After(now):
		id.State = IdentityExpired
	default:
		id.State = IdentityOK
	}
	return id, nil
}

// loginExpiring is rule 8a: Claude Code 2.1.289's own test for its "Your
// login expires in N days" notice, transcribed —
//
//	if (typeof refreshTokenExpiresAt !== "number") return null
//	if (typeof expiresAt === "number" && expiresAt > refreshTokenExpiresAt + 3d) return null
//	m = refreshTokenExpiresAt - now; if (m > 3d || m <= 0) return null
//
// with the window in place of its three days.
func loginExpiring(id Identity, now time.Time, window time.Duration) bool {
	if id.LoginExpiresAt.IsZero() {
		return false
	}
	if id.ExpiresAt.After(id.LoginExpiresAt.Add(window)) {
		return false
	}
	left := id.LoginExpiresAt.Sub(now)
	return left > 0 && left <= window
}
