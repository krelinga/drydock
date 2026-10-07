package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/sys"
)

var featureBin, _ = filepath.Abs(filepath.Join("..", "..", "feature", "src", "drydock", "bin"))

const (
	wsGranted   = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	wsUngranted = "01JBBBBBBBBBBBBBBBBBBBBBBB"
)

func randomAlnum(n int) string {
	const a = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = a[int(b[i])%len(a)]
	}
	return string(b)
}

// forms is every way a string could reach a sink through an encoder
// (testing §4.2: the sweep searches for transformations too).
func forms(s string) map[string]string {
	j, _ := json.Marshal(s)
	return map[string]string{
		"raw":       s,
		"json":      strings.Trim(string(j), `"`),
		"url":       url.QueryEscape(s),
		"path":      url.PathEscape(s),
		"base64":    base64.StdEncoding.EncodeToString([]byte(s)),
		"base64url": base64.RawURLEncoding.EncodeToString([]byte(s)),
		"hex":       hex.EncodeToString([]byte(s)),
	}
}

// secretsServer is a real server with an App (faked) and a master key, two
// running workspaces on two repositories, and their broker sockets.
//
// seed is SQL run after those rows and before Serve, for a test that needs the
// database in a state no route can produce.
func secretsServer(t *testing.T, dir string, masterKey []byte, seed ...string) *running {
	t.Helper()
	cfg := testConfig(t, dir)
	f := githubtest.New(t, 5189455, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/alpha", DefaultBranch: "main"},
		{ID: 202, FullName: "krelinga/beta", DefaultBranch: "main"},
	}}}
	appKey := filepath.Join(dir, "app.pem")
	os.WriteFile(appKey, githubtest.KeyPEM(t), 0o400)
	keyPath := filepath.Join(dir, "secrets.key")
	if err := os.WriteFile(keyPath, masterKey, 0o400); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI, cfg.SecretsKey = 5189455, appKey, f.URL, keyPath

	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append([]string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (202, 77, 'krelinga/beta', 'main')`,
		// Stopped, so boot reconciliation leaves them alone whether or not
		// this machine has Docker; marked running once it has finished.
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + wsGranted + `', 101, '/x', 'main', 'stopped')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + wsUngranted + `', 202, '/y', 'main', 'stopped')`,
	}, seed...) {
		if _, err := srv.DB.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-srv.reconciled:
	case <-time.After(30 * time.Second):
		t.Fatal("boot reconciliation did not finish")
	}
	srv.DB.ExecContext(context.Background(), `UPDATE workspace SET state = 'running'`)
	// Boot opens sockets only for running workspaces, and these were
	// stopped until now; open them as step 5 would.
	for _, ws := range []string{wsGranted, wsUngranted} {
		if err := srv.Broker.Open(context.Background(), ws); err != nil {
			t.Fatal(err)
		}
	}
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket), gh: f}
	for _, ws := range []string{wsGranted, wsUngranted} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(srv.Broker.SocketPath(ws)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no broker socket for %s", ws)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return r
}

// export runs the prelude Claude Code runs — the Feature's env file, read
// from the source tree so this is the line that ships — against a
// workspace's socket, and returns what the next command's environment holds.
func (r *running) export(t *testing.T, ws string) (env, stderr string, code int) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(featureBin, "..", "etc", "claude-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", strings.TrimSpace(string(text))+"\nenv")
	cmd.Env = []string{
		"PATH=" + featureBin + ":" + os.Getenv("PATH"),
		"DRYDOCK_BROKER_SOCK=" + r.srv.Broker.SocketPath(ws),
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// TestSecretsCanarySweep is testing §4.2 for secrets, end to end through the
// real gate, socket, store and broker: a high-entropy canary secret is
// created, granted, fetched, rotated, refused in a bad write, and deleted;
// then every response body, every SSE frame, the export's stderr, and the
// whole temporary tree — the SQLite file and its WAL byte for byte — are
// searched for it and for the master key, in every encoding.
//
// Positive controls in the same function: the canary IS delivered over the
// broker to its granted workspace (and the rotated one after it), it is NOT
// delivered to the ungranted one, and a non-secret marker — the reach — IS
// found by the same sweep in the database, the SSE transcript and the list.
func TestSecretsCanarySweep(t *testing.T) {
	dir := t.TempDir()
	masterKey := make([]byte, 32)
	rand.Read(masterKey)
	r := secretsServer(t, dir, masterKey)
	cookie := r.signIn(t)

	canary := randomAlnum(24)
	canary2 := randomAlnum(24)
	canaryRefused := randomAlnum(24)
	value := "postgres://drydock:" + canary + "@db.internal:5432/test"
	value2 := "postgres://drydock:" + canary2 + "@db.internal:5432/test"
	marker := "reaches-the-scratch-db-" + randomAlnum(8)

	// The SSE transcript, for the whole scenario.
	var sse bytes.Buffer
	var sseMu sync.Mutex
	stream := r.do(t, req{method: "GET", path: "/api/events", cookie: cookie})
	if stream.StatusCode != 200 {
		t.Fatalf("GET /api/events = %d", stream.StatusCode)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stream.Body.Read(buf)
			sseMu.Lock()
			sse.Write(buf[:n])
			sseMu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	var bodies []string
	call := func(method, path, body string, want int) string {
		t.Helper()
		resp := r.do(t, req{method: method, path: path, body: body, origin: uiOrigin, cookie: cookie})
		b, _ := io.ReadAll(resp.Body)
		bodies = append(bodies, string(b))
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, b)
		}
		return string(b)
	}
	jsonBody := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	created := call("PUT", "/api/secrets/TEST_DATABASE_URL", jsonBody(map[string]string{
		"value": value, "reach": marker, "description": "rotate in the scratch console"}), 200)
	if !strings.Contains(created, `"created":true`) {
		t.Errorf("create: %s", created)
	}
	// Default deny: stored is not granted.
	if env, _, code := r.export(t, wsGranted); code != 0 || strings.Contains(env, canary) {
		t.Errorf("before any grant the workspace got it (exit %d)", code)
	}
	call("PUT", "/api/secrets/TEST_DATABASE_URL/grants", `{"repository_ids":[101]}`, 200)

	// Positive control: the granted workspace holds the value, byte for byte.
	env, stderr, code := r.export(t, wsGranted)
	if code != 0 || stderr != "" || !strings.Contains(env, "TEST_DATABASE_URL="+value+"\n") {
		t.Fatalf("control: the granted workspace did not receive the secret (exit %d, stderr %q)", code, stderr)
	}
	// And the ungranted one does not.
	if env, stderr, code := r.export(t, wsUngranted); code != 0 || stderr != "" || strings.Contains(env, canary) || strings.Contains(env, "TEST_DATABASE_URL") {
		t.Errorf("the ungranted workspace: exit %d, stderr %q, holds it: %v", code, stderr, strings.Contains(env, canary))
	}

	list := call("GET", "/api/secrets", "", 200)
	var parsed struct {
		Secrets []map[string]json.RawMessage `json:"secrets"`
	}
	json.Unmarshal([]byte(list), &parsed)
	if len(parsed.Secrets) != 1 {
		t.Fatalf("list: %s", list)
	}
	for k := range parsed.Secrets[0] {
		if strings.Contains(strings.ToLower(k), "value") || strings.Contains(k, "cipher") || strings.Contains(k, "nonce") {
			t.Errorf("the list has a field %q", k)
		}
	}
	if !strings.Contains(list, marker) || !strings.Contains(list, `"accessed_by":["`+wsGranted+`"]`) || !strings.Contains(list, `"full_name":"krelinga/alpha"`) {
		t.Errorf("list: %s", list)
	}

	// Rotation: the running granted workspace is stale, and its next command
	// gets the new value with nothing restarted.
	rotated := call("PUT", "/api/secrets/TEST_DATABASE_URL", jsonBody(map[string]string{"value": value2, "reach": marker}), 200)
	var rot struct {
		Rotated bool `json:"rotated"`
		Stale   struct {
			NewCommands []struct {
				WorkspaceID string `json:"workspace_id"`
			} `json:"new_commands"`
			NeedsSupervisorRestart []json.RawMessage `json:"needs_supervisor_restart"`
		} `json:"stale"`
	}
	json.Unmarshal([]byte(rotated), &rot)
	if !rot.Rotated || len(rot.Stale.NewCommands) != 1 || rot.Stale.NewCommands[0].WorkspaceID != wsGranted || rot.Stale.NeedsSupervisorRestart == nil {
		t.Errorf("rotate: %s", rotated)
	}
	if env, _, _ := r.export(t, wsGranted); !strings.Contains(env, "TEST_DATABASE_URL="+value2+"\n") {
		t.Error("control: the rotated value did not reach the next command")
	}

	// Refused writes do not echo what they refused.
	for _, c := range []struct {
		path, body, code string
	}{
		{"/api/secrets/TEST_DATABASE_URL", jsonBody(map[string]string{"value": "x\nGH_TOKEN " + canaryRefused, "reach": "r"}), api.CodeSecretValueControl},
		{"/api/secrets/GH_TOKEN", jsonBody(map[string]string{"value": canaryRefused, "reach": "r"}), api.CodeSecretNameReserved},
		{"/api/secrets/TEST_X", jsonBody(map[string]string{"value": canaryRefused}), api.CodeSecretReachRequired},
		{"/api/secrets/TEST_X", `{"value":"` + canaryRefused + `","reach":"r","valu":1}`, api.CodeBadRequest},
		{"/api/secrets/TEST_X", `{"value":"` + canaryRefused, api.CodeBadRequest},
	} {
		b := call("PUT", c.path, c.body, 400)
		var e struct{ Error api.Error }
		json.Unmarshal([]byte(b), &e)
		if e.Error.Code != c.code {
			t.Errorf("PUT %s: code %q, want %q (%s)", c.path, e.Error.Code, c.code, b)
		}
	}
	call("DELETE", "/api/secrets/TEST_DATABASE_URL", "", 204)
	call("DELETE", "/api/secrets/TEST_DATABASE_URL", "", 404)
	if env, _, _ := r.export(t, wsGranted); strings.Contains(env, "TEST_DATABASE_URL") {
		t.Error("a deleted secret was delivered")
	}

	// Let the stream catch up with the last event, then stop everything so
	// the database is quiescent before its bytes are read.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sseMu.Lock()
		got := strings.Contains(sse.String(), "secret.deleted")
		sseMu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never carried secret.deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sseMu.Lock()
	transcript := sse.String()
	sseMu.Unlock()
	for _, kind := range []string{"secret.created", "secret.grants", "secret.rotated", "secret.deleted"} {
		if !strings.Contains(transcript, kind) {
			t.Errorf("the stream lacks %s", kind)
		}
	}

	// ---- the sweep ----
	sinks := map[string]string{"SSE transcript": transcript}
	for i, b := range bodies {
		sinks[fmt.Sprintf("response body %d", i)] = b
	}
	var tree []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSocket != 0 {
			return nil
		}
		if p == r.cfg.SecretsKey {
			return nil // the key's own file is the one place it lives
		}
		b, err := os.ReadFile(p)
		if err == nil {
			sinks["file "+strings.TrimPrefix(p, dir)] = string(b)
			tree = append(tree, p)
		}
		return nil
	})
	if len(tree) < 2 {
		t.Fatalf("control: the sweep found only %v", tree)
	}
	dbBytes := ""
	for _, suffix := range []string{"", "-wal"} {
		b, _ := os.ReadFile(r.cfg.DatabasePath + suffix)
		dbBytes += string(b)
	}
	if !strings.Contains(dbBytes, marker) || !strings.Contains(transcript, marker) {
		t.Fatal("control: the non-secret marker is not in the database bytes and the stream; the sweep read nothing")
	}
	needles := map[string]string{}
	for _, c := range []struct{ what, s string }{
		{"the canary", canary}, {"the rotated canary", canary2}, {"the refused canary", canaryRefused},
		{"the value", value}, {"the rotated value", value2},
	} {
		for form, s := range forms(c.s) {
			needles[c.what+" ("+form+")"] = s
		}
	}
	for form, s := range forms(string(masterKey)) {
		needles["the master key ("+form+")"] = s
	}
	for sink, content := range sinks {
		for what, needle := range needles {
			if strings.Contains(content, needle) {
				t.Errorf("%s holds %s", sink, what)
			}
		}
	}
	// The master key is in no environment: this process's (the server runs
	// in it) and the export's children's, which printed theirs above.
	self, _ := os.ReadFile("/proc/self/environ")
	for what, needle := range needles {
		if strings.HasPrefix(what, "the master key") && (strings.Contains(string(self), needle) || strings.Contains(env, needle)) {
			t.Errorf("an environment holds %s", what)
		}
	}
}

// §4: a repository the installation drops takes its secret grants with it,
// and one that comes back is granted nothing — including when a workspace
// held it at the drop. Then §12 keeps the grants while that workspace lives
// (it keeps working on what it already has), and its delete releases them.
// Through the real server: the catalog's refresh, the delete's Remove, and
// the broker's GET-SECRETS over a socket, so a grant the tables no longer
// hold but the broker's decrypted snapshot still does is caught too.
//
// Controls in the same function: the held workspace still receives the
// secret after the drop, the other repository's workspace receives it
// throughout, and the history (secret_access) of the deleted workspace is
// kept.
func TestReleasedRepositoryGrantsDoNotRevive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	masterKey := make([]byte, 32)
	rand.Read(masterKey)
	r := secretsServer(t, dir, masterKey)
	value := "deploy-" + randomAlnum(24)
	if _, err := r.srv.Secrets.Put(ctx, "DEPLOY_KEY", value, "deploys krelinga/alpha", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.srv.Secrets.SetGrants(ctx, "DEPLOY_KEY", []int64{101, 202}, false); err != nil {
		t.Fatal(err)
	}
	holds := func(ws string) bool {
		t.Helper()
		env, stderr, code := r.export(t, ws)
		if code != 0 || stderr != "" {
			t.Fatalf("export for %s: exit %d, stderr %q", ws, code, stderr)
		}
		return strings.Contains(env, "DEPLOY_KEY="+value+"\n")
	}
	refresh := func() {
		t.Helper()
		if _, err := r.srv.Catalog.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	grantsOn := func(repo int64) int {
		t.Helper()
		var n int
		if err := r.srv.DB.QueryRowContext(ctx, `SELECT count(*) FROM secret_grant WHERE repository_id = ?`, repo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if !holds(wsGranted) || !holds(wsUngranted) {
		t.Fatal("control: a granted workspace did not receive the secret")
	}
	alpha := r.gh.Installations[0].Repos[0]

	// The installation drops alpha while its workspace holds it: kept (§12).
	r.gh.Mu.Lock()
	r.gh.Installations[0].Repos = r.gh.Installations[0].Repos[1:]
	r.gh.Mu.Unlock()
	refresh()
	if !holds(wsGranted) {
		t.Error("control: the holding workspace lost its secrets when its repository left the installation (§12)")
	}

	// The workspace is deleted — the delete's last step — which releases it.
	if _, err := r.srv.DB.ExecContext(ctx, `UPDATE workspace SET state = 'deleting' WHERE id = ?`, wsGranted); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Workspaces.Remove(ctx, wsGranted); err != nil {
		t.Fatal(err)
	}
	if n := grantsOn(101); n != 0 {
		t.Errorf("the released repository still has %d grants after its workspace's delete", n)
	}
	refresh()

	// Re-added, and cloned again: granted nothing.
	r.gh.Mu.Lock()
	r.gh.Installations[0].Repos = append(r.gh.Installations[0].Repos, alpha)
	r.gh.Mu.Unlock()
	refresh()
	const wsAgain = "01JCCCCCCCCCCCCCCCCCCCCCCC"
	if _, err := r.srv.DB.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state)
		VALUES (?, 101, '/z', 'main', 'running')`, wsAgain); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Broker.Open(ctx, wsAgain); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(r.srv.Broker.SocketPath(wsAgain)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no broker socket for the re-added repository's workspace")
		}
	}
	if holds(wsAgain) {
		t.Error("a grant revived: the re-added repository's new workspace received the secret")
	}
	if n := grantsOn(101); n != 0 {
		t.Errorf("the re-added repository has %d grants", n)
	}
	if !holds(wsUngranted) || grantsOn(202) != 1 {
		t.Error("control: the still-installed repository lost its grant across the refreshes")
	}
	var history int
	r.srv.DB.QueryRowContext(ctx, `SELECT count(*) FROM secret_access WHERE workspace_id = ?`, wsGranted).Scan(&history)
	if history == 0 {
		t.Error("the deleted workspace's secret_access history is gone")
	}

	// The refresh's own sweep: beta is dropped while held, and its workspace
	// row then vanishes without Remove — as a release before this fix left
	// such rows. The next refresh deletes the grant, and the broker must
	// not keep serving it from its snapshot to beta's next workspace.
	beta := r.gh.Installations[0].Repos[0]
	r.gh.Mu.Lock()
	r.gh.Installations[0].Repos = r.gh.Installations[0].Repos[1:]
	r.gh.Mu.Unlock()
	refresh()
	if !holds(wsUngranted) {
		t.Error("control: beta's workspace lost its secrets when beta left the installation (§12)")
	}
	r.srv.Broker.Close(wsUngranted)
	if _, err := r.srv.DB.ExecContext(ctx, `DELETE FROM workspace WHERE id = ?`, wsUngranted); err != nil {
		t.Fatal(err)
	}
	refresh()
	if n := grantsOn(202); n != 0 {
		t.Errorf("the released repository beta still has %d grants after a refresh", n)
	}
	r.gh.Mu.Lock()
	r.gh.Installations[0].Repos = append(r.gh.Installations[0].Repos, beta)
	r.gh.Mu.Unlock()
	refresh()
	const wsBetaAgain = "01JDDDDDDDDDDDDDDDDDDDDDDD"
	if _, err := r.srv.DB.ExecContext(ctx, `INSERT INTO workspace (id, repository_id, host_path, branch, state)
		VALUES (?, 202, '/w', 'main', 'running')`, wsBetaAgain); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Broker.Open(ctx, wsBetaAgain); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(r.srv.Broker.SocketPath(wsBetaAgain)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no broker socket for beta's new workspace")
		}
	}
	if holds(wsBetaAgain) {
		t.Error("a grant revived: re-added beta's new workspace received the secret")
	}
}

// A master key file that others can read stops the server, as an open App
// key does (§13.5). Control: the same key at 0400 starts.
func TestRefusesAnOpenSecretsKey(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	cfg.SecretsKey = filepath.Join(dir, "secrets.key")
	key := make([]byte, 32)
	rand.Read(key)
	os.WriteFile(cfg.SecretsKey, key, 0o640)
	os.Chmod(cfg.SecretsKey, 0o640)
	if _, err := New(context.Background(), cfg, sys.Production()); err == nil || !strings.Contains(err.Error(), "0400") {
		t.Errorf("New with a 0640 key: %v", err)
	}
	os.Chmod(cfg.SecretsKey, 0o400)
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatalf("control: a 0400 key: %v", err)
	}
	if srv.Secrets == nil {
		t.Error("control: the store was not built")
	}
	srv.apiLn.Close()
	srv.prevLn.Close()
	srv.DB.Close()
}

// Without a master key the secret routes say so — they do not 501, and they
// do not pretend there are no secrets.
func TestSecretsWithoutAKey(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)
	resp := r.do(t, req{method: "GET", path: "/api/secrets", cookie: cookie})
	if resp.StatusCode != http.StatusServiceUnavailable || code(t, resp) != api.CodeSecretsNotConfigured {
		t.Errorf("GET /api/secrets without a key = %d", resp.StatusCode)
	}
}
