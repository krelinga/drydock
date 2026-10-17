package githubtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture's repositories start no auto-maintenance: neither the commit
// that builds the work tree (which `clone --bare` then copies) nor a push
// into the bare remote. Each half has its control: the same git command
// without the fixture's settings is seen starting `maintenance run --auto`,
// so a trace that never shows maintenance at all cannot pass it.
func TestFixturesStartNoAutoMaintenance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	plainEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	run := func(env []string, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(append([]string(nil), env...), "GIT_TRACE=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	const started = "maintenance run --auto"
	commit := func(env []string) string {
		work := t.TempDir()
		run(env, work, "init", "-q", "-b", "main")
		os.WriteFile(filepath.Join(work, "README.md"), []byte("x\n"), 0o644)
		run(env, work, "add", "-A")
		// Foreground, so the control's maintenance has ended before the
		// directory is removed.
		return run(env, work, "-c", "maintenance.autoDetach=false", "commit", "-q", "-m", "x")
	}
	if out := commit(plainEnv); !strings.Contains(out, started) {
		t.Fatalf("control: a commit without the fixture's settings started no maintenance:\n%s", out)
	}
	if out := commit(fixtureGitEnv()); strings.Contains(out, started) {
		t.Errorf("a fixture commit started maintenance:\n%s", out)
	}

	// The push is made in a plain environment: what is under test is the
	// bare repository's own config, which is all receive-pack has under
	// http-backend.
	f := New(t, 1, time.Now)
	f.Installations = []Installation{{ID: 1, Account: "krelinga", Repos: []Repo{{ID: 1, FullName: "krelinga/r"}}}}
	f.EnableGit(t)
	push := func(bare string) string {
		work := t.TempDir()
		run(plainEnv, work, "clone", "-q", bare, ".")
		os.WriteFile(filepath.Join(work, "pushed.txt"), []byte("y\n"), 0o644)
		run(plainEnv, work, "add", "-A")
		run(plainEnv, work, "-c", "maintenance.auto=false", "commit", "-q", "-m", "y")
		return run(plainEnv, work, "push", "-q", "origin", "HEAD:refs/heads/drydock/x")
	}
	control := filepath.Join(t.TempDir(), "control.git")
	run(plainEnv, t.TempDir(), "-c", "maintenance.auto=false", "init", "-q", "--bare", control)
	// Foreground, as above (git clears GIT_CONFIG_* for receive-pack, so
	// only the repository's own config reaches it).
	run(plainEnv, control, "config", "maintenance.autoDetach", "false")
	if out := push(control); !strings.Contains(out, "receive-pack") || !strings.Contains(out, started) {
		t.Fatalf("control: a push into a bare repository without the fixture's settings started no maintenance:\n%s", out)
	}
	if out := push(filepath.Join(f.gitDir, "krelinga/r.git")); !strings.Contains(out, "receive-pack") || strings.Contains(out, started) {
		t.Errorf("a push into the fixture's remote started maintenance:\n%s", out)
	}
}
