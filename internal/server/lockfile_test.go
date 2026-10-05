package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// Drydock died during `devcontainer up`, after the CLI rewrote a committed
// devcontainer-lock.json and before the run put it back (design §6). What it
// left on disk is a modified clone and the save beside it; the next boot
// restores the clone before it serves anything. The control is the clone
// being dirty before the boot.
func TestBootRestoresALockfileAnInterruptedRunSaved(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	wsDir := filepath.Join(cfg.WorkspaceRoot, "01JABCDEFGHJKMNPQRSTVWXYZ0")
	repo := filepath.Join(wsDir, "repo")
	lock := filepath.Join(repo, ".devcontainer", "devcontainer-lock.json")
	committed := "{\n  \"features\": {}\n}\n"
	os.MkdirAll(filepath.Dir(lock), 0o755)
	os.WriteFile(lock, []byte(committed), 0o644)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "init")

	// What the run wrote before up, and what up then did.
	save, _ := json.Marshal(map[string]any{"path": lock, "mode": 0o644, "content": []byte(committed)})
	os.MkdirAll(filepath.Join(wsDir, ".drydock"), 0o700)
	os.WriteFile(filepath.Join(wsDir, ".drydock", "lockfile.json"), save, 0o600)
	os.WriteFile(lock, []byte(`{"features":{"ghcr.io/devcontainers/features/github-cli:1":{}}}`), 0o644)
	if git("status", "--porcelain", "--ignored") == "" {
		t.Fatal("control: the clone is clean before the boot, so the test would prove nothing")
	}

	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(10 * time.Second)
	for git("status", "--porcelain", "--ignored") != "" {
		if time.Now().After(deadline) {
			t.Fatalf("the boot did not restore the lockfile:\n%s", git("status", "--porcelain", "--ignored"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(wsDir, ".drydock", "lockfile.json")); err == nil {
		t.Error("the save outlived the restore")
	}
}
