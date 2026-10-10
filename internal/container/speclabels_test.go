package container

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A rebuild — and a create, which has no container to reattach to — gives
// up the spec's two labels as id-labels beside Drydock's four, so the CLI
// sets them on the container, and the guard's policy carries the same six:
// its ${devcontainerId} must be the CLI's, which hashes every id-label. The
// control is a spec without a ConfigFile, whose argv has neither.
func TestUpLabelsTheContainerAsADevContainer(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n",
		"docker":       "true",
	})
	m := guarded(t, run, "drydock.test")
	s := upSpec(t)
	s.Rebuild = true
	s.ConfigFile = ConfigFiles(s.Folder)[0]
	if _, _, err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv(t, dir, "devcontainer"), " ")
	for _, want := range []string{
		"--id-label drydock.test.workspace=" + wsID,
		"--id-label " + LabelLocalFolder + "=" + s.Folder,
		"--id-label " + LabelConfigFile + "=" + filepath.Join(s.Folder, ".devcontainer", "devcontainer.json"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv lacks %q:\n%s", want, got)
		}
	}
	p := m.guardPolicy(s)
	if p.IDLabels[LabelLocalFolder] != s.Folder || p.IDLabels[LabelConfigFile] != s.ConfigFile || len(p.IDLabels) != 6 {
		t.Errorf("policy id-labels %v", p.IDLabels)
	}

	plain := spec()
	args, err := m.Args(plain)
	if err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(args, " "); strings.Contains(a, "devcontainer.") {
		t.Errorf("control: a spec with no ConfigFile labelled the container: %s", a)
	}
	if p := m.guardPolicy(plain); len(p.IDLabels) != 4 {
		t.Errorf("control: policy id-labels %v", p.IDLabels)
	}
}

// A start reattaches, and up matches by every id-label it is given: so it
// gives the labels the container was made with. A container from before the
// spec's labels gets none (else up would not find it and would make a second
// container); one made with them gets them back; with no container at all
// the spec's own are given, since up makes one.
func TestStartGivesTheLabelsTheContainerWasMadeWith(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s0 := upSpec(t)
	want := ConfigFiles(s0.Folder)[0]
	inspect := func(labels string) string {
		return `case "$1" in ps) echo ` + id + `;; inspect) cat <<'EOF'
[{"Id":"` + id + `","Name":"/x","State":{"Status":"exited"},"Config":{"Labels":{"drydock.test.workspace":"` + wsID + `"` + labels + `}}}]
EOF
;; esac`
	}
	for _, c := range []struct {
		name   string
		docker string
		want   string
	}{
		{"a container from before the labels", inspect(""), ""},
		{"a container made with them", inspect(`,"` + LabelLocalFolder + `":"` + s0.Folder + `","` +
			LabelConfigFile + `":"` + ConfigFiles(s0.Folder)[1] + `"`), ConfigFiles(s0.Folder)[1]},
		{"a container labelled for another folder", inspect(`,"` + LabelLocalFolder + `":"/elsewhere","` +
			LabelConfigFile + `":"/elsewhere/.devcontainer.json"`), ""},
		{"no container", "true", want},
	} {
		run, dir := fakes(t, map[string]string{
			"devcontainer": "cat <<'EOF'\n" + fixture(t, "up-ok.json") + "\nEOF\n",
			"docker":       c.docker,
		})
		m := guarded(t, run, "drydock.test")
		s := s0
		s.ConfigFile = want
		if _, _, err := m.Up(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := strings.Join(argv(t, dir, "devcontainer"), " ")
		if c.want == "" {
			if strings.Contains(got, "devcontainer.") {
				t.Errorf("%s: argv carries the spec's labels:\n%s", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, "--id-label "+LabelConfigFile+"="+c.want) ||
			!strings.Contains(got, "--id-label "+LabelLocalFolder+"="+s.Folder) {
			t.Errorf("%s: argv lacks the labels for %s:\n%s", c.name, c.want, got)
		}
	}
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
