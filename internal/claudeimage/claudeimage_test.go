package claudeimage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/subproc"
)

const (
	base = "node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c"
	id   = "sha256:bc02bacaa6e79376bdb7f95f26e00792e97054b18c59cc296f5729f1c22f3c76"
)

// TestDockerfilePinsBoth: the recipe names the base by digest and Claude Code
// by exact version, and checks the installed binary reports exactly that
// version. Inputs that are not pinned, or could add a line, are refused.
func TestDockerfilePinsBoth(t *testing.T) {
	df, err := Dockerfile(base, "2.1.289")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"FROM " + base + "\n", "@anthropic-ai/claude-code@2.1.289 ",
		`grep -qx '2\.1\.289 (Claude Code)'`, "DISABLE_AUTOUPDATER=1"} {
		if !strings.Contains(df, must) {
			t.Errorf("Dockerfile lacks %q:\n%s", must, df)
		}
	}
	for _, c := range []struct{ base, version string }{
		{"node:22-bookworm-slim", "2.1.289"},
		{base + "\nRUN curl evil", "2.1.289"},
		{base, "latest"},
		{base, "2.1"},
		{base, "2.1.289\nRUN x"},
		{base, "^2.1.289"},
	} {
		if _, err := Dockerfile(c.base, c.version); err == nil {
			t.Errorf("Dockerfile(%q, %q) was accepted", c.base, c.version)
		}
	}
}

// TestTagFollowsTheRecipe: a changed base is a changed tag, so an image built
// from the old recipe is never taken for the new one.
func TestTagFollowsTheRecipe(t *testing.T) {
	a, _ := (&Builder{Base: base, Version: "2.1.289"}).Tag()
	b, _ := (&Builder{Base: strings.Replace(base, "43ac", "43ad", 1), Version: "2.1.289"}).Tag()
	c, _ := (&Builder{Base: base, Version: "2.1.290"}).Tag()
	if a == b || a == c || !strings.HasPrefix(a, "drydock-claude:2.1.289-") {
		t.Errorf("tags %q %q %q", a, b, c)
	}
}

type runner struct {
	cmds    []subproc.Cmd
	stdin   string
	inspect subproc.Result
	inspOut string
	build   subproc.Result
	buildOK string
}

func (r *runner) Run(_ context.Context, c subproc.Cmd) subproc.Result {
	r.cmds = append(r.cmds, c)
	switch c.Args[0] {
	case "image":
		io.WriteString(c.Stdout, r.inspOut)
		return r.inspect
	case "build":
		if c.Stdin != nil {
			b, _ := io.ReadAll(c.Stdin)
			r.stdin = string(b)
		}
		io.WriteString(c.Stdout, r.buildOK)
		return r.build
	}
	return subproc.Result{Err: errors.New("unexpected")}
}

func (r *runner) Start(context.Context, subproc.Cmd) (subproc.Process, error) { return nil, nil }

// TestEnsureBuildsOnlyWhatIsMissing: an image already here is used without a
// build (the control); a missing one is built from the recipe on stdin, with
// no context directory, and its printed ID returned; a failed build is an
// error and never an ID.
func TestEnsureBuildsOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	r := &runner{inspOut: id + "\n"}
	got, err := (&Builder{Run: r, Base: base, Version: "2.1.289"}).Ensure(ctx)
	if err != nil || got != id || len(r.cmds) != 1 {
		t.Errorf("present image: %q, %v after %d commands; want %s after 1", got, err, len(r.cmds), id)
	}

	r = &runner{inspect: subproc.Result{ExitCode: 1}, buildOK: id + "\n"}
	b := &Builder{Run: r, Base: base, Version: "2.1.289"}
	got, err = b.Ensure(ctx)
	if err != nil || got != id {
		t.Fatalf("missing image: %q, %v", got, err)
	}
	tag, _ := b.Tag()
	if a := strings.Join(r.cmds[1].Args, " "); a != "build --quiet --tag "+tag+" -" {
		t.Errorf("build argv = %s", a)
	}
	if want, _ := Dockerfile(base, "2.1.289"); r.stdin != want {
		t.Errorf("build stdin = %q; want the recipe", r.stdin)
	}

	r = &runner{inspect: subproc.Result{ExitCode: 1}, build: subproc.Result{ExitCode: 1}, buildOK: id}
	if got, err := (&Builder{Run: r, Base: base, Version: "2.1.289"}).Ensure(ctx); err == nil {
		t.Errorf("a failed build returned %q", got)
	}
	r = &runner{inspect: subproc.Result{ExitCode: 1}, buildOK: "not an id"}
	if got, err := (&Builder{Run: r, Base: base, Version: "2.1.289"}).Ensure(ctx); err == nil {
		t.Errorf("a build that printed no id returned %q", got)
	}
}
