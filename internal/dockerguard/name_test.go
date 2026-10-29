package dockerguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const fixtureName = "drydock-fixture-tvre00-ab12"

func namedPolicy(root string, approved []Setting) *Policy {
	p := FixturePolicy(root, approved)
	p.Name = fixtureName
	return p
}

// WithAdded puts the policy's name first, as --name, then its labels, right
// after run or create; a policy with a name and no labels adds the name
// alone. The recorded run, so named, passes the check: the guard's own
// addition is never what it refuses.
func TestWithAddedNamesTheContainer(t *testing.T) {
	root := t.TempDir()
	run := command(t, Recorded(t, "image", root), "run")

	p := labelPolicy(root)
	p.Name = fixtureName
	got := WithAdded(p, run)
	want := append([]string{"run", "--name", fixtureName,
		"-l", "devcontainer.config_file=" + p.Labels["devcontainer.config_file"],
		"-l", "devcontainer.local_folder=" + p.Labels["devcontainer.local_folder"]}, run[1:]...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("run:\n got %q\nwant %q", got, want)
	}
	if d := Check(p, got); d.Refused {
		t.Errorf("the named, labelled recorded run refused: %+v", d)
	}

	only := namedPolicy(root, nil)
	got = WithAdded(only, run)
	if !reflect.DeepEqual(got, append([]string{"run", "--name", fixtureName}, run[1:]...)) {
		t.Errorf("a name alone: %q", got)
	}
	if d := Check(only, got); d.Refused {
		t.Errorf("the named recorded run refused: %+v", d)
	}
	for _, args := range [][]string{{"create", "img"}, {"container", "create", "img"}} {
		if out := WithAdded(only, args); !slices.Contains(out, "--name") || out[len(out)-1] != "img" {
			t.Errorf("%v: %q", args, out)
		}
	}
	for _, args := range [][]string{{"start", "x"}, {"exec", "x", "sh"}, {"build", "."}} {
		if out := WithAdded(only, args); !reflect.DeepEqual(out, args) {
			t.Errorf("%v changed: %q", args, out)
		}
	}
}

// A --name the guard did not add, and no approved runArgs carries, is
// refused as runArgs, in both of docker's forms — and under a policy with no
// name, every --name left is: before the guard named containers, --name was
// an option it did not know, refused the same way. The control is the
// guard's own name, which passes.
func TestAnotherNameIsRefused(t *testing.T) {
	root := t.TempDir()
	p := namedPolicy(root, nil)
	run := WithAdded(p, command(t, Recorded(t, "image", root), "run"))
	if d := Check(p, run); d.Refused {
		t.Fatalf("control: %+v", d)
	}
	for name, extra := range map[string][]string{
		"--name x":            {"--name", "elsewhere"},
		"--name=x":            {"--name=elsewhere"},
		"--name=<ours>-other": {"--name=" + fixtureName + "-other"},
		"an empty --name=":    {"--name="},
	} {
		// After the guard's own, where the CLI writes a repository's runArgs.
		d := Check(p, insert(run, 3, extra...))
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingRunArgs}) {
			t.Errorf("%s: %+v, want refused naming %s", name, d, SettingRunArgs)
		}
		plain := command(t, Recorded(t, "image", root), "run")
		if d := Check(FixturePolicy(root, nil), insert(plain, 1, extra...)); !d.Refused {
			t.Errorf("%s, under a policy with no name: passed", name)
		}
	}
	if d := Check(FixturePolicy(root, nil), insert(command(t, Recorded(t, "image", root), "run"), 1, "--name", fixtureName)); !d.Refused {
		t.Error("the fixture's name, under a policy that names nothing: passed")
	}
}

// A repository's own --name, approved as runArgs, is kept: it is removed as
// the approved run before the parse, the guard's own name is checked as
// usual, and in argv the repository's comes after the guard's — docker keeps
// the last --name (measured on Docker 29: `create --name a --name b` and
// `--name=x --name y` are named b and y), so the container carries the
// repository's name, as it did before the guard named containers. The
// controls are the same words unapproved, refused.
func TestARepositorysOwnNameIsKept(t *testing.T) {
	root := t.TempDir()
	for _, theirs := range [][]string{{"--name", "their-box"}, {"--name=their-box"}} {
		raw, _ := json.Marshal(theirs)
		p := namedPolicy(root, []Setting{{"runArgs", raw}})
		run := insert(WithAdded(p, command(t, Recorded(t, "image", root), "run")), 3, theirs...)
		if d := Check(p, run); d.Refused {
			t.Errorf("%q approved: refused %+v", theirs, d)
		}
		if i, j := slices.Index(run, fixtureName), slices.IndexFunc(run, func(s string) bool { return strings.Contains(s, "their-box") }); i < 0 || j < i {
			t.Errorf("%q: the repository's name is not after the guard's in %q", theirs, run)
		}
		if d := Check(namedPolicy(root, nil), run); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingRunArgs}) {
			t.Errorf("control: %q unapproved: %+v", theirs, d)
		}
	}
}

// A policy whose name Docker would not accept is no policy: the guard
// refuses what it would have run under it. The control is a valid name.
func TestAPolicyWithABadNameIsUnreadable(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	p := namedPolicy(root, nil)
	if err := WritePolicy(dir, *p); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPolicy(dir); err != nil || got.Name != fixtureName {
		t.Fatalf("control: %+v %v", got, err)
	}
	for _, bad := range []string{"-drydock", "drydock x", "drydock/x", "d", "drydock\nx", ".x"} {
		p.Name = bad
		if err := WritePolicy(dir, *p); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadPolicy(dir); err == nil {
			t.Errorf("name %q read: %+v", bad, got)
		}
	}
	os.Remove(filepath.Join(dir, PolicyName))
}
