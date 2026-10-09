package container_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
)

// TestTheOwnerHelperAgainstRealDocker: §6 step 4's owner helper, which the
// login calls too, against a real volume.
//
//   - A fresh volume is Drydock's uid, 0700, with the marker in it.
//   - An empty volume another uid owns — what a non-vscode first workspace
//     left before the fix, and what the login then refused — is given back to
//     Drydock's uid, marker and all. This is the positive control for the
//     refusal below: the helper does change an owner when it should.
//   - A volume another uid has written to is refused, naming that uid, and
//     its owner and its file are left exactly as they were.
func TestTheOwnerHelperAgainstRealDocker(t *testing.T) {
	needDocker(t)
	pullImage(t, config.DefaultCleanupImage)
	p := prefix(t)
	vol := claudeVolume(p)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	ctx := context.Background()
	m := manager(p)
	in := func(script string) string {
		t.Helper()
		return docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+vol+",target=/v",
			"--entrypoint", "sh", config.DefaultCleanupImage, "-c", script)
	}
	state := func() string { return in("stat -c '%u %a' /v; ls -A /v") }
	ours := strconv.Itoa(os.Getuid()) + " 700\n.drydock-volume"

	if _, err := m.EnsureClaudeVolume(ctx, vol); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != ours {
		t.Fatalf("a fresh volume is %q; want %q", got, ours)
	}

	in("rmdir /v/.drydock-volume && chown 1500:1500 /v")
	if got := state(); got != "1500 700" {
		t.Fatalf("setup: %q", got)
	}
	if _, err := m.EnsureClaudeVolume(ctx, vol); err != nil {
		t.Fatalf("an empty volume another uid owns: %v", err)
	}
	if got := state(); got != ours {
		t.Errorf("an empty volume another uid owned is %q; want %q", got, ours)
	}

	in("echo '{}' > /v/.claude.json && chown -R 1500:1500 /v")
	_, err := m.EnsureClaudeVolume(ctx, vol)
	var oe *container.VolumeOwnerError
	if !errors.As(err, &oe) || oe.OwnerName() != "uid 1500" {
		t.Fatalf("a written volume another uid owns: %v", err)
	}
	if got := state(); got != "1500 700\n.claude.json\n.drydock-volume" {
		t.Errorf("the refusal changed the volume: %q", got)
	}
}
