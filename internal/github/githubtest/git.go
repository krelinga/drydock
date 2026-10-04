package githubtest

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fake's git remote (testing §6.3): a smart-HTTP server in front of real
// bare repositories, one per Repo, built from Repo.Files on first use. It is
// git's own `http-backend`, so the protocol is the real one; what the fake
// adds is GitHub's authorization — Basic auth as x-access-token:<token>, a
// token covering the repository, contents:read to fetch and contents:write
// to push — and a record of every Authorization it saw, so the credential
// helper's output is observed rather than inferred.

// GitAuth is one recorded git request: the repository, the service, and
// whether it carried a token the fake issued.
type GitAuth struct {
	Repo    string // owner/name
	Service string // git-upload-pack (fetch) or git-receive-pack (push)
	Token   string // the token presented, "" if none
}

// gitRoot is where the bare repositories live, made on first use.
func (f *Fake) gitRoot(t testing.TB) string {
	if f.gitDir == "" {
		f.gitDir = t.TempDir()
	}
	return f.gitDir
}

// EnableGit turns on the git remote at f.URL/<owner>/<name>.git. Separate
// from New because it needs git on PATH.
func (f *Fake) EnableGit(t testing.TB) {
	t.Helper()
	backend, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git is required for the fake's git remote: %v", err)
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.gitBackend = filepath.Join(strings.TrimSpace(string(backend)), "git-http-backend")
	f.gitRoot(t)
	for _, in := range f.Installations {
		for _, r := range in.Repos {
			f.makeRepo(t, r)
		}
	}
}

// makeRepo builds a bare repository holding r.Files on r.DefaultBranch.
func (f *Fake) makeRepo(t testing.TB, r Repo) {
	t.Helper()
	bare := filepath.Join(f.gitDir, r.FullName+".git")
	if _, err := os.Stat(bare); err == nil {
		return
	}
	work := t.TempDir()
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	branch := r.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	git(work, "init", "-q", "-b", branch)
	files := r.Files
	if len(files) == 0 {
		files = []string{"README.md"}
	}
	for _, p := range files {
		os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(work, p), []byte(p+"\n"), 0o644)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "fixture")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	git(work, "clone", "-q", "--bare", work, bare)
	// http-backend refuses pushes unless this is set.
	git(bare, "config", "http.receivepack", "true")
}

// GitAuths returns every git request the fake has seen.
func (f *Fake) GitAuths() []GitAuth {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]GitAuth(nil), f.gitAuths...)
}

// serveGit handles /<owner>/<name>.git/…, f.Mu held on entry and released
// before the backend runs. It reports whether the path was a git path.
func (f *Fake) serveGit(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/")
	name, _, isGit := strings.Cut(rest, ".git/")
	if !ok || !isGit || f.gitBackend == "" {
		return false
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		service = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	}
	push := service == "git-receive-pack"

	token := ""
	if user, pass, ok := basicAuth(r); ok && user == "x-access-token" {
		token = pass
	}
	f.gitAuths = append(f.gitAuths, GitAuth{Repo: name, Service: service, Token: token})

	var repo *Repo
	for i := range f.Installations {
		if rp := repoByName(&f.Installations[i], name); rp != nil {
			repo = rp
		}
	}
	tok, valid := f.tokens[token]
	switch {
	case token == "":
		// GitHub asks for credentials, which is what makes git consult
		// its credential helper at all.
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		f.Mu.Unlock()
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		f.Mu.Lock()
		return true
	case !valid || !f.Now().Before(tok.expires):
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		f.Mu.Unlock()
		http.Error(w, "Invalid username or token", http.StatusUnauthorized)
		f.Mu.Lock()
		return true
	case repo == nil || (tok.repos != nil && !tok.repos[repo.ID]) || f.installation(tok.installation) == nil ||
		repoByID(f.installation(tok.installation), repo.ID) == nil:
		f.Mu.Unlock()
		http.Error(w, "Repository not found.", http.StatusNotFound)
		f.Mu.Lock()
		return true
	case tok.perms["contents"] == "" || (push && tok.perms["contents"] != "write"):
		f.Mu.Unlock()
		http.Error(w, "Permission to "+name+".git denied to x-access-token.", http.StatusForbidden)
		f.Mu.Lock()
		return true
	}

	h := &cgi.Handler{
		Path: f.gitBackend,
		Env:  []string{"GIT_PROJECT_ROOT=" + f.gitDir, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"},
	}
	f.Mu.Unlock()
	h.ServeHTTP(w, r)
	f.Mu.Lock()
	return true
}

func basicAuth(r *http.Request) (user, pass string, ok bool) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Basic ")
	if !ok {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(b), ":")
	return user, pass, ok
}
