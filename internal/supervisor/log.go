package supervisor

import (
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Line is one line of a session server's log as Drydock keeps it: the
// visible text of a terminal line, escapes removed, redacted, and never the
// raw bytes. N is its sequence number in this workspace's log, so a reader
// can tell a gap (the ring dropped what it had no room for) from a quiet spell.
type Line struct {
	N    int64     `json:"n"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Ring is the bounded in-memory log of one workspace's session server (§8:
// a 1 MB ring per supervisor, never persisted). It holds structured, redacted
// lines rather than PTY bytes — "redact by default" (§13.5) is a property of
// what is stored, not of what is served, so nothing that reads the ring can
// see what the redaction removed.
//
// It is fed raw terminal output in whatever pieces reads return. An escape
// sequence or a line split across two reads is held back until it completes,
// so a token cut in half by a read boundary is still whole when it is
// redacted. The status block Claude Code reprints in place (Spike 02: twelve
// times in a 6.7 KB capture) would otherwise fill the ring with copies, so a
// line identical to one of the last few is not kept again.
type Ring struct {
	// Max bounds the ring by the bytes of text it holds.
	Max int

	mu      sync.Mutex
	lines   []Line
	size    int
	seq     int64
	pending []byte
	recent  []string
}

const (
	maxLine     = 2000 // bytes of text kept per line
	maxPending  = 8 << 10
	recentLines = 16
)

// Write adds terminal output. redact masks extra literal values — the
// workspace's own secret values — beside the fixed token patterns.
func (r *Ring) Write(now time.Time, b []byte, redact []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, b...)
	cut := lastBreak(r.pending)
	var complete []byte
	switch {
	case cut >= 0:
		complete = r.pending[:cut+1]
		r.pending = append([]byte(nil), r.pending[cut+1:]...)
	case len(r.pending) > maxPending:
		complete, r.pending = r.pending, nil
	default:
		return
	}
	for _, l := range strings.FieldsFunc(visible(complete), func(c rune) bool { return c == '\n' }) {
		r.add(now, l, redact)
	}
}

// Flush adds whatever partial line is held back: the process has ended and no
// more of it is coming.
func (r *Ring) Flush(now time.Time, redact []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return
	}
	text := visible(r.pending)
	r.pending = nil
	for _, l := range strings.FieldsFunc(text, func(c rune) bool { return c == '\n' }) {
		r.add(now, l, redact)
	}
}

func (r *Ring) add(now time.Time, l string, redact []string) { // r.mu held
	l = strings.TrimRight(l, " \t")
	if strings.TrimSpace(l) == "" {
		return
	}
	l = Redact(l, redact)
	if len(l) > maxLine {
		cut := maxLine
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		l = l[:cut] + "…"
	}
	for _, p := range r.recent {
		if p == l {
			return
		}
	}
	r.recent = append(r.recent, l)
	if len(r.recent) > recentLines {
		r.recent = r.recent[1:]
	}
	r.seq++
	r.lines = append(r.lines, Line{N: r.seq, At: now.UTC(), Text: l})
	r.size += len(l)
	max := r.Max
	if max <= 0 {
		max = 1 << 20
	}
	for r.size > max && len(r.lines) > 1 {
		r.size -= len(r.lines[0].Text)
		r.lines = r.lines[1:]
	}
}

// Mark adds a line of Drydock's own — "restarting", "stopped" — so the log
// reads as one story. Not redacted beyond the patterns: it is Drydock's text.
func (r *Ring) Mark(now time.Time, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recent = nil // a mark separates two runs; a repeated line after it is news
	r.add(now, "— "+text, nil)
	r.recent = nil
}

// Tail returns the last n lines, oldest first, and whether older lines were
// dropped from the ring.
func (r *Ring) Tail(n int) ([]Line, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]Line, n)
	copy(out, r.lines[len(r.lines)-n:])
	dropped := len(r.lines) > 0 && r.lines[0].N > 1
	return out, dropped || n < len(r.lines)
}

func lastBreak(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' || b[i] == '\r' {
			return i
		}
	}
	return -1
}

// tokenPatterns are credentials by shape: GitHub's token prefixes, Anthropic
// keys and OAuth tokens, a bearer header, and a credential-looking query
// parameter. Matched in the visible text, after escapes are gone, so an
// escape inside a token cannot split it past the pattern.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)([?&](?:code|token|access_token|refresh_token|key)=)[^&\s"']+`),
}

// Redact masks credential shapes and the given literal values in s. A value
// shorter than four bytes is not masked: masking "a" everywhere would make
// the log unreadable and protect nothing.
func Redact(s string, values []string) string {
	for _, v := range values {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	for _, re := range tokenPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, "${1}[redacted]")
		} else {
			s = re.ReplaceAllString(s, "[redacted]")
		}
	}
	return s
}

// visible returns the text a terminal would show for b, line breaks kept:
// CSI, OSC and the other string controls removed (an OSC 8 hyperlink keeps
// its label and loses its target), carriage returns turned into line breaks,
// and every other control byte but tab dropped. An escape with no terminator
// is dropped to the end of b.
func visible(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\r' || c == '\n':
			out.WriteByte('\n')
			i++
		case c == 0x1b:
			i = skipEscape(b, i)
		case c == '\t' || c >= 0x20 && c != 0x7f:
			out.WriteByte(c)
			i++
		default:
			i++
		}
	}
	return strings.ToValidUTF8(out.String(), "")
}

func skipEscape(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	switch b[i+1] {
	case '[':
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		return j + 1
	case ']', 'P', 'X', '^', '_':
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 && b[i+1] == ']' {
				return j + 1
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
		return len(b)
	default:
		j := i + 1
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
			j++
		}
		return j + 1
	}
}
