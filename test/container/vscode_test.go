package container_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/vscode"
)

// TestReopenInContainerFindsDrydocksContainer is the measurement behind the
// spec's two labels (design §6, "Opening a workspace in VS Code"), with the
// real CLI and Docker. Drydock's up — through the guard, policy and all —
// labels the container devcontainer.local_folder and devcontainer.config_file
// as well as by its prefix; then `devcontainer up --workspace-folder <clone>`
// with no id-labels, which is what VS Code's Reopen in Container computes,
// finds that container and reports it, and no second container appears. The
// same holds for a repository with no configuration of its own once one is
// added at the default path, and the link the card carries names the real
// container and its workspace folder.
//
// The control is the up Drydock made before the labels: there the same
// command builds a second, Drydock-unaware container. A start (not a
// rebuild) of either container reattaches to it rather than making another,
// since the labels are the guard's and not id-labels; and a pre-label
// container rebuilt gets them with its ${devcontainerId} unchanged — the
// volume a configuration names with it is the same volume.
//
// Every container here carries this test's own prefix, and each
// devcontainer.local_folder is under this test's own temporary directory, so
// neither another test nor another Drydock can be found by it.
func TestReopenInContainerFindsDrydocksContainer(t *testing.T) {
	needDevcontainer(t)
	pullImage(t, image)
	ctx := context.Background()
	p := prefix(t)
	m := manager(p)
	root, err := os.MkdirTemp("", "ddvs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	// The containers a plain `devcontainer up` makes carry no prefix: remove
	// them by the folder label, which only this test's folders carry.
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+container.LabelLocalFolder).Output()
		for _, id := range strings.Fields(string(out)) {
			l, _ := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "`+container.LabelLocalFolder+`"}}`, id).Output()
			if strings.HasPrefix(strings.TrimSpace(string(l)), root+"/") {
				exec.Command("docker", "rm", "-f", id).Run()
			}
		}
	})
	cfg := `{"image":"` + image + `"}`
	workspace := func(id string, withConfig bool) container.UpSpec {
		folder := filepath.Join(root, id, "repo")
		s := container.UpSpec{WorkspaceID: id, RepositoryID: 101, FullName: "krelinga/alpha", Branch: "main",
			Folder: folder, TempDir: container.TempDirFor(folder), ConfigFile: container.ConfigFiles(folder)[0]}
		os.MkdirAll(s.TempDir, 0o700)
		if withConfig {
			os.MkdirAll(filepath.Dir(s.ConfigFile), 0o700)
			os.WriteFile(s.ConfigFile, []byte(cfg), 0o600)
		} else {
			// Drydock's minimal config, beside the clone (§6 step 3).
			s.OverrideConfig = filepath.Join(root, id, ".drydock", "devcontainer.json")
			os.MkdirAll(filepath.Dir(s.OverrideConfig), 0o700)
			os.WriteFile(s.OverrideConfig, []byte(cfg), 0o600)
			os.MkdirAll(folder, 0o700)
		}
		return s
	}
	up := func(s container.UpSpec) string {
		t.Helper()
		c, stderr, err := m.Up(ctx, s)
		if err != nil || c.Outcome != classify.ContainerRunning {
			t.Fatalf("up %s: %+v %v\n%s", s.WorkspaceID, c, err, stderr)
		}
		return c.ContainerID
	}
	// reopen is VS Code's Reopen in Container, as the CLI computes it: the
	// folder, no id-labels, docker unguarded — bounded, as every up here is.
	reopen := func(folder string, extra ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		res := boundedRunner{Inner: subproc.Exec{}}.Run(ctx, subproc.Cmd{Name: "devcontainer",
			Args: append([]string{"up", "--workspace-folder", folder}, extra...), Stdout: &out, Stderr: &stderr})
		var r struct {
			Outcome     string `json:"outcome"`
			ContainerID string `json:"containerId"`
		}
		if json.Unmarshal(out.Bytes(), &r) != nil || r.Outcome != "success" {
			t.Fatalf("devcontainer up --workspace-folder %s: %s %v\n%s", folder, out.Bytes(), res.Err, stderr.Bytes())
		}
		return r.ContainerID
	}
	byFolder := func(folder string) []string {
		out, _ := exec.Command("docker", "ps", "-aq", "--no-trunc", "--filter", "label="+container.LabelLocalFolder+"="+folder).Output()
		return strings.Fields(string(out))
	}
	labels := func(id string) map[string]string {
		out, _ := exec.Command("docker", "inspect", "--format", "{{json .Config.Labels}}", id).Output()
		var l map[string]string
		json.Unmarshal(out, &l)
		return l
	}

	// A repository with a configuration.
	a := workspace("01JVSCDE000000000000000001", true)
	a.Rebuild = true
	id := up(a)
	if l := labels(id); l[container.LabelLocalFolder] != a.Folder || l[container.LabelConfigFile] != a.ConfigFile ||
		l[p+".workspace"] != a.WorkspaceID {
		t.Fatalf("labels %v", l)
	}
	if got := reopen(a.Folder); got != id {
		t.Errorf("Reopen in Container found %.12s, not Drydock's %.12s", got, id)
	}
	if ids := byFolder(a.Folder); len(ids) != 1 {
		t.Errorf("containers for the clone: %v", ids)
	}
	// The card's link names that container, by the name Docker has now, and
	// the folder its clone is mounted at.
	h, _ := config.ParseSSHHost("owner@devbox")
	found, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var name, folder string
	for _, f := range found {
		if f.ContainerID == id {
			name = f.Name
			for _, mt := range f.Mounts {
				if mt.Type == "bind" && mt.Source == a.Folder {
					folder = mt.Destination
				}
			}
		}
	}
	want, err := vscode.AttachURL(h, name, folder)
	if err != nil || folder != "/workspaces/repo" {
		t.Fatalf("name %q folder %q: %v", name, folder, err)
	}
	m2 := regexp.MustCompile(`attached-container\+([0-9a-f]+)@`).FindStringSubmatch(want)
	b, _ := hex.DecodeString(m2[1])
	real, _ := exec.Command("docker", "inspect", "--format", "{{.Name}}", id).Output()
	if string(b) != `{"containerName":"`+strings.TrimSpace(string(real))+`"}` {
		t.Errorf("the link names %s; docker calls it %s", b, real)
	}

	// A start of it, stopped, reattaches.
	exec.Command("docker", "stop", id).Run()
	a.Rebuild = false
	if got := up(a); got != id {
		t.Errorf("a start made %.12s beside %.12s", got, id)
	}

	// A repository with none: Drydock's override, labelled with the default
	// path; once a configuration is added there, Reopen finds Drydock's.
	b2 := workspace("01JVSCDE000000000000000002", false)
	b2.Rebuild = true
	id2 := up(b2)
	if l := labels(id2); l[container.LabelConfigFile] != filepath.Join(b2.Folder, ".devcontainer", "devcontainer.json") {
		t.Errorf("labels %v", l)
	}
	if got := reopen(b2.Folder, "--override-config", b2.OverrideConfig); got != id2 {
		t.Errorf("up with the override found %.12s, not %.12s", got, id2)
	}
	os.MkdirAll(filepath.Join(b2.Folder, ".devcontainer"), 0o700)
	os.WriteFile(filepath.Join(b2.Folder, ".devcontainer", "devcontainer.json"), []byte(cfg), 0o600)
	if got := reopen(b2.Folder); got != id2 {
		t.Errorf("after a config was added, Reopen found %.12s, not %.12s", got, id2)
	}

	// The control: the up Drydock made before the labels (no ConfigFile).
	c := workspace("01JVSCDE000000000000000003", true)
	c.Rebuild, c.ConfigFile = true, ""
	id3 := up(c)
	if _, ok := labels(id3)[container.LabelLocalFolder]; ok {
		t.Fatal("control: the old up labelled the container")
	}
	// Its start, by a Drydock that labels, reattaches: the labels are not
	// id-labels, so up matches it as before (and docker start adds none).
	exec.Command("docker", "stop", id3).Run()
	c.Rebuild, c.ConfigFile = false, container.ConfigFiles(c.Folder)[0]
	if got := up(c); got != id3 {
		t.Errorf("a start of a pre-label container made %.12s beside %.12s", got, id3)
	}
	if got := reopen(c.Folder); got == id3 {
		t.Error("control: Reopen found a container that carries no folder label")
	}
	if ids := byFolder(c.Folder); len(ids) != 1 {
		t.Errorf("control: containers labelled for the clone: %v", ids)
	}

	// A container made before the labels, rebuilt by a Drydock that labels:
	// the new container has them, and ${devcontainerId} is what it was — the
	// configuration's ${devcontainerId} volume is the same volume, so a
	// docker-in-docker /var/lib/docker keeps its contents.
	d := workspace("01JVSCDE000000000000000004", true)
	os.WriteFile(d.ConfigFile, []byte(`{"image":"`+image+`","mounts":["source=ddvs-${devcontainerId},target=/data,type=volume"]}`), 0o600)
	volumeOf := func(id string) string {
		out, _ := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}`, id).Output()
		return strings.TrimSpace(string(out))
	}
	d.Rebuild, d.ConfigFile = true, ""
	id4 := up(d)
	vol := volumeOf(id4)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	if !strings.HasPrefix(vol, "ddvs-") {
		t.Fatalf("the ${devcontainerId} volume: %q", vol)
	}
	d.ConfigFile = container.ConfigFiles(d.Folder)[0]
	id5 := up(d)
	if id5 == id4 {
		t.Fatal("the rebuild kept the container")
	}
	if l := labels(id5); l[container.LabelLocalFolder] != d.Folder || l[container.LabelConfigFile] != d.ConfigFile {
		t.Errorf("rebuilt by a labelling Drydock: labels %v", l)
	}
	if got := volumeOf(id5); got != vol {
		t.Errorf("the rebuild moved ${devcontainerId}: volume %q, was %q", got, vol)
	}
}
