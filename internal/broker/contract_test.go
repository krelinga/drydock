package broker

// Phase 3's deliverable as a contract test (design §14): from a workspace's
// socket, git — through drydock-credential — can fetch and push its own
// repository, and cannot touch another. Against githubtest's fake on every
// run, and against the dev App and its testbed repositories in the live job,
// where the push is real and its branch is deleted after.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type contractEnv struct {
	backend githubtest.Backend
	broker  *Broker
	ws      map[string]string // testbed full name → workspace id
}

func newContractEnv(t *testing.T) *contractEnv {
	t.Helper()
	ctx := context.Background()
	b := githubtest.NewBackend(t)
	ins, err := b.Client.Installations(ctx)
	if err != nil || len(ins) != 1 {
		t.Fatalf("installations %+v: %v", ins, err)
	}
	tok, err := b.Client.InstallationToken(ctx, github.TokenRequest{InstallationID: ins[0].ID,
		Permissions: map[string]string{"metadata": "read"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := b.Client.Repositories(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ce := &contractEnv{backend: b, ws: map[string]string{}}
	ids := map[string]string{githubtest.TestbedA: "01JCAAAAAAAAAAAAAAAAAAAAAA", githubtest.TestbedB: "01JCBBBBBBBBBBBBBBBBBBBBBB"}
	for _, r := range repos {
		ws, ok := ids[r.FullName]
		if !ok {
			continue
		}
		db.ExecContext(ctx, `INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (?, ?, ?, ?)`,
			r.ID, ins[0].ID, r.FullName, r.DefaultBranch)
		db.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES (?, ?, '/x', 'main', 'running')`,
			ws, r.ID)
		ce.ws[r.FullName] = ws
	}
	if len(ce.ws) != 2 {
		t.Fatalf("the installation lacks a testbed: %v", ce.ws)
	}
	ce.broker = &Broker{Dir: shortDir(t), GitHub: b.Client, DB: db.DB,
		Events: events.New(db.DB, sys.RealClock{}), Env: sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}}}
	t.Cleanup(ce.broker.CloseAll)
	for _, ws := range ce.ws {
		if err := ce.broker.Open(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	return ce
}

// git runs git as a workspace container would: our helper and no other, no
// user or system config, no prompting, and the workspace's socket.
func (ce *contractEnv) git(t *testing.T, workspaceOf, dir string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + filepath.Join(binDir, "drydock-credential")}, args...)
	cmd := exec.Command("git", full...)
	// Never inside a repository this test did not make. In CI the test's
	// working directory is the drydock checkout, whose .git/config carries
	// actions/checkout's http.extraheader with the workflow's own token; git
	// sends that header and never consults the credential helper, so every
	// request went out as krelinga/drydock and the testbeds were "not
	// found". GIT_CEILING_DIRECTORIES keeps git from walking up into one.
	if dir == "" {
		dir = t.TempDir()
	}
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir),
		"DRYDOCK_BROKER_SOCK=" + ce.broker.SocketPath(ce.ws[workspaceOf]),
		"DRYDOCK_GITHUB_HOST=" + ce.backend.GitHost(),
		// The dev App's bot identity (design §9.3's note: bot *user* id).
		"GIT_AUTHOR_NAME=krelinga-drydock-dev[bot]", "GIT_COMMITTER_NAME=krelinga-drydock-dev[bot]",
		"GIT_AUTHOR_EMAIL=337849865+krelinga-drydock-dev[bot]@users.noreply.github.com",
		"GIT_COMMITTER_EMAIL=337849865+krelinga-drydock-dev[bot]@users.noreply.github.com",
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// From A's socket: A's refs are readable, B's are not — and B's own socket
// reads B, so the refusal is about the socket and not about B.
func TestContractGitReachesOnlyItsOwnRepository(t *testing.T) {
	ce := newContractEnv(t)
	a, b := githubtest.TestbedA, githubtest.TestbedB
	out, err := ce.git(t, a, "", "ls-remote", ce.backend.GitURL(t, a))
	if err != nil || !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("A's socket reading A: %v\n%s", err, out)
	}
	if out, err := ce.git(t, a, "", "ls-remote", ce.backend.GitURL(t, b)); err == nil {
		t.Errorf("A's socket read B:\n%s", out)
	}
	if out, err := ce.git(t, b, "", "ls-remote", ce.backend.GitURL(t, b)); err != nil {
		t.Errorf("control: B's socket reading B: %v\n%s", err, out)
	}
	// On the fake, the remote saw the token the broker minted — the helper's
	// output observed, not inferred (testing §6.3).
	if !ce.backend.Live {
		issued := map[string]bool{}
		for _, tok := range ce.backend.Fake.IssuedTokens() {
			issued[tok] = true
		}
		presented := 0
		for _, g := range ce.backend.Fake.GitAuths() {
			if g.Token != "" && !issued[g.Token] {
				t.Errorf("git presented a token the broker did not mint for %s", g.Repo)
			}
			if g.Token != "" && g.Repo == a && g.Service == "git-upload-pack" {
				presented++
			}
		}
		if presented == 0 {
			t.Error("control: git never presented a token for A, so the check above is vacuous")
		}
	}
}

// The push half: a branch under drydock/ lands on A, the same push to B is
// refused, and the branch is deleted again — through the helper both ways.
func TestContractPushABranch(t *testing.T) {
	ce := newContractEnv(t)
	a, b := githubtest.TestbedA, githubtest.TestbedB
	work := t.TempDir()
	if out, err := ce.git(t, a, work, "clone", "-q", "--depth", "1", ce.backend.GitURL(t, a), "repo"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	repo := filepath.Join(work, "repo")
	suffix := make([]byte, 4)
	rand.Read(suffix)
	branch := "drydock/contract-" + hex.EncodeToString(suffix)
	if v := os.Getenv("GITHUB_RUN_ID"); v != "" {
		branch += "-" + v
	}
	os.WriteFile(filepath.Join(repo, "contract.txt"), []byte(branch+"\n"), 0o644)
	for _, args := range [][]string{{"checkout", "-q", "-b", branch}, {"add", "contract.txt"}, {"commit", "-q", "-m", "contract test"}} {
		if out, err := ce.git(t, a, repo, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	out, err := ce.git(t, a, repo, "push", "-q", "origin", branch)
	if err != nil {
		t.Fatalf("pushing %s to A: %v\n%s", branch, err, out)
	}
	t.Cleanup(func() {
		if out, err := ce.git(t, a, repo, "push", "-q", "origin", "--delete", branch); err != nil {
			t.Errorf("deleting %s: %v\n%s", branch, err, out)
		}
	})
	if out, _ := ce.git(t, a, "", "ls-remote", ce.backend.GitURL(t, a), branch); !strings.Contains(out, "refs/heads/"+branch) {
		t.Errorf("the pushed branch is not on A:\n%s", out)
	}

	// The same commit, pushed to B through A's socket: refused.
	if out, err := ce.git(t, a, repo, "push", "-q", ce.backend.GitURL(t, b), branch); err == nil {
		t.Errorf("A's socket pushed to B:\n%s", out)
		ce.git(t, b, repo, "push", "-q", ce.backend.GitURL(t, b), "--delete", branch)
	}
}
