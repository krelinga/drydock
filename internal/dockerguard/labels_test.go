package dockerguard

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func labelPolicy(root string) *Policy {
	clone := filepath.Join(root, "repo")
	p := FixturePolicy(root, nil)
	p.Labels = map[string]string{
		"devcontainer.local_folder": clone,
		"devcontainer.config_file":  filepath.Join(clone, ".devcontainer", "devcontainer.json"),
	}
	return p
}

// WithLabels adds the policy's labels to a run or create — after the
// command, among the options, in key order — and to nothing else; with no
// labels, or no policy, argv is as given. The recorded run, labelled, passes
// the check, so the guard's own addition is never what it refuses.
func TestWithLabelsAddsThePolicysLabelsToARun(t *testing.T) {
	root := t.TempDir()
	p := labelPolicy(root)
	run := command(t, Recorded(t, "image", root), "run")
	got := WithLabels(p, run)
	want := append([]string{"run",
		"-l", "devcontainer.config_file=" + p.Labels["devcontainer.config_file"],
		"-l", "devcontainer.local_folder=" + p.Labels["devcontainer.local_folder"]}, run[1:]...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("run:\n got %q\nwant %q", got, want)
	}
	if d := Check(p, got); d.Refused {
		t.Errorf("the labelled recorded run refused: %+v", d)
	}
	for _, args := range [][]string{{"create", "img"}, {"container", "run", "img"}, {"container", "create", "img"}} {
		out := WithLabels(p, args)
		if len(out) != len(args)+4 || !strings.HasPrefix(strings.Join(out, " "), strings.Join(args[:len(args)-1], " ")+" -l devcontainer.config_file=") {
			t.Errorf("%v: %q", args, out)
		}
	}
	for _, args := range [][]string{{"start", "x"}, {"exec", "x", "sh"}, {"build", "."}, {"inspect", "x"}, {"container"}, {}} {
		if out := WithLabels(p, args); !reflect.DeepEqual(out, args) {
			t.Errorf("%v changed: %q", args, out)
		}
	}
	if out := WithLabels(FixturePolicy(root, nil), run); !reflect.DeepEqual(out, run) {
		t.Errorf("no labels: %q", out)
	}
	if out := WithLabels(nil, run); !reflect.DeepEqual(out, run) {
		t.Errorf("no policy: %q", out)
	}
}

// One of the policy's labels with another value is refused, on run and on
// start: docker keeps the last -l, and VS Code would find another folder's
// container by it. Under a policy that sets no labels the same label passes
// (labels off the prefix pass), and a start of a container made before the
// labels — carrying none — starts.
func TestTheGuardsLabelsCannotBeOverridden(t *testing.T) {
	root := t.TempDir()
	p := labelPolicy(root)
	run := WithLabels(p, command(t, Recorded(t, "image", root), "run"))
	if d := Check(p, run); d.Refused {
		t.Fatalf("control: %+v", d)
	}
	for name, extra := range map[string][]string{
		"another local_folder":       {"-l", "devcontainer.local_folder=/home/owner/project"},
		"another config_file":        {"--label", "devcontainer.config_file=" + filepath.Join(root, "repo", ".devcontainer.json")},
		"another local_folder, =":    {"--label=devcontainer.local_folder=/srv/drydock/ws/OTHER/repo"},
		"an empty local_folder":      {"-l", "devcontainer.local_folder="},
		"local_folder with no value": {"-l", "devcontainer.local_folder"},
	} {
		// After the guard's own -l pair, as a repository's runArgs would be.
		d := Check(p, insert(run, 5, extra...))
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingRunArgs}) {
			t.Errorf("%s: %+v, want refused naming %s", name, d, SettingRunArgs)
		}
		if d := Check(FixturePolicy(root, nil), insert(command(t, Recorded(t, "image", root), "run"), 1, extra...)); d.Refused {
			t.Errorf("%s, under a policy with no labels: refused %+v", name, d)
		}
	}

	c, id := recordedInspect(t, "image", root)
	labelled := func(local string) []byte {
		b, _ := json.Marshal(c)
		var cp map[string]any
		json.Unmarshal(b, &cp)
		ls := cp["Config"].(map[string]any)["Labels"].(map[string]any)
		if local != "" {
			ls["devcontainer.local_folder"] = local
			ls["devcontainer.config_file"] = p.Labels["devcontainer.config_file"]
		}
		out, _ := json.Marshal([]any{cp})
		return out
	}
	if d := CheckStarted(p, []string{id}, labelled(p.Labels["devcontainer.local_folder"]), nil); d.Refused {
		t.Fatalf("control, a start of a container labelled as the policy says: %+v", d)
	}
	if d := CheckStarted(p, []string{id}, labelled(""), nil); d.Refused {
		t.Errorf("a start of a container made before the labels: %+v", d)
	}
	if d := CheckStarted(p, []string{id}, labelled("/home/owner/project"), nil); !d.Refused ||
		!reflect.DeepEqual(d.Settings, []string{SettingRunArgs}) {
		t.Errorf("a start of a container labelled for another folder: %+v", d)
	}
}
