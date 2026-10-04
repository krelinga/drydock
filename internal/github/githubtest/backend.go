package githubtest

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/sys"
)

// The testbed: two repositories that the dev App (krelinga-drydock-dev) is
// installed on and nothing else uses. The contract tests run against these
// twice — against the fake, which carries this description of them, and
// against GitHub, which carries the real thing — so a belief the fake encodes
// wrongly fails against GitHub by name (testing §6.1).
//
// Changing a testbed repository is changing a fixture: update this file in
// the same breath, or the live run fails.
const (
	TestbedAccount = "krelinga"
	TestbedA       = "krelinga/drydock-testbed-a" // declares a dev container
	TestbedB       = "krelinga/drydock-testbed-b" // does not
)

// Testbed is the fake's copy of the two repositories, as of their creation.
// The IDs are GitHub's real ones, for realism only: tests look repositories
// up by name and never hardcode an ID.
func Testbed() Installation {
	return Installation{ID: 1, Account: TestbedAccount, AccountType: "User", Repos: []Repo{
		{ID: 1404825194, FullName: TestbedA, DefaultBranch: "main", Private: true,
			PushedAt: time.Date(2026, 10, 4, 20, 48, 22, 0, time.UTC), Files: []string{
				".devcontainer/devcontainer.json", ".github/copilot-instructions.md", ".github/dependabot.yml",
				".github/workflows/release.yaml", ".gitignore", ".vscode/settings.json", "LICENSE",
			}},
		{ID: 1404825533, FullName: TestbedB, DefaultBranch: "main", Private: true,
			PushedAt: time.Date(2026, 10, 4, 20, 48, 50, 0, time.UTC), Files: []string{"README.md"}},
	}}
}

// The live run is configured by environment, which only the test process
// sees — never Drydock: the App key's own rule (§13.5) is about the server.
const (
	EnvLiveAppID   = "DRYDOCK_GITHUB_LIVE_APP_ID"
	EnvLiveAppKey  = "DRYDOCK_GITHUB_LIVE_APP_KEY" // a path to the .pem, not the key
	EnvRequireLive = "DRYDOCK_REQUIRE_GITHUB_LIVE"
)

// Backend is where a contract test sends its requests.
type Backend struct {
	Client *github.Client
	// Live is true against GitHub itself; Fake is nil then.
	Live bool
	Fake *Fake
}

// NewBackend returns the dev App against GitHub when EnvLiveAppID and
// EnvLiveAppKey are set, and otherwise the fake carrying the testbed. A test
// written once against Backend therefore runs on every `go test` and, in the
// live job, against the real thing. EnvRequireLive turns a missing live
// configuration into a failure, so the live job cannot quietly test the fake.
func NewBackend(t *testing.T) Backend {
	t.Helper()
	idStr, keyPath := os.Getenv(EnvLiveAppID), os.Getenv(EnvLiveAppKey)
	if idStr == "" || keyPath == "" {
		if os.Getenv(EnvRequireLive) != "" {
			t.Fatalf("%s is set but %s and %s are not: the live job would be testing the fake", EnvRequireLive, EnvLiveAppID, EnvLiveAppKey)
		}
		clock := sys.RealClock{}
		f := New(t, 4242, clock.Now)
		f.Installations = []Installation{Testbed()}
		key, err := github.ParseKey(KeyPEM(t))
		if err != nil {
			t.Fatal(err)
		}
		return Backend{Fake: f, Client: &github.Client{AppID: 4242, Key: key, BaseURL: f.URL, Clock: clock}}
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", EnvLiveAppID, idStr, err)
	}
	key, err := github.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return Backend{Live: true, Client: &github.Client{AppID: id, Key: key, Clock: sys.RealClock{}}}
}
