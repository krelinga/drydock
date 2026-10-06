package identity

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/subproc"
)

const (
	testFileImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	testClaudeID  = "sha256:bc02bacaa6e79376bdb7f95f26e00792e97054b18c59cc296f5729f1c22f3c76"
)

// scripted is a Runner that answers each docker invocation by its first
// argument, and records them all.
type scripted struct {
	cmds    []subproc.Cmd
	answers map[string]func(c subproc.Cmd) subproc.Result
}

func (s *scripted) Run(_ context.Context, c subproc.Cmd) subproc.Result {
	s.cmds = append(s.cmds, c)
	key := c.Args[0]
	switch key {
	case "run":
		key = "run " + entrypoint(c.Args)
	case "volume":
		key = "volume " + c.Args[1]
	}
	if f := s.answers[key]; f != nil {
		return f(c)
	}
	return subproc.Result{Err: errors.New("unscripted: " + key)}
}

func (s *scripted) Start(context.Context, subproc.Cmd) (subproc.Process, error) {
	return nil, errors.New("not used")
}

func entrypoint(args []string) string {
	for i, a := range args {
		if a == "--entrypoint" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func write(s string, code int) func(subproc.Cmd) subproc.Result {
	return func(c subproc.Cmd) subproc.Result {
		if c.Stdout != nil {
			io.WriteString(c.Stdout, s)
		}
		return subproc.Result{ExitCode: code}
	}
}

type fixedImage struct {
	id    string
	err   error
	calls int
}

func (f *fixedImage) Ensure(context.Context) (string, error) { f.calls++; return f.id, f.err }

func newSource(r *scripted, img *fixedImage) DockerSource {
	return DockerSource{Run: r, Image: img, FileImage: testFileImage, Volume: "drydock-claude-config", LabelPrefix: "drydock.test"}
}

// TestRunArgsAreReadOnlyAndBare: the helper mounts the login every workspace
// runs on, so its argv is its whole reach. The exact argv, and then each
// property on its own so a reordering does not hide one.
func TestRunArgsAreReadOnlyAndBare(t *testing.T) {
	d := newSource(nil, nil)
	got, err := d.RunArgs(testClaudeID, "claude", "auth", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	want := "run --rm --label drydock.test.identity=1 --network none --read-only" +
		" --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m --cap-drop ALL --cap-add DAC_READ_SEARCH" +
		" --security-opt no-new-privileges --user 0:0" +
		" --mount type=volume,source=drydock-claude-config,target=/claude,readonly" +
		" --env CLAUDE_CONFIG_DIR=/claude --env HOME=/tmp --env DISABLE_AUTOUPDATER=1" +
		" --entrypoint claude " + testClaudeID + " auth status --json"
	if strings.Join(got, " ") != want {
		t.Errorf("argv\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	joined := " " + strings.Join(got, " ") + " "
	for _, must := range []string{" --network none ", ",readonly ", " --read-only ", " --cap-drop ALL "} {
		if !strings.Contains(joined, must) {
			t.Errorf("argv lacks %q", must)
		}
	}
	for _, mustNot := range []string{"--privileged", " -v ", "--volume", "DAC_OVERRIDE", "docker.sock",
		"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "drydock.test.workspace"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("argv carries %q", mustNot)
		}
	}
	adds := strings.Count(joined, "--cap-add")
	if adds != 1 {
		t.Errorf("%d capabilities added; want only DAC_READ_SEARCH", adds)
	}

	// Refusals: what could add a mount option or name an unpinned image.
	for _, c := range []struct{ name, volume, image string }{
		{"a volume with mount options", "v,readonly=false", testClaudeID},
		{"a path for a volume", "/var/lib/docker", testClaudeID},
		{"a volume that reads as a flag", "-x", testClaudeID},
		{"an unpinned image", "drydock-claude-config", "node:22"},
		{"a tag-only image", "drydock-claude-config", "busybox:1.37.0"},
	} {
		d := newSource(nil, nil)
		d.Volume = c.volume
		if _, err := d.RunArgs(c.image, "sh"); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestCredentialsReadsTheFileOrSaysAbsent: a missing volume is absent with
// no container run at all (docker run -v would create it); a name that only
// contains the volume's is not it; exit 3 from the script is absent; exit 0
// is the bytes; anything else is an error — never absent. The file is read
// with the busybox image, never the Claude one.
func TestCredentialsReadsTheFileOrSaysAbsent(t *testing.T) {
	ctx := context.Background()
	const ours = `{"drydock.test.claude-config":"true"}` + "\n"
	img := &fixedImage{id: testClaudeID}

	r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
		"volume ls": write("drydock-claude-config-old\nx-drydock-claude-config\n", 0)}}
	if b, err := newSource(r, img).Credentials(ctx); err != nil || b != nil {
		t.Errorf("no such volume: %q, %v; want nil, nil", b, err)
	}
	if len(r.cmds) != 1 {
		t.Errorf("a missing volume ran %d commands; want only the listing", len(r.cmds))
	}

	for _, c := range []struct {
		name    string
		code    int
		out     string
		want    string
		wantNil bool
		wantErr bool
	}{
		{"the file", 0, `{"claudeAiOauth":{}}`, `{"claudeAiOauth":{}}`, false, false},
		{"an empty file", 0, "", "", false, false},
		{"no file", 3, "", "", true, false},
		{"cat failed", 1, "", "", false, true},
		{"docker failed", 125, "", "", false, true},
	} {
		r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
			"volume ls":      write("drydock-claude-config\n", 0),
			"volume inspect": write(ours, 0),
			"run sh":         write(c.out, c.code)}}
		b, err := newSource(r, img).Credentials(ctx)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("%s: no error", c.name)
		case !c.wantErr && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.wantNil && b != nil:
			t.Errorf("%s: %q; want nil (absent)", c.name, b)
		case !c.wantNil && !c.wantErr && (b == nil || string(b) != c.want):
			t.Errorf("%s: %q; want %q, non-nil", c.name, b, c.want)
		}
		if run := r.cmds[len(r.cmds)-1].Args; !strings.Contains(strings.Join(run, " "), " "+testFileImage+" ") {
			t.Errorf("%s: the file was not read with the busybox image: %v", c.name, run)
		}
	}
	if img.calls != 0 {
		t.Errorf("reading the file built the Claude image %d times", img.calls)
	}

	// Over the cap is an error, not a truncated credential.
	r = &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
		"volume ls":      write("drydock-claude-config\n", 0),
		"volume inspect": write(ours, 0),
		"run sh":         write(strings.Repeat("x", maxRead+1), 0)}}
	if _, err := newSource(r, img).Credentials(ctx); err == nil {
		t.Error("an oversized read was accepted")
	}
	// The daemon down is a docker problem.
	r = &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{"volume ls": write("", 1)}}
	var re *ReadError
	if _, err := newSource(r, img).Credentials(ctx); !errors.As(err, &re) || re.Problem != ProblemDocker {
		t.Errorf("docker down: %v; want a docker ReadError", err)
	}
}

// TestAuthStatusTakesZeroAndOne: auth status exits 1 for loggedIn:false, so
// both are answers; 125 is docker failing, and an image that cannot be built
// is its own problem.
func TestAuthStatusTakesZeroAndOne(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{0, 1} {
		r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{"run claude": write(`{"loggedIn":false}`, code)}}
		if b, err := newSource(r, &fixedImage{id: testClaudeID}).AuthStatus(ctx); err != nil || string(b) != `{"loggedIn":false}` {
			t.Errorf("exit %d: %q, %v", code, b, err)
		}
		if a := strings.Join(r.cmds[0].Args, " "); !strings.Contains(a, " "+testClaudeID+" auth status --json") {
			t.Errorf("not run in the Claude image: %s", a)
		}
	}
	r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{"run claude": write("", 125)}}
	if _, err := newSource(r, &fixedImage{id: testClaudeID}).AuthStatus(ctx); err == nil {
		t.Error("exit 125 was an answer")
	}
	var re *ReadError
	if _, err := newSource(r, &fixedImage{err: errors.New("npm: network")}).AuthStatus(ctx); !errors.As(err, &re) || re.Problem != ProblemImage {
		t.Errorf("image failure: %v; want an image ReadError", err)
	}
}

// TestAForeignVolumeIsNotRead: a volume of the configured name that this
// Drydock did not make — no <prefix>.claude-config label, which §6 step 4
// puts on the one it makes and refuses to mount without — is a check that
// failed, never a verdict: not absent, and its file is never read. Another
// prefix's label is not ours. The control is the same volume labelled with
// this prefix, which is read.
func TestAForeignVolumeIsNotRead(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, labels string }{
		{"no labels", "null\n"},
		{"other labels", `{"com.example":"x"}` + "\n"},
		{"another Drydock's", `{"drydock.other.claude-config":"true"}` + "\n"},
	} {
		r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
			"volume ls":      write("drydock-claude-config\n", 0),
			"volume inspect": write(c.labels, 0),
			"run sh":         write(`{"claudeAiOauth":{}}`, 0)}}
		b, err := newSource(r, &fixedImage{id: testClaudeID}).Credentials(ctx)
		var re *ReadError
		if !errors.As(err, &re) || re.Problem != ProblemForeign || b != nil {
			t.Errorf("%s: %q, %v; want a foreign_volume ReadError", c.name, b, err)
		}
		for _, cmd := range r.cmds {
			if cmd.Args[0] == "run" {
				t.Errorf("%s: a foreign volume was mounted: %v", c.name, cmd.Args)
			}
		}
	}
	r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
		"volume ls":      write("drydock-claude-config\n", 0),
		"volume inspect": write(`{"drydock.test.claude-config":"true"}`+"\n", 0),
		"run sh":         write(`{"claudeAiOauth":{}}`, 0)}}
	if b, err := newSource(r, &fixedImage{id: testClaudeID}).Credentials(ctx); err != nil || string(b) != `{"claudeAiOauth":{}}` {
		t.Errorf("control, our label: %q, %v", b, err)
	}
	if got := r.cmds[1].Args; strings.Join(got, " ") != "volume inspect --format {{json .Labels}} -- drydock-claude-config" {
		t.Errorf("inspect argv: %v", got)
	}
}
