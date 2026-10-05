//go:build linux

package claudetest_test

// The contract test for fakeclaude (testing §6.1). A fake encodes a belief
// about the real thing, so two things are pinned here:
//
//   - what fakeclaude writes, on a real PTY, is byte-for-byte the recorded
//     corpus for every mode that has a recording, and structurally the corpus
//     for the ones that are synthetic — so a re-recorded corpus fails here
//     until the fake is brought into line;
//   - driving each mode through the classifiers yields the verdicts their own
//     tests expect, so the fake cannot drift into a world the classifiers do
//     not live in.
//
// Every negative here has its positive control in the same function
// (testing §4.1): a hang is asserted beside a refusal that does exit, a
// rejected id beside an accepted one, a violation beside a clean submission.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudetest"
)

const (
	// settle bounds anything that should happen promptly.
	settle = 10 * time.Second
	// hangWindow is how long a hang has to stay hung to count as one. It is
	// the assertion's own timeout, not a claim about the real binary.
	hangWindow = 1500 * time.Millisecond
)

func tr(t *testing.T, name string) []byte { return claudetest.Transcript(t, name) }

func meta(t *testing.T, name string) map[string]string { return sidecar(t, name+".meta") }

// sidecar reads a `key: value` file: a .meta, or a refusal's .exit.
func sidecar(t *testing.T, file string) map[string]string {
	t.Helper()
	b := tr(t, file)
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// diverges says where a stream parts from the recording, with a little of
// each side, rather than dumping kilobytes of escapes.
func diverges(got, want []byte) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	if i == len(got) && i == len(want) {
		return "identical"
	}
	return fmt.Sprintf("diverges from the recording at byte %d (got %d bytes, want %d): got %q, want %q",
		i, len(got), len(want), got[i:min(len(got), i+40)], want[i:min(len(want), i+40)])
}

// reaches is a WaitOutput condition: the stream is as long as want, or has
// already diverged from it — so a wrong byte fails at once, with the stream
// in hand, rather than as a timeout.
func reaches(want []byte) func([]byte) bool {
	return func(b []byte) bool { return len(b) >= len(want) || !bytes.HasPrefix(want, b) }
}

// --- the version ------------------------------------------------------------

func TestVersionIsThePinnedOneAndNoOther(t *testing.T) {
	if claudetest.Version != classify.ClaudeCodeVersion {
		t.Fatalf("fakeclaude impersonates %s but the classifiers are pinned to %s: re-derive the fake from the new corpus",
			claudetest.Version, classify.ClaudeCodeVersion)
	}

	// Positive: it answers --version exactly as 2.1.289 does.
	f := claudetest.Install(t, claudetest.Script{})
	out, err := f.Command("--version").Output()
	if err != nil || string(out) != claudetest.VersionLine || claudetest.VersionLine != "2.1.289 (Claude Code)\n" {
		t.Fatalf("--version = %q, %v", out, err)
	}

	// Negative: asked to be another version, it refuses every command
	// rather than pretend.
	g := claudetest.Install(t, claudetest.Script{Version: "2.1.300", RemoteControl: []claudetest.Step{{Mode: claudetest.RCServe}}})
	for _, args := range [][]string{{"--version"}, {"remote-control"}} {
		var stdout, stderr bytes.Buffer
		cmd := g.Command(args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if cmd.ProcessState.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "refusing to impersonate 2.1.300") {
			t.Errorf("%v as 2.1.300: exit %d, stdout %q, stderr %q, err %v", args, cmd.ProcessState.ExitCode(), stdout.String(), stderr.String(), err)
		}
	}
}

// A corpus recorded for a newer version than the fake impersonates is a bump
// the fake has not followed. This is what fails first after §11.1's re-record.
func TestNoNewerCorpusThanTheFake(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(claudetest.CorpusRoot(t), "transcripts", "claude-*"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range dirs {
		v := strings.TrimPrefix(filepath.Base(d), "claude-")
		entries, _ := os.ReadDir(d)
		if v == claudetest.Version {
			found = len(entries) > 0
			continue
		}
		if len(entries) > 0 && newer(v, claudetest.Version) {
			t.Errorf("the corpus has %s, newer than fakeclaude's %s: re-derive the fake", v, claudetest.Version)
		}
	}
	if !found {
		t.Fatalf("no corpus for %s itself", claudetest.Version)
	}
}

func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Every file the fake replays carries a .meta for this version, and the ones
// it synthesizes say so.
func TestEveryReplayedFixtureIsLabelled(t *testing.T) {
	synthetic := []string{claudetest.FixtureLoginSuccess, claudetest.FixtureDelayedSession, claudetest.FixtureModelOutputID,
		claudetest.FixtureHangDialog, claudetest.FixtureCrash}
	recorded := []string{claudetest.FixtureLoginPrompt80, claudetest.FixtureLoginPrompt, claudetest.FixtureLoginInvalid,
		claudetest.FixtureServe, claudetest.FixtureServeRunning, claudetest.FixtureHangTrust,
		claudetest.FixtureRefuseWait, claudetest.FixtureRefuseTrust, claudetest.FixtureRefuseOrg, claudetest.FixtureRefuseCmdline}
	for _, n := range append(slices.Clone(synthetic), recorded...) {
		m := meta(t, n)
		if m["claude_version"] != claudetest.Version {
			t.Errorf("%s.meta: claude_version %q", n, m["claude_version"])
		}
		want := "recorded"
		if slices.Contains(synthetic, n) {
			want = "synthetic"
		}
		if m["provenance"] != want {
			t.Errorf("%s.meta: provenance %q, want %q", n, m["provenance"], want)
		}
	}
}

// The pins are the corpus as it stands. When this fails after a re-record,
// the fix is not to paste the new hashes: it is to re-run this file against
// the new bytes, re-derive whatever mode was built from the changed file, and
// then update the pin.
func TestPinsAreTheCorpus(t *testing.T) {
	root := claudetest.CorpusRoot(t)
	for rel, want := range claudetest.Pinned {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s has changed since fakeclaude was derived from it", rel)
		}
	}
}

// A changed recording stops the fake. Control: the same copy of the corpus,
// unchanged, is replayed.
func TestTheFakeRefusesAChangedRecording(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		root := t.TempDir()
		dir := filepath.Join(root, "transcripts", "claude-"+claudetest.Version)
		os.MkdirAll(dir, 0o755)
		b := tr(t, claudetest.FixtureRefuseWait)
		if tamper {
			b = bytes.Replace(b, []byte("already served by a terminal"), []byte("already served by a terminal (409)"), 1)
		}
		os.WriteFile(filepath.Join(dir, claudetest.FixtureRefuseWait), b, 0o644)

		f := claudetest.Install(t, claudetest.Script{Corpus: root, RemoteControl: []claudetest.Step{{Mode: claudetest.RCRefuseWaitRegistration}}})
		var stderr bytes.Buffer
		cmd := f.Command(serveArgs...)
		cmd.Stderr = &stderr
		cmd.Run()
		switch rc := cmd.ProcessState.ExitCode(); {
		case !tamper && (rc != 1 || !bytes.Equal(stderr.Bytes(), b)):
			t.Fatalf("control: an unchanged copy was not replayed: exit %d %q", rc, stderr.String())
		case tamper && (rc != 3 || bytes.Contains(stderr.Bytes(), []byte("terminal (409)")) || len(f.Violations(t)) != 1):
			t.Fatalf("a changed recording was replayed: exit %d %q", rc, stderr.String())
		}
	}
}

// --- synthetic fixtures are structurally the recordings ---------------------

var gatePrompt = regexp.MustCompile(`\x1b\[1G\x1b\[0J([^\x1b]*)\x1b\[([0-9]+)G$`)

// The dialog prompt is synthetic, so it must be framed exactly as the one
// recorded gate prompt is: erase the line, write the prompt, park the cursor
// one column after it. If a re-record changes how the trust prompt is drawn,
// this fails and the synthetic one is rebuilt to match.
func TestSyntheticFixturesHaveTheRecordedShape(t *testing.T) {
	for _, n := range []string{claudetest.FixtureHangTrust, claudetest.FixtureHangDialog} {
		m := gatePrompt.FindSubmatch(tr(t, n))
		if m == nil {
			t.Fatalf("%s does not end in a drawn prompt", n)
		}
		if col, _ := strconv.Atoi(string(m[2])); col != len(m[1])+1 {
			t.Errorf("%s: cursor at %d after a %d-column prompt", n, col, len(m[1]))
		}
	}
	if !bytes.Contains(tr(t, claudetest.FixtureHangTrust), []byte("? [y/N] ")) ||
		!bytes.HasSuffix(gatePrompt.FindSubmatch(tr(t, claudetest.FixtureHangDialog))[1], []byte("Enable Remote Control? (y/n) ")) {
		t.Error("the gate prompts are not the two Spike 02 measured")
	}

	serve := tr(t, claudetest.FixtureServe)
	for _, n := range []string{claudetest.FixtureServeRunning, claudetest.FixtureCrash} {
		if !bytes.HasPrefix(serve, tr(t, n)) {
			t.Errorf("%s is no longer a prefix of %s", n, claudetest.FixtureServe)
		}
	}
	crash := tr(t, claudetest.FixtureCrash)
	if !bytes.HasSuffix(crash, []byte("\r\n")) || bytes.Contains(crash, []byte("Connected")) || !bytes.Contains(crash, []byte("Connecting")) {
		t.Error("crash-after-start is not the header through the Connecting line")
	}
	for _, n := range []string{claudetest.FixtureDelayedSession} {
		if !bytes.HasPrefix(tr(t, n), tr(t, claudetest.FixtureServeRunning)) {
			t.Errorf("%s no longer starts with the recorded serve", n)
		}
	}
	prompt := tr(t, claudetest.FixtureLoginPrompt)
	for _, n := range []string{claudetest.FixtureLoginInvalid, claudetest.FixtureLoginSuccess} {
		if !bytes.HasPrefix(tr(t, n), prompt) {
			t.Errorf("%s no longer starts with %s", n, claudetest.FixtureLoginPrompt)
		}
	}
}

// --- login ------------------------------------------------------------------

const code = "fixturecodeAAAA#fixturestateBBBB"

func loginFake(t *testing.T, l claudetest.Login) *claudetest.Fake {
	return claudetest.Install(t, claudetest.Script{Login: &l})
}

func promptFor(t *testing.T, width int) []byte {
	if width == 80 {
		return tr(t, claudetest.FixtureLoginPrompt80)
	}
	return tr(t, claudetest.FixtureLoginPrompt)
}

func suffix(t *testing.T, name string) []byte {
	return bytes.TrimPrefix(tr(t, name), tr(t, claudetest.FixtureLoginPrompt))
}

// A wrong code, then the right one, on the same PTY at both widths. At 1000
// columns the stream is byte-for-byte the recording at every stage.
func TestLoginInvalidCodeThenSuccess(t *testing.T) {
	for _, width := range []int{1000, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			f := loginFake(t, claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(code)})
			tm := f.Start(t, width, "auth", "login", "--claudeai")

			prompt := promptFor(t, width)
			out, err := tm.WaitOutput(settle, reaches(prompt))
			if err != nil || !bytes.Equal(out, prompt) {
				t.Fatalf("prompt: %v: %s", err, diverges(out, prompt))
			}
			v, err := classify.ClassifyLogin(out)
			if err != nil || v.Phase != classify.LoginAwaitingCode || len(v.AuthorizeURL) < 400 {
				t.Fatalf("at the prompt: %+v, %v", v, err)
			}
			url := v.AuthorizeURL

			if err := tm.Write([]byte("wrongcode#wrongstate\r")); err != nil {
				t.Fatal(err)
			}
			want := append(slices.Clone(prompt), suffix(t, claudetest.FixtureLoginInvalid)...)
			out, err = tm.WaitOutput(settle, reaches(want))
			if err != nil || !bytes.Equal(out, want) {
				t.Fatalf("after a wrong code: %v: %s", err, diverges(out, want))
			}
			if width == 1000 && !bytes.Equal(out, tr(t, claudetest.FixtureLoginInvalid)) {
				t.Fatal("at 1000 columns the invalid-code stream is not the recording")
			}
			v, err = classify.ClassifyLogin(out)
			if err != nil || v.Phase != classify.LoginInvalidCode || v.AuthorizeURL != url {
				t.Fatalf("after a wrong code: %+v, %v", v, err)
			}
			// Still at the prompt: no teardown needed (Spike 01).
			if exited, _, _ := tm.Wait(300 * time.Millisecond); exited {
				t.Fatal("a wrong code ended the login")
			}

			if err := tm.Write([]byte(code + "\r")); err != nil {
				t.Fatal(err)
			}
			exited, rc, out := tm.Wait(settle)
			want = append(want, suffix(t, claudetest.FixtureLoginSuccess)...)
			if !exited || rc != 0 || !bytes.Equal(out, want) {
				t.Fatalf("after the right code: exited %v rc %d\n got %q\nwant %q", exited, rc, out, want)
			}
			if v, err := classify.ClassifyLogin(out); err != nil || v.Phase != classify.LoginSuccess {
				t.Fatalf("after the right code: %+v, %v", v, err)
			}

			evs := f.Events(t)
			starts := claudetest.Kind(evs, claudetest.EventStart)
			if len(starts) != 1 || starts[0].Width != width || !starts[0].TTY {
				t.Fatalf("the fake did not see a %d-column terminal: %+v", width, starts)
			}
			subs := claudetest.Kind(evs, claudetest.EventSubmission)
			if len(subs) != 2 || subs[0].Verdict != "rejected" || subs[1].Verdict != "accepted" || subs[1].SHA256 != claudetest.CodeSHA256(code) {
				t.Fatalf("submissions: %+v", subs)
			}
			f.NoViolations(t)
			assertNoCanary(t, code, f.Dir, f.Script.StateDir)
		})
	}
}

// The success path alone is the synthetic success fixture, byte for byte.
func TestLoginSuccessIsTheFixture(t *testing.T) {
	f := loginFake(t, claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(code)})
	tm := f.Start(t, 1000, "auth", "login", "--claudeai")
	if _, err := tm.WaitFor(settle, []byte("prompted > ")); err != nil {
		t.Fatal(err)
	}
	tm.Write([]byte(code + "\r"))
	exited, rc, out := tm.Wait(settle)
	if !exited || rc != 0 || !bytes.Equal(out, tr(t, claudetest.FixtureLoginSuccess)) {
		t.Fatalf("exited %v rc %d: %q", exited, rc, out)
	}
	f.NoViolations(t)
}

// Timeout is an absence: the prompt, a submission, and then nothing. The
// verdict is the caller's clock. Control: the answering mode, given the same
// submission, answers.
func TestLoginTimeoutNeverAnswers(t *testing.T) {
	for _, mode := range []claudetest.LoginMode{claudetest.LoginAnswer, claudetest.LoginTimeout} {
		f := loginFake(t, claudetest.Login{Mode: mode, AcceptSHA256: claudetest.CodeSHA256(code)})
		tm := f.Start(t, 1000, "auth", "login", "--claudeai")
		prompt := tr(t, claudetest.FixtureLoginPrompt)
		if _, err := tm.WaitOutput(settle, reaches(prompt)); err != nil {
			t.Fatal(err)
		}
		tm.Write([]byte(code + "\r"))
		exited, _, out := tm.Wait(hangWindow)
		v, _ := classify.ClassifyLogin(out)
		switch mode {
		case claudetest.LoginAnswer:
			if !exited || v.Phase != classify.LoginSuccess {
				t.Fatalf("control: the answering mode did not answer: %+v", v)
			}
		case claudetest.LoginTimeout:
			if exited || !bytes.Equal(out, prompt) || v.Phase != classify.LoginAwaitingCode {
				t.Fatalf("timeout mode answered: exited %v, %+v, %q", exited, v, out)
			}
			subs := claudetest.Kind(f.Events(t), claudetest.EventSubmission)
			if len(subs) != 1 || subs[0].Verdict != "unanswered" {
				t.Fatalf("the submission was not received: %+v", subs)
			}
		}
	}
}

// fakeclaude asserts on what it receives: the code once, framed by one
// carriage return. The clean case comes first, and must raise nothing — or
// every row below could be passing on a fake that complains about everything.
func TestLoginSubmissionFraming(t *testing.T) {
	for _, tc := range []struct {
		name, write, violation string
		accept                 bool
	}{
		{"clean", code + "\r", "", true},
		{"twice in one write", code + "\r" + code + "\r", "after the accepted code", true},
		{"a wrong code twice", "bad#code\rbad#code\r", "more than once", false},
		{"LF", code + "\n", "LF, not CR", false},
		{"CRLF", "bad#code\r\n", "LF, not CR", false},
		{"trailing space", code + " \r", "whitespace or control", false},
		{"empty", "\r", "empty submission", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := loginFake(t, claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(code)})
			tm := f.Start(t, 1000, "auth", "login", "--claudeai")
			if _, err := tm.WaitFor(settle, []byte("prompted > ")); err != nil {
				t.Fatal(err)
			}
			tm.Write([]byte(tc.write))
			if tc.accept {
				if exited, rc, _ := tm.Wait(settle); !exited || rc != 0 {
					t.Fatalf("not accepted: exited %v rc %d", exited, rc)
				}
			}
			// The log is the observable, not the terminal; poll it.
			var got []string
			for deadline := time.Now().Add(settle); ; time.Sleep(20 * time.Millisecond) {
				got = got[:0]
				for _, v := range f.Violations(t) {
					got = append(got, v.What)
				}
				if tc.violation == "" || len(got) > 0 || time.Now().After(deadline) {
					break
				}
			}
			if tc.violation == "" {
				if len(got) != 0 {
					t.Fatalf("a clean submission raised %q", got)
				}
				return
			}
			if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, tc.violation) }) {
				t.Fatalf("want a violation containing %q, got %q", tc.violation, got)
			}
		})
	}
}

// assertNoCanary is a small form of the canary sweep (testing §4.2) over the
// fake's own files: the event log and the script must not hold the code.
func assertNoCanary(t *testing.T, canary string, dirs ...string) {
	t.Helper()
	files := 0
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files++
			if bytes.Contains(b, []byte(canary)) {
				t.Errorf("the login code is in %s", p)
			}
			return nil
		})
	}
	if files < 3 { // binary, script, events at least: a sweep of nothing proves nothing
		t.Fatalf("the sweep saw only %d files", files)
	}
}

// --- remote-control: serving and discovery ----------------------------------

func rcFake(t *testing.T, steps ...claudetest.Step) *claudetest.Fake {
	return claudetest.Install(t, claudetest.Script{RemoteControl: steps})
}

var serveArgs = []string{"remote-control", "--verbose", "--spawn", "worktree", "--capacity", "4"}

const recordedSession = "session_01AZLp4a8noWuZ5eRHrecDgz"

// Serving, then SIGTERM: the whole stream is env-status-block, byte for byte,
// and discovery reads the environment, the one session and its capacity.
func TestServeThenSIGTERMIsTheRecording(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServe})
	tm := f.Start(t, 200, serveArgs...)
	running := tr(t, claudetest.FixtureServeRunning)
	// Wait on the session's visible label, which survives any escape
	// mangling, so the classifier gets its say before the byte comparison.
	if _, err := tm.WaitFor(settle, []byte("gleaming-feather")); err != nil {
		t.Fatal(err)
	}
	out, err := tm.WaitOutput(settle, func(b []byte) bool { return len(b) >= len(running) })
	d, _ := classify.ClassifyDiscovery(out)
	if d.EnvironmentID != "env_01SWWUTySnsAEuAGMd6azA24" || !slices.Equal(d.SessionIDs, []string{recordedSession}) || d.CapacityUsed != 1 || d.CapacityTotal != 4 {
		t.Fatalf("discovery while serving: %+v", d)
	}
	if err != nil || !bytes.Equal(out, running) {
		t.Fatalf("serving: %v: not the recording", err)
	}
	if exited, _, _ := tm.Wait(300 * time.Millisecond); exited {
		t.Fatal("the server exited on its own")
	}
	tm.Signal(syscall.SIGTERM)
	exited, rc, out := tm.Wait(settle)
	if !exited || rc != 0 || !bytes.Equal(out, tr(t, claudetest.FixtureServe)) {
		t.Fatalf("after SIGTERM: exited %v rc %d, %d bytes", exited, rc, len(out))
	}
	if !bytes.Contains(out, []byte("Environment preserved")) {
		t.Fatal("the clean stop did not say the environment was preserved")
	}
	f.NoViolations(t)
}

// The session is held back by a caller-chosen delay, and before it arrives
// discovery has the environment and no session — the control for "ids
// arriving late still upsert" is that they were absent first.
func TestServeHoldsTheSessionBack(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServe, SessionDelay: claudetest.Duration(time.Second)})
	tm := f.Start(t, 200, serveArgs...)
	running := tr(t, claudetest.FixtureServeRunning)
	early, err := tm.WaitFor(settle, []byte("Connected"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	early = tm.Output()
	d, _ := classify.ClassifyDiscovery(early)
	if d.EnvironmentID == "" || len(d.SessionIDs) != 0 || !bytes.HasPrefix(running, early) {
		t.Fatalf("before the delay: %+v (%d bytes)", d, len(early))
	}
	out, err := tm.WaitOutput(settle, reaches(running))
	if err != nil || !bytes.Equal(out, running) {
		t.Fatalf("after the delay: %v: %s", err, diverges(out, running))
	}
	if d, _ := classify.ClassifyDiscovery(out); !slices.Equal(d.SessionIDs, []string{recordedSession}) {
		t.Fatalf("after the delay: %+v", d)
	}
}

func TestServeDelayedSecondSessionIsTheFixture(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServeDelayedSession, SessionDelay: claudetest.Duration(300 * time.Millisecond)})
	tm := f.Start(t, 200, serveArgs...)
	want := tr(t, claudetest.FixtureDelayedSession)
	out, err := tm.WaitOutput(settle, reaches(want))
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("%v: %s", err, diverges(out, want))
	}
	d, _ := classify.ClassifyDiscovery(out)
	if !slices.Equal(d.SessionIDs, []string{recordedSession, "session_01SYNTHETICDELAYED00002"}) || d.CapacityUsed != 2 {
		t.Fatalf("%+v", d)
	}
	tm.Signal(syscall.SIGTERM)
	if exited, rc, _ := tm.Wait(settle); !exited || rc != 0 {
		t.Fatalf("SIGTERM: exited %v rc %d", exited, rc)
	}
}

// A `session_…` the model printed is in the stream and is not a session. The
// control is in the same stream: the server's own announcement is found.
func TestModelOutputIDIsNotASession(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServeModelOutputID})
	tm := f.Start(t, 200, serveArgs...)
	want := append(tr(t, claudetest.FixtureServeRunning), tr(t, claudetest.FixtureModelOutputID)...)
	out, err := tm.WaitOutput(settle, reaches(want))
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("%v: %s", err, diverges(out, want))
	}
	if !bytes.Contains(out, []byte("session_01SYNTHETICMODELOUTPUT00")) {
		t.Fatal("the model's id is not in the stream, so its rejection would prove nothing")
	}
	d, _ := classify.ClassifyDiscovery(out)
	if !slices.Equal(d.SessionIDs, []string{recordedSession}) {
		t.Fatalf("sessions %q: want only the announced one", d.SessionIDs)
	}
}

// Drydock writes nothing to a serving remote-control — a keystroke there is a
// command — and the fake reports it if it does. Control: no write, no complaint.
func TestServeReportsInput(t *testing.T) {
	for _, write := range []bool{false, true} {
		f := rcFake(t, claudetest.Step{Mode: claudetest.RCServe})
		tm := f.Start(t, 200, serveArgs...)
		if _, err := tm.WaitFor(settle, []byte("Capacity")); err != nil {
			t.Fatal(err)
		}
		if write {
			tm.Write([]byte("w"))
			if !awaitViolation(t, f) {
				t.Fatal("a keystroke to a serving remote-control went unreported")
			}
			continue
		}
		time.Sleep(300 * time.Millisecond)
		if got := len(f.Violations(t)); got != 0 {
			t.Fatalf("no write, yet %d violations", got)
		}
	}
}

// awaitViolation polls the fake's log for a complaint, up to settle.
func awaitViolation(t *testing.T, f *claudetest.Fake) bool {
	t.Helper()
	for deadline := time.Now().Add(settle); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if len(f.Violations(t)) > 0 {
			return true
		}
	}
	return false
}

// Serving needs a PTY; over a pipe the fake refuses and says why, rather than
// quietly replaying a stream the real binary would not have written.
func TestServeRequiresAPTY(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServe})
	cmd := f.Command(serveArgs...)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "needs a PTY") {
		t.Fatalf("over a pipe: exit %d, %q", cmd.ProcessState.ExitCode(), out)
	}
	if v := f.Violations(t); len(v) != 1 {
		t.Fatalf("violations: %+v", v)
	}
}

// --- remote-control: refusals, hangs, crashes --------------------------------

var refusals = []struct {
	mode    claudetest.RCMode
	fixture string
	verdict classify.Refusal
}{
	{claudetest.RCRefuseWaitRegistration, claudetest.FixtureRefuseWait, classify.RefusalWaitRegistration},
	{claudetest.RCRefuseNotTrusted, claudetest.FixtureRefuseTrust, classify.RefusalWorkspaceNotTrusted},
	{claudetest.RCRefuseNoOrganization, claudetest.FixtureRefuseOrg, classify.RefusalNoOrganization},
	{claudetest.RCRefuseBadCommandLine, claudetest.FixtureRefuseCmdline, classify.RefusalBadCommandLine},
}

func recordedExit(t *testing.T, name string) int {
	m := sidecar(t, name+".exit")
	n, err := strconv.Atoi(m["exit_code"])
	if err != nil {
		t.Fatalf("%s.exit: %v", name, err)
	}
	return n
}

// Four refusals, one exit code, four verdicts — over a pipe, where the bytes
// are the recording exactly, and on a PTY, where the terminal turns each `\n`
// into `\r\n` as it would for the real one.
func TestFourRefusalsOneExitCodeFourVerdicts(t *testing.T) {
	verdicts := map[classify.Refusal]bool{}
	for _, r := range refusals {
		t.Run(string(r.mode), func(t *testing.T) {
			f := rcFake(t, claudetest.Step{Mode: r.mode})
			want := tr(t, r.fixture)

			var stdout, stderr bytes.Buffer
			cmd := f.Command(serveArgs...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			cmd.Run()
			if rc := cmd.ProcessState.ExitCode(); rc != 1 || rc != recordedExit(t, r.fixture) {
				t.Fatalf("pipe: exit %d", rc)
			}
			if stdout.Len() != 0 || !bytes.Equal(stderr.Bytes(), want) {
				t.Fatalf("pipe: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			v, err := classify.ClassifyRefusal(stderr.Bytes())
			if err != nil || v != r.verdict {
				t.Fatalf("pipe: verdict %v, %v", v, err)
			}
			if v.Retryable() != (r.verdict == classify.RefusalWaitRegistration) {
				t.Fatal("only the registration wait is retryable")
			}
			verdicts[v] = true

			tm := f.Start(t, 200, serveArgs...)
			exited, rc, out := tm.Wait(settle)
			if !exited || rc != 1 || !bytes.Equal(out, bytes.ReplaceAll(want, []byte("\n"), []byte("\r\n"))) {
				t.Fatalf("pty: exited %v rc %d %q", exited, rc, out)
			}
			if v, _ := classify.ClassifyRefusal(out); v != r.verdict {
				t.Fatalf("pty: verdict %v", v)
			}
		})
	}
	if len(verdicts) != 4 {
		t.Fatalf("%d distinct verdicts from four refusals", len(verdicts))
	}
}

// The real binary refuses `-c` with `--spawn` whatever its config, so the fake
// derives that refusal from argv — the supervisor's argv is what a test of
// it is about. Control: the same script without `-c` serves.
func TestBadCommandLineComesFromArgv(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCServe})
	bad := f.Start(t, 200, "remote-control", "--verbose", "-c", "--spawn", "worktree", "--capacity", "4")
	exited, rc, out := bad.Wait(settle)
	if v, _ := classify.ClassifyRefusal(out); !exited || rc != 1 || v != classify.RefusalBadCommandLine {
		t.Fatalf("with -c: exited %v rc %d verdict %v", exited, rc, v)
	}
	good := f.Start(t, 200, serveArgs...)
	if _, err := good.WaitFor(settle, []byte("Environment ID")); err != nil {
		t.Fatalf("without -c: %v", err)
	}
}

// The two gates that hang. Each is asserted as a timeout — there is no
// message — beside a control that does exit with a verdict inside the same
// window, so the window is long enough to have seen one.
func TestGatesHangOnAPTY(t *testing.T) {
	for _, tc := range []struct {
		mode    claudetest.RCMode
		fixture string
	}{
		{claudetest.RCHangTrust, claudetest.FixtureHangTrust},
		{claudetest.RCHangRemoteDialog, claudetest.FixtureHangDialog},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			// Control: a refusal under the same harness exits in the window.
			ctl := rcFake(t, claudetest.Step{Mode: claudetest.RCRefuseNotTrusted}).Start(t, 200, serveArgs...)
			if exited, _, out := ctl.Wait(hangWindow); !exited {
				t.Fatalf("control: a refusal did not exit within %v: %q", hangWindow, out)
			}

			f := rcFake(t, claudetest.Step{Mode: tc.mode})
			tm := f.Start(t, 200, serveArgs...)
			exited, _, out := tm.Wait(hangWindow)
			if exited {
				t.Fatalf("the gate exited instead of hanging: %q", out)
			}
			if !bytes.Equal(out, tr(t, tc.fixture)) {
				t.Fatalf("got %q\nwant %q", out, tr(t, tc.fixture))
			}
			if v, _ := classify.ClassifyRefusal(out); v != classify.RefusalNone {
				t.Fatalf("a hang classified as refusal %v", v)
			}
			if d, _ := classify.ClassifyDiscovery(out); d.EnvironmentID != "" || len(d.SessionIDs) != 0 {
				t.Fatalf("a hang yielded discovery %+v", d)
			}
			f.NoViolations(t)
			// Answering the gate is reported, never obeyed.
			tm.Write([]byte("y\r"))
			if !awaitViolation(t, f) {
				t.Fatal("typing at a gate went unreported")
			}
			if !tm.Running() {
				t.Fatal("typing at a gate ended the hang")
			}
		})
	}
}

// Redirected, the trust gate is a refusal, as measured on 2.1.289 — the
// control that makes the PTY hang above a property of the PTY.
func TestTrustGateRefusesWhenRedirected(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCHangTrust})
	var stderr bytes.Buffer
	cmd := f.Command(serveArgs...)
	cmd.Stderr = &stderr
	cmd.Run()
	v, _ := classify.ClassifyRefusal(stderr.Bytes())
	if cmd.ProcessState.ExitCode() != 1 || v != classify.RefusalWorkspaceNotTrusted || !bytes.Equal(stderr.Bytes(), tr(t, claudetest.FixtureRefuseTrust)) {
		t.Fatalf("redirected: exit %d verdict %v %q", cmd.ProcessState.ExitCode(), v, stderr.String())
	}
}

// A crash loop that recovers: two crashes (exit 1, no refusal message, so an
// ordinary crash that spends the restart budget), then a serve.
func TestCrashLoopThenServe(t *testing.T) {
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCCrash, Times: 2}, claudetest.Step{Mode: claudetest.RCServe})
	for i := range 2 {
		tm := f.Start(t, 200, serveArgs...)
		exited, rc, out := tm.Wait(settle)
		if !exited || rc != 1 || !bytes.Equal(out, tr(t, claudetest.FixtureCrash)) {
			t.Fatalf("crash %d: exited %v rc %d %q", i, exited, rc, out)
		}
		if v, _ := classify.ClassifyRefusal(out); v != classify.RefusalNone {
			t.Fatalf("crash %d classified as refusal %v", i, v)
		}
		if d, _ := classify.ClassifyDiscovery(out); d.EnvironmentID == "" {
			t.Fatalf("crash %d: no environment id before the crash", i)
		}
	}
	tm := f.Start(t, 200, serveArgs...)
	if _, err := tm.WaitFor(settle, []byte(recordedSession)); err != nil {
		t.Fatalf("third start did not serve: %v", err)
	}
	if exited, _, _ := tm.Wait(300 * time.Millisecond); exited {
		t.Fatal("third start exited")
	}
}

// The registration wait persists for a span the test chooses and then lapses.
// Nothing about the span is a constant in the fake: it is whatever the step
// says, which is what lets a supervisor test prove it has none either.
func TestWaitRegistrationLapsesAfterTheChosenSpan(t *testing.T) {
	span := 800 * time.Millisecond
	f := rcFake(t, claudetest.Step{Mode: claudetest.RCRefuseWaitRegistration, For: claudetest.Duration(span)}, claudetest.Step{Mode: claudetest.RCServe})
	start := time.Now()
	waits := 0
	for {
		if time.Since(start) > settle {
			t.Fatal("the wait never lapsed")
		}
		tm := f.Start(t, 200, serveArgs...)
		out, err := tm.WaitOutput(settle, func(b []byte) bool {
			return bytes.Contains(b, []byte("Environment ID")) || bytes.Contains(b, []byte("Exiting in about"))
		})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("Environment ID")) {
			break
		}
		exited, rc, out := tm.Wait(settle)
		v, _ := classify.ClassifyRefusal(out)
		if !exited || rc != 1 || v != classify.RefusalWaitRegistration || bytes.Contains(out, []byte("409")) {
			t.Fatalf("wait %d: exited %v rc %d verdict %v", waits, exited, rc, v)
		}
		waits++
		time.Sleep(100 * time.Millisecond)
	}
	if elapsed := time.Since(start); waits < 2 || elapsed < span {
		t.Fatalf("served after %d waits and %v; the span was %v", waits, elapsed, span)
	}
}

// --- auth status ------------------------------------------------------------

func TestAuthStatusShapes(t *testing.T) {
	root := claudetest.CorpusRoot(t)
	for _, shape := range []string{"absent", "valid", "expired", "blanked"} {
		f := claudetest.Install(t, claudetest.Script{AuthStatus: &claudetest.AuthStatus{Shape: shape}})
		out, err := f.Command("auth", "status", "--json").Output()
		want, _ := os.ReadFile(filepath.Join(root, "authstatus", shape+".json"))
		if err != nil || !bytes.Equal(out, want) {
			t.Errorf("%s: %v %q", shape, err, out)
		}
	}
	f := claudetest.Install(t, claudetest.Script{AuthStatus: &claudetest.AuthStatus{Shape: "valid"}})
	if err := f.Command("auth", "status").Run(); err == nil {
		t.Error("auth status without --json was answered; only the JSON form is recorded")
	}
}

// from-config answers from the credential file as 2.1.289 does, and the
// identity classifier, given the fake's answer and the same file, reaches the
// verdict its own tests expect for each of the six shapes. `corrupt` is the
// refusal: the fake will not invent an answer the corpus never recorded.
func TestAuthStatusFromConfigThroughTheIdentityClassifier(t *testing.T) {
	root := claudetest.CorpusRoot(t)
	// The fixtures' expiries are absolute; now is their recorded_at.
	now := time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC)
	for _, tc := range []struct {
		file string
		want classify.IdentityState
	}{
		{"ok.json", classify.IdentityOK},
		{"expiring.json", classify.IdentityExpiring},
		{"expired.json", classify.IdentityExpired},
		{"blanked.json", classify.IdentityBlanked},
		{"", classify.IdentityAbsent},
		{"corrupt.json", 0},
	} {
		cfg := t.TempDir()
		var creds []byte
		if tc.file != "" {
			creds, _ = os.ReadFile(filepath.Join(root, "credentials", tc.file))
			os.WriteFile(filepath.Join(cfg, ".credentials.json"), creds, 0o600)
		}
		f := claudetest.Install(t, claudetest.Script{AuthStatus: &claudetest.AuthStatus{Shape: "from-config"}})
		cmd := f.Command("auth", "status", "--json")
		cmd.Env = []string{"CLAUDE_CONFIG_DIR=" + cfg}
		out, err := cmd.Output()
		if tc.file == "corrupt.json" {
			if err == nil || len(f.Violations(t)) == 0 {
				t.Errorf("corrupt: answered %q", out)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		got, err := classify.ClassifyIdentity(out, creds, now)
		if err != nil || got.State != tc.want {
			t.Errorf("%q: identity %v, want %v (%v)", tc.file, got.State, tc.want, err)
		}
	}
}

// Unscripted argv is a violation and an exit 2, never a guess.
func TestUnscriptedCommandIsRefused(t *testing.T) {
	f := claudetest.Install(t, claudetest.Script{})
	for _, args := range [][]string{{"doctor"}, {"auth", "login"}, {"remote-control"}} {
		cmd := f.Command(args...)
		if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	if n := len(f.Violations(t)); n != 3 {
		t.Fatalf("%d violations for three unscripted commands", n)
	}
	// Control: a scripted one is answered.
	g := claudetest.Install(t, claudetest.Script{AuthStatus: &claudetest.AuthStatus{Shape: "absent"}})
	if err := g.Command("auth", "status", "--json").Run(); err != nil {
		t.Fatal(err)
	}
}

// A PTY read can end anywhere, so the classifiers are run over every prefix
// a paced stream delivers: the login URL is either absent or complete, never
// an error and never a fragment, and discovery never reports a truncated id.
// The control is the end of each stream, where the full verdict must appear.
func TestClassifiersAtEveryReadOfAPacedStream(t *testing.T) {
	pace := claudetest.Pace{Chunk: 1, Delay: claudetest.Duration(100 * time.Microsecond)}

	f := claudetest.Install(t, claudetest.Script{Pace: pace, Login: &claudetest.Login{Mode: claudetest.LoginTimeout}})
	tm := f.Start(t, 1000, "auth", "login", "--claudeai")
	full, _ := classify.ClassifyLogin(tr(t, claudetest.FixtureLoginPrompt))
	reads := 0
	if _, err := tm.WaitOutput(settle, func(b []byte) bool {
		reads++
		v, err := classify.ClassifyLogin(b)
		if err != nil || (v.AuthorizeURL != "" && v.AuthorizeURL != full.AuthorizeURL) {
			t.Fatalf("after %d bytes: %+v, %v", len(b), v, err)
		}
		return v.Phase == classify.LoginAwaitingCode
	}); err != nil {
		t.Fatal(err)
	}
	if reads < 100 {
		t.Fatalf("only %d reads: the stream was not paced", reads)
	}

	g := claudetest.Install(t, claudetest.Script{Pace: pace, RemoteControl: []claudetest.Step{{Mode: claudetest.RCServe}}})
	tm = g.Start(t, 200, serveArgs...)
	if _, err := tm.WaitOutput(settle, func(b []byte) bool {
		d, _ := classify.ClassifyDiscovery(b)
		if (d.EnvironmentID != "" && d.EnvironmentID != "env_01SWWUTySnsAEuAGMd6azA24") ||
			len(d.SessionIDs) > 1 || (len(d.SessionIDs) == 1 && d.SessionIDs[0] != recordedSession) {
			t.Fatalf("after %d bytes: %+v", len(b), d)
		}
		return len(d.SessionIDs) == 1
	}); err != nil {
		t.Fatal(err)
	}
}
