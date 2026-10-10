package container

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// Given a ConfigFile, up's guard policy carries the spec's two labels for the
// guard to add to the container — and up's own argv does not: they are not
// id-labels, so the CLI matches containers, and computes ${devcontainerId},
// from Drydock's four alone, exactly as without them. The control is a spec
// without a ConfigFile, whose policy has no labels.
func TestUpGivesTheGuardTheSpecLabels(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n",
	})
	m := guarded(t, run, "drydock.test")
	s := upSpec(t)
	s.ConfigFile = ConfigFiles(s.Folder)[0]
	if _, _, err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := argv(t, dir, "devcontainer")
	if a := strings.Join(got, " "); strings.Contains(a, "devcontainer.") {
		t.Errorf("up's argv carries the spec's labels:\n%s", a)
	}
	if ids := flagValues(got, "--id-label"); len(ids) != 4 {
		t.Errorf("id-labels %v", ids)
	}
	p := m.guardPolicy(s)
	want := map[string]string{LabelLocalFolder: s.Folder,
		LabelConfigFile: filepath.Join(s.Folder, ".devcontainer", "devcontainer.json")}
	if !reflect.DeepEqual(p.Labels, want) {
		t.Errorf("policy labels %v, want %v", p.Labels, want)
	}
	plain := s
	plain.ConfigFile = ""
	pp := m.guardPolicy(plain)
	if pp.Labels != nil {
		t.Errorf("control: policy labels %v", pp.Labels)
	}
	if !reflect.DeepEqual(p.IDLabels, pp.IDLabels) || dockerguard.DevcontainerID(p.IDLabels) != dockerguard.DevcontainerID(pp.IDLabels) {
		t.Errorf("the spec's labels moved the id-labels: %v vs %v", p.IDLabels, pp.IDLabels)
	}
}

// Through the real guard (this test binary, TestMain), the docker run the
// CLI writes reaches docker with the spec's two labels added right after
// `run` and nothing else changed. The control is the same up with no
// ConfigFile, whose docker run is the CLI's unaltered.
func TestTheGuardLabelsTheContainer(t *testing.T) {
	m, dir := upThroughGuard(t, "")
	s := upSpec(t)
	if _, _, err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	plain := argv(t, dir, "docker")
	os.Remove(filepath.Join(dir, "docker.argv"))
	s.ConfigFile = ConfigFiles(s.Folder)[1]
	if _, _, err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := argv(t, dir, "docker")
	want := append([]string{"run", "-l", LabelConfigFile + "=" + filepath.Join(s.Folder, ".devcontainer.json"),
		"-l", LabelLocalFolder + "=" + s.Folder}, plain[1:]...)
	if plain[0] != "run" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("docker ran with\n %q\nwant\n %q", got, want)
	}
}

func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

// The config_file label is a path VS Code computes, so only the two the CLI
// looks at in the folder are accepted — never another folder's, and never a
// relative path. The controls are the two it does look at.
func TestArgsRefuseAConfigFileTheCLIWouldNotCompute(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	s := spec()
	for _, ok := range ConfigFiles(s.Folder) {
		s.ConfigFile = ok
		if _, err := m.Args(s); err != nil {
			t.Errorf("control %s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"/srv/drydock/ws/OTHER/repo/.devcontainer/devcontainer.json",
		".devcontainer/devcontainer.json",
		s.Folder + "/.devcontainer/python/devcontainer.json",
		s.Folder + "/../.drydock/devcontainer.json",
	} {
		s.ConfigFile = bad
		if _, err := m.Args(s); err == nil {
			t.Errorf("config file %q accepted", bad)
		}
	}
}
