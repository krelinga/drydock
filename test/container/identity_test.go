package container_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudeimage"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// TestIdentityWatchReadsARealVolume is the watch against real Docker: a real
// named volume holding fixture files the way a workspace would leave them —
// the directory 0700 and the file 0600, owned by uid 1000, not by whoever
// runs the helper — read through the real helpers, the real Claude image
// built from its pinned recipe, and the real `claude auth status --json`.
//
//   - No volume at all is absent, and creates no volume.
//   - A volume with no credential file is absent.
//   - The tombstone is blanked.
//   - A live credential is ok, with the account `auth status` reports from
//     the volume's .claude.json — the one verdict that needs Claude Code.
//
// Afterwards the volume's bytes are unchanged (the mount is read-only) and no
// helper is left behind (--rm). The control for "read-only" is the write the
// test makes itself, which does change the bytes.
func TestIdentityWatchReadsARealVolume(t *testing.T) {
	needDocker(t)
	if out, err := exec.Command("docker", "pull", "--quiet", config.DefaultCleanupImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull: %v: %s", err, out)
	}
	p := prefix(t)
	ctx := context.Background()
	rnd := make([]byte, 4)
	rand.Read(rnd)
	vol := "drydock-test-claude-" + hex.EncodeToString(rnd)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })

	img := &claudeimage.Builder{Run: subproc.Exec{}, Base: config.DefaultClaudeBaseImage, Version: classify.ClaudeCodeVersion}
	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := img.Ensure(buildCtx); err != nil {
		t.Fatalf("building the Claude image: %v", err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// The fixtures' own clock: ok.json is a month out from it.
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC))
	w := &identity.Watch{DB: db.DB, Events: events.New(db.DB, clock), Clock: clock, Volume: vol, Window: 72 * time.Hour,
		Source: identity.DockerSource{Run: subproc.Exec{}, Image: img, FileImage: config.DefaultCleanupImage,
			Volume: vol, LabelPrefix: p}}
	check := func(want identity.State) identity.View {
		t.Helper()
		v, err := w.Check(ctx)
		if err != nil {
			t.Fatalf("check (want %s): %v", want, err)
		}
		if v.State == nil || *v.State != want {
			t.Fatalf("state = %v; want %s", v.State, want)
		}
		return v
	}

	check(identity.Absent)
	if out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name="+vol).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("checking a missing volume created it: %q", out)
	}

	// A volume of the name that this Drydock did not make (§6 step 4 labels
	// the one it makes, and refuses to mount one without): the check fails
	// rather than reading it, and keeps the state. Then the label, as step 4
	// puts it, and the same volume is read — the control.
	docker(t, "volume", "create", vol)
	var re *identity.ReadError
	if _, err := w.Check(ctx); !errors.As(err, &re) || re.Problem != identity.ProblemForeign {
		t.Fatalf("an unlabelled volume: %v; want a foreign_volume read error", err)
	}
	docker(t, "volume", "rm", vol)
	docker(t, "volume", "create", "--label", p+"."+identity.LabelVolume+"=true", vol)
	check(identity.Absent)

	fixtures, _ := filepath.Abs(filepath.Join("..", "fixtures", "credentials"))
	// put writes a credential fixture and an account record into the
	// volume as a workspace container would leave them.
	put := func(name string) {
		t.Helper()
		docker(t, "run", "--rm", "--network", "none",
			"--mount", "type=volume,source="+vol+",target=/v",
			"--mount", "type=bind,source="+fixtures+",target=/src,readonly",
			"--entrypoint", "sh", config.DefaultCleanupImage, "-c",
			`cp /src/`+name+` /v/.credentials.json &&
			 printf '%s' '{"oauthAccount":{"emailAddress":"fixture@example.invalid","organizationUuid":"11111111-1111-1111-1111-111111111111"}}' > /v/.claude.json &&
			 chown -R 1000:1000 /v && chmod 700 /v && chmod 600 /v/.credentials.json /v/.claude.json`)
	}
	snapshot := func() string {
		t.Helper()
		return docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+vol+",target=/v,readonly",
			"--entrypoint", "sh", config.DefaultCleanupImage, "-c", "ls -ln /v; md5sum /v/.credentials.json /v/.claude.json")
	}

	put("blanked.json")
	check(identity.Blanked)

	put("ok.json")
	before := snapshot()
	v := check(identity.OK)
	if v.AccountEmail == nil || *v.AccountEmail != "fixture@example.invalid" {
		t.Errorf("account = %v; want the volume's", v.AccountEmail)
	}
	want, _ := os.ReadFile(filepath.Join(fixtures, "ok.json"))
	if !bytes.Contains(want, []byte("1793686477000")) || v.ExpiresAt == nil || v.ExpiresAt.UnixMilli() != 1793686477000 {
		t.Errorf("expires_at = %v; want the file's", v.ExpiresAt)
	}
	if after := snapshot(); after != before {
		t.Errorf("a check changed the volume:\nbefore %s\nafter  %s", before, after)
	}
	// Control: the snapshot does see a change.
	put("expiring.json")
	if after := snapshot(); after == before {
		t.Error("control: the snapshot did not see a rewritten file")
	}

	if left := docker(t, "ps", "-aq", "--filter", "label="+p+"."+identity.LabelIdentity); left != "" {
		t.Errorf("helpers left behind: %s", left)
	}
}
