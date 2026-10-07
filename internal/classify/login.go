package classify

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

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
	// LoginTimedOut is the caller's verdict, never ClassifyLogin's: a
	// deadline is a clock decision, and no byte in the stream says "five
	// minutes have passed". It lives here so the handshake's state has one
	// type.
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

// ErrAuthorizeURL reports that a complete line carried something shaped like
// the authorize URL that is not a usable one: it does not parse, or a required
// query parameter is missing, empty, repeated, or has the wrong value. Waiting
// will not fix it, so it is an error and not a phase — it means Claude Code
// changed the URL, which is a parser change (fixtures README).
var ErrAuthorizeURL = errors.New("login: authorize URL is incomplete or malformed")

const (
	loginPrompt  = "Paste code here if prompted >"
	loginSuccess = "Login successful"
	loginInvalid = "Invalid code"
)

// authorizeURLPattern is the pattern from design §7.2. It only *finds* a
// candidate; validAuthorizeURL decides whether the candidate is usable.
var authorizeURLPattern = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?[A-Za-z0-9&=_%.~+-]+`)

// ClassifyLogin reads a raw PTY byte stream from `claude auth login`.
//
// Three facts it must respect, all measured:
//   - the authorize URL is ~465 characters and arrives UNBROKEN in the byte
//     stream at every PTY width measured (80, 200, 1000). The mid-token
//     wrapping Spike 01 originally reported was a rendering artifact of
//     `tmux capture-pane`, not something the process writes, and is retracted.
//   - `Login successful` has several forms (`.`, `. Press …`), so it is matched
//     as a prefix and never as a whole line.
//   - the paste prompt is not re-printed after a rejection, so nothing may
//     wait for it to reappear as a readiness signal.
//
// The URL is matched per line, and newlines are deliberately NOT stripped
// first. The design calls de-wrapping "harmless insurance"; on these bytes it
// is not harmless. The URL's line ends with `state=<base64url>` and the next
// line begins `Paste code…`, and the URL character class contains letters, so
// joining the lines yields `state=…Paste` — a URL that parses, carries every
// parameter, and has the wrong state. A wrapped URL from some future version
// fails validation loudly instead, which is the better failure.
//
// A candidate URL is judged only once its line is complete (a `\n`, or a lone
// `\r`, follows it). On the unterminated last line the URL may still be
// arriving — a PTY read can end anywhere — so a partial URL there is not an
// error: the verdict stays LoginStarting with an empty AuthorizeURL, and the
// next read decides. A URL on a complete line that fails validation returns
// ErrAuthorizeURL with LoginStarting and no URL, because no later byte can
// repair it — unless the stream has already reached LoginSuccess, which needs
// no URL. Either way a fragment is never reported as a usable URL.
//
// The phase is the latest event in the stream:
//   - `Login successful` → LoginSuccess, needing no URL (the caller has it).
//   - `Invalid code` with a paste prompt somewhere before it → LoginInvalidCode.
//     The prompt is not re-printed after a rejection, so a second rejection is
//     recognized without one in between.
//   - the paste prompt, with a valid URL → LoginAwaitingCode.
//   - otherwise LoginStarting.
//
// Both verdict markers must begin a segment: the start of a line (after
// leading blanks), or the text right after the paste prompt. The second is
// not a corner case — the prompt does not echo, so the verdict lands on the
// prompt's own line (`… prompted > Invalid code.`), and a real success almost
// certainly will too. A marker in the middle of other prose (`Error: Login
// successful but …`) is not a verdict, and neither is a longer word
// (`Login successfully`): the marker must be followed by the end of the
// segment or a non-alphanumeric character. Both are case-sensitive.
//
// ClassifyLogin never returns LoginTimedOut; see that constant.
func ClassifyLogin(pty []byte) (Login, error) {
	text := stripTerminal(pty)

	var (
		url       string
		urlErr    error
		firstPmt  = -1
		lastPmt   = -1
		lastInv   = -1
		lastOK    = -1
		offset    int
		remaining = text
	)
	for len(remaining) > 0 {
		line, rest, terminated := strings.Cut(remaining, "\n")

		if terminated {
			for _, cand := range authorizeURLPattern.FindAllString(line, -1) {
				if err := validAuthorizeURL(cand); err != nil {
					urlErr = err
				} else {
					url, urlErr = cand, nil
				}
			}
		}

		// Segments: the line itself, and whatever follows each prompt on it.
		segStarts := []int{0}
		for i := 0; ; {
			j := strings.Index(line[i:], loginPrompt)
			if j < 0 {
				break
			}
			p := offset + i + j
			if firstPmt < 0 {
				firstPmt = p
			}
			lastPmt = p
			i += j + len(loginPrompt)
			segStarts = append(segStarts, i)
		}
		for _, s := range segStarts {
			seg := strings.TrimLeft(line[s:], " \t")
			at := offset + len(line) - len(seg)
			if hasMarker(seg, loginSuccess) {
				lastOK = max(lastOK, at)
			}
			if hasMarker(seg, loginInvalid) && firstPmt >= 0 && at > firstPmt {
				lastInv = max(lastInv, at)
			}
		}

		offset += len(line) + 1
		remaining = rest
	}

	switch {
	case lastOK >= 0 && lastOK > lastInv && lastOK > lastPmt:
		return Login{Phase: LoginSuccess, AuthorizeURL: url}, nil
	case urlErr != nil:
		return Login{Phase: LoginStarting}, urlErr
	case lastInv >= 0 && lastInv > lastPmt:
		return Login{Phase: LoginInvalidCode, AuthorizeURL: url}, nil
	case lastPmt >= 0 && url != "":
		return Login{Phase: LoginAwaitingCode, AuthorizeURL: url}, nil
	default:
		return Login{Phase: LoginStarting, AuthorizeURL: url}, nil
	}
}

// hasMarker reports whether seg begins with marker as a whole word.
func hasMarker(seg, marker string) bool {
	if !strings.HasPrefix(seg, marker) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(seg[len(marker):])
	return next == utf8.RuneError || !(unicode.IsLetter(next) || unicode.IsDigit(next))
}

// validAuthorizeURL is the verdict on a candidate: it parses, it points where
// it should, and every parameter the server needs is present exactly once.
// The error names the parameter, never its value.
func validAuthorizeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: does not parse", ErrAuthorizeURL)
	}
	if u.Scheme != "https" || u.Host != "claude.com" || u.Path != "/cai/oauth/authorize" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%w: wrong origin or path", ErrAuthorizeURL)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("%w: query does not parse", ErrAuthorizeURL)
	}
	fixed := map[string]string{
		"code":                  "true",
		"response_type":         "code",
		"code_challenge_method": "S256",
	}
	for _, k := range []string{"code", "client_id", "response_type", "redirect_uri", "scope", "code_challenge", "code_challenge_method", "state"} {
		v := q[k]
		if len(v) != 1 || v[0] == "" {
			return fmt.Errorf("%w: parameter %q missing, empty, or repeated", ErrAuthorizeURL, k)
		}
		if want, ok := fixed[k]; ok && v[0] != want {
			return fmt.Errorf("%w: parameter %q has an unexpected value", ErrAuthorizeURL, k)
		}
	}
	if r, err := url.Parse(q.Get("redirect_uri")); err != nil || r.Scheme != "https" || r.Host == "" {
		return fmt.Errorf("%w: redirect_uri is not an absolute https URL", ErrAuthorizeURL)
	}
	return nil
}

// stripTerminal removes terminal control sequences and normalizes line ends.
// CSI sequences, OSC strings (which is what wraps the URL in an OSC 8
// hyperlink, so the hyperlink *target* disappears and the visible label is
// what gets matched), the DCS/SOS/PM/APC strings, and two-byte escapes are
// dropped. A sequence cut off by the end of the input is dropped whole. `\r\n`
// becomes `\n`, and a lone `\r` also becomes `\n`, because a carriage return
// puts whatever follows it at the start of a line. Every other C0 control but
// tab is dropped.
func stripTerminal(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b:
			i = skipEscape(b, i)
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				continue
			}
			out.WriteByte('\n')
		case c == '\n' || c == '\t':
			out.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			// dropped
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// skipEscape returns the index of the last byte of the escape sequence that
// starts at b[i] (an ESC).
func skipEscape(b []byte, i int) int {
	if i+1 >= len(b) {
		return i
	}
	switch b[i+1] {
	case '[': // CSI: parameters and intermediates, then one final byte.
		for j := i + 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				return j
			}
		}
		return len(b) - 1
	case ']', 'P', 'X', '^', '_': // string: ends at BEL or ST (ESC \).
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 1
			}
		}
		return len(b) - 1
	default: // ESC, any intermediates, one final byte.
		j := i + 1
		for j < len(b)-1 && b[j] >= 0x20 && b[j] <= 0x2f {
			j++
		}
		return j
	}
}

// Errors from ValidateCodeShape. Each is a constant string: the code is a
// one-time credential, so no message may contain it or either half of it.
var (
	ErrCodeEmpty        = errors.New("login code is empty")
	ErrCodeNoSeparator  = errors.New("login code has no '#': copy the whole code, both halves")
	ErrCodeHalfMissing  = errors.New("login code is missing the part before or after the '#': copy the whole code")
	ErrCodeExtraHash    = errors.New("login code has more than one '#'")
	ErrCodeBadCharacter = errors.New("login code contains a space or a control character")
)

// ValidateCodeShape checks the pasted code before it is written to the PTY.
//
// Spike 01 read the format out of the binary: it is `<code>#<state>`, and
// Claude Code rejects a missing half locally. Checking it here turns the
// likeliest user error — a truncated copy — into an instant, precise message
// instead of a terminal round-trip, and removes one case from the set the
// scrape has to interpret.
//
// Surrounding whitespace is trimmed before the check, because a pasted code
// routinely carries a trailing newline. That makes the contract with the
// caller exact: what it writes to the PTY must be bytes.TrimSpace(code),
// never the raw input, whose own newline would submit the line early.
//
// After trimming, the shape is design §7.2's `^[^#\s]+#[^#\s]+$`, narrowed
// further to printable ASCII. The narrowing is deliberate: the value is typed
// into a terminal, where a control byte is a command to the line discipline
// (`^C` kills the login, `^U` erases the line), not a character of a code.
//
// It takes bytes, and reads them in place: a string conversion would be an
// immutable copy of a one-time credential that no one can zero, and the
// caller's slice is zeroed after use (§13.5, redact by default). No error
// carries the code or any part of it; the messages are constants.
func ValidateCodeShape(code []byte) error {
	code = bytes.TrimSpace(code)
	if len(code) == 0 {
		return ErrCodeEmpty
	}
	for _, c := range code {
		if c <= 0x20 || c >= 0x7f {
			return ErrCodeBadCharacter
		}
	}
	switch n := bytes.Count(code, []byte{'#'}); {
	case n == 0:
		return ErrCodeNoSeparator
	case n > 1:
		return ErrCodeExtraHash
	}
	if i := bytes.IndexByte(code, '#'); i == 0 || i == len(code)-1 {
		return ErrCodeHalfMissing
	}
	return nil
}
