package supervisor

import (
	"regexp"
	"slices"
	"sort"
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
//
// What it masks is the ring's own, not each caller's: the literal values come
// from the source it was made with, asked afresh on every Write, Flush and
// Mark, and every line reaches the ring through add, which masks them. So no
// path in — a terminated line, the held-back tail flushed at exit, on a stop
// or at a hang gate, or a mark — can store a line unmasked, and no call site
// has a list to forget to pass. (A Flush once passed nil and stored a final
// unterminated line with a granted secret's value in clear.)
type Ring struct {
	max    int
	values func() []string

	mu      sync.Mutex
	lines   []Line
	size    int
	seq     int64
	pending []byte
	recent  []string
	// masks is every literal value values has returned in this ring's life,
	// longest first. It never shrinks: a value rotated out or a grant
	// revoked is still in a running server's environment until it is
	// restarted (§10.3's needs_supervisor_restart), and a source that fails
	// for a moment (an undeliverable snapshot) must not unmask what it
	// masked before.
	masks []string
}

// NewRing returns a ring bounded at max bytes of text (0: 1 MB) that masks,
// beside the credential patterns, every value values returns — a
// workspace's granted secrets' values. values may be nil, and is never
// called with the ring's lock held.
func NewRing(max int, values func() []string) *Ring {
	return &Ring{max: max, values: values}
}

// current asks the source for the values to mask now. Called before r.mu is
// taken, so a slow source never blocks a reader of the ring.
func (r *Ring) current() []string {
	if r.values == nil {
		return nil
	}
	return r.values()
}

func (r *Ring) mergeLocked(vals []string) { // r.mu held
	added := false
	for _, v := range vals {
		if len(v) < minMask || slices.Contains(r.masks, v) {
			continue
		}
		r.masks = append(r.masks, v)
		added = true
	}
	if added {
		// Longest first, so a value that contains another is masked whole
		// rather than left with its remainder showing.
		sort.SliceStable(r.masks, func(i, j int) bool { return len(r.masks[i]) > len(r.masks[j]) })
	}
}

const (
	minMask     = 4    // the shortest literal value Redact masks
	maxLine     = 2000 // bytes of text kept per line
	maxPending  = 8 << 10
	recentLines = 16
)

// Write adds terminal output.
func (r *Ring) Write(now time.Time, b []byte) {
	vals := r.current()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mergeLocked(vals)
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
		r.add(now, l)
	}
}

// Flush adds whatever partial line is held back: the process has ended and no
// more of it is coming, or it waits at a prompt that has no line end.
func (r *Ring) Flush(now time.Time) {
	vals := r.current()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mergeLocked(vals)
	if len(r.pending) == 0 {
		return
	}
	text := visible(r.pending)
	r.pending = nil
	for _, l := range strings.FieldsFunc(text, func(c rune) bool { return c == '\n' }) {
		r.add(now, l)
	}
}

// add is the one way a line enters the ring, and it masks: the credential
// patterns and every value in masks.
func (r *Ring) add(now time.Time, l string) { // r.mu held
	l = strings.TrimRight(l, " \t")
	if strings.TrimSpace(l) == "" {
		return
	}
	l = Redact(l, r.masks)
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
	max := r.max
	if max <= 0 {
		max = 1 << 20
	}
	for r.size > max && len(r.lines) > 1 {
		r.size -= len(r.lines[0].Text)
		r.lines = r.lines[1:]
	}
}

// Mark adds a line of Drydock's own — "restarting", "stopped" — so the log
// reads as one story. It is Drydock's text, and masked like any other line.
func (r *Ring) Mark(now time.Time, text string) {
	vals := r.current()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mergeLocked(vals)
	r.recent = nil // a mark separates two runs; a repeated line after it is news
	r.add(now, "— "+text)
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
		if len(v) >= minMask {
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
