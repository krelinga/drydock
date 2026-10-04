// Package classify turns foreign bytes into Drydock states.
//
// Five places in the system read output produced by something Drydock does not
// control, and every one of them must be callable without the thing that
// produced the bytes (testing §5.5). That is what converts the most brittle
// dependencies in the design — two scrapes of Claude Code's TUI, one JSON
// contract with the `devcontainer` CLI — into table-driven unit tests over a
// recorded corpus.
//
// Every function here is `func(input) (state, error)` with no clock, no
// filesystem, and no process. They are also the most parallelizable work in
// the project: five independent functions, five fixture directories, nothing
// shared between them.
//
// What the corpus proves and does not: it proves the parser handles the
// recorded bytes, not that the bytes are still what Claude Code emits. Only a
// live run shows that, which is why re-recording is a ritual (testing §11.1)
// and why the four spike harnesses exist.
package classify

import "time"

// FixtureRoot is where recorded bytes live, one directory per version so an
// old corpus stays until that version is no longer deployable.
//
//	test/fixtures/transcripts/claude-2.1.246/login-url-200col
//	test/fixtures/transcripts/claude-2.1.246/session-url-osc8
//	test/fixtures/devcontainer/up-ok.json
//
// Each transcript file is **raw PTY bytes, escapes intact** — never a cleaned
// up transcript — with a header naming the version, the date, the command, and
// the PTY width it was recorded at. The width is part of the fixture because
// the authorize URL wraps mid-token at ordinary widths (Spike 01).
const FixtureRoot = "test/fixtures"

// ClaudeCodeVersion is the pinned version every transcript was recorded
// against. A bump re-runs all four Claude Code harnesses and re-records the
// corpus (testing §11.1), and a consistency test asserts this constant agrees
// with CLAUDE.md, the Feature, and all four spike reports.
const ClaudeCodeVersion = "2.1.246"

// ---------------------------------------------------------------------------
// 1. The login handshake (design §7.2, Spike 01)
// ---------------------------------------------------------------------------

// LoginPhase is where a handshake has got to.
type LoginPhase uint8

const (
	LoginStarting LoginPhase = iota
	// LoginAwaitingCode: the authorize URL has been scraped and the process
	// is at the paste prompt.
	LoginAwaitingCode
	// LoginInvalidCode is a loop, not an exit. Spike 01 measured that the
	// process stays at the prompt with the same URL still valid, so the
	// form stays open and the operator pastes again. A state machine with a
	// terminal failure node here would throw away a live PTY for the most
	// likely user error there is.
	LoginInvalidCode
	LoginSuccess
	LoginTimedOut
)

// Login is the verdict on a login PTY stream.
type Login struct {
	Phase LoginPhase
	// AuthorizeURL is the complete URL, de-wrapped. The assertion that
	// matters is that it *parses* and carries the expected query-parameter
	// set — not merely that a regex matched something. A fragment passes a
	// `!= ""` check and then fails when a human clicks it.
	AuthorizeURL string
}

// ClassifyLogin reads a raw PTY byte stream from `claude auth login`.
//
// Three hazards it must survive, all measured:
//   - the authorize URL wraps mid-token at ordinary PTY widths, so matching
//     must happen against the stream with newlines removed;
//   - `Login successful` has several forms (`.`, `. Press …`), so it is matched
//     as a prefix and never as a whole line;
//   - the paste prompt is not re-printed after a rejection, so nothing may
//     wait for it to reappear as a readiness signal.
func ClassifyLogin(pty []byte) (Login, error) { panic("not implemented: Phase 5") }

// ValidateCodeShape checks the pasted code before it is written to the PTY.
//
// Spike 01 read the format out of the binary: it is `<code>#<state>`, and
// Claude Code rejects a missing half locally. Checking it here turns the
// likeliest user error — a truncated copy — into an instant, precise message
// instead of a terminal round-trip, and removes one case from the set the
// scrape has to interpret.
func ValidateCodeShape(code string) error { panic("not implemented: Phase 5") }

// ---------------------------------------------------------------------------
// 2. Claude identity (design §7.3, Spike 01)
// ---------------------------------------------------------------------------

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
	// once (Spike 00). "Signed out. Sign in again." — not a countdown.
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

// ---------------------------------------------------------------------------
// 3. Supervisor startup refusals (design §8, Spike 02)
// ---------------------------------------------------------------------------

// Refusal is why a `remote-control` server would not start.
//
// All four exit `1`. **Exit status is not a discriminator**, and the natural
// wrong implementation — branching on it — fails by crash-looping against a
// config error or by giving up on a wait. One of these must be retried and
// three must not.
type Refusal uint8

const (
	// RefusalNone: the stream is not a refusal.
	RefusalNone Refusal = iota
	// RefusalWaitRegistration: `409 … already served by a terminal`. The
	// previous server's folder registration has not lapsed. **Retry,
	// patiently** — measured at 60–200s, not a fixed value, so nothing may
	// be coded against a constant. It is a wait, not a crash, and it must
	// not spend the restart budget.
	RefusalWaitRegistration
	// RefusalWorkspaceNotTrusted: the Feature or postCreate is broken. Do
	// not retry; the fix is a rebuild.
	RefusalWorkspaceNotTrusted
	// RefusalNoOrganization: credential present, account record missing.
	// Do not retry; this is awaiting_login (§7.3).
	RefusalNoOrganization
	// RefusalBadCommandLine: Drydock built an invalid invocation. Do not
	// retry; this is a bug report.
	RefusalBadCommandLine
)

// Retryable reports whether the supervisor should try again. Exactly one
// refusal is.
func (r Refusal) Retryable() bool { return r == RefusalWaitRegistration }

// ClassifyRefusal reads one line of `remote-control` stderr. The exit code is
// deliberately not a parameter: it is always 1 and accepting it would invite
// an implementation that uses it.
func ClassifyRefusal(stderrLine []byte) (Refusal, error) { panic("not implemented: Phase 5") }

// ---------------------------------------------------------------------------
// 4. The discovery tail (design §8, Spike 02)
// ---------------------------------------------------------------------------

// Discovery is what the supervisor's `--verbose` stream reveals.
type Discovery struct {
	// EnvironmentID is the durable handle — one per workspace, survives a
	// restart, and the card's only link. Stored in workspace.environment_id.
	EnvironmentID string
	// SessionIDs are matched as `session_[A-Za-z0-9]+` rather than parsed
	// out of a URL: per-session URLs arrive wrapped in OSC 8 hyperlink
	// escapes, so the URL and its label run together in the byte stream and
	// only an id match is unambiguous.
	SessionIDs []string
	// CapacityUsed and CapacityTotal come from `Capacity: N/4`, reprinted
	// on every repaint. The pre-created session counts toward Used, so a
	// total of 4 buys three on-demand sessions and the UI must not imply
	// otherwise.
	CapacityUsed, CapacityTotal int
}

// ClassifyDiscovery reads an ANSI + OSC 8 byte stream.
//
// Two hazards: ANSI cursor movement reprints the status block in place, so the
// same line recurs constantly and the caller must upsert by id rather than
// append; and a `session_…` id the *model* printed in its own output is not a
// server announcement, which is the false-positive the negative corpus exists
// to catch.
func ClassifyDiscovery(stream []byte) (Discovery, error) { panic("not implemented: Phase 5") }

// ---------------------------------------------------------------------------
// 5. The devcontainer CLI result (design §6)
// ---------------------------------------------------------------------------

// ContainerOutcome is the result of a `devcontainer up`.
type ContainerOutcome uint8

const (
	ContainerRunning ContainerOutcome = iota
	ContainerFailed
)

// Container is the parsed result. Step is what makes a failure actionable:
// design §6 writes an event for every step precisely so the UI can name the
// one that failed, and "failed" alone throws away the only thing that makes a
// rebuild an informed choice.
type Container struct {
	Outcome     ContainerOutcome
	ContainerID string
	RemoteUser  string
	Step        string
}

// ClassifyContainer parses `devcontainer up --json` output. Never scrape
// `docker ps` — the machine-readable result is the contract (§2.5), and the
// fake binary's contract test pins its shape (testing §6.1).
func ClassifyContainer(upJSON []byte) (Container, error) { panic("not implemented: Phase 5") }
