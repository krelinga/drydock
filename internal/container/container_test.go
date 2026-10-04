package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/classify"
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

func TestUpBuildsTheArgvAndParsesTheResult(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\necho 'log line' >&2\n",
	})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	c, stderr, err := m.Up(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	if c.Outcome != classify.ContainerRunning || c.ContainerID == "" {
		t.Errorf("result %+v", c)
	}
	if string(stderr) != "log line\n" {
		t.Errorf("stderr %q", stderr)
	}
	want := []string{"up", "--workspace-folder", "/srv/drydock/ws/" + wsID + "/repo",
		"--id-label", "drydock.test.workspace=" + wsID,
		"--id-label", "drydock.test.repository-id=42",
		"--id-label", "drydock.test.repo=krelinga/foo",
		"--id-label", "drydock.test.branch=main", ""}
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
	c, _, err := Manager{Run: run, LabelPrefix: "drydock"}.Up(context.Background(), spec())
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
