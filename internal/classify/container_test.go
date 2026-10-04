package classify

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func containerFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", FixtureRoot, "devcontainer", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

var containerIDShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestClassifyContainerFixtures(t *testing.T) {
	cases := []struct {
		fixture    string
		want       ContainerOutcome
		wantID     bool
		remoteUser string
	}{
		{"up-ok.json", ContainerRunning, true, "root"},
		{"up-error-image-pull.json", ContainerFailed, false, ""},
		{"up-error-config.json", ContainerFailed, false, ""},
		// A failed postCreateCommand leaves the container behind.
		{"up-error-postcreate.json", ContainerFailed, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			got, err := ClassifyContainer(containerFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Outcome != tc.want {
				t.Errorf("outcome %d, want %d", got.Outcome, tc.want)
			}
			if tc.wantID != containerIDShape.MatchString(got.ContainerID) {
				t.Errorf("containerID %q, want present=%v", got.ContainerID, tc.wantID)
			}
			if got.RemoteUser != tc.remoteUser {
				t.Errorf("remoteUser %q, want %q", got.RemoteUser, tc.remoteUser)
			}
			// The CLI names no step; the classifier must not invent one.
			if got.Step != "" {
				t.Errorf("Step %q invented from CLI output", got.Step)
			}
			if tc.want == ContainerFailed && got.Message == "" {
				t.Errorf("failure carried no CLI message")
			}
		})
	}
}

// Malformed input is an error, never a verdict — and in particular never a
// plausible ContainerRunning.
func TestClassifyContainerRejectsMalformed(t *testing.T) {
	ok := containerFixture(t, "up-ok.json")

	// Positive control: the recorded success parses, with or without its
	// trailing newline, so the rejections below are not a parser that
	// rejects everything.
	for _, in := range [][]byte{ok, bytes.TrimSpace(ok)} {
		if got, err := ClassifyContainer(in); err != nil || got.Outcome != ContainerRunning {
			t.Fatalf("positive control: got %+v, %v", got, err)
		}
	}

	cases := []struct {
		name  string
		input []byte
	}{
		// Recorded: `--json` is a usage error and stdout is empty.
		{"recorded empty stdout", containerFixture(t, "up-unknown-argument-json.stdout")},
		{"whitespace", []byte(" \n")},
		{"not json", []byte("Error: something\n")},
		{"log line then result", append([]byte("[2026-10-04T06:23:48.385Z] Start\n"), ok...)},
		{"result then log line", append(bytes.TrimSpace(ok), []byte("\n[2026-10-04T06:23:48.385Z] Start\n")...)},
		{"two results", append(append([]byte{}, ok...), ok...)},
		{"truncated", ok[:len(ok)/2]},
		{"null", []byte("null")},
		{"array", []byte("[]")},
		{"empty object", []byte("{}")},
		{"empty outcome", []byte(`{"outcome":"","containerId":"abc"}`)},
		{"outcome not a string", []byte(`{"outcome":true,"containerId":"abc"}`)},
		{"success without containerId", []byte(`{"outcome":"success","remoteUser":"root"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyContainer(tc.input)
			if err == nil {
				t.Fatalf("want error, got %+v", got)
			}
			if got.Outcome == ContainerRunning && got.ContainerID != "" {
				t.Fatalf("error path returned a usable Running result: %+v", got)
			}
		})
	}
}

// Only outcome "success" is running; well-formed JSON with a container id is
// not enough.
func TestClassifyContainerOnlySuccessIsRunning(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  ContainerOutcome
	}{
		{"success (positive control)", `{"outcome":"success","containerId":"abc","remoteUser":"vscode"}`, ContainerRunning},
		{"error with containerId", `{"outcome":"error","containerId":"abc","remoteUser":"vscode","message":"m","description":"d"}`, ContainerFailed},
		{"unknown outcome", `{"outcome":"partial","containerId":"abc"}`, ContainerFailed},
		{"case differs", `{"outcome":"Success","containerId":"abc"}`, ContainerFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyContainer([]byte(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("outcome %d, want %d", got.Outcome, tc.want)
			}
			if got.ContainerID != "abc" {
				t.Fatalf("containerId %q, want abc", got.ContainerID)
			}
		})
	}
}
