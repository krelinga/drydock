package broker

// The in-container clients are shell scripts, tested here by running them
// against a real broker socket (testing §6.6): no shell test framework, and
// the assertions sit beside the broker's own.

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binDir, _ = filepath.Abs(filepath.Join("..", "..", "feature", "src", "drydock", "bin"))

type run struct {
	stdout, stderr string
	code           int
}

// client runs one of the scripts with the workspace's socket, the scripts'
// own directory first on PATH, and the given transport.
func (e *env) client(t *testing.T, ws, transport, stdin string, path string, args ...string) run {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, args[0]), args[1:]...)
	cmd.Env = []string{
		"PATH=" + binDir + ":" + path,
		"DRYDOCK_BROKER_SOCK=" + e.b.SocketPath(ws),
		"DRYDOCK_BROKER_TRANSPORT=" + transport,
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return run{out.String(), errb.String(), code}
}

var transports = []string{"socat", "nc"}

func TestCredentialHelper(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			e := newEnv(t)
			r := e.client(t, wsA, tr, "protocol=https\nhost=github.com\n\n", os.Getenv("PATH"), "drydock-credential", "get")
			issued := e.fake.IssuedTokens()
			if r.code != 0 || len(issued) != 1 {
				t.Fatalf("get: %+v, %d issued", r, len(issued))
			}
			// Exactly two lines, and the password is the token the broker
			// minted for this workspace.
			if want := "username=x-access-token\npassword=" + issued[0] + "\n"; r.stdout != want || r.stderr != "" {
				t.Errorf("get printed %q (stderr %q)", r.stdout, r.stderr)
			}
		})
	}
}

// store and erase succeed silently: a helper that fails them makes git warn
// on every push. And another host gets nothing — the token is github.com's.
func TestCredentialHelperOtherOperations(t *testing.T) {
	e := newEnv(t)
	for _, op := range []string{"store", "erase"} {
		r := e.client(t, wsA, "socat", "protocol=https\nhost=github.com\nusername=x\npassword=y\n\n", os.Getenv("PATH"), "drydock-credential", op)
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Errorf("%s: %+v", op, r)
		}
	}
	r := e.client(t, wsA, "socat", "protocol=https\nhost=evil.example\n\n", os.Getenv("PATH"), "drydock-credential", "get")
	if r.code != 0 || r.stdout != "" {
		t.Errorf("another host: %+v", r)
	}
	if e.fake.Count("POST") != 0 {
		t.Error("store, erase or a foreign host minted a token")
	}
}

// The broker unreachable is "GitHub access unavailable", with a non-zero
// exit — never a git authentication error, never an empty password.
func TestClientsSayAccessIsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.b.Close(wsA)
	r := e.client(t, wsA, "socat", "host=github.com\n\n", os.Getenv("PATH"), "drydock-credential", "get")
	if r.code == 0 || r.stdout != "" || !strings.Contains(r.stderr, "GitHub access unavailable") {
		t.Errorf("socket gone: %+v", r)
	}
	// A refusal names its reason.
	e.db.ExecContext(t.Context(), `UPDATE repository SET archived = 1 WHERE id = 202`)
	r = e.client(t, wsB, "nc", "host=github.com\n\n", os.Getenv("PATH"), "drydock-credential", "get")
	if r.code == 0 || r.stdout != "" || !strings.Contains(r.stderr, "GitHub access unavailable (repo_archived)") {
		t.Errorf("archived: %+v", r)
	}
}

// An App without actions:write refuses the gh scope. The shim says what to
// check, not "revoked", and the real gh never runs; git's scope, which does
// not ask for actions, still works through the helper (the control).
func TestClientsNameAMissingAppPermission(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			e := newEnv(t)
			e.fake.Mu.Lock()
			delete(e.fake.AppPermissions, "actions")
			e.fake.Mu.Unlock()
			realDir := t.TempDir()
			os.WriteFile(filepath.Join(realDir, "gh"), []byte("#!/bin/sh\necho ran\n"), 0o755)
			r := e.client(t, wsA, tr, "", realDir+":"+os.Getenv("PATH"), "gh", "repo", "view")
			want := "drydock: GitHub access unavailable (the GitHub App lacks a permission; see the workspace's events in Drydock)\n"
			if r.code == 0 || r.stdout != "" || r.stderr != want {
				t.Errorf("gh without actions:write: %+v; want stderr %q", r, want)
			}
			r = e.client(t, wsA, tr, "protocol=https\nhost=github.com\n\n", os.Getenv("PATH"), "drydock-credential", "get")
			if r.code != 0 || !strings.Contains(r.stdout, "password=ghs_") {
				t.Errorf("control: git's scope through the helper: %+v", r)
			}
		})
	}
}

// A reason this copy of the client does not know — a newer server's — is
// named as it came, with exit 69: an older Feature fails cleanly.
func TestClientsNameAnUnknownReason(t *testing.T) {
	sock := shortDir(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadString('\n')
			c.Write([]byte("ERR reason=some_future_reason\n"))
			c.Close()
		}
	}()
	for _, tr := range transports {
		cmd := exec.Command(filepath.Join(binDir, "drydock-broker"), "GET-TOKEN", "scope=git")
		cmd.Env = []string{"PATH=" + binDir + ":" + os.Getenv("PATH"), "DRYDOCK_BROKER_SOCK=" + sock, "DRYDOCK_BROKER_TRANSPORT=" + tr}
		var errb bytes.Buffer
		cmd.Stderr = &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 69 || errb.String() != "drydock: GitHub access unavailable (some_future_reason)\n" {
			t.Errorf("%s: %v, stderr %q", tr, err, errb.String())
		}
	}
}

// The gh shim fetches a token per invocation and execs the real gh with it
// in the environment, never in argv (testing §6.6).
func TestGHShim(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			e := newEnv(t)
			// The "real" gh: reports its argv and its GH_TOKEN.
			realDir := t.TempDir()
			os.WriteFile(filepath.Join(realDir, "gh"), []byte("#!/bin/sh\necho \"argv=$*\"\necho \"token=$GH_TOKEN\"\n"), 0o755)
			path := realDir + ":" + os.Getenv("PATH")
			r := e.client(t, wsA, tr, "", path, "gh", "pr", "list", "--repo", "x/y")
			issued := e.fake.IssuedTokens()
			if r.code != 0 || len(issued) != 1 {
				t.Fatalf("%+v, %d issued", r, len(issued))
			}
			if r.stdout != "argv=pr list --repo x/y\ntoken="+issued[0]+"\n" {
				t.Errorf("the real gh saw %q", r.stdout)
			}
			if strings.Contains(strings.SplitN(r.stdout, "\n", 2)[0], issued[0]) {
				t.Error("the token reached gh's argv")
			}
			// Per invocation, served from the broker's cache.
			e.client(t, wsA, tr, "", path, "gh", "api", "user")
			if n := e.fake.Count("POST"); n != 1 {
				t.Errorf("two gh calls made %d mints; want 1 (cached)", n)
			}
			// The gh scope, not git's.
			e.fake.Mu.Lock()
			perms := e.fake.TokenRequests[0].Permissions
			e.fake.Mu.Unlock()
			if perms["pull_requests"] != "write" {
				t.Errorf("the shim asked for %v", perms)
			}
		})
	}
}

// The shim finds the real gh past itself on PATH, and says so plainly when
// there is none, rather than exec'ing itself forever.
func TestGHShimWithoutARealGH(t *testing.T) {
	e := newEnv(t)
	r := e.client(t, wsA, "socat", "", "/usr/bin:/bin", "gh", "version")
	if _, err := exec.LookPath("gh"); err == nil {
		// The devcontainer has gh in /usr/bin; point PATH somewhere without.
		empty := t.TempDir()
		for _, tool := range []string{"sh", "dirname", "socat", "nc", "cat"} {
			if p, err := exec.LookPath(tool); err == nil {
				os.Symlink(p, filepath.Join(empty, tool))
			}
		}
		r = e.client(t, wsA, "socat", "", empty, "gh", "version")
	}
	if r.code != 127 || !strings.Contains(r.stderr, "gh is not installed") {
		t.Errorf("no real gh: %+v", r)
	}
}
