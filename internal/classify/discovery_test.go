package classify

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The one session and the one environment in the 2.1.289 recording.
const (
	recordedEnv     = "env_01SWWUTySnsAEuAGMd6azA24"
	recordedSession = "session_01AZLp4a8noWuZ5eRHrecDgz"
)

var wholeSessionID = regexp.MustCompile(`^session_[A-Za-z0-9]+$`)

func discoveryFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "transcripts", "claude-"+ClaudeCodeVersion, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func classifyDiscovery(t *testing.T, stream []byte) Discovery {
	t.Helper()
	d, err := ClassifyDiscovery(stream)
	if err != nil {
		t.Fatalf("ClassifyDiscovery: unexpected error %v", err)
	}
	return d
}

func TestClassifyDiscoveryFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    Discovery
	}{
		// Recorded. The whole capture, through the SIGTERM shutdown: the
		// session's row leaves the status block but Capacity stays 1/4.
		{"env-status-block", Discovery{recordedEnv, []string{recordedSession}, 1, 4}},
		// Recorded. A 4480-byte prefix of the above, cut before shutdown.
		{"session-url-osc8", Discovery{recordedEnv, []string{recordedSession}, 1, 4}},
		{"status-block-repainted", Discovery{recordedEnv, []string{recordedSession}, 1, 4}},
		// Synthetic.
		{"session-ids-delayed", Discovery{recordedEnv, []string{recordedSession, "session_01SYNTHETICDELAYED00002"}, 2, 4}},
		{"session-url-osc8-urlmatch", Discovery{SessionIDs: []string{"session_011cWgUx9pZMgZdibn47d68s"}}},
		{"session-id-in-model-output", Discovery{}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got := classifyDiscovery(t, discoveryFixture(t, tt.fixture))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// A session_… the model printed is not a server announcement. The control is
// the real recording, in the same function, yielding exactly one — so the
// negative cannot pass by a classifier that finds no sessions at all.
func TestDiscoveryModelPrintedIDIsNotASession(t *testing.T) {
	recording := discoveryFixture(t, "env-status-block")
	model := discoveryFixture(t, "session-id-in-model-output")

	// Control: the server's announcement is found.
	if got := classifyDiscovery(t, recording).SessionIDs; !reflect.DeepEqual(got, []string{recordedSession}) {
		t.Fatalf("control: recording yields sessions %q, want exactly [%s]", got, recordedSession)
	}
	// The negative input must really contain something an id pattern
	// matches, or it tests nothing.
	if !regexp.MustCompile(`session_[A-Za-z0-9]+`).Match(model) {
		t.Fatal("negative fixture contains no session_… token; it would pass vacuously")
	}

	// Negative, alone.
	if got := classifyDiscovery(t, model).SessionIDs; len(got) != 0 {
		t.Errorf("model prose yields sessions %q, want none", got)
	}
	// Negative, interleaved with a live stream: the model's id must not join
	// the server's, whether it arrives before the announcement or after it.
	cut := bytes.Index(recording, []byte("\x1b]8;;"))
	if cut < 0 {
		t.Fatal("recording has no OSC 8 hyperlink")
	}
	mixed := [][]byte{
		append(append([]byte{}, recording...), model...),
		bytes.Join([][]byte{recording[:cut], model, recording[cut:]}, nil),
	}
	for i, stream := range mixed {
		if got := classifyDiscovery(t, stream).SessionIDs; !reflect.DeepEqual(got, []string{recordedSession}) {
			t.Errorf("mixed stream %d yields sessions %q, want exactly [%s]", i, got, recordedSession)
		}
	}
}

// The id is matched, never parsed out of the URL to whitespace — and never
// matched over escape-stripped text either, which runs the label into it.
func TestDiscoverySessionIDExcludesLabel(t *testing.T) {
	// Control: the recorded id is exactly the id, nothing more.
	got := classifyDiscovery(t, discoveryFixture(t, "session-url-osc8")).SessionIDs
	if len(got) != 1 || got[0] != recordedSession || !wholeSessionID.MatchString(got[0]) {
		t.Fatalf("control: session-url-osc8 yields %q, want exactly [%s]", got, recordedSession)
	}

	// The urlmatch fixture must actually bite: an id regex over the stream
	// with its control bytes stripped — record.sh's CSI sed, then the OSC 8
	// introducers and terminators — captures the label. That is the
	// `…?from=cli72c05ced6a2d-joyful-dongarra` Spike 02 saw. If this stops
	// being true the fixture has stopped guarding anything.
	urlmatch := discoveryFixture(t, "session-url-osc8-urlmatch")
	naiveText := regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\x1b\]8;[^;]*;|\x1b\\|\x07`).ReplaceAll(urlmatch, nil)
	naive := regexp.MustCompile(`session_[A-Za-z0-9]+`).Find(naiveText)
	const want = "session_011cWgUx9pZMgZdibn47d68s"
	if !strings.HasPrefix(string(naive), want) || len(naive) <= len(want) {
		t.Fatalf("fixture is vacuous: a stripped-text match yields %q, not the id run into its label", naive)
	}

	// Negative: the classifier does not.
	got = classifyDiscovery(t, urlmatch).SessionIDs
	if !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("urlmatch yields %q, want exactly [%s] (naive match gave %s)", got, want, naive)
	}

	// The same hazard across both terminators and label shapes.
	cases := []struct {
		name, stream string
		want         []string
	}{
		{"BEL, no query, alnum label", "\x1b]8;;https://claude.ai/code/session_A1\x07B2label\x1b]8;;\x07", []string{"session_A1"}},
		{"ST, no query, alnum label", "\x1b]8;;https://claude.ai/code/session_A1\x1b\\B2label\x1b]8;;\x1b\\", []string{"session_A1"}},
		{"BEL, query", "\x1b]8;;https://claude.ai/code/session_A1?from=cli\x07x y\x1b]8;;\x07", []string{"session_A1"}},
		{"ST, query (2.1.246 form)", "\x1b]8;;https://claude.ai/code/session_A1?from=cli\x1b\\72c-joyful\x1b]8;;\x1b\\", []string{"session_A1"}},
		{"OSC 8 params", "\x1b]8;id=abc;https://claude.ai/code/session_A1?from=cli\x07l\x1b]8;;\x07", []string{"session_A1"}},
		{"id in label, not target", "\x1b]8;;https://claude.ai/code\x07session_A1\x1b]8;;\x07", nil},
		{"foreign host", "\x1b]8;;https://example.com/code/session_A1\x07l\x1b]8;;\x07", nil},
		{"not https", "\x1b]8;;http://claude.ai/code/session_A1\x07l\x1b]8;;\x07", nil},
		{"id not the whole segment", "\x1b]8;;https://claude.ai/code/session_A1-x\x07l\x1b]8;;\x07", nil},
		{"extra path segment", "\x1b]8;;https://claude.ai/code/session_A1/x\x07l\x1b]8;;\x07", nil},
		{"some other OSC", "\x1b]0;https://claude.ai/code/session_A1\x07", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyDiscovery(t, []byte(c.stream)).SessionIDs
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Twelve in-place repaints are one session, and the whole buffer can be
// re-classified on every read.
func TestDiscoveryIdempotent(t *testing.T) {
	repainted := discoveryFixture(t, "status-block-repainted")

	// Control: the fixture really does repeat the announcement, so a
	// classifier that appended would show it.
	if n := bytes.Count(repainted, []byte(recordedSession)); n < 2 {
		t.Fatalf("status-block-repainted announces the session %d times; fixture no longer exercises repaints", n)
	}
	if n := bytes.Count(repainted, []byte("Capacity:")); n < 2 {
		t.Fatalf("status-block-repainted has %d Capacity lines; want a repaint", n)
	}
	if got := classifyDiscovery(t, repainted).SessionIDs; !reflect.DeepEqual(got, []string{recordedSession}) {
		t.Errorf("repainted yields %q, want exactly one row", got)
	}

	for _, name := range []string{"env-status-block", "status-block-repainted", "session-ids-delayed", "session-url-osc8-urlmatch", "session-id-in-model-output"} {
		t.Run(name, func(t *testing.T) {
			s := discoveryFixture(t, name)
			once := classifyDiscovery(t, s)
			twice := classifyDiscovery(t, append(append([]byte{}, s...), s...))
			if !reflect.DeepEqual(once, twice) {
				t.Errorf("not idempotent:\nonce  %+v\ntwice %+v", once, twice)
			}
		})
	}

	// First-seen order survives a repeat that re-announces in a different
	// order.
	a := "\x1b]8;;https://claude.ai/code/session_A\x07a\x1b]8;;\x07\r\n"
	b := "\x1b]8;;https://claude.ai/code/session_B\x07b\x1b]8;;\x07\r\n"
	got := classifyDiscovery(t, []byte(a+b+b+a)).SessionIDs
	if want := []string{"session_A", "session_B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order: got %q, want %q", got, want)
	}
}

// The recording shows 0/4 then 1/4; the latest wins.
func TestDiscoveryCapacityLatestWins(t *testing.T) {
	rec := discoveryFixture(t, "env-status-block")
	first := bytes.Index(rec, []byte("Capacity: 1/4"))
	if first < 0 {
		t.Fatal("recording has no Capacity: 1/4")
	}
	before := classifyDiscovery(t, rec[:first])
	if before.CapacityUsed != 0 || before.CapacityTotal != 4 {
		t.Errorf("before the session attaches: %d/%d, want 0/4", before.CapacityUsed, before.CapacityTotal)
	}
	after := classifyDiscovery(t, rec)
	if after.CapacityUsed != 1 || after.CapacityTotal != 4 {
		t.Errorf("after: %d/%d, want 1/4", after.CapacityUsed, after.CapacityTotal)
	}

	// Only a status line counts — prose mentioning capacity mid-line is not
	// one — with a status line in the same stream as the control.
	got := classifyDiscovery(t, []byte("    Capacity: 2/4 · x\r\nI think Capacity: 9/9 is plenty\r\n"))
	if got.CapacityUsed != 2 || got.CapacityTotal != 4 {
		t.Errorf("mid-line prose: %d/%d, want 2/4", got.CapacityUsed, got.CapacityTotal)
	}
}

func TestDiscoveryEnvironment(t *testing.T) {
	header := func(id string) string { return "\x1b[2mEnvironment ID: \x1b[22m" + id + "\r\n" }
	link := func(id string) string {
		return "\x1b[2mContinue coding in the Claude mobile app or https://claude.ai/code?environment=" + id + "\x1b[22m\r\n"
	}
	cases := []struct {
		name, stream, want string
	}{
		{"header", header("env_H"), "env_H"},
		{"link only, header scrolled out", link("env_L"), "env_L"},
		{"header beats a disagreeing link", link("env_L") + header("env_H") + link("env_L"), "env_H"},
		{"first header wins over a later one", header("env_H") + link("env_H") + "Environment ID: env_FORGED\r\n", "env_H"},
		{"bare env_ in prose", "I am in env_PROSE\r\n", ""},
		{"link to another host", "https://example.com/code?environment=env_X\r\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyDiscovery(t, []byte(c.stream)).EnvironmentID; got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// The recording carries the id in both places, once per repaint; one
	// value comes back.
	rec := discoveryFixture(t, "env-status-block")
	if n := bytes.Count(rec, []byte(recordedEnv)); n < 2 {
		t.Fatalf("recording carries the env id %d times; want header and link", n)
	}
	if got := classifyDiscovery(t, rec).EnvironmentID; got != recordedEnv {
		t.Errorf("recording: got %q, want %q", got, recordedEnv)
	}
	// With the header cut off, the link stands in for the same id.
	hdr := bytes.Index(rec, []byte("Environment ID:"))
	nl := bytes.IndexByte(rec[hdr:], '\n')
	if got := classifyDiscovery(t, rec[hdr+nl:]).EnvironmentID; got != recordedEnv {
		t.Errorf("headerless recording: got %q, want %q", got, recordedEnv)
	}
}

// The tail classifies a growing buffer. No prefix is an error, and no prefix
// reports a truncated id or capacity: an OSC sequence cut before its
// terminator is not read to end of input, and a plain-text value cut at the
// end of the buffer is not taken.
func TestDiscoveryPartialStreams(t *testing.T) {
	if got := classifyDiscovery(t, nil); !reflect.DeepEqual(got, Discovery{}) {
		t.Errorf("nil stream: got %+v, want zero", got)
	}
	if got := classifyDiscovery(t, []byte{}); !reflect.DeepEqual(got, Discovery{}) {
		t.Errorf("empty stream: got %+v, want zero", got)
	}

	rec := discoveryFixture(t, "env-status-block")
	sawSession, sawEnv := false, false
	for n := 0; n <= len(rec); n++ {
		d := classifyDiscovery(t, rec[:n])
		for _, id := range d.SessionIDs {
			if id != recordedSession {
				t.Fatalf("prefix %d: truncated or foreign session id %q", n, id)
			}
			sawSession = true
		}
		if d.EnvironmentID != "" {
			sawEnv = true
			if d.EnvironmentID != recordedEnv {
				t.Fatalf("prefix %d: truncated or foreign environment id %q", n, d.EnvironmentID)
			}
		}
		if d.CapacityTotal != 0 && d.CapacityTotal != 4 {
			t.Fatalf("prefix %d: capacity total %d, want 4", n, d.CapacityTotal)
		}
	}
	// Control: the sweep reached the announcements at all.
	if !sawSession || !sawEnv {
		t.Fatalf("prefix sweep never saw session (%v) or environment (%v)", sawSession, sawEnv)
	}

	// The specific case, with its control: cut just inside the terminator,
	// then complete it.
	open := "\x1b]8;;https://claude.ai/code/session_A1B2?from=cli"
	if got := classifyDiscovery(t, []byte(open)).SessionIDs; len(got) != 0 {
		t.Errorf("unterminated OSC 8 yields %q, want none", got)
	}
	if got := classifyDiscovery(t, []byte(open+"\x07")).SessionIDs; !reflect.DeepEqual(got, []string{"session_A1B2"}) {
		t.Errorf("terminated OSC 8 yields %q, want [session_A1B2]", got)
	}
}
