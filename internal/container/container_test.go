package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/subproc"
)

const wsID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

// fakes builds fake devcontainer and docker binaries: each records its argv,
// one per line, to <dir>/<name>.argv and runs the given body.
func fakes(t *testing.T, bodies map[string]string) (subproc.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	res := subproc.FixedResolver{}
	for name, body := range bodies {
		p := filepath.Join(dir, name)
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + filepath.Join(dir, name+".argv") + "\necho '--' >> " +
			filepath.Join(dir, name+".argv") + "\n" + body
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		res[name] = p
	}
	return subproc.Exec{Resolver: res}, dir
}

func argv(t *testing.T, dir, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".argv"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "--\n"), "\n")
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func spec() UpSpec {
	return UpSpec{WorkspaceID: wsID, RepositoryID: 42, FullName: "krelinga/foo", Branch: "main",
		Folder: "/srv/drydock/ws/" + wsID + "/repo"}
}

// upSpec is spec() laid out on disk as a workspace is — the clone at
// <root>/<id>/repo, the CLI's TMPDIR at <root>/<id>/.drydock/tmp — for a
// test that runs Up, which prepares the guard beside the clone.
func upSpec(t *testing.T) UpSpec {
	t.Helper()
	root := filepath.Join(t.TempDir(), wsID)
	s := spec()
	s.Folder = filepath.Join(root, "repo")
	s.TempDir = filepath.Join(root, ".drydock", "tmp")
	for _, d := range []string{s.Folder, s.TempDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// guarded is a Manager running run, with this test binary as its docker
// guard (TestMain) and, unless run's resolver has one, /bin/true as the
// docker it passes commands to.
func guarded(t *testing.T, run subproc.Runner, prefix string) Manager {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	g := &dockerguard.Guard{Binary: self}
	if e, ok := run.(subproc.Exec); ok {
		if _, err := e.Resolver.Resolve("docker"); err != nil {
			g.Resolver = subproc.FixedResolver{"docker": "/bin/true"}
		}
	}
	return Manager{Run: run, LabelPrefix: prefix, Guard: g}
}

func TestUpBuildsTheArgvAndParsesTheResult(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\necho 'log line' >&2\n",
	})
	m := guarded(t, run, "drydock.test")
	s := upSpec(t)
	c, stderr, err := m.Up(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if c.Outcome != classify.ContainerRunning || c.ContainerID == "" {
		t.Errorf("result %+v", c)
	}
	if string(stderr) != "log line\n" {
		t.Errorf("stderr %q", stderr)
	}
	want := []string{"up", "--workspace-folder", s.Folder, "--no-lockfile",
		"--id-label", "drydock.test.workspace=" + wsID,
		"--id-label", "drydock.test.repository-id=42",
		"--id-label", "drydock.test.repo=krelinga/foo",
		"--id-label", "drydock.test.branch=main",
		"--docker-path", filepath.Join(filepath.Dir(s.Folder), ".drydock", "guard", "docker"),
		"--docker-compose-path", filepath.Join(filepath.Dir(s.Folder), ".drydock", "guard", "docker"), ""}
	if got := argv(t, dir, "devcontainer"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
}

// A rebuild must pass --remove-existing-container: without it `up` reattaches
// to the old container and reports success (§6). Both halves, so neither is
// vacuous.
func TestRebuildRemovesTheExistingContainer(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	plain, _ := m.Args(spec())
	s := spec()
	s.Rebuild = true
	rebuild, _ := m.Args(s)
	has := func(a []string) bool {
		for _, x := range a {
			if x == "--remove-existing-container" {
				return true
			}
		}
		return false
	}
	if has(plain) || !has(rebuild) {
		t.Errorf("plain up %v, rebuild %v", plain, rebuild)
	}
}

// A failed up is a verdict, and it keeps the container id a failed
// postCreateCommand leaves behind.
func TestFailedUpIsAResultNotAnError(t *testing.T) {
	run, _ := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-error-postcreate.json") + "\nEOF\nexit 1\n",
	})
	c, _, err := guarded(t, run, "drydock").Up(context.Background(), upSpec(t))
	if err != nil || c.Outcome != classify.ContainerFailed || c.ContainerID == "" {
		t.Errorf("result %+v, err %v; want a failed verdict carrying the container id", c, err)
	}
}

// Workspace data reaches argv, so malformed data is refused before anything
// runs rather than passed along.
func TestArgsRefuseMalformedWorkspaceData(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	for name, mut := range map[string]func(*UpSpec){
		"id not a ULID":    func(s *UpSpec) { s.WorkspaceID = "../../etc" },
		"name with a flag": func(s *UpSpec) { s.FullName = "--mount=type=bind,source=/,target=/host" },
		"name with spaces": func(s *UpSpec) { s.FullName = "a b/c" },
		"relative folder":  func(s *UpSpec) { s.Folder = "repo" },
		"no repository":    func(s *UpSpec) { s.RepositoryID = 0 },
		"no branch":        func(s *UpSpec) { s.Branch = "" },
	} {
		s := spec()
		mut(&s)
		if _, err := m.Args(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := m.Args(spec()); err != nil {
		t.Errorf("control: a valid spec was refused: %v", err)
	}
}

func TestListFindsByLabelAndReadsInspect(t *testing.T) {
	inspect := `[
	 {"Id":"aaa","State":{"Status":"running","Running":true},
	  "Config":{"Labels":{"drydock.test.workspace":"` + wsID + `","drydock.test.repository-id":"42",
	   "drydock.test.repo":"krelinga/foo","drydock.test.branch":"main"}}},
	 {"Id":"bbb","State":{"Status":"exited","Running":false},
	  "Config":{"Labels":{"drydock.test.workspace":"01JZZZZZZZZZZZZZZZZZZZZZZZ"}}}]`
	run, dir := fakes(t, map[string]string{
		"docker": `case "$1" in ps) printf 'aaa\nbbb\n';; inspect) cat <<'EOF'
` + inspect + `
EOF
;; esac`,
	})
	found, err := Manager{Run: run, LabelPrefix: "drydock.test"}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || !found[0].Running || found[0].RepositoryID != 42 || found[0].Branch != "main" ||
		found[1].Running || found[1].Status != "exited" || found[1].RepositoryID != 0 {
		t.Errorf("found %+v", found)
	}
	calls := strings.Join(argv(t, dir, "docker"), " ")
	if !strings.Contains(calls, "ps --all --quiet --no-trunc --filter label=drydock.test.workspace") ||
		!strings.Contains(calls, "inspect --type container aaa bbb") {
		t.Errorf("docker calls: %s", calls)
	}
}

// A container a Drydock before the directory mount made has the socket
// mounted as a file; one made now has the directory. The first is reported
// legacy, by List and by LegacyBrokerMount, and the second — the control —
// is not; nor is a container with no broker mount at all, nor a volume that
// happens to sit at the old path.
func TestLegacyBrokerMountIsReadFromInspect(t *testing.T) {
	const old, cur, none = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	labels := `"Config":{"Labels":{"drydock.workspace":"` + wsID + `"}}`
	inspect := map[string]string{
		old: `{"Id":"` + old + `","State":{"Running":true},` + labels + `,"Mounts":[
		  {"Type":"bind","Source":"/run/drydock/sock/` + wsID + `.sock","Destination":"/run/drydock/broker.sock"},
		  {"Type":"volume","Name":"drydock-claude-config","Destination":"/home/vscode/.claude"}]}`,
		cur: `{"Id":"` + cur + `","State":{"Running":true},` + labels + `,"Mounts":[
		  {"Type":"bind","Source":"/run/drydock/sock/` + wsID + `","Destination":"/run/drydock","RW":false}]}`,
		none: `{"Id":"` + none + `","State":{"Running":true},` + labels + `,"Mounts":[
		  {"Type":"volume","Name":"x","Destination":"/run/drydock/broker.sock"}]}`,
	}
	for _, c := range []struct {
		id     string
		legacy bool
	}{{old, true}, {cur, false}, {none, false}} {
		body := `case "$1" in ps) echo ` + c.id + `;; inspect) cat <<'EOF'
[` + inspect[c.id] + `]
EOF
;; esac`
		run, _ := fakes(t, map[string]string{"docker": body})
		m := Manager{Run: run, LabelPrefix: "drydock"}
		got, err := m.LegacyBrokerMount(context.Background(), wsID)
		if err != nil || got != c.legacy {
			t.Errorf("container %.3s: LegacyBrokerMount %v, %v; want %v", c.id, got, err, c.legacy)
		}
		found, err := m.List(context.Background())
		if err != nil || len(found) != 1 || found[0].LegacyBrokerMount != c.legacy {
			t.Errorf("container %.3s: List %+v, %v", c.id, found, err)
		}
	}
	// No container at all is not legacy, and needs no inspect.
	run, _ := fakes(t, map[string]string{"docker": `[ "$1" = inspect ] && exit 9; true`})
	if got, err := (Manager{Run: run, LabelPrefix: "drydock"}).LegacyBrokerMount(context.Background(), wsID); got || err != nil {
		t.Errorf("no container: %v, %v", got, err)
	}
}

func TestListWithNothingFoundSkipsInspect(t *testing.T) {
	run, dir := fakes(t, map[string]string{"docker": `[ "$1" = inspect ] && exit 9; true`})
	found, err := Manager{Run: run, LabelPrefix: "drydock"}.List(context.Background())
	if err != nil || found != nil {
		t.Errorf("%v, %v", found, err)
	}
	if strings.Contains(strings.Join(argv(t, dir, "docker"), " "), "inspect") {
		t.Error("inspect ran with no ids")
	}
}

// A container without the label it was listed by means the contract moved;
// acting on it would be guessing which workspace it is.
func TestListRefusesAContainerWithoutItsLabel(t *testing.T) {
	run, _ := fakes(t, map[string]string{
		"docker": `case "$1" in ps) echo aaa;; inspect) echo '[{"Id":"aaa","State":{},"Config":{"Labels":{"other":"x"}}}]';; esac`,
	})
	if _, err := (Manager{Run: run, LabelPrefix: "drydock"}).List(context.Background()); err == nil {
		t.Error("a container lacking the workspace label was accepted")
	}
}

func TestListReportsDockerFailure(t *testing.T) {
	run, _ := fakes(t, map[string]string{"docker": `echo 'Cannot connect to the Docker daemon' >&2; exit 1`})
	_, err := Manager{Run: run, LabelPrefix: "drydock"}.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("err %v; want docker's message", err)
	}
}

func TestArgsMountTheBrokerAndPassFeatures(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	s := spec()
	s.BrokerDir = "/run/drydock/sock/" + wsID
	s.Features = map[string]map[string]any{"ghcr.io/krelinga/drydock/drydock:0": {"botName": "x[bot]"}}
	s.RemoteEnv = map[string]string{"B": "2", "A": "1"}
	args, err := m.Args(s)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--mount type=bind,source=/run/drydock/sock/" + wsID + ",target=/run/drydock",
		`--additional-features {"ghcr.io/krelinga/drydock/drydock:0":{"botName":"x[bot]"}}`,
		"--remote-env A=1 --remote-env B=2", // sorted, so the argv is stable
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv lacks %q:\n%s", want, got)
		}
	}
}

// The socket path is spliced into --mount's comma-separated options, so a
// comma or '=' in it would let it add options of its own — a second source,
// say, mounting the host's root.
func TestArgsRefuseAnInjectableMount(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	for _, bad := range []string{
		"/tmp/x,target=/host,source=/",
		"/tmp/a=b",
		"relative",
	} {
		s := spec()
		s.BrokerDir = bad
		if _, err := m.Args(s); err == nil {
			t.Errorf("broker directory %q accepted", bad)
		}
	}
	s := spec()
	s.RemoteEnv = map[string]string{"BAD-NAME": "x"}
	if _, err := m.Args(s); err == nil {
		t.Error("a malformed --remote-env name accepted")
	}
}
