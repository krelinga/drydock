package classify

// 1. The login handshake (design §7.2, Spike 01)

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
	// AuthorizeURL is the complete URL. The assertion that matters is that
	// it *parses* and carries the expected query-parameter set — not merely
	// that a regex matched something. A fragment passes a `!= ""` check and
	// then fails when a human clicks it.
	AuthorizeURL string
}

// ClassifyLogin reads a raw PTY byte stream from `claude auth login`.
//
// Three facts it must respect, all measured:
//   - the authorize URL is ~465 characters and arrives UNBROKEN in the byte
//     stream at every PTY width measured (80, 200, 1000). The mid-token
//     wrapping Spike 01 originally reported was a rendering artifact of
//     `tmux capture-pane`, not something the process writes, and is retracted.
//     A per-line match is sufficient; stripping newlines first is harmless
//     insurance against a future version that does wrap.
//   - `Login successful` has several forms (`.`, `. Press …`), so it is matched
//     as a prefix and never as a whole line.
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
