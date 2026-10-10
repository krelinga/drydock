package vscode

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

func host(t *testing.T, s string) config.SSHHost {
	t.Helper()
	h, err := config.ParseSSHHost(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The URL as the Dev Containers extension writes it for Attach to Running
// Container from a Remote-SSH window (its NV and kk functions, 0.470.0), as
// golden strings — and each decoded back: VS Code's handler takes the first
// path segment as the authority, the extension splits it name+config@parent
// and reads the hex config as JSON, and Remote-SSH reads a hex host as JSON.
func TestAttachURLIsTheExtensionsOwn(t *testing.T) {
	for _, c := range []struct {
		host, name, folder string
		golden             string
		sshJSON            string // the decoded hex SSH host, "" for a bare one
		path               string
	}{
		{"devbox", "/modest_herschel", "/workspaces/repo",
			"vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f6d6f646573745f686572736368656c227d@ssh-remote+devbox/workspaces/repo",
			"", "/workspaces/repo"},
		{"owner@devbox.lan", "/nifty_johnson", "/workspaces/repo",
			"vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f6e696674795f6a6f686e736f6e227d@ssh-remote+7b22686f73744e616d65223a22646576626f782e6c616e222c2275736572223a226f776e6572227d/workspaces/repo",
			`{"hostName":"devbox.lan","user":"owner"}`, "/workspaces/repo"},
		{"owner@DevBox:2222", "/x", "/work space/a#b",
			"vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f78227d@ssh-remote+7b22686f73744e616d65223a22446576426f78222c2275736572223a226f776e6572222c22706f7274223a323232327d/work%20space/a%23b",
			`{"hostName":"DevBox","user":"owner","port":2222}`, "/work space/a#b"},
	} {
		got, err := AttachURL(host(t, c.host), c.name, c.folder)
		if err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		if got != c.golden {
			t.Errorf("%s:\n got %s\nwant %s", c.host, got, c.golden)
		}
		// Decode it as VS Code does: vscode://vscode-remote/<authority><path>.
		u, err := url.Parse(got)
		if err != nil || u.Scheme != "vscode" || u.Host != "vscode-remote" {
			t.Fatalf("%s: %v %+v", c.host, err, u)
		}
		authority, p, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		if "/"+p != c.path {
			t.Errorf("%s: path %q, want %q", c.host, "/"+p, c.path)
		}
		m := regexp.MustCompile(`^attached-container\+([^@+]+)@(ssh-remote\+.*)$`).FindStringSubmatch(authority)
		if m == nil {
			t.Fatalf("%s: authority %q", c.host, authority)
		}
		cfg, _ := hex.DecodeString(m[1])
		var att map[string]any
		if err := json.Unmarshal(cfg, &att); err != nil || len(att) != 1 || att["containerName"] != c.name {
			t.Errorf("%s: attached-container config %s", c.host, cfg)
		}
		ssh := strings.TrimPrefix(m[2], "ssh-remote+")
		if c.sshJSON == "" {
			if ssh != c.host {
				t.Errorf("%s: ssh authority %q", c.host, m[2])
			}
		} else if b, _ := hex.DecodeString(ssh); string(b) != c.sshJSON {
			t.Errorf("%s: ssh host %s, want %s", c.host, b, c.sshJSON)
		}
	}
}

// Nothing a name or a folder holds reaches the authority: a name outside
// docker's pattern is refused, and a folder is percent-encoded segment by
// segment, so '@', '/', '?' and '#' in it cannot end the authority or the
// path. The control is the plain folder passing.
func TestAttachURLRefusesWhatCouldInject(t *testing.T) {
	h := host(t, "devbox")
	for _, name := range []string{"x", "/", "/-x", `/a"b`, "/a@ssh-remote+evil", "/a b", ""} {
		if _, err := AttachURL(h, name, "/workspaces/repo"); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, folder := range []string{"workspaces/repo", "/a/../b", "/a/", ""} {
		if _, err := AttachURL(h, "/x", folder); err == nil {
			t.Errorf("folder %q accepted", folder)
		}
	}
	u, err := AttachURL(h, "/x", "/w/a@b?c#d")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u, "@ssh-remote+devbox/w/a@b%3Fc%23d") || strings.Count(u, "?")+strings.Count(u, "#") != 0 {
		t.Errorf("folder escaped as %s", u)
	}
	if _, err := AttachURL(config.SSHHost{}, "/x", "/w"); err == nil {
		t.Error("no SSH host accepted")
	}
}

type fakeContainers struct {
	found []container.Found
	err   error
	calls int
}

func (f *fakeContainers) List(context.Context) ([]container.Found, error) {
	f.calls++
	return f.found, f.err
}

// Fill: unconfigured says so on every view and asks Docker nothing; once
// configured, a running workspace with one running container that has its
// clone mounted gets the link, read from Docker on this call — and a stopped
// workspace, a stopped container, two containers, no clone mount, or a Docker
// that cannot be read gets none, never an error.
func TestFill(t *testing.T) {
	const a, b, c, d, e = "01JAAAAAAAAAAAAAAAAAAAAAAA", "01JBBBBBBBBBBBBBBBBBBBBBBB", "01JCCCCCCCCCCCCCCCCCCCCCCC",
		"01JDDDDDDDDDDDDDDDDDDDDDDD", "01JEEEEEEEEEEEEEEEEEEEEEEE"
	root := "/srv/drydock/ws"
	clone := func(id string) []container.Mount {
		return []container.Mount{{Type: "volume", Source: "", Destination: "/home/vscode/.claude"},
			{Type: "bind", Source: filepath.Join(root, id, "repo"), Destination: "/workspaces/repo"}}
	}
	fc := &fakeContainers{found: []container.Found{
		{WorkspaceID: a, Running: true, Name: "/alpha_box", Mounts: clone(a)},
		{WorkspaceID: b, Running: false, Name: "/beta_box", Mounts: clone(b)},
		{WorkspaceID: c, Running: true, Name: "/c1", Mounts: clone(c)},
		{WorkspaceID: c, Running: true, Name: "/c2", Mounts: clone(c)},
		{WorkspaceID: d, Running: true, Name: "/d", Mounts: []container.Mount{{Type: "bind", Source: "/elsewhere", Destination: "/w"}}},
	}}
	views := func() []workspace.View {
		var vs []workspace.View
		for _, id := range []string{a, b, c, d} {
			vs = append(vs, workspace.View{ID: id, State: workspace.Running})
		}
		return append(vs, workspace.View{ID: e, State: workspace.Stopped})
	}

	vs := views()
	Linker{Containers: fc, Root: root}.Fill(context.Background(), vs)
	for _, v := range vs {
		if v.VSCode == nil || v.VSCode.Configured || v.VSCode.URL != nil {
			t.Errorf("unconfigured %s: %+v", v.ID, v.VSCode)
		}
	}
	if fc.calls != 0 {
		t.Errorf("unconfigured, Docker was asked %d times", fc.calls)
	}

	h := host(t, "owner@devbox")
	vs = views()
	Linker{Host: &h, Containers: fc, Root: root}.Fill(context.Background(), vs)
	want, _ := AttachURL(h, "/alpha_box", "/workspaces/repo")
	if vs[0].VSCode == nil || !vs[0].VSCode.Configured || vs[0].VSCode.URL == nil || *vs[0].VSCode.URL != want {
		t.Errorf("the running workspace: %+v", vs[0].VSCode)
	}
	for _, v := range vs[1:] {
		if v.VSCode == nil || !v.VSCode.Configured || v.VSCode.URL != nil {
			t.Errorf("%s: %+v, want configured with no link", v.ID, v.VSCode)
		}
	}
	if fc.calls != 1 {
		t.Errorf("one Fill asked Docker %d times", fc.calls)
	}

	fc.err = errors.New("Cannot connect to the Docker daemon")
	var logged []string
	vs = views()
	Linker{Host: &h, Containers: fc, Root: root, Logf: func(f string, a ...any) { logged = append(logged, f) }}.Fill(context.Background(), vs)
	if vs[0].VSCode == nil || vs[0].VSCode.URL != nil || len(logged) != 1 {
		t.Errorf("Docker unreadable: %+v, logged %v", vs[0].VSCode, logged)
	}
}
