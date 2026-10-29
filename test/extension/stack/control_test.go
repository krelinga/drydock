package stack

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github/githubtest"
)

func groupName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return g.Name
}

// client is a signed-in operator on the API socket, without Caddy: the
// stack's own check of itself, not the client tier.
type client struct {
	t      *testing.T
	http   *http.Client
	origin string
	cookie string
}

func (c *client) do(method, path, body string) (int, string) {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://drydock.test"+path, r)
	req.Host = "drydock.test"
	if method != "GET" {
		req.Header.Set("Origin", c.origin)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: c.cookie})
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 204 && method == "POST" && path == "/api/auth/session" {
		for _, ck := range resp.Cookies() {
			if ck.Name == "__Host-drydock" {
				c.cookie = ck.Value
			}
		}
	}
	return resp.StatusCode, string(b)
}

type wsView struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	FullName string `json:"full_name"`
	Approval *struct {
		Hash  string `json:"hash"`
		Added []struct {
			Field string `json:"field"`
		} `json:"added"`
	} `json:"approval"`
	VSCode *struct {
		Configured bool    `json:"configured"`
		URL        *string `json:"url"`
	} `json:"vscode"`
}

// TestStackControl is the stack's positive control (vscode-extension §12.4),
// run by the ordinary go test with no Caddy and no Node: through the real
// server and the fakes, the catalog lists the seeded repositories, a clone
// of the plain one reaches running with a VS Code link of the shape the
// clients accept, and the docker-in-docker one stops for approval naming
// privileged — then, approved, runs too. The fakes' argv logs show what
// Drydock really ran.
func TestStackControl(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	short, err := os.MkdirTemp("", "ddxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	c := Config{
		Root: root, UIOrigin: "https://drydock.test:8443", UIHost: "drydock.test", PreviewDomain: "drydock-preview.test",
		APISocket: filepath.Join(short, "http.sock"), PreviewSocket: filepath.Join(short, "preview.sock"),
		Password: "stack-control-password",
	}
	state := installFakes(t, root)
	t.Setenv("DRYDOCK_STACK_REPO", repoRoot())
	f := githubtest.New(t, AppID, time.Now)
	seed(f)
	f.EnableGit(t)
	srv := start(t, stackConfig(t, c, f), f)
	if err := srv.Auth.SetPassword(context.Background(), c.Password); err != nil {
		t.Fatal(err)
	}
	cl := &client{t: t, origin: c.UIOrigin, http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.APISocket)
		}}}}
	if code, body := cl.do("POST", "/api/auth/session", fmt.Sprintf(`{"password":%q}`, c.Password)); code != 204 {
		t.Fatalf("sign in: %d %s", code, body)
	}

	eventually(t, "the catalog lists both seeded repositories", func() bool {
		_, body := cl.do("GET", "/api/repos", "")
		return strings.Contains(body, PlainName) && strings.Contains(body, DindName)
	})

	view := func(id string) (v wsView) {
		code, body := cl.do("GET", "/api/workspaces/"+id, "")
		if code != 200 {
			t.Fatalf("view %s: %d %s", id, code, body)
		}
		json.Unmarshal([]byte(body), &v)
		return v
	}
	create := func(repo int) string {
		code, body := cl.do("POST", "/api/workspaces", fmt.Sprintf(`{"repository_id":%d}`, repo))
		var id struct{ ID string }
		if code != 202 || json.Unmarshal([]byte(body), &id) != nil {
			t.Fatalf("create %d: %d %s", repo, code, body)
		}
		return id.ID
	}

	// The plain repository runs, and its view carries a link (§2.6).
	plain := create(PlainID)
	var v wsView
	eventually(t, "the plain clone is running with a link", func() bool {
		v = view(plain)
		if v.State == "failed" {
			t.Fatalf("the plain clone failed: %+v", v)
		}
		return v.State == "running" && v.VSCode != nil && v.VSCode.URL != nil
	})
	link := regexp.MustCompile(`^vscode://vscode-remote/attached-container\+[0-9a-f]+@ssh-remote\+devbox/workspaces/plain$`)
	if !link.MatchString(*v.VSCode.URL) {
		t.Errorf("link %q", *v.VSCode.URL)
	}

	// The docker-in-docker one stops for approval, naming privileged, before
	// any up; approved by that hash, it runs.
	dind := create(DindID)
	eventually(t, "the dind clone stops for approval", func() bool {
		v = view(dind)
		return v.State == "stopped" && v.Approval != nil
	})
	if len(v.Approval.Added) == 0 || v.Approval.Added[0].Field != "privileged" {
		t.Errorf("the request: %+v", v.Approval)
	}
	if ups := countArgv(t, state, "devcontainer", dindFolderIn(t, root, dind), "up"); ups != 0 {
		t.Errorf("%d ups before the approval", ups)
	}
	if code, body := cl.do("POST", "/api/workspaces/"+dind+"/config-approval", fmt.Sprintf(`{"hash":%q}`, v.Approval.Hash)); code != 202 {
		t.Fatalf("approve: %d %s", code, body)
	}
	eventually(t, "the approved dind clone runs", func() bool {
		v = view(dind)
		return v.State == "running"
	})
	if ups := countArgv(t, state, "devcontainer", dindFolderIn(t, root, dind), "up"); ups != 1 {
		t.Errorf("%d ups after the approval, want 1", ups)
	}
}

func dindFolderIn(t *testing.T, root, id string) string {
	return filepath.Join(root, "ws", id, "dind")
}

// countArgv counts the fake's invocations whose first argument is verb and
// which name folder.
func countArgv(t *testing.T, state, name, folder, verb string) int {
	t.Helper()
	f, err := os.Open(filepath.Join(state, name+"-argv.log"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var args []string
		if json.Unmarshal(sc.Bytes(), &args) != nil || len(args) == 0 || args[0] != verb {
			continue
		}
		if flag(args, "--workspace-folder") == folder {
			n++
		}
	}
	return n
}

// eventually polls ok for up to two minutes: a clone to running takes well
// under a second on a quiet machine and about thirty under -race on a loaded
// one, so the budget is generous rather than tuned.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
