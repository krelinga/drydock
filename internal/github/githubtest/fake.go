// Package githubtest is a fake GitHub for the App client's callers (testing
// §6.2, fakegithub). It is strict where GitHub is strict — the JWT's
// signature, issuer and lifetime; a token's expiry, permissions and
// repositories — because a lenient fake is one a broken client passes.
//
// Recorded against GitHub's REST API version 2022-11-28. Its contract test is
// the manual ritual against the real App (testing §6.1): a fake encodes a
// belief, and the ritual is what checks the belief.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Repo is one repository in a fake installation.
type Repo struct {
	ID            int64
	FullName      string
	DefaultBranch string
	Private       bool
	Archived      bool
	PushedAt      time.Time
	// Files are the paths that exist in the repository, e.g.
	// ".devcontainer/devcontainer.json". Directories are implied.
	Files []string
	// Contents gives a file in Files its bytes in the git remote. A file
	// without an entry holds its own path and a newline, which is enough for
	// everything but a devcontainer.json that has to build.
	Contents map[string]string
}

// Installation is one account the fake App is installed on.
type Installation struct {
	ID          int64
	Account     string
	AccountType string // "User" unless set
	Repos       []Repo
}

// TokenRequest is a recorded POST .../access_tokens.
type TokenRequest struct {
	InstallationID int64
	Permissions    map[string]string
	RepositoryIDs  []int64
}

type token struct {
	installation int64
	perms        map[string]string
	repos        map[int64]bool // nil: every repository of the installation
	expires      time.Time
}

// Fake is the server. Its fields may be changed between requests; Mu guards
// them against the handler.
type Fake struct {
	Mu            sync.Mutex
	AppID         int64
	Key           *rsa.PrivateKey
	Installations []Installation
	// AppPermissions is what the App was granted. A token request for more
	// is refused, as GitHub refuses it.
	AppPermissions map[string]string
	// Now is the fake's clock, for checking JWT and token lifetimes.
	Now func() time.Time
	// Fail, if set, may answer a request with an error status instead.
	Fail func(r *http.Request) (status int, message string)

	TokenRequests []TokenRequest
	Requests      []string // "METHOD /path" per request, without the query
	tokens        map[string]token
	URL           string

	gitDir     string
	gitBackend string
	gitAuths   []GitAuth
}

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

// TestKey is one 2048-bit key per test binary: generating one per test would
// cost a second each.
func TestKey(t testing.TB) *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return key
}

// KeyPEM is TestKey as a PKCS#1 PEM, the format GitHub issues.
func KeyPEM(t testing.TB) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(TestKey(t))})
}

// New starts a fake for App appID, signed with TestKey, closed at test end.
func New(t testing.TB, appID int64, now func() time.Time) *Fake {
	f := &Fake{
		AppID: appID, Key: TestKey(t), Now: now, tokens: map[string]token{},
		AppPermissions: map[string]string{
			"metadata": "read", "contents": "write", "pull_requests": "write", "issues": "write",
			"workflows": "write", "actions": "write", "checks": "read",
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// Count reports how many recorded requests match "METHOD /path-prefix".
func (f *Fake) Count(prefix string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// IssuedTokens returns every token value the fake has issued, so a test can
// sweep for them in places they must not appear.
func (f *Fake) IssuedTokens() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []string
	for v := range f.tokens {
		out = append(out, v)
	}
	return out
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	if f.serveGit(w, r) {
		return
	}
	if f.Fail != nil {
		if status, msg := f.Fail(r); status != 0 {
			fail(w, status, msg)
			return
		}
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("User-Agent") == "" {
		fail(w, http.StatusBadRequest, "missing API version or User-Agent header")
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/app/installations":
		if !f.appAuth(w, r) {
			return
		}
		var out []map[string]any
		for _, in := range f.Installations {
			typ := in.AccountType
			if typ == "" {
				typ = "User"
			}
			out = append(out, map[string]any{"id": in.ID, "account": map[string]any{"login": in.Account, "type": typ}})
		}
		page(w, r, out)

	case r.Method == "GET" && path == "/app":
		if !f.appAuth(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": f.AppID, "permissions": f.AppPermissions})

	case r.Method == "POST" && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		if !f.appAuth(w, r) {
			return
		}
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/app/installations/"), "/access_tokens"), 10, 64)
		in := f.installation(id)
		if in == nil {
			fail(w, http.StatusNotFound, "Not Found")
			return
		}
		var body struct {
			Permissions   map[string]string `json:"permissions"`
			RepositoryIDs []int64           `json:"repository_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		for p, level := range body.Permissions {
			granted, ok := f.AppPermissions[p]
			if !ok || (level == "write" && granted != "write") {
				// GitHub's own sentence, which does not name the permission
				// (pinned by TestContractTokenRequestsBeyondTheAppAreRefused).
				fail(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
				return
			}
		}
		tok := token{installation: id, perms: body.Permissions, expires: f.Now().Add(time.Hour)}
		if len(body.RepositoryIDs) > 0 {
			tok.repos = map[int64]bool{}
			for _, rid := range body.RepositoryIDs {
				if repoByID(in, rid) == nil {
					fail(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
					return
				}
				tok.repos[rid] = true
			}
		}
		f.TokenRequests = append(f.TokenRequests, TokenRequest{InstallationID: id, Permissions: body.Permissions, RepositoryIDs: body.RepositoryIDs})
		b := make([]byte, 18)
		rand.Read(b)
		value := "ghs_" + hex.EncodeToString(b)
		f.tokens[value] = tok
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": value, "expires_at": tok.expires.UTC().Format(time.RFC3339)})

	case r.Method == "GET" && path == "/installation/repositories":
		tok, ok := f.tokenAuth(w, r)
		if !ok {
			return
		}
		in := f.installation(tok.installation)
		var repos []map[string]any
		for _, rp := range in.Repos {
			if tok.repos != nil && !tok.repos[rp.ID] {
				continue
			}
			repos = append(repos, map[string]any{
				"id": rp.ID, "full_name": rp.FullName, "default_branch": rp.DefaultBranch,
				"private": rp.Private, "archived": rp.Archived, "pushed_at": rp.PushedAt.UTC().Format(time.RFC3339),
			})
		}
		per, pg := paging(r)
		lo, hi := min((pg-1)*per, len(repos)), min(pg*per, len(repos))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"total_count": len(repos), "repositories": nonNil(repos[lo:hi])})

	case r.Method == "GET" && strings.HasPrefix(path, "/repos/"):
		tok, ok := f.tokenAuth(w, r)
		if !ok {
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 4)
		if len(parts) < 3 || parts[2] != "contents" {
			fail(w, http.StatusNotFound, "Not Found")
			return
		}
		if tok.perms["contents"] == "" {
			fail(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		rp := repoByName(f.installation(tok.installation), parts[0]+"/"+parts[1])
		if rp == nil || (tok.repos != nil && !tok.repos[rp.ID]) {
			fail(w, http.StatusNotFound, "Not Found")
			return
		}
		target := ""
		if len(parts) == 4 {
			target = strings.Trim(parts[3], "/")
		}
		f.contents(w, rp, target)

	default:
		fail(w, http.StatusNotFound, "Not Found")
	}
}

func (f *Fake) contents(w http.ResponseWriter, rp *Repo, target string) {
	w.Header().Set("Content-Type", "application/json")
	children := map[string]string{}
	for _, file := range rp.Files {
		if file == target {
			name := file[strings.LastIndex(file, "/")+1:]
			json.NewEncoder(w).Encode(map[string]any{"name": name, "type": "file", "path": file})
			return
		}
		if rest, ok := strings.CutPrefix(file, target+"/"); ok || target == "" {
			if target == "" {
				rest = file
			}
			name, _, isDir := strings.Cut(rest, "/")
			if isDir {
				children[name] = "dir"
			} else {
				children[name] = "file"
			}
		}
	}
	if len(children) == 0 {
		fail(w, http.StatusNotFound, "Not Found")
		return
	}
	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []map[string]any
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "type": children[n]})
	}
	json.NewEncoder(w).Encode(out)
}

// appAuth checks a JWT the way GitHub does: RS256 under the App's key, the
// App as issuer, unexpired, issued no later than now, and a lifetime of at
// most ten minutes.
func (f *Fake) appAuth(w http.ResponseWriter, r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(&f.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	var header struct{ Alg string }
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(hb, &header)
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if header.Alg != "RS256" || json.Unmarshal(cb, &claims) != nil {
		fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	now := f.Now().Unix()
	switch {
	case claims.Iss != strconv.FormatInt(f.AppID, 10):
		fail(w, http.StatusUnauthorized, "'Issuer' claim ('iss') must be an Integer")
	case claims.Exp <= now:
		fail(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")
	case claims.Iat > now:
		fail(w, http.StatusUnauthorized, "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued")
	case claims.Exp-claims.Iat > 600:
		fail(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') is too far in the future")
	default:
		return true
	}
	return false
}

func (f *Fake) tokenAuth(w http.ResponseWriter, r *http.Request) (token, bool) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok {
		v, ok = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	tok, found := f.tokens[v]
	if !ok || !found || !f.Now().Before(tok.expires) {
		fail(w, http.StatusUnauthorized, "Bad credentials")
		return token{}, false
	}
	return tok, true
}

func (f *Fake) installation(id int64) *Installation {
	for i := range f.Installations {
		if f.Installations[i].ID == id {
			return &f.Installations[i]
		}
	}
	return nil
}

func repoByID(in *Installation, id int64) *Repo {
	for i := range in.Repos {
		if in.Repos[i].ID == id {
			return &in.Repos[i]
		}
	}
	return nil
}

func repoByName(in *Installation, name string) *Repo {
	if in == nil {
		return nil
	}
	for i := range in.Repos {
		if strings.EqualFold(in.Repos[i].FullName, name) {
			return &in.Repos[i]
		}
	}
	return nil
}

func paging(r *http.Request) (per, pg int) {
	per, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 || per > 100 {
		per = 30 // GitHub's default
	}
	if pg <= 0 {
		pg = 1
	}
	return per, pg
}

func page(w http.ResponseWriter, r *http.Request, all []map[string]any) {
	per, pg := paging(r)
	lo, hi := min((pg-1)*per, len(all)), min(pg*per, len(all))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nonNil(all[lo:hi]))
}

func nonNil(s []map[string]any) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	return s
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
