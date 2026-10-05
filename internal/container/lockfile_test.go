package container

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each up runs with the TMPDIR it is given, replacing Drydock's own: CLI
// 0.89.0 stages Features under $TMPDIR in a folder named by the millisecond,
// and two concurrent ups sharing one built each other's Features. The
// control is the same fake without a TempDir, which sees the inherited one.
func TestUpRunsWithItsOwnTempDir(t *testing.T) {
	inherited := t.TempDir()
	t.Setenv("TMPDIR", inherited)
	m := Manager{LabelPrefix: "drydock"}
	seen := func(s UpSpec) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "tmpdir")
		m.Run, _ = fakes(t, map[string]string{"devcontainer": "echo \"$TMPDIR\" > " + out +
			"\ncat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n"})
		if _, _, err := m.Up(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(out)
		return strings.TrimSpace(string(b))
	}
	s := spec()
	if got := seen(s); got != inherited {
		t.Errorf("control: without a TempDir up saw TMPDIR=%q", got)
	}
	s.TempDir = "/srv/drydock/ws/" + wsID + "/.drydock/tmp"
	if got := seen(s); got != s.TempDir {
		t.Errorf("up saw TMPDIR=%q, want %q", got, s.TempDir)
	}
	s.TempDir = "relative"
	if _, _, err := m.Up(context.Background(), s); err == nil {
		t.Error("a relative TempDir was accepted")
	}
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// Both branches of the lockfile decision reach argv. Ignoring passes
// --no-lockfile; honouring passes no lockfile flag at all — not
// --frozen-lockfile, which refuses every lockfile VS Code writes once
// Drydock's Feature and its dependency are added (measured).
func TestArgsCarryTheLockfileDecision(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	ignore, err := m.Args(spec())
	if err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.Lockfile = LockfileHonour
	honour, err := m.Args(s)
	if err != nil {
		t.Fatal(err)
	}
	if !has(ignore, "--no-lockfile") || has(ignore, "--frozen-lockfile") {
		t.Errorf("the zero value: %v; want --no-lockfile", ignore)
	}
	for _, a := range honour {
		if strings.Contains(a, "lockfile") {
			t.Errorf("LockfileHonour: %v; want no lockfile flag", honour)
		}
	}
	if len(honour) != len(ignore)-1 {
		t.Errorf("honour %v and ignore %v differ by more than the flag", honour, ignore)
	}
	s.Lockfile = Lockfile(7)
	if _, err := m.Args(s); err == nil {
		t.Error("an unknown lockfile mode was accepted")
	}
}

// Honouring a lockfile with Drydock's override is a contradiction — with no
// flag the CLI fails writing a lockfile at the repository's default path,
// and never reads one beside the override — so Args refuses it.
func TestArgsRefuseHonouringWithAnOverride(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	s := spec()
	s.OverrideConfig = "/srv/drydock/ws/" + wsID + "/.drydock/devcontainer.json"
	if _, err := m.Args(s); err != nil {
		t.Fatalf("control: an override with no lockfile was refused: %v", err)
	}
	s.Lockfile = LockfileHonour
	if _, err := m.Args(s); err == nil {
		t.Error("honouring a lockfile with an override was accepted")
	}
}

func TestLockfilePathFollowsTheConfigsName(t *testing.T) {
	for cfg, want := range map[string]string{
		"/r/.devcontainer/devcontainer.json":        "/r/.devcontainer/devcontainer-lock.json",
		"/r/.devcontainer.json":                     "/r/.devcontainer-lock.json",
		"/r/.devcontainer/python/devcontainer.json": "/r/.devcontainer/python/devcontainer-lock.json",
	} {
		if got := LockfilePath(cfg); got != want {
			t.Errorf("LockfilePath(%s) = %s, want %s", cfg, got, want)
		}
	}
}

const injected = "ghcr.io/krelinga/drydock/drydock:0"

func lockRepo(t *testing.T, lock *string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".devcontainer")
	os.MkdirAll(dir, 0o755)
	cfg := filepath.Join(dir, "devcontainer.json")
	os.WriteFile(cfg, []byte(`{"image":"x"}`), 0o644)
	if lock != nil {
		os.WriteFile(filepath.Join(dir, "devcontainer-lock.json"), []byte(*lock), 0o644)
	}
	return cfg
}

func ptr(s string) *string { return &s }

const goodLock = `{
  "features": {
    "ghcr.io/devcontainers/features/node:1": {
      "version": "1.6.3",
      "resolved": "ghcr.io/devcontainers/features/node@sha256:aa",
      "integrity": "sha256:aa"
    }
  }
}
`

func TestLockfileMode(t *testing.T) {
	for _, c := range []struct {
		name string
		lock *string
		want Lockfile
		err  error
	}{
		{"none committed", nil, LockfileIgnore, nil},
		{"committed", ptr(goodLock), LockfileHonour, nil},
		{"committed, no Features", ptr(`{"features":{}}`), LockfileHonour, nil},
		// The CLI fills a blank lockfile in, which is a write; ignoring it
		// installs the same thing and writes nothing.
		{"empty", ptr(""), LockfileIgnore, nil},
		{"whitespace", ptr(" \n\t\n"), LockfileIgnore, nil},
		{"not JSON", ptr(`{"features":`), LockfileIgnore, ErrLockfileUnreadable},
		{"no features object", ptr(`{"version":1}`), LockfileIgnore, ErrLockfileUnreadable},
		{"an array", ptr(`[]`), LockfileIgnore, ErrLockfileUnreadable},
		{"too large", ptr(`{"features":{}}` + strings.Repeat(" ", MaxLockfile)), LockfileIgnore, ErrLockfileUnreadable},
		{"pins Drydock's Feature", ptr(`{"features":{"` + injected + `":{"version":"0.1.0"}}}`), LockfileIgnore, ErrLockfilePinsInjected},
	} {
		got, err := LockfileMode(lockRepo(t, c.lock), []string{injected})
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("%s: %v, %v; want %v, %v", c.name, got, err, c.want, c.err)
		}
	}
}

// A symbolic link is refused, not followed; the control is the same bytes as
// a regular file.
func TestLockfileModeRefusesASymlink(t *testing.T) {
	cfg := lockRepo(t, nil)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	os.WriteFile(target, []byte(goodLock), 0o644)
	if err := os.Symlink(target, LockfilePath(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := LockfileMode(cfg, nil); !errors.Is(err, ErrLockfileUnreadable) {
		t.Errorf("a symlinked lockfile: %v", err)
	}
	if m, err := LockfileMode(lockRepo(t, ptr(goodLock)), nil); m != LockfileHonour || err != nil {
		t.Errorf("control: %v %v", m, err)
	}
}

// The recorded matrix is what the design rests on, so a re-record on a CLI
// bump that changes any of these beliefs fails here rather than in a
// workspace. Columns: scenario | flags | exit | message | marker | git status | lockfile.
func TestRecordedLockfileBehaviour(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "lockfile-behaviour.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		cols := strings.Split(sc.Text(), " | ")
		if len(cols) != 7 {
			t.Fatalf("malformed row %q", sc.Text())
		}
		rows[cols[0]] = cols
	}
	for _, c := range []struct {
		row, exit, marker, status, why string
	}{
		{"nolockfile-pinned", "0", "1.1.0", "clean", "--no-lockfile ignores a committed lockfile"},
		{"frozen-pinned", "0", "1.0.0", "clean", "--frozen-lockfile honours a committed lockfile and writes nothing"},
		{"default-pinned", "0", "1.0.0", "clean", "no flag honours an in-sync lockfile"},
		{"default-pinned-dependent", "0", "1.0.0", " M .devcontainer/devcontainer-lock.json;", "an injected Feature's dependency is written into a committed lockfile"},
		{"frozen-pinned-dependent", "1", "-", "clean", "--frozen-lockfile therefore refuses every lockfile once Drydock's Feature is added"},
		{"default-none", "0", "1.1.0", "?? .devcontainer/devcontainer-lock.json;", "no flag writes a lockfile into the clone"},
		{"nolockfile-none", "0", "1.1.0", "clean", "--no-lockfile writes nothing"},
		{"frozen-none", "1", "-", "clean", "--frozen-lockfile needs a lockfile"},
		{"default-empty", "0", "1.1.0", " M .devcontainer/devcontainer-lock.json;", "no flag fills a blank lockfile in"},
		{"frozen-stale", "1", "-", "clean", "--frozen-lockfile refuses a stale lockfile and writes nothing"},
		{"default-stale", "0", "1.0.0", " M .devcontainer/devcontainer-lock.json;", "no flag rewrites a stale lockfile"},
		{"nolockfile-override-noconfig", "0", "1.1.0", "clean", "the override path writes nothing with --no-lockfile"},
		{"default-override-beside", "0", "1.1.0", "?? .devcontainer/devcontainer-lock.json;", "a lockfile beside the override is not read"},
		{"default-additional-pinned", "0", "1.0.0", " M .devcontainer/devcontainer-lock.json;", "an --additional-features Feature is pinned by a lockfile entry"},
		{"frozen-additional-pinned", "1", "-", "clean", "--frozen-lockfile refuses an entry for an --additional-features Feature"},
	} {
		r, ok := rows[c.row]
		if !ok {
			t.Errorf("%s: not recorded", c.row)
			continue
		}
		if r[2] != c.exit || r[4] != c.marker || r[5] != c.status {
			t.Errorf("%s: exit %s, marker %s, status %q; the design assumes %s", c.row, r[2], r[4], r[5], c.why)
		}
	}

	// Drydock's own Feature, injected as Drydock injects it. These rows'
	// "lockfile after" compares bytes with what was committed: "unchanged",
	// or the sha256 of what up left.
	const dirty = " M .devcontainer/devcontainer-lock.json;"
	for _, c := range []struct {
		row, marker, status, lock, why string
	}{
		// The rows the design rests on: with no dependsOn, a lockfile VS Code
		// wrote comes through a workspace's whole life byte for byte, and its
		// pin is honoured (1.0.0, where the tag now resolves to 1.1.0).
		{"vscode-create", "1.0.0", "clean", "unchanged", "a VS Code lockfile is untouched by a create"},
		{"vscode-start", "1.0.0", "clean", "unchanged", "and by a start after a stop"},
		{"vscode-rebuild", "1.0.0", "clean", "unchanged", "and by a rebuild"},
		// The control: the same Feature with the dependsOn it used to carry
		// rewrites that same lockfile on the first create.
		{"vscode-create-dependson", "1.0.0", dirty, "", "a Feature dependency is written into the lockfile"},
		{"stale-vscode", "1.0.0", dirty, "", "VS Code rewrites a stale lockfile"},
		{"stale-drydock", "1.0.0", dirty, "", "so does up with Drydock's Feature"},
		{"stale-drydock-dependson", "1.0.0", dirty, "", "and with the dependency, differently"},
	} {
		r, ok := rows[c.row]
		if !ok {
			t.Errorf("%s: not recorded", c.row)
			continue
		}
		if r[2] != "0" || r[4] != c.marker || r[5] != c.status || (c.lock != "" && r[6] != c.lock) {
			t.Errorf("%s: exit %s, marker %s, status %q, lockfile %s; the design assumes %s", c.row, r[2], r[4], r[5], r[6], c.why)
		}
	}
	// A stale lockfile is rewritten to exactly the bytes VS Code writes —
	// which is why Drydock leaves the rewrite in the clone — and the
	// dependency is what made it differ before.
	vs, dd, dep := rows["stale-vscode"], rows["stale-drydock"], rows["stale-drydock-dependson"]
	if vs == nil || dd == nil || dep == nil {
		t.Fatal("the stale rows are not recorded")
	}
	if !strings.HasPrefix(vs[6], "sha256:") || dd[6] != vs[6] {
		t.Errorf("a stale lockfile became %s with Drydock and %s with VS Code; the design assumes the same bytes", dd[6], vs[6])
	}
	if dep[6] == vs[6] {
		t.Errorf("with the dependency the stale lockfile became the same bytes as VS Code's (%s), so the comparison above proves nothing", dep[6])
	}
}
