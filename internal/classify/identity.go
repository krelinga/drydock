package classify

import "time"

// 2. Claude identity (design §7.3, Spike 01)

// IdentityState is the five-way verdict stored in claude_identity.state. It is
// a stored column rather than a derived value because a state the prose
// requires and the schema cannot hold gets inferred differently by every
// reader — the banner and the card would have disagreed.
type IdentityState uint8

const (
	IdentityOK IdentityState = iota
	IdentityExpiring
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

// Identity is the verdict plus the countdown the UI needs.
type Identity struct {
	State        IdentityState
	AccountEmail string
	ExpiresAt    time.Time
}

// ClassifyIdentity needs **two** inputs, which is the finding rather than an
// inconvenience (Spike 01, result 8):
//
//   - `claude auth status --json` reports `loggedIn:true` for a credential that
//     expired an hour ago, so it cannot supply the countdown;
//   - it reports `loggedIn:false` for a blanked credential *and* for a missing
//     one, so it cannot tell the worst failure in the system from a routine
//     first run.
//
// The verdict comes from the JSON, the countdown from the file, and whether the
// file exists-with-empty-tokens or does not exist at all is the only thing that
// separates blanked from absent. Neither input alone is sufficient, and a
// classifier that trusts `auth status` reports a healthy login for an expired
// one.
//
// credentialsJSON is nil when the file is absent.
func ClassifyIdentity(authStatusJSON, credentialsJSON []byte, now time.Time) (Identity, error) {
	panic("not implemented: Phase 5")
}
