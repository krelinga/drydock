package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/sys"
)

const appID = 5189455

func setup(t *testing.T) (*github.Client, *githubtest.Fake, *sys.FakeClock) {
	t.Helper()
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	f := githubtest.New(t, appID, clock.Now)
	var repos []githubtest.Repo
	for i := int64(1); i <= 150; i++ { // more than one page of 100
		repos = append(repos, githubtest.Repo{ID: 1000 + i, FullName: fmt.Sprintf("krelinga/r%03d", i), DefaultBranch: "main"})
	}
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: repos}}
	key, err := github.ParseKey(githubtest.KeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	return &github.Client{AppID: appID, Key: key, BaseURL: f.URL, Clock: clock}, f, clock
}

func TestLoadKeyRefusesAKeyOthersCanRead(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o440, 0o404, 0o644, 0o600 | 0o004} {
		p := filepath.Join(dir, fmt.Sprintf("k%o.pem", mode))
		os.WriteFile(p, githubtest.KeyPEM(t), 0o600)
		os.Chmod(p, mode)
		if _, err := github.LoadKey(p); err == nil {
			t.Errorf("mode %#o accepted", mode)
		}
	}
	// Control: the specified mode loads, as does 0600.
	for _, mode := range []os.FileMode{0o400, 0o600} {
		p := filepath.Join(dir, fmt.Sprintf("ok%o.pem", mode))
		os.WriteFile(p, githubtest.KeyPEM(t), 0o600)
		os.Chmod(p, mode)
		if _, err := github.LoadKey(p); err != nil {
			t.Errorf("mode %#o: %v", mode, err)
		}
	}
}

// The fake verifies the JWT's signature, issuer and lifetime, so listing
// installations at all is the JWT's test — and a wrong key, a wrong App ID,
// and a clock too far out are each refused.
func TestJWTIsAcceptedOnlyWhenRight(t *testing.T) {
	ctx := context.Background()
	c, f, clock := setup(t)
	ins, err := c.Installations(ctx)
	if err != nil || len(ins) != 1 || ins[0].ID != 77 || ins[0].Account != "krelinga" {
		t.Fatalf("control: %+v %v", ins, err)
	}
	if got := ins[0].SettingsURL(); got != "https://github.com/settings/installations/77" {
		t.Errorf("settings URL %s", got)
	}

	wrongID := &github.Client{AppID: 1, Key: c.Key, BaseURL: c.BaseURL, Clock: clock}
	if _, err := wrongID.Installations(ctx); err == nil {
		t.Error("a JWT naming another App was accepted")
	}
	wrongKey := &github.Client{AppID: appID, Key: otherKey(), BaseURL: c.BaseURL, Clock: clock}
	if _, err := wrongKey.Installations(ctx); err == nil {
		t.Error("a JWT signed with another key was accepted")
	}
	// Drydock's clock ten minutes ahead of GitHub's: the JWT's iat is in
	// GitHub's future. The one-minute backdating covers skew, not this.
	f.Now = func() time.Time { return clock.Now().Add(-10 * time.Minute) }
	if _, err := c.Installations(ctx); err == nil {
		t.Error("a JWT from a clock ten minutes fast was accepted")
	}
}

func TestRepositoriesPaginates(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setup(t)
	tok, err := c.InstallationToken(ctx, github.TokenRequest{InstallationID: 77, Permissions: map[string]string{"metadata": "read"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := c.Repositories(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 150 || repos[149].FullName != "krelinga/r150" {
		t.Errorf("%d repos, last %+v", len(repos), repos[len(repos)-1])
	}
}

func TestTokensAreCachedByWhatTheyGrant(t *testing.T) {
	ctx := context.Background()
	c, f, clock := setup(t)
	meta := github.TokenRequest{InstallationID: 77, Permissions: map[string]string{"metadata": "read"}}
	a, _ := c.InstallationToken(ctx, meta)
	b, _ := c.InstallationToken(ctx, meta)
	if a.Value() != b.Value() || f.Count("POST /app/installations/77/access_tokens") != 1 {
		t.Errorf("a second identical request minted a new token")
	}
	// A different grant is a different token.
	scoped, _ := c.InstallationToken(ctx, github.TokenRequest{InstallationID: 77,
		Permissions: map[string]string{"contents": "read"}, RepositoryIDs: []int64{1001}})
	if scoped.Value() == a.Value() {
		t.Error("a contents token reused the metadata token")
	}
	// Reused for its first 55 minutes, re-minted after.
	clock.Advance(54 * time.Minute)
	if c2, _ := c.InstallationToken(ctx, meta); c2.Value() != a.Value() {
		t.Error("re-minted at 54 minutes")
	}
	clock.Advance(2 * time.Minute)
	if c3, _ := c.InstallationToken(ctx, meta); c3.Value() == a.Value() {
		t.Error("reused a token with four minutes left")
	}
	// What was asked for is exactly what was requested of GitHub.
	f.Mu.Lock()
	reqs := f.TokenRequests
	f.Mu.Unlock()
	if len(reqs) != 3 || reqs[1].Permissions["contents"] != "read" || len(reqs[1].RepositoryIDs) != 1 || reqs[0].RepositoryIDs != nil {
		t.Errorf("token requests %+v", reqs)
	}
}

// An unscoped request would get every permission the App holds; §9.3 says
// each token asks for what its caller needs.
func TestTokenRequestMustNamePermissions(t *testing.T) {
	c, f, _ := setup(t)
	if _, err := c.InstallationToken(context.Background(), github.TokenRequest{InstallationID: 77}); err == nil {
		t.Error("a token with no permissions named was requested")
	}
	if f.Count("POST") != 0 {
		t.Error("the refused request reached GitHub")
	}
}

// A token, wherever it is formatted by accident, prints nothing usable.
func TestTokenNeverFormats(t *testing.T) {
	ctx := context.Background()
	c, f, _ := setup(t)
	tok, _ := c.InstallationToken(ctx, github.TokenRequest{InstallationID: 77, Permissions: map[string]string{"metadata": "read"}})
	issued := f.IssuedTokens()
	if len(issued) != 1 || tok.Value() != issued[0] {
		t.Fatalf("control: the token's value is not the issued one")
	}
	wrapped := fmt.Errorf("while doing X with %v", tok)
	b, jerr := json.Marshal(map[string]any{"t": tok})
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok), wrapped.Error(), string(b)} {
		if strings.Contains(s, issued[0]) {
			t.Errorf("a token was formatted: %q", s)
		}
	}
	if jerr == nil {
		t.Error("a token marshalled to JSON")
	}
}

func TestContentsAndErrors(t *testing.T) {
	ctx := context.Background()
	c, f, _ := setup(t)
	f.Installations[0].Repos[0].Files = []string{".devcontainer/devcontainer.json", "README.md"}
	tok, _ := c.InstallationToken(ctx, github.TokenRequest{InstallationID: 77, Permissions: map[string]string{"contents": "read"}})
	dir, err := c.Contents(ctx, tok, "krelinga/r001", ".devcontainer", "main")
	if err != nil || len(dir) != 1 || dir[0].Name != "devcontainer.json" || dir[0].Type != "file" {
		t.Errorf("dir %+v %v", dir, err)
	}
	if _, err := c.Contents(ctx, tok, "krelinga/r001", ".devcontainer.json", "main"); !github.IsNotFound(err) {
		t.Errorf("a missing file: %v; want IsNotFound", err)
	}
	// A token without contents permission is refused — and the error
	// carries GitHub's words, never the token.
	meta, _ := c.InstallationToken(ctx, github.TokenRequest{InstallationID: 77, Permissions: map[string]string{"metadata": "read"}})
	_, err = c.Contents(ctx, meta, "krelinga/r001", "README.md", "")
	var ae *github.APIError
	if !errors.As(err, &ae) || ae.Status != 403 || strings.Contains(err.Error(), meta.Value()) {
		t.Errorf("err %v", err)
	}
}

var otherKey = sync.OnceValue(func() *rsa.PrivateKey {
	// 1024 bits: only its difference from the App's key matters, and a
	// 2048-bit key costs a second to generate.
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	return k
})
