package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/workspace"
)

// lockfile is a devcontainer-lock.json as VS Code writes one: in sync with
// the repository's configuration.
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

// staleLockfile is one the repository's configuration has moved on from.
// The fake up below tells it from an in-sync one by its "stale" entry.
const staleLockfile = `{
  "features": {
    "ghcr.io/devcontainers/features/stale:1": {}
  }
}
`

// refreshed is what the fake up writes over a stale lockfile.
const refreshed = `{"features":{"refreshed":{}}}`

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

// upWrites is a fake `up` that does to the lockfile what CLI 0.89.0 does with
// Drydock's Feature injected (test/fixtures/devcontainer/lockfile-behaviour.txt):
// with --no-lockfile or --frozen-lockfile, nothing; with no flag it leaves an
// in-sync lockfile byte for byte, rewrites a stale one, and creates one where
// there is none. $3 is the workspace folder.
const upWrites = `l="$3/.devcontainer/devcontainer-lock.json"
case " $* " in
*" --no-lockfile "*|*" --frozen-lockfile "*) ;;
*) if [ ! -e "$l" ] || grep -q stale "$l"; then printf '%s\n' '` + refreshed + `' > "$l"; fi ;;
esac
`

func upOK(t *testing.T) string {
	return "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"
}

// A committed lockfile is honoured — up gets no lockfile flag, so the CLI
// reads it — and a repository without one gets --no-lockfile, so none is
// created. An in-sync lockfile comes through untouched with nothing to
// report. A stale one is rewritten, as VS Code would, and Drydock leaves the
// rewrite in the clone and names the file on the up step instead of putting
// the old bytes back.
func TestTheLockfileIsHonouredAndARewriteIsReported(t *testing.T) {
	for _, c := range []struct {
		name, lock string
		honour     bool
		note       string // on resolve_config
		upNote     string // on up
		status     string // the clone's git status afterwards
		after      string // the lockfile's bytes afterwards, when committed
	}{
		{name: "no lockfile"},
		{name: "in-sync lockfile", lock: lockfile, honour: true,
			note: "pinned Feature versions are the ones installed", after: lockfile},
		{name: "stale lockfile", lock: staleLockfile, honour: true,
			note:   "pinned Feature versions are the ones installed",
			upNote: `rewrote ".devcontainer/devcontainer-lock.json" in the clone`,
			status: " M .devcontainer/devcontainer-lock.json\n", after: refreshed + "\n"},
		// A blank lockfile asks the CLI to fill it in: nothing to honour.
		{name: "blank lockfile", lock: "\n", after: "\n"},
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
			up := v.Steps[workspace.StepUp]
			if up.Status != "done" || (c.upNote == "") != (up.Detail == "") || !strings.Contains(up.Detail, c.upNote) {
				t.Errorf("up is %s, saying %q; want done, saying %q", up.Status, up.Detail, c.upNote)
			}
			repo := filepath.Join(e.root, v.ID, "repo")
			if out := gitOut(t, repo, "status", "--porcelain", "--ignored"); out != c.status {
				t.Errorf("the clone's status is %q; want %q", out, c.status)
			}
			if c.lock != "" {
				if b, _ := os.ReadFile(filepath.Join(repo, ".devcontainer", "devcontainer-lock.json")); string(b) != c.after {
					t.Errorf("the lockfile is %q; want %q", b, c.after)
				}
			}
			if _, err := os.Stat(filepath.Join(e.root, v.ID, ".drydock", "lockfile.json")); err == nil {
				t.Error("a lockfile was saved beside the clone")
			}
		})
	}
}

// The control for the test above: the fake up does what the recording says
// the real one does — leaves an in-sync lockfile, rewrites a stale one,
// creates a missing one — so the "untouched" rows mean something, and the
// --no-lockfile rows are clean because of the flag, not because the fake
// never writes.
func TestTheFakeUpBehavesAsRecorded(t *testing.T) {
	for _, c := range []struct {
		name, lock, flag string
		want             string // "" for no file
	}{
		{name: "in sync", lock: lockfile, want: lockfile},
		{name: "stale", lock: staleLockfile, want: refreshed + "\n"},
		{name: "none", want: refreshed + "\n"},
		{name: "none, --no-lockfile", flag: "--no-lockfile"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			l := filepath.Join(dir, ".devcontainer", "devcontainer-lock.json")
			os.MkdirAll(filepath.Dir(l), 0o755)
			if c.lock != "" {
				os.WriteFile(l, []byte(c.lock), 0o644)
			}
			args := []string{"-c", upWrites, "up", "up", "--workspace-folder", dir}
			if c.flag != "" {
				args = append(args, c.flag)
			}
			if out, err := exec.Command("sh", args...).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			b, err := os.ReadFile(l)
			if c.want == "" {
				if err == nil {
					t.Errorf("the fake up wrote %q", b)
				}
			} else if string(b) != c.want {
				t.Errorf("the fake up left %q, %v; want %q", b, err, c.want)
			}
		})
	}
}

// lockfileChange compares bytes, so what was in the clone before up — an
// agent's uncommitted edit to the lockfile, say — is not blamed on up; and
// every way up can change the file is named. The unchanged case is the
// control for the rest.
func TestLockfileChangeNamesOnlyWhatUpChanged(t *testing.T) {
	for _, c := range []struct {
		name   string
		before func(l string) // the clone before up
		up     func(l string) // what up does
		want   string         // in the note; "" for none
	}{
		{name: "unchanged", before: write(lockfile), up: func(string) {}},
		{name: "edited before up, unchanged by it", before: write(staleLockfile), up: func(string) {}},
		{name: "rewritten", before: write(staleLockfile), up: write(refreshed), want: `rewrote ".devcontainer/devcontainer-lock.json"`},
		{name: "created", before: func(string) {}, up: write(refreshed), want: `created ".devcontainer/devcontainer-lock.json"`},
		{name: "removed", before: write(lockfile), up: func(l string) { os.Remove(l) }, want: "something other than the lockfile"},
		{name: "replaced by a directory", before: write(lockfile),
			up: func(l string) { os.Remove(l); os.MkdirAll(l, 0o755) }, want: "something other than the lockfile"},
		{name: "absent throughout", before: func(string) {}, up: func(string) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			clone := t.TempDir()
			l := filepath.Join(clone, ".devcontainer", "devcontainer-lock.json")
			os.MkdirAll(filepath.Dir(l), 0o755)
			c.before(l)
			s := snapshotLockfile(l)
			c.up(l)
			err := lockfileChange(s, clone)
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Errorf("got %v; want %q", err, c.want)
			}
		})
	}
	// Drydock's minimal config has no lockfile path: nothing to compare.
	if err := lockfileChange(snapshotLockfile(""), t.TempDir()); err != nil {
		t.Errorf("no path: %v", err)
	}
}

func write(body string) func(string) {
	return func(path string) { os.WriteFile(path, []byte(body), 0o644) }
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
	e.cli.readConfig = "cat <<'EOF'\n" + fixture(t, "read-configuration-merged-override.json") + "\nEOF\n"
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
