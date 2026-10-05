package provision

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/workspace"
)

// lockfile is a devcontainer-lock.json as VS Code writes one.
const lockfile = `{
  "features": {
    "ghcr.io/devcontainers/features/node:1": {
      "version": "1.6.3",
      "resolved": "ghcr.io/devcontainers/features/node@sha256:aa",
      "integrity": "sha256:aa"
    }
  }
}
`

// withLockfile makes the alpha repository commit body as its
// .devcontainer/devcontainer-lock.json; a setup for newEnv.
func withLockfile(body string) func(*githubtest.Fake) {
	return func(f *githubtest.Fake) {
		r := &f.Installations[0].Repos[0]
		r.Files = append(r.Files, ".devcontainer/devcontainer-lock.json")
		if r.Contents == nil {
			r.Contents = map[string]string{}
		}
		r.Contents[".devcontainer/devcontainer-lock.json"] = body
	}
}

// upWrites is a fake `up` that does to the lockfile what CLI 0.89.0 does
// (test/fixtures/devcontainer/lockfile-behaviour.txt): with --no-lockfile
// nothing; with no lockfile flag it writes one — a new file if there was
// none, a rewrite if there was — because Drydock's Feature brings a
// dependency no committed lockfile lists. Then it reports success. $3 is
// the workspace folder.
const upWrites = `case " $* " in
*" --no-lockfile "*|*" --frozen-lockfile "*) ;;
*) printf '{"features":{"rewritten":{}}}\n' > "$3/.devcontainer/devcontainer-lock.json" ;;
esac
`

func upOK(t *testing.T) string {
	return "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"
}

// A committed lockfile is honoured — up gets no lockfile flag, so the CLI
// reads it — and a repository without one gets --no-lockfile. Either way the
// clone is left as it was cloned: the fake up rewrites the lockfile as the
// real one does, and Drydock puts the committed bytes back.
func TestTheLockfileIsHonouredAndPutBack(t *testing.T) {
	for _, c := range []struct {
		name, lock string
		honour     bool
		note       string
	}{
		{name: "no lockfile"},
		{name: "committed lockfile", lock: lockfile, honour: true,
			note: "pinned Feature versions are the ones installed"},
		// A blank lockfile asks the CLI to fill it in: nothing to honour.
		{name: "blank lockfile", lock: "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var setup []func(*githubtest.Fake)
			if c.lock != "" {
				setup = append(setup, withLockfile(c.lock))
			}
			e := newEnv(t, setup...)
			e.cli.up = upWrites + upOK(t)
			e.wire(t)
			v := e.create(t, alpha, "")
			if v.State != workspace.Running {
				t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
			}
			ups := e.cli.callsTo(t, "up")
			if len(ups) != 1 {
				t.Fatalf("up calls: %v", ups)
			}
			got := " " + strings.Join(ups[0], " ") + " "
			if strings.Contains(got, " --frozen-lockfile ") || strings.Contains(got, " --no-lockfile ") == c.honour {
				t.Errorf("up: %s; honour %v", got, c.honour)
			}
			if d := v.Steps[workspace.StepResolveConfig].Detail; (c.note == "") != (d == "") || !strings.Contains(d, c.note) {
				t.Errorf("resolve_config says %q; want %q", d, c.note)
			}
			repo := filepath.Join(e.root, v.ID, "repo")
			if out := gitOut(t, repo, "status", "--porcelain", "--ignored"); out != "" {
				t.Errorf("the clone has changes:\n%s", out)
			}
			if _, err := os.Stat(savePath(filepath.Join(e.root, v.ID))); err == nil {
				t.Error("the save outlived a restore")
			}
		})
	}
}

// The control for the test above: the fake up really does dirty the clone
// when nothing puts the lockfile back. Without this, a fake that never wrote
// would pass it.
func TestTheFakeUpDirtiesAnUnrestoredClone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755)
	cmd := exec.Command("sh", "-c", upWrites, "up", "up", "--workspace-folder", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".devcontainer", "devcontainer-lock.json")); err != nil || !strings.Contains(string(b), "rewritten") {
		t.Errorf("the fake up wrote %q, %v", b, err)
	}
}

// Drydock's minimal config is used only when the repository has no
// devcontainer.json, and the CLI looks for the lockfile beside the
// repository's default path, not the override's — so the override path is
// always --no-lockfile, even with a stray lockfile in the repository.
func TestTheMinimalConfigNeverHonoursALockfile(t *testing.T) {
	e := newEnv(t, func(f *githubtest.Fake) {
		r := &f.Installations[0].Repos[1] // plain: no devcontainer.json
		r.Files = append(r.Files, ".devcontainer/devcontainer-lock.json")
		r.Contents = map[string]string{".devcontainer/devcontainer-lock.json": lockfile}
	})
	e.cli.readConfig = "cat <<'EOF'\n" + fixture(t, "read-configuration-override.json") + "\nEOF\n"
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/workspaces/repo", "/workspaces/plain2")
	e.wire(t)
	v := e.create(t, plain, "")
	if v.State != workspace.Running {
		t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
	}
	up := " " + strings.Join(e.cli.callsTo(t, "up")[0], " ") + " "
	if !strings.Contains(up, " --no-lockfile ") {
		t.Errorf("up: %s", up)
	}
}

// The crash: Drydock dies after up rewrote the lockfile and before the
// restore. The fake up rewrites it and then hangs; the test copies the
// workspace directory as it stands on disk at that moment — what a crash
// would leave — and then recovers the copy the way the next boot does. The
// save must already be on disk when up runs, and recovery must leave the
// copied clone exactly as cloned.
func TestACrashDuringUpIsRecovered(t *testing.T) {
	e := newEnv(t, withLockfile(lockfile))
	e.cli.up = `wsdir=$(dirname "$3")
[ -f "$wsdir/.drydock/lockfile.json" ] || { echo 'no save before up' >&2; exit 3; }
` + upWrites + `touch "$wsdir/up-wrote"; exec sleep 60`
	e.wire(t)
	w, err := e.p.Create(context.Background(), alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(e.root, w.ID)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(wsDir, "up-wrote")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("up never wrote; steps %+v", e.view(t, w.ID).Steps)
		}
		time.Sleep(20 * time.Millisecond)
	}
	crashed := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(crashed, 0o700)
	if out, err := exec.Command("cp", "-a", wsDir, crashed).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	e.p.Shutdown(10 * time.Second)

	repo := filepath.Join(crashed, w.ID, "repo")
	// Control: the crash really left the clone modified.
	if out := gitOut(t, repo, "status", "--porcelain", "--ignored"); !strings.Contains(out, "devcontainer-lock.json") {
		t.Fatalf("the crash left the clone clean, so recovery would prove nothing:\n%s", out)
	}
	// The saved path names the original root; a boot finds it in its own.
	// Rewrite it as the copy's, which is where this "boot" runs.
	save := savePath(filepath.Join(crashed, w.ID))
	b, err := os.ReadFile(save)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(save, []byte(strings.ReplaceAll(string(b), e.root, crashed)), 0o600)

	boot := &Provisioner{Workspaces: &workspace.Store{Root: crashed}}
	if err := boot.RecoverLockfiles(); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, repo, "status", "--porcelain", "--ignored"); out != "" {
		t.Errorf("after recovery the clone has changes:\n%s", out)
	}
	if _, err := os.Stat(save); err == nil {
		t.Error("the save outlived the recovery")
	}

	// And the original, which Shutdown cancelled rather than killed, was put
	// back by the run itself.
	if out := gitOut(t, filepath.Join(wsDir, "repo"), "status", "--porcelain", "--ignored"); out != "" {
		t.Errorf("after a cancelled up the clone has changes:\n%s", out)
	}
}

// A save left by a crash is restored by the next run of that workspace too,
// before it reads the clone — so a boot pass that failed is not the only
// chance.
func TestARunRestoresALeftoverSave(t *testing.T) {
	e := newEnv(t, withLockfile(lockfile))
	e.cli.up = upWrites + upOK(t)
	e.wire(t)
	v := e.create(t, alpha, "")
	if v.State != workspace.Running {
		t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
	}
	wsDir := filepath.Join(e.root, v.ID)
	lock := filepath.Join(wsDir, "repo", ".devcontainer", "devcontainer-lock.json")
	if _, err := saveLockfile(wsDir, lock); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(lock, []byte("{}\n"), 0o644)
	if err := (&runState{p: e.p}).recoverLockfile(workspace.Workspace{ID: v.ID, HostPath: filepath.Join(wsDir, "repo")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(lock); string(b) != lockfile {
		t.Errorf("the lockfile is %q after a run's recovery", b)
	}
}

// up runs with a TMPDIR of the workspace's own, beside the clone: the CLI
// stages Features in a folder under $TMPDIR named by the millisecond, which
// two concurrent creates otherwise share (measured, see
// container.UpSpec.TempDir). The fake refuses to succeed with any other.
func TestUpGetsTheWorkspacesOwnTempDir(t *testing.T) {
	e := newEnv(t)
	e.cli.up = `[ "$TMPDIR" = "$(dirname "$3")/.drydock/tmp" ] && [ -d "$TMPDIR" ] || { echo "TMPDIR=$TMPDIR" >&2; exit 3; }
` + upOK(t)
	e.wire(t)
	v := e.create(t, alpha, "")
	if v.State != workspace.Running {
		t.Fatalf("state %s (%s)", v.State, deref(v.StateDetail))
	}
	if _, err := os.Stat(filepath.Join(e.root, v.ID, ".drydock", "tmp")); err == nil {
		t.Error("the temporary directory outlived up")
	}
}

// A save naming a path outside the clone is refused, not written through:
// the save is Drydock's own file, but a refusal costs nothing.
func TestARestoreStaysInsideTheClone(t *testing.T) {
	wsDir := t.TempDir()
	clone := filepath.Join(wsDir, "repo")
	os.MkdirAll(filepath.Join(clone, ".devcontainer"), 0o755)
	inside := filepath.Join(clone, ".devcontainer", "devcontainer-lock.json")
	os.WriteFile(inside, []byte(lockfile), 0o644)
	if _, err := saveLockfile(wsDir, inside); err != nil {
		t.Fatal(err)
	}
	if err := restoreLockfile(wsDir, clone); err != nil {
		t.Fatalf("control: a save inside the clone: %v", err)
	}
	outside := filepath.Join(wsDir, "elsewhere.json")
	os.WriteFile(outside, []byte(lockfile), 0o644)
	if _, err := saveLockfile(wsDir, outside); err != nil {
		t.Fatal(err)
	}
	if err := restoreLockfile(wsDir, clone); err == nil {
		t.Error("a save naming a path outside the clone was restored")
	}
}
