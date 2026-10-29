package container_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/vscode"
)

// zeros draws 0 every time: every up of a Manager given it names its
// container with the same suffix, as one fixed name per workspace would.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// nameWorkspace is a workspace of this test's own, with a configuration of
// cfg, under root.
func nameWorkspace(t *testing.T, root, id, cfg string) container.UpSpec {
	t.Helper()
	folder := filepath.Join(root, id, "alpha")
	s := container.UpSpec{WorkspaceID: id, RepositoryID: 101, FullName: "krelinga/alpha", Branch: "main",
		Folder: folder, TempDir: container.TempDirFor(folder), ConfigFile: container.ConfigFiles(folder)[0], Rebuild: true}
	s.ConfigDir = filepath.Dir(s.ConfigFile)
	os.MkdirAll(s.TempDir, 0o700)
	os.MkdirAll(s.ConfigDir, 0o700)
	if err := os.WriteFile(s.ConfigFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func nameOf(t *testing.T, id string) string {
	t.Helper()
	return strings.TrimPrefix(docker(t, "inspect", "--format", "{{.Name}}", id), "/")
}

// TestWorkspaceContainersAreNamed measures the name the docker guard gives a
// workspace's container (design §6, "Opening a workspace in VS Code"), with
// the real CLI and Docker: <prefix>-<repo>-<last six of the id>-<suffix>,
// which the VS Code link carries. A rebuild names its container afresh, while
// ${devcontainerId} — the volume a configuration names with it — stays what
// it was, since the CLI never sees the name; a start reattaches to the
// container it had, name and all. A repository's own --name, approved as
// runArgs, in either of docker's forms, is the name the container gets; the
// control, the same configuration unapproved, is refused by the guard and
// creates nothing.
func TestWorkspaceContainersAreNamed(t *testing.T) {
	needDevcontainer(t)
	pullImage(t, image)
	ctx := context.Background()
	p := prefix(t)
	m := manager(p)
	root, err := os.MkdirTemp("", "ddname")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	up := func(s container.UpSpec) string {
		t.Helper()
		c, stderr, err := m.Up(ctx, s)
		if err != nil || c.Outcome != classify.ContainerRunning {
			t.Fatalf("up %s: %+v %v\n%s", s.WorkspaceID, c, err, stderr)
		}
		return c.ContainerID
	}

	s := nameWorkspace(t, root, "01JNAMED0000000000000000A1",
		`{"image":"`+image+`","mounts":["source=ddname-${devcontainerId},target=/data,type=volume"]}`)
	volumeOf := func(id string) string {
		return docker(t, "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}`, id)
	}
	shape := regexp.MustCompile(`^` + regexp.QuoteMeta(p) + `-alpha-0000a1-[0-9a-hjkmnp-tv-z]{4}$`)
	id1 := up(s)
	vol := volumeOf(id1)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	n1 := nameOf(t, id1)
	if !shape.MatchString(n1) {
		t.Fatalf("the container is named %q", n1)
	}
	h, _ := config.ParseSSHHost("owner@devbox")
	if _, err := vscode.AttachURL(h, "/"+n1, "/workspaces/alpha"); err != nil {
		t.Errorf("the link refuses the name %q: %v", n1, err)
	}

	id2 := up(s)
	n2 := nameOf(t, id2)
	if id2 == id1 || n2 == n1 || !shape.MatchString(n2) {
		t.Errorf("a rebuild: %.12s named %q, after %.12s named %q", id2, n2, id1, n1)
	}
	if got := volumeOf(id2); got != vol {
		t.Errorf("the name moved ${devcontainerId}: volume %q, was %q", got, vol)
	}
	docker(t, "stop", id2)
	s.Rebuild = false
	if got := up(s); got != id2 || nameOf(t, got) != n2 {
		t.Errorf("a start made %.12s named %q, beside %.12s named %q", got, nameOf(t, got), id2, n2)
	}

	// A repository's own name: kept, as it was before the guard named
	// anything. Docker keeps the last --name, and the repository's runArgs
	// come after the guard's.
	var b [4]byte
	rand.Read(b[:])
	for i, theirs := range [][]string{{"--name", "ddname-their-" + hex.EncodeToString(b[:])}, {"--name=ddname-their-eq-" + hex.EncodeToString(b[:])}} {
		raw, _ := json.Marshal(theirs)
		want := strings.TrimPrefix(theirs[len(theirs)-1], "--name=")
		r := nameWorkspace(t, root, "01JNAMED0000000000000000B"+strconv.Itoa(i),
			`{"image":"`+image+`","runArgs":`+string(raw)+`}`)
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", want).Run() })
		_, _, err := m.Up(ctx, r)
		var refused *container.GuardRefusal
		if !errors.As(err, &refused) || len(refused.Settings) != 1 || refused.Settings[0] != "runArgs" {
			t.Errorf("control, %q unapproved: %v", theirs, err)
		}
		if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+p+".workspace="+r.WorkspaceID).Output(); len(strings.TrimSpace(string(out))) > 0 {
			t.Errorf("control, %q unapproved: a container was made", theirs)
		}
		r.Approved = []container.HostSetting{{Field: "runArgs", Source: "repository", Value: raw}}
		if got := nameOf(t, up(r)); got != want {
			t.Errorf("approved %q: the container is named %q", theirs, got)
		}
	}
}

// TestAFixedNameWouldConflict is why each up names its container afresh. A
// rebuild runs `up --remove-existing-container`, and the CLI removes only the
// first container its label lookup lists — so when a second container of the
// workspace stands (a cut-off up's create that landed after the lookup,
// which the CLI lists first, as the newer), the one it keeps still holds its
// name. Here a Manager whose every name is the same (zeros), the control,
// fails that rebuild on the name; the real one, drawing a new suffix per up,
// rebuilds beside it. Beside it: the older container still stands after,
// as it did before containers were named — the suffix stops the rebuild
// failing, and removes nothing (design §6).
func TestAFixedNameWouldConflict(t *testing.T) {
	needDevcontainer(t)
	pullImage(t, image)
	ctx := context.Background()
	p := prefix(t)
	root, err := os.MkdirTemp("", "ddname")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	s := nameWorkspace(t, root, "01JNAMED0000000000000000C1", `{"image":"`+image+`"}`)
	leftover := func() {
		t.Helper()
		docker(t, "create", "-l", p+".workspace="+s.WorkspaceID, "-l", p+".repository-id=101",
			"-l", p+".repo="+s.FullName, "-l", p+".branch="+s.Branch, image, "true")
	}

	fixed := manager(p)
	fixed.Random = zeros{}
	c, stderr, err := fixed.Up(ctx, s)
	if err != nil || c.Outcome != classify.ContainerRunning {
		t.Fatalf("first up: %+v %v\n%s", c, err, stderr)
	}
	leftover()
	c, stderr, err = fixed.Up(ctx, s)
	if err == nil && c.Outcome == classify.ContainerRunning {
		t.Fatal("control: a rebuild with one fixed name per workspace succeeded beside the workspace's other container")
	}
	if !strings.Contains(string(stderr), "is already in use") {
		t.Errorf("control: the rebuild failed, but not on the name:\n%s", stderr)
	}

	leftover()
	real := manager(p)
	c, stderr, err = real.Up(ctx, s)
	if err != nil || c.Outcome != classify.ContainerRunning {
		t.Fatalf("a rebuild naming its container afresh: %+v %v\n%s", c, err, stderr)
	}
}
