package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestParseConfigurationAgainstTheCorpus: the recorded read-configuration
// results, each with the verdict its .meta says it must yield. The
// unparseable one is the case that matters — the CLI exits 0 for it, so a
// parser that trusted the exit code would hand `up` a config with no image.
func TestParseConfigurationAgainstTheCorpus(t *testing.T) {
	c, err := ParseConfiguration([]byte(fixture(t, "read-configuration-ok.json")))
	if err != nil || c.WorkspaceFolder != "/workspaces/repo" ||
		c.ConfigFile != "/srv/drydock/ws/FIXTURE/repo/.devcontainer/devcontainer.json" {
		t.Errorf("ok: %+v %v", c, err)
	}
	c, err = ParseConfiguration([]byte(fixture(t, "read-configuration-override.json")))
	if err != nil || c.WorkspaceFolder != "/workspaces/plain2" {
		t.Errorf("override: %+v %v", c, err)
	}
	if _, err := ParseConfiguration([]byte(fixture(t, "read-configuration-unparseable.json"))); !errors.Is(err, ErrUnbuildable) {
		t.Errorf("unparseable: %v, want ErrUnbuildable", err)
	}
	if _, err := ParseConfiguration([]byte(fixture(t, "read-configuration-noconfig.stdout"))); err == nil {
		t.Error("an empty stdout parsed")
	}
	for _, in := range []string{
		`{"configuration":{"image":"x"}}`, // no workspace
		`{"configuration":{"image":"x"},"workspace":{"workspaceFolder":"relative"}}`,
		`{"configuration":{"image":"x"},"workspace":{"workspaceFolder":"/w"}} {}`,
		`[]`,
	} {
		if _, err := ParseConfiguration([]byte(in)); err == nil || errors.Is(err, ErrUnbuildable) {
			t.Errorf("%s: %v; want a contract error", in, err)
		}
	}
	for _, cfg := range []string{`"image":"x"`, `"dockerFile":"Dockerfile"`, `"build":{"dockerfile":"D"}`,
		`"dockerComposeFile":"c.yml"`, `"dockerComposeFile":["a.yml","b.yml"]`} {
		in := `{"configuration":{` + cfg + `},"workspace":{"workspaceFolder":"/w"}}`
		if _, err := ParseConfiguration([]byte(in)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{`"image":""`, `"build":{}`, `"dockerComposeFile":[]`, `"name":"x"`} {
		in := `{"configuration":{` + cfg + `},"workspace":{"workspaceFolder":"/w"}}`
		if _, err := ParseConfiguration([]byte(in)); !errors.Is(err, ErrUnbuildable) {
			t.Errorf("%s: %v, want ErrUnbuildable", cfg, err)
		}
	}
}

// TestReadConfigurationArgvAndExitCodes: the override is passed when given
// and only then, and a non-zero exit is a ReadError rather than a verdict.
func TestReadConfigurationArgvAndExitCodes(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "read-configuration-ok.json") + "\nEOF\n",
	})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/repo", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/repo", "/srv/ws/.drydock/devcontainer.json"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv(t, dir, "devcontainer"), " ")
	merged := "--include-merged-configuration --id-label drydock.test.read-configuration=none "
	want := "read-configuration --workspace-folder /srv/ws/repo " + merged + "-- " +
		"read-configuration --workspace-folder /srv/ws/repo " + merged + "--override-config /srv/ws/.drydock/devcontainer.json "
	if got != want {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/repo", "relative.json"); err == nil {
		t.Error("a relative override was accepted")
	}

	run, _ = fakes(t, map[string]string{"devcontainer": "echo banner >&2; exit 1"})
	m.Run = run
	var re *ReadError
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/repo", ""); !errors.As(err, &re) || re.ExitCode != 1 || re.Stderr != "banner" {
		t.Errorf("exit 1: %v", err)
	}
}

func TestUpAndExecPassTheOverrideConfig(t *testing.T) {
	s := spec()
	s.OverrideConfig = "/srv/drydock/ws/" + wsID + "/.drydock/devcontainer.json"
	args, err := Manager{LabelPrefix: "drydock"}.Args(s)
	if err != nil || !strings.Contains(strings.Join(args, " "), " --override-config "+s.OverrideConfig) {
		t.Errorf("up argv %v %v", args, err)
	}
	s.OverrideConfig = "relative.json"
	if _, err := (Manager{LabelPrefix: "drydock"}).Args(s); err == nil {
		t.Error("a relative override was accepted by up")
	}

	run, dir := fakes(t, map[string]string{"devcontainer": "exit 0"})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	if _, err := m.ExecIn(context.Background(), ExecSpec{WorkspaceID: wsID, Folder: "/srv/ws/repo",
		OverrideConfig: "/srv/ws/.drydock/devcontainer.json", Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv(t, dir, "devcontainer"), " ")
	want := "exec --workspace-folder /srv/ws/repo --id-label drydock.test.workspace=" + wsID +
		" --override-config /srv/ws/.drydock/devcontainer.json -- true "
	if got != want {
		t.Errorf("exec argv\n got %q\nwant %q", got, want)
	}
}
