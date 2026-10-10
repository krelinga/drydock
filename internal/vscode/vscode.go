// Package vscode builds the workspace card's *Open in VS Code* link: a
// vscode:// URL that opens the operator's own VS Code attached to a
// workspace's container, on the Drydock host, over the Remote-SSH connection
// VS Code already makes to it (design §6, "Opening a workspace in VS Code").
//
// # The URL
//
//	vscode://vscode-remote/attached-container+<hex of {"containerName":"/<name>"}>@ssh-remote+<host><folder>
//
// VS Code's URL handler (src/vs/code/electron-main/app.ts,
// getWindowOpenableFromProtocolUrl) turns vscode://vscode-remote/<authority>/<path>
// into the remote folder vscode-remote://<authority>/<path>, after asking the
// user to confirm. The authority is the Dev Containers extension's (read from
// ms-vscode-remote.remote-containers 0.470.0): the attached-container resolver
// splits it as `name+config@parentAuthority`, decodes config from hex, and,
// when it is JSON, reads containerName; the parent authority is the
// connection the extension runs docker through. That is the authority the
// extension itself builds when *Attach to Running Container* is used from a
// Remote-SSH window — {"containerName": docker's name, leading "/" kept},
// hex, then "@" and the window's own authority — so the link is the
// operator's manual route, written down. (The extension also takes a
// settings.host of "ssh://user@host" in the JSON, with no parent authority:
// that form opens its own ssh connection and installs its own server, outside
// Remote-SSH's configuration, and is not used here.)
//
// The SSH authority is Remote-SSH's: ssh-remote+<host> for a bare lowercase
// host, and otherwise ssh-remote+<hex of {"hostName","user","port"}>, which
// is how the extension writes one with a user, a port or capitals (its kk
// function), since user@host cannot sit in a URI authority after another @.
//
// # What it carries
//
// The container's name, the SSH host, and the folder: no token, no secret,
// nothing a repository chose but the folder, which is percent-encoded per
// segment. The JSON is marshalled, never spliced, and hex-encoded, and the
// name is held to docker's own pattern — so nothing in a name or a path can
// reach the authority. The SSH host is configuration (`serve
// --vscode-ssh-host`), held by config.ParseSSHHost to [user@]host[:port] with no
// character that could end the authority or read as an ssh option.
//
// # Where the parts come from
//
// Docker is the truth: the name and the folder are read by label each time a
// view is served (Linker.Fill), never stored — a rebuild renames the
// container. The folder is the destination of the clone's bind mount, which is
// the workspace folder the CLI reports for every configuration that does not
// set workspaceFolder (measured: /workspaces/<basename>); one that sets it
// inside its mount opens at the mount. Only a running container gets a link:
// attaching to a stopped one would start it outside Drydock, with no broker.
package vscode

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/workspace"
)

// Authority is Remote-SSH's remote authority for h, as the Dev Containers
// extension writes one.
func Authority(h config.SSHHost) string {
	if h.User == "" && h.Port == 0 && h.Host == strings.ToLower(h.Host) {
		return "ssh-remote+" + h.Host
	}
	b, _ := json.Marshal(struct {
		HostName string `json:"hostName"`
		User     string `json:"user,omitempty"`
		Port     int    `json:"port,omitempty"`
	}{h.Host, h.User, h.Port})
	return "ssh-remote+" + hex.EncodeToString(b)
}

// containerName is docker's own rule for a name, as inspect reports it.
var containerName = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// AttachURL is the vscode:// URL that opens folder in the container named
// name (docker inspect's .Name, leading "/" included) on h.
func AttachURL(h config.SSHHost, name, folder string) (string, error) {
	if !containerName.MatchString(name) {
		return "", fmt.Errorf("vscode: %q is not a container name", name)
	}
	if !strings.HasPrefix(folder, "/") || path.Clean(folder) != folder {
		return "", fmt.Errorf("vscode: %q is not an absolute, clean path", folder)
	}
	if h.Host == "" {
		return "", errors.New("vscode: no SSH host")
	}
	cfg, err := json.Marshal(struct {
		ContainerName string `json:"containerName"`
	}{name})
	if err != nil {
		return "", err
	}
	var p strings.Builder
	for _, seg := range strings.Split(strings.TrimPrefix(folder, "/"), "/") {
		if seg == "" {
			continue
		}
		p.WriteString("/" + url.PathEscape(seg))
	}
	if p.Len() == 0 {
		p.WriteString("/")
	}
	return "vscode://vscode-remote/attached-container+" + hex.EncodeToString(cfg) + "@" + Authority(h) + p.String(), nil
}

// Containers is what Fill reads: container.Manager.List.
type Containers interface {
	List(ctx context.Context) ([]container.Found, error)
}

// Linker fills each view's vscode field as the view is served.
type Linker struct {
	// Host is the SSH host VS Code reaches this server at; nil when
	// `serve --vscode-ssh-host` was not given, and every view says the link
	// is not configured.
	Host       *config.SSHHost
	Containers Containers
	// Root is the workspace root: a workspace's clone is <Root>/<id>/repo.
	Root string
	// Logf is the service log, told when docker could not be read; nil
	// drops it. The views then carry no link, never an error.
	Logf func(string, ...any)
}

// Fill sets vs[i].VSCode on every view: not configured, or configured with
// the link for a running workspace whose one running container Docker has
// now, and no link otherwise.
func (l Linker) Fill(ctx context.Context, vs []workspace.View) {
	if l.Host == nil {
		for i := range vs {
			vs[i].VSCode = &workspace.VSCodeLink{}
		}
		return
	}
	var found []container.Found
	listed := false
	for i := range vs {
		vs[i].VSCode = &workspace.VSCodeLink{Configured: true}
		if vs[i].State != workspace.Running {
			continue
		}
		if !listed {
			var err error
			if found, err = l.Containers.List(ctx); err != nil {
				found = nil // a partial list is no truth to link from
				if l.Logf != nil {
					l.Logf("drydock: reading containers for the VS Code links: %v", err)
				}
			}
			listed = true
		}
		if u, ok := l.link(vs[i].ID, found); ok {
			vs[i].VSCode.URL = &u
		}
	}
}

// link is the URL for workspace id among found: exactly one running
// container carrying its label, with the clone bind-mounted.
func (l Linker) link(id string, found []container.Found) (string, bool) {
	var c *container.Found
	for i := range found {
		if found[i].WorkspaceID != id || !found[i].Running {
			continue
		}
		if c != nil {
			return "", false // two: which one is not Drydock's to guess
		}
		c = &found[i]
	}
	if c == nil {
		return "", false
	}
	clone := filepath.Join(l.Root, id, "repo")
	for _, m := range c.Mounts {
		if m.Type == "bind" && m.Source == clone {
			u, err := AttachURL(*l.Host, c.Name, m.Destination)
			if err != nil {
				if l.Logf != nil {
					l.Logf("drydock: workspace %s: no VS Code link: %v", id, err)
				}
				return "", false
			}
			return u, true
		}
	}
	return "", false
}
