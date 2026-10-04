package classify

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
)

// 4. The discovery tail (design §8, Spike 02)

// Discovery is what the supervisor's `--verbose` stream reveals.
type Discovery struct {
	// EnvironmentID is the durable handle — one per workspace, survives a
	// restart, and the card's only link. Stored in workspace.environment_id.
	EnvironmentID string
	// SessionIDs are the `session_[A-Za-z0-9]+` ids the server announced,
	// each once, in first-seen order. An id is taken only from the *target*
	// of an OSC 8 hyperlink (`ESC]8;;<url>BEL`), never from visible text:
	// the target is bounded by its terminator, so the label that follows it
	// cannot run into the id, and model prose that merely mentions an id is
	// visible text. See ClassifyDiscovery for the full rule.
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
// same line recurs constantly (12 times in the recorded fixture) and the result
// must be deduplicated by id rather than appended; and a `session_…` id the
// *model* printed in its own output is not a server announcement, which is the
// false positive the negative fixture exists to catch.
//
// The stream is first split into visible text and escape sequences by a small
// scanner (CSI, OSC, the string controls, and two-byte escapes). Then:
//
// Session ids come only from OSC 8 hyperlink targets whose URL is
// `https://claude.ai/code/session_<id>` — the server wraps every per-session
// URL that way (Spike 02 on 2.1.246 with an ST terminator, `ESC \`; re-measured
// on 2.1.289 with BEL), and both terminators are accepted. This one rule
// satisfies both halves of the contract. "Match the id, not the URL" exists
// because a parse over escape-stripped text sees the URL and its label run
// together (`…/session_01ABCdrydockrec-gleaming-feather`), so even an id regex
// over stripped text captures the label whenever the URL has no query string.
// Reading the target instead keeps the id bounded by the escape terminator,
// and the id must be the whole final path segment, so a label can never
// extend it. And because model prose is visible text — Claude Code does not
// pass raw escape bytes through from model output — a `session_…` the model
// printed is never a hyperlink target, so it yields no session. The residual
// risk is a future version that renders the model's own markdown links as OSC
// 8 hyperlinks to claude.ai session URLs into this stream; the
// session-id-in-model-output fixture does not cover that, and a re-record
// (testing §11.1) is where it would surface. An OSC sequence with no
// terminator yet — the tail of a stream cut mid-escape — is ignored rather
// than read to end of input, so a truncated id is never reported; the next
// read, with the terminator, picks it up.
//
// The environment id comes from the `Environment ID: env_…` header line, and
// the *first* one wins. The header is printed once, at startup, before any
// session — and so before any model — exists, so the first occurrence is the
// server's by construction, and the id cannot change within one process.
// Only when the header is absent (a ring buffer that has dropped the start of
// the stream) does the `https://claude.ai/code?environment=env_…` link in the
// repainted status block stand in, again first-seen. When both are present
// and disagree, the header wins: it is the labelled announcement, and the link
// is the derived one.
//
// Capacity comes from the *last* `Capacity: N/M` line, anchored at the start
// of a visible line (the status block indents it), because it legitimately
// changes — `0/4` before the pre-created session attaches, `1/4` after.
//
// Nothing here is an error, and the returned error is always nil. The tail is
// incremental: a stream that has not yet reached the header, or in which no
// session has appeared, is a normal partial state, reported as zero fields.
// "No environment id after N seconds" is a supervisor timeout, not something a
// pure function over a prefix of the stream can decide. The error stays in the
// signature for the same reason it is on every classifier (testing §5.5).
//
// The result is idempotent under repetition: classifying a stream concatenated
// with itself yields the same Discovery, which is what lets the supervisor
// re-run this over its whole buffer on every read.
func ClassifyDiscovery(stream []byte) (Discovery, error) {
	text, targets := splitEscapes(stream)

	var d Discovery
	if m := envHeaderRE.FindSubmatch(text); m != nil {
		d.EnvironmentID = string(m[1])
	} else if m := envLinkRE.FindSubmatch(text); m != nil {
		d.EnvironmentID = string(m[1])
	}

	seen := make(map[string]bool)
	for _, t := range targets {
		id, ok := sessionFromHyperlink(t)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		d.SessionIDs = append(d.SessionIDs, id)
	}

	for _, m := range capacityRE.FindAllSubmatch(text, -1) {
		used, err1 := strconv.Atoi(string(m[1]))
		total, err2 := strconv.Atoi(string(m[2]))
		if err1 != nil || err2 != nil {
			continue // out of range for an int: not a status line
		}
		d.CapacityUsed, d.CapacityTotal = used, total
	}
	return d, nil
}

var (
	// Each pattern demands the byte after the value, so a stream cut
	// mid-value (`env_01SW`, `Capacity: 1/4` of `1/40`) matches nothing
	// rather than a truncation; escapes are already gone, so that byte is
	// the line end or the text that follows.
	envHeaderRE = regexp.MustCompile(`Environment ID: (env_[A-Za-z0-9]+)[^A-Za-z0-9]`)
	envLinkRE   = regexp.MustCompile(`https://claude\.ai/code\?environment=(env_[A-Za-z0-9]+)[^A-Za-z0-9]`)
	capacityRE  = regexp.MustCompile(`(?m)^[ \t]*Capacity: ([0-9]+)/([0-9]+)[^0-9]`)
	sessionRE   = regexp.MustCompile(`^/code/(session_[A-Za-z0-9]+)$`)
)

// sessionFromHyperlink returns the session id an OSC 8 target names, if it is
// exactly a claude.ai per-session URL. The id must be the whole final path
// segment, so nothing after it — a label, a stray byte — can be absorbed.
func sessionFromHyperlink(target []byte) (string, bool) {
	u, err := url.Parse(string(target))
	if err != nil || u.Scheme != "https" || u.Host != "claude.ai" {
		return "", false
	}
	m := sessionRE.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

const (
	esc = 0x1b
	bel = 0x07
)

// splitEscapes separates a terminal byte stream into its visible text and the
// URIs of the OSC 8 hyperlinks it opens. Escape sequences are dropped from the
// text, and carriage returns become newlines so that a repaint begins a line.
// A sequence with no terminator before end of input is dropped whole.
func splitEscapes(b []byte) (text []byte, hyperlinks [][]byte) {
	text = make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c == '\r' {
			text = append(text, '\n')
			i++
			continue
		}
		if c != esc {
			text = append(text, c)
			i++
			continue
		}
		if i+1 >= len(b) {
			break
		}
		switch b[i+1] {
		case '[': // CSI: parameters and intermediates, then one final byte.
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			i = j + 1
		case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: to BEL or ST.
			body, next, ok := stringControl(b, i+2, b[i+1] == ']')
			if !ok {
				return text, hyperlinks
			}
			if b[i+1] == ']' {
				if uri, ok := osc8Target(body); ok {
					hyperlinks = append(hyperlinks, uri)
				}
			}
			i = next
		default: // Two-byte escape, possibly with intermediates (ESC ( B).
			j := i + 1
			for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
				j++
			}
			i = j + 1
		}
	}
	return text, hyperlinks
}

// stringControl finds the end of a string control whose body starts at
// start. ST (`ESC \`) always terminates it; BEL does too for OSC, which is how
// xterm and Claude Code 2.1.289 end it. It returns the body, the index after
// the terminator, and whether a terminator was found at all.
func stringControl(b []byte, start int, belEnds bool) (body []byte, next int, ok bool) {
	for j := start; j < len(b); j++ {
		switch {
		case b[j] == bel && belEnds:
			return b[start:j], j + 1, true
		case b[j] == esc && j+1 < len(b) && b[j+1] == '\\':
			return b[start:j], j + 2, true
		}
	}
	return nil, len(b), false
}

// osc8Target returns the URI of an OSC 8 body (`8;params;URI`) that opens a
// hyperlink. The closing form, with an empty URI, is not a target.
func osc8Target(body []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(body, []byte("8;"))
	if !ok {
		return nil, false
	}
	_, uri, ok := bytes.Cut(rest, []byte(";"))
	if !ok || len(uri) == 0 {
		return nil, false
	}
	return uri, true
}
