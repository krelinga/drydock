// Package stack is the client tier's server (vscode-extension §12.4): the
// real Drydock — server.New, Serve, its two Unix sockets — against the fake
// GitHub App (githubtest, which needs a testing.TB, hence a test), with the
// devcontainer CLI and docker replaced by fake executables first on PATH.
// subproc.Exec resolves both by name, so every copy of the container manager
// (the provisioner's, the VS Code linker's, the reconciler's) runs them, and
// what is tested is the argv Drydock really builds. The fakes are this test
// binary itself, run through a wrapper script that names which one it is
// (TestMain).
//
// TestServeStack is skipped unless DRYDOCK_EXTENSION_STACK names a config
// file (the harness writes it: test/harness/stack.ts). It serves until
// SIGTERM, then returns, so t.Cleanup shuts the server and the fake GitHub
// down. Run it with `go test -timeout 0`.
//
// It does not test the docker guard: --docker-path names this binary, which
// the fake devcontainer never runs. The guard is the container tier's.
package stack

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// Config is what the harness hands the stack.
type Config struct {
	// Root holds everything mutable: the database, the workspace root, the
	// fakes' state, the sockets.
	Root          string `json:"root"`
	UIOrigin      string `json:"ui_origin"`
	UIHost        string `json:"ui_host"`
	PreviewDomain string `json:"preview_domain"`
	APISocket     string `json:"api_socket"`
	PreviewSocket string `json:"preview_socket"`
	// Password is set as the operator's, so the harness can sign in to mint
	// tokens through the real routes.
	Password string `json:"password"`
	// Ready is written once the server is serving.
	Ready string `json:"ready"`
}

// The seeded repositories, under an installation account no real owner has.
const (
	Account   = "drydock-fixture"
	PlainID   = 101
	PlainName = Account + "/plain"
	DindID    = 102
	DindName  = Account + "/dind"
	// Obviously not a real App's id: the fake GitHub accepts any.
	AppID      = 999000001
	SSHHost    = "devbox"
	dindMarker = ".devcontainer/dind"
)

func TestMain(m *testing.M) {
	switch os.Getenv("DRYDOCK_STACK_FAKE") {
	case "docker":
		os.Exit(fakeDocker(os.Getenv("DRYDOCK_STACK_STATE"), os.Args[1:]))
	case "devcontainer":
		os.Exit(fakeDevcontainer(os.Getenv("DRYDOCK_STACK_STATE"), os.Args[1:]))
	}
	os.Exit(m.Run())
}

func readConfig(t *testing.T, path string) Config {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// installFakes writes the two wrapper scripts and puts their directory first
// on PATH. Each runs this test binary with DRYDOCK_STACK_FAKE naming it, so
// the fakes are Go, not shell, and need no build of their own.
func installFakes(t *testing.T, root string) (state string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "fakebin")
	state = filepath.Join(root, "fakestate")
	for _, d := range []string{bin, filepath.Join(state, "containers")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"docker", "devcontainer"} {
		script := fmt.Sprintf("#!/bin/sh\nDRYDOCK_STACK_FAKE=%s DRYDOCK_STACK_STATE='%s' exec '%s' \"$@\"\n", name, state, self)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

func stackConfig(t *testing.T, c Config, f *githubtest.Fake) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DiskLimitPercent = 100 // not a test of the disk
	cfg.UIOrigin, cfg.UIHost, cfg.PreviewDomain = c.UIOrigin, c.UIHost, c.PreviewDomain
	cfg.DatabasePath = filepath.Join(c.Root, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = c.APISocket, c.PreviewSocket
	cfg.SocketGroup = groupName(t)
	short, err := os.MkdirTemp("", "ddx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	cfg.BrokerDir = filepath.Join(short, "sock") // a socket path must fit 107 bytes
	// Its own label namespace, so reconciliation can never adopt or delete a
	// container another Drydock owns (testing §5.4).
	cfg.LabelPrefix = "test.extension." + hexID(4)
	cfg.WorkspaceRoot = filepath.Join(c.Root, "ws")
	cfg.ClaudeVolume = "drydock-extension-stack-no-volume"
	cfg.VSCodeSSHHost = SSHHost
	cfg.Version = "dev"
	keyPath := filepath.Join(c.Root, "app.pem")
	if err := os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = AppID, keyPath, f.URL
	// Secrets work, so a bearer stream can be shown to carry none of theirs.
	secrets := filepath.Join(c.Root, "secrets.key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secrets, key, 0o400); err != nil {
		t.Fatal(err)
	}
	cfg.SecretsKey = secrets
	return cfg
}

// seed is the fake GitHub's one installation: a plain repository, and one
// whose configuration asks for privileged, as docker-in-docker does.
func seed(f *githubtest.Fake) {
	f.Installations = []githubtest.Installation{{ID: 77, Account: Account, Repos: []githubtest.Repo{
		{ID: PlainID, FullName: PlainName, DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{".devcontainer/devcontainer.json"}},
		{ID: DindID, FullName: DindName, DefaultBranch: "main", PushedAt: time.Now().Add(-time.Hour),
			Files: []string{".devcontainer/devcontainer.json", dindMarker}},
	}}}
}

// TestServeStack serves the stack until SIGTERM (see the package comment).
func TestServeStack(t *testing.T) {
	path := os.Getenv("DRYDOCK_EXTENSION_STACK")
	if path == "" {
		t.Skip("DRYDOCK_EXTENSION_STACK is unset: the client tier's harness runs this (test/extension/run.sh)")
	}
	c := readConfig(t, path)
	installFakes(t, c.Root)
	f := githubtest.New(t, AppID, time.Now)
	seed(f)
	f.EnableGit(t)
	srv := start(t, stackConfig(t, c, f), f)
	if err := srv.Auth.SetPassword(context.Background(), c.Password); err != nil {
		t.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	if err := os.WriteFile(c.Ready, []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("stack serving at %s (api socket %s)", c.UIOrigin, c.APISocket)
	<-stop
	t.Log("stack stopping")
}

func start(t *testing.T, cfg config.Config, f *githubtest.Fake) *server.Server {
	t.Helper()
	srv, err := server.New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	// Cloner is a pointer, so this reaches the provisioner's (§12.4).
	srv.Provisioner.Cloner.BaseURL = f.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Logf("serve: %v", err)
		}
	})
	return srv
}
