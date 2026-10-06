package github_test

// The contract tests (testing §6.1): what Drydock believes about GitHub,
// asserted against githubtest.NewBackend — the fake on every run, and the
// real dev App in the live CI job. Each test is about GitHub's behaviour, not
// Drydock's branches, and each names the belief it checks, so a live failure
// says which assumption in the fake was wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"testing"

	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
)

func meta() map[string]string     { return map[string]string{"metadata": github.Read} }
func contents() map[string]string { return map[string]string{"contents": github.Read} }

// installation returns the testbed installation, failing unless the App is
// installed exactly where the contract expects.
func installation(t *testing.T, b githubtest.Backend) github.Installation {
	t.Helper()
	ins, err := b.Client.Installations(context.Background())
	if err != nil {
		t.Fatalf("listing installations: %v", err)
	}
	if len(ins) != 1 || ins[0].Account != githubtest.TestbedAccount {
		t.Fatalf("installations %+v; the dev App must be installed on %s alone", ins, githubtest.TestbedAccount)
	}
	return ins[0]
}

// testbed lists the installation's repositories by name.
func testbed(t *testing.T, b githubtest.Backend, in github.Installation) map[string]github.Repository {
	t.Helper()
	tok, err := b.Client.InstallationToken(context.Background(), github.TokenRequest{InstallationID: in.ID, Permissions: meta()})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := b.Client.Repositories(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]github.Repository{}
	for _, r := range repos {
		out[r.FullName] = r
	}
	return out
}

func status(err error) int {
	var ae *github.APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// Belief: GitHub accepts our JWT (RS256, issuer the App ID as a string, iat
// backdated a minute, nine-minute life), and an installation on a user
// account reports type "User" — which picks the settings URL.
func TestContractInstallation(t *testing.T) {
	b := githubtest.NewBackend(t)
	in := installation(t, b)
	if in.AccountType != "User" {
		t.Errorf("account type %q; the settings URL assumes User or Organization", in.AccountType)
	}
	t.Logf("live=%v installation=%d settings=%s", b.Live, in.ID, in.SettingsURL())
}

// Belief: a JWT naming another App is refused with 401 — so a wrong App ID
// in configuration fails loudly at the first refresh.
func TestContractWrongAppIsRefused(t *testing.T) {
	b := githubtest.NewBackend(t)
	wrong := &github.Client{AppID: b.Client.AppID + 1, Key: b.Client.Key, BaseURL: b.Client.BaseURL, Clock: b.Client.Clock}
	if _, err := wrong.Installations(context.Background()); status(err) != http.StatusUnauthorized {
		t.Errorf("a JWT for another App: %v; want 401", err)
	}
}

// Belief: a metadata-only token lists exactly the installation's
// repositories, with their default branch and visibility.
func TestContractListing(t *testing.T) {
	b := githubtest.NewBackend(t)
	repos := testbed(t, b, installation(t, b))
	if len(repos) != 2 {
		t.Fatalf("repositories %v; want exactly the two testbeds", repos)
	}
	for _, name := range []string{githubtest.TestbedA, githubtest.TestbedB} {
		r, ok := repos[name]
		if !ok {
			t.Fatalf("%s is not in the installation", name)
		}
		if r.DefaultBranch != "main" || r.ID == 0 {
			t.Errorf("%s: %+v", name, r)
		}
		// A public testbed proves nothing about scoping: anyone can read a
		// public repository's contents with or without the permission.
		if !r.Private {
			t.Errorf("%s is public; make it private, or the scoping tests below test nothing", name)
		}
	}
}

// Belief: the contents API lists a directory as an array of typed entries,
// returns a file as one object, and answers a missing path 404 — which is
// exactly what the dev-container probe distinguishes.
func TestContractContents(t *testing.T) {
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	in := installation(t, b)
	tok, err := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID, Permissions: contents()})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := b.Client.Contents(ctx, tok, githubtest.TestbedA, ".devcontainer", "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range dir {
		found = found || (e.Name == "devcontainer.json" && e.Type == "file")
	}
	if !found {
		t.Errorf("%s/.devcontainer listed %+v; want devcontainer.json as a file", githubtest.TestbedA, dir)
	}
	file, err := b.Client.Contents(ctx, tok, githubtest.TestbedB, "README.md", "main")
	if err != nil || len(file) != 1 || file[0].Type != "file" || file[0].Name != "README.md" {
		t.Errorf("a file: %+v %v", file, err)
	}
	for _, missing := range []struct{ repo, path string }{
		{githubtest.TestbedB, ".devcontainer"},
		{githubtest.TestbedB, ".devcontainer.json"},
		{githubtest.TestbedA, ".devcontainer.json"},
	} {
		if _, err := b.Client.Contents(ctx, tok, missing.repo, missing.path, "main"); !github.IsNotFound(err) {
			t.Errorf("%s/%s: %v; want 404", missing.repo, missing.path, err)
		}
	}
}

// Belief: a token scoped by repository_ids sees only those repositories, in
// listing and in contents alike. This is the property a workspace's token
// rests on (§9.1): one repository, and nothing else the App can reach.
func TestContractScopedToken(t *testing.T) {
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	in := installation(t, b)
	repos := testbed(t, b, in)
	a, other := repos[githubtest.TestbedA], repos[githubtest.TestbedB]

	tok, err := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID,
		Permissions: map[string]string{"metadata": github.Read, "contents": github.Read}, RepositoryIDs: []int64{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := b.Client.Repositories(ctx, tok)
	if err != nil || len(listed) != 1 || listed[0].ID != a.ID {
		t.Errorf("a token scoped to %s lists %+v (%v)", a.FullName, listed, err)
	}
	// Control: it can read its own repository…
	if _, err := b.Client.Contents(ctx, tok, a.FullName, ".devcontainer", "main"); err != nil {
		t.Fatalf("control: the scoped token cannot read its own repository: %v", err)
	}
	// …and not the other one. 404, not 403: GitHub does not confirm the
	// existence of a repository the token cannot see.
	if _, err := b.Client.Contents(ctx, tok, other.FullName, "README.md", "main"); !github.IsNotFound(err) {
		t.Errorf("a token scoped to %s read %s: %v; want 404", a.FullName, other.FullName, err)
	}
}

// Belief: a token without the contents permission cannot read contents —
// 403, "Resource not accessible by integration". The catalog's listing token
// is metadata-only on purpose (§9.3); this is what makes that mean something.
func TestContractPermissionIsEnforced(t *testing.T) {
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	in := installation(t, b)
	tok, err := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID, Permissions: meta()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Client.Contents(ctx, tok, githubtest.TestbedA, ".devcontainer", "main"); status(err) != http.StatusForbidden {
		t.Errorf("a metadata-only token reading contents: %v; want 403", err)
	}
}

// Belief: GitHub refuses a token request for more than the App holds, or for
// a repository the installation does not cover — 422, not a quieter token
// with less in it. The broker relies on a refusal it can report (§12).
//
// And the two refusals share that 422, so the status cannot say which one it
// was: only GitHub's message does. One is the operator's to fix in the App's
// settings, the other is a revocation, and the broker tells the container
// which (design §9.4). So this pins each message against the matcher the
// broker uses — each matches its own refusal and not the other's — and logs
// both verbatim, so a live run records what GitHub actually said.
func TestContractTokenRequestsBeyondTheAppAreRefused(t *testing.T) {
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	in := installation(t, b)

	// Precondition: the App really lacks administration:write, or the first
	// request below proves nothing about a missing permission.
	// The App's permissions are a fixture (githubtest.DevAppPermissions):
	// the broker's contract relies on the dev App lacking actions:write.
	perms := appPermissions(t, b)
	t.Logf("live=%v the App's permissions: %v", b.Live, perms)
	if !maps.Equal(perms, githubtest.DevAppPermissions()) {
		t.Errorf("the App holds %v; githubtest.DevAppPermissions says %v. Changing the dev App's permissions is changing a fixture: update it", perms, githubtest.DevAppPermissions())
	}
	if perms["administration"] == github.Write {
		t.Fatal("the dev App holds administration:write; pick a permission it lacks for this test")
	}

	_, permErr := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID,
		Permissions: map[string]string{"administration": github.Write}})
	t.Logf("a permission the App lacks: %v", permErr)
	if status(permErr) != http.StatusUnprocessableEntity {
		t.Errorf("a permission the App lacks: %v; want 422", permErr)
	}
	// krelinga/drydock exists and is not in the dev App's installation.
	_, repoErr := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID,
		Permissions: meta(), RepositoryIDs: []int64{drydockRepoID}})
	t.Logf("a repository outside the installation: %v", repoErr)
	if status(repoErr) != http.StatusUnprocessableEntity {
		t.Errorf("a repository outside the installation: %v; want 422", repoErr)
	}

	// Each matcher recognises its own refusal and not the other's: a matcher
	// that said yes to every 422 would pass the first half alone.
	if !github.IsPermissionNotGranted(permErr) || github.IsRepositoryNotIncluded(permErr) {
		t.Errorf("a missing permission (%v): IsPermissionNotGranted=%v IsRepositoryNotIncluded=%v; want true, false",
			permErr, github.IsPermissionNotGranted(permErr), github.IsRepositoryNotIncluded(permErr))
	}
	if !github.IsRepositoryNotIncluded(repoErr) || github.IsPermissionNotGranted(repoErr) {
		t.Errorf("a repository outside the installation (%v): IsRepositoryNotIncluded=%v IsPermissionNotGranted=%v; want true, false",
			repoErr, github.IsRepositoryNotIncluded(repoErr), github.IsPermissionNotGranted(repoErr))
	}

	// Control: the same installation mints a token for what it does hold, so
	// the refusals above are about what was asked for.
	if _, err := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: in.ID, Permissions: meta()}); err != nil {
		t.Errorf("control: a metadata token: %v", err)
	}
}

// appPermissions reads GET /app: the permissions the App holds. Only the
// contract needs it, so it is a raw request here rather than a client method.
func appPermissions(t *testing.T, b githubtest.Backend) map[string]string {
	t.Helper()
	jwt, err := github.JWT(b.Client.AppID, b.Client.Key, b.Client.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	base := b.Client.BaseURL
	if base == "" {
		base = github.DefaultBaseURL
	}
	req, _ := http.NewRequest("GET", base+"/app", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "drydock-contract-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var app struct {
		Permissions map[string]string `json:"permissions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&app) != nil || app.Permissions == nil {
		t.Fatalf("GET /app: %s", resp.Status)
	}
	return app.Permissions
}

// drydockRepoID is krelinga/drydock's id: a real repository the dev App is
// deliberately not installed on.
const drydockRepoID = 1346725772
