package classify

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func refusalTranscript(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", FixtureRoot, "transcripts", "claude-"+ClaudeCodeVersion, name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// refusalExitCode parses a `.exit` sidecar (`exit_code: N`).
func refusalExitCode(t *testing.T, name string) int {
	t.Helper()
	b := refusalTranscript(t, name+".exit")
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "exit_code" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("%s.exit: %v", name, err)
			}
			return n
		}
	}
	t.Fatalf("%s.exit: no exit_code line", name)
	return 0
}

var refusalFixtures = []struct {
	fixture string
	want    Refusal
}{
	{"refusal-409", RefusalWaitRegistration},
	{"refusal-not-trusted", RefusalWorkspaceNotTrusted},
	{"refusal-no-organization", RefusalNoOrganization},
	{"refusal-bad-commandline", RefusalBadCommandLine},
}

func TestClassifyRefusalFixtures(t *testing.T) {
	for _, tc := range refusalFixtures {
		t.Run(tc.fixture, func(t *testing.T) {
			got, err := ClassifyRefusal(refusalTranscript(t, tc.fixture))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// Testing §7: all four refusals exit 1, so feed each fixture beside its
// recorded exit status and demand four distinct verdicts. A classifier that
// used the exit code could not produce them.
func TestClassifyRefusalExitStatusIsNotADiscriminator(t *testing.T) {
	seen := map[Refusal]string{}
	for _, tc := range refusalFixtures {
		if code := refusalExitCode(t, tc.fixture); code != 1 {
			t.Fatalf("%s: recorded exit %d; the premise of this test is that all four exit 1", tc.fixture, code)
		}
		got, err := ClassifyRefusal(refusalTranscript(t, tc.fixture))
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if got == RefusalNone {
			t.Errorf("%s: classified as RefusalNone", tc.fixture)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s share verdict %d", prev, tc.fixture, got)
		}
		seen[got] = tc.fixture
	}
	if len(seen) != 4 {
		t.Fatalf("got %d distinct verdicts from four exit-1 fixtures, want 4", len(seen))
	}
}

// The retryable refusal is matched on its phrase, never on 409.
func TestClassifyRefusalNeverMatchesOn409(t *testing.T) {
	real := refusalTranscript(t, "refusal-409")
	if bytes.Contains(real, []byte("409")) {
		t.Fatal("the 2.1.289 fixture contains 409; this test's premise is that it does not — re-read Spike 02")
	}
	cases := []struct {
		name  string
		input string
		want  Refusal
	}{
		// Positive controls: the phrase, with and without the number.
		{"2.1.289 recorded, no 409", string(real), RefusalWaitRegistration},
		{"2.1.246 form, 409 prefixed", "Registration: Failed with status 409: This folder is already served by a terminal claude remote-control on this device. Stop it first.\n", RefusalWaitRegistration},
		// Negatives: the number without the phrase is not the wait.
		{"bare status", "Registration: Failed with status 409: Conflict\n", RefusalNone},
		{"http 409", "Error: request failed with HTTP 409\n", RefusalNone},
		{"409 in a path", "Error: could not read /tmp/ws-409/config\n", RefusalNone},
		// 409 beside another refusal's phrase yields that refusal.
		{"409 with trust phrase", "status 409\nError: Workspace not trusted. Please run `claude` in /w first.\n", RefusalWorkspaceNotTrusted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyRefusal([]byte(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// On a PTY an untrusted workspace hangs on a prompt instead of refusing.
// That is the supervisor's timeout to detect; the classifier must not
// mistake the prompt for the redirected refusal, nor for anything else.
func TestClassifyRefusalTrustPromptIsNotARefusal(t *testing.T) {
	hang := refusalTranscript(t, "hang-untrusted-tty")
	if !bytes.Contains(hang, []byte("[y/N]")) || !bytes.Contains(hang, []byte("Trust ")) {
		t.Fatal("hang-untrusted-tty no longer holds the trust prompt; the fixture moved")
	}
	got, err := ClassifyRefusal(hang)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != RefusalNone {
		t.Errorf("trust prompt classified as %d, want RefusalNone", got)
	}

	// Positive control: the same condition, redirected, is a refusal.
	got, err = ClassifyRefusal(refusalTranscript(t, "refusal-not-trusted"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != RefusalWorkspaceNotTrusted {
		t.Errorf("redirected not-trusted classified as %d, want RefusalWorkspaceNotTrusted", got)
	}
}

// The supervisor owns a PTY, so stderr may arrive with escapes and CRLFs.
// These inputs are synthetic.
func TestClassifyRefusalSurvivesEscapesAndWrapping(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Refusal
	}{
		{"colour inside phrase", "\x1b[31mError:\x1b[0m This folder is \x1b[1malready served\x1b[22m by a terminal\r\n", RefusalWaitRegistration},
		{"soft-wrapped at a space", "Error: This folder is already served by a\r\nterminal `claude remote-control`\r\n", RefusalWaitRegistration},
		{"osc 8 around phrase", "Error: \x1b]8;;https://example.invalid\x07Unable to determine your organization\x1b]8;;\x07 for RC\n", RefusalNoOrganization},
		{"osc 8 with ST", "Error: \x1b]8;;x\x1b\\cannot be used with --spawn\x1b]8;;\x1b\\\n", RefusalBadCommandLine},
		{"empty", "", RefusalNone},
		{"unrelated error", "Error: something else entirely\n", RefusalNone},
		{"near miss", "Error: This folder is served by a terminal\n", RefusalNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyRefusal([]byte(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRefusalExactlyOneRetryable(t *testing.T) {
	var retryable []Refusal
	for r := RefusalNone; r <= RefusalBadCommandLine; r++ {
		if r.Retryable() {
			retryable = append(retryable, r)
		}
	}
	if len(retryable) != 1 || retryable[0] != RefusalWaitRegistration {
		t.Fatalf("retryable refusals = %v, want exactly [RefusalWaitRegistration]", retryable)
	}
}
