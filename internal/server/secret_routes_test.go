package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
)

// The secret routes' three request shapes for `value` — absent, "" and null —
// and the codes for prose, end to end through the real gate and store
// (frontend §4.5 #13, #14).

type secretsClient struct {
	t      *testing.T
	r      *running
	cookie string
}

func (c secretsClient) call(method, path, body string) (int, string) {
	c.t.Helper()
	resp := c.r.do(c.t, req{method: method, path: path, body: body, origin: uiOrigin, cookie: c.cookie})
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func errCode(body string) string {
	var e struct{ Error api.Error }
	json.Unmarshal([]byte(body), &e)
	return e.Error.Code
}

// An absent value keeps the stored one; an empty one is refused; a null one
// is a bad request. Three requests that differ in nothing but how `value` is
// spelt, three outcomes — and the export proves which value the workspace
// holds after each. The prose-only PUT is not a rotation; its control, a PUT
// with a new value, is.
func TestSecretPutAbsentEmptyAndNullValue(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	r := secretsServer(t, dir, key)
	c := secretsClient{t, r, r.signIn(t)}

	if st, b := c.call("PUT", "/api/secrets/TEST_KEY", `{"value":"v-one","reach":"a scratch bucket"}`); st != 200 {
		t.Fatalf("create = %d %s", st, b)
	}
	c.call("PUT", "/api/secrets/TEST_KEY/grants", `{"repository_ids":[101]}`)
	holds := func(v string) bool {
		env, _, code := r.export(t, wsGranted)
		return code == 0 && strings.Contains(env, "TEST_KEY="+v+"\n")
	}
	if !holds("v-one") {
		t.Fatal("control: the granted workspace does not hold the value")
	}

	// Absent: the prose moves, the value stays, nothing is stale.
	st, b := c.call("PUT", "/api/secrets/TEST_KEY", `{"reach":"only the scratch bucket","description":"see the console"}`)
	var res struct {
		Created, Rotated bool
		Secret           struct{ Reach, Description string }
		Stale            struct {
			NewCommands            []json.RawMessage `json:"new_commands"`
			NeedsSupervisorRestart []json.RawMessage `json:"needs_supervisor_restart"`
		}
	}
	json.Unmarshal([]byte(b), &res)
	if st != 200 || res.Created || res.Rotated || res.Secret.Reach != "only the scratch bucket" || res.Secret.Description != "see the console" ||
		res.Stale.NewCommands == nil || len(res.Stale.NewCommands)+len(res.Stale.NeedsSupervisorRestart) != 0 {
		t.Errorf("absent value = %d %s", st, b)
	}
	if !holds("v-one") {
		t.Error("an absent value changed the stored one")
	}

	// Empty: refused, and nothing changes — not the value, not the prose.
	if st, b := c.call("PUT", "/api/secrets/TEST_KEY", `{"value":"","reach":"reach from the empty PUT"}`); st != 400 || errCode(b) != api.CodeSecretValueEmpty {
		t.Errorf(`"value":"" = %d %s; want 400 %s`, st, b, api.CodeSecretValueEmpty)
	}
	// Null: neither, so a bad request.
	if st, b := c.call("PUT", "/api/secrets/TEST_KEY", `{"value":null,"reach":"reach from the null PUT"}`); st != 400 || errCode(b) != api.CodeBadRequest {
		t.Errorf(`"value":null = %d %s; want 400 %s`, st, b, api.CodeBadRequest)
	}
	if !holds("v-one") {
		t.Error("a refused PUT changed the stored value")
	}
	_, list := c.call("GET", "/api/secrets", "")
	if !strings.Contains(list, "only the scratch bucket") || strings.Contains(list, "from the empty PUT") || strings.Contains(list, "from the null PUT") {
		t.Errorf("a refused PUT changed the prose: %s", list)
	}

	// Control: a new value is a rotation, and reaches the next command.
	st, b = c.call("PUT", "/api/secrets/TEST_KEY", `{"value":"v-two","reach":"only the scratch bucket"}`)
	json.Unmarshal([]byte(b), &res)
	if st != 200 || !res.Rotated || len(res.Stale.NewCommands) != 1 {
		t.Errorf("control: a new value = %d %s", st, b)
	}
	if !holds("v-two") {
		t.Error("control: the rotated value did not reach the workspace")
	}

	// A new name with no value has nothing to keep; with one it is created.
	if st, b := c.call("PUT", "/api/secrets/OTHER_KEY", `{"reach":"r"}`); st != 400 || errCode(b) != api.CodeSecretValueRequired {
		t.Errorf("a new name with no value = %d %s; want 400 %s", st, b, api.CodeSecretValueRequired)
	}
	if st, b := c.call("PUT", "/api/secrets/OTHER_KEY", `{"value":"x","reach":"r"}`); st != 200 || !strings.Contains(b, `"created":true`) {
		t.Errorf("control: a new name with a value = %d %s", st, b)
	}
}

// Each over-long or blank field gets its own code over HTTP (frontend §4.5
// #14). Control: at the limit, accepted.
func TestSecretProseCodesOverHTTP(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	r := secretsServer(t, dir, key)
	c := secretsClient{t, r, r.signIn(t)}
	body := func(reach, description string) string {
		b, _ := json.Marshal(map[string]string{"value": "v", "reach": reach, "description": description})
		return string(b)
	}
	for _, x := range []struct{ body, want string }{
		{body("  ", ""), api.CodeSecretReachRequired},
		{body(strings.Repeat("r", 2001), ""), api.CodeSecretReachTooLong},
		{body("r", strings.Repeat("d", 4001)), api.CodeSecretDescriptionTooLong},
	} {
		if st, b := c.call("PUT", "/api/secrets/TEST_KEY", x.body); st != 400 || errCode(b) != x.want {
			t.Errorf("%d bytes: %d %s; want %s", len(x.body), st, b, x.want)
		}
	}
	if st, b := c.call("PUT", "/api/secrets/TEST_KEY", body(strings.Repeat("r", 2000), strings.Repeat("d", 4000))); st != 200 {
		t.Errorf("control: at the limits = %d %s", st, b)
	}
}

// A row the master key cannot open — a replaced key — is in GET /api/secrets
// from boot, every workspace's prelude fails closed while it is, and storing
// the value again clears it: the GET says so and the stream carries
// secret.deliverable. Control first, in the same function: the healthy server
// beside it reports `"undeliverable":null`.
func TestUndeliverableInGetAndClearedOnTheStream(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	healthy := secretsServer(t, t.TempDir(), key)
	hc := secretsClient{t, healthy, healthy.signIn(t)}
	if st, b := hc.call("GET", "/api/secrets", ""); st != 200 || !strings.Contains(b, `"undeliverable":null`) {
		t.Fatalf("control: a healthy GET = %d %s", st, b)
	}

	const lost = "01JYYYYYYYYYYYYYYYYYYYYYYY"
	r := secretsServer(t, t.TempDir(), key,
		`INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('`+lost+`', 'LOST_KEY', x'00112233445566778899', zeroblob(24), 'a scratch bucket')`,
		`INSERT INTO secret_grant (secret_id, repository_id) VALUES ('`+lost+`', 101)`)
	c := secretsClient{t, r, r.signIn(t)}

	st, b := c.call("GET", "/api/secrets", "")
	var got struct {
		Undeliverable *struct {
			Since   time.Time
			Secrets []struct{ Name, Reason string }
		}
	}
	json.Unmarshal([]byte(b), &got)
	if st != 200 || got.Undeliverable == nil || len(got.Undeliverable.Secrets) != 1 ||
		got.Undeliverable.Secrets[0].Name != "LOST_KEY" || got.Undeliverable.Secrets[0].Reason != "does_not_open" || got.Undeliverable.Since.IsZero() {
		t.Fatalf("GET with a broken row = %d %s", st, b)
	}
	// Fail closed: the granted workspace's command does not run, and the
	// ungranted one's does not either — the condition is fleet-wide.
	for _, ws := range []string{wsGranted, wsUngranted} {
		if _, _, code := r.export(t, ws); code != 69 {
			t.Errorf("workspace %s: export exit %d while undeliverable; want 69", ws, code)
		}
	}

	var sse bytes.Buffer
	var mu sync.Mutex
	stream := r.do(t, req{method: "GET", path: "/api/events", cookie: c.cookie})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stream.Body.Read(buf)
			mu.Lock()
			sse.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	transcript := func() string { mu.Lock(); defer mu.Unlock(); return sse.String() }
	waitFor := func(s string) bool {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if strings.Contains(transcript(), s) {
				return true
			}
		}
		return false
	}
	if !waitFor(": connected") {
		t.Fatal("the stream did not open")
	}

	// The repair: store the value again.
	if st, b := c.call("PUT", "/api/secrets/LOST_KEY", `{"value":"restored-v","reach":"a scratch bucket"}`); st != 200 || !strings.Contains(b, `"rotated":true`) {
		t.Fatalf("the repair = %d %s", st, b)
	}
	if !waitFor(`"kind":"secret.deliverable"`) {
		t.Errorf("the stream never carried secret.deliverable: %s", transcript())
	}
	if st, b := c.call("GET", "/api/secrets", ""); st != 200 || !strings.Contains(b, `"undeliverable":null`) {
		t.Errorf("GET after the repair = %d %s", st, b)
	}
	if env, _, code := r.export(t, wsGranted); code != 0 || !strings.Contains(env, "LOST_KEY=restored-v\n") {
		t.Errorf("after the repair: export exit %d", code)
	}
	if strings.Contains(transcript(), "restored-v") {
		t.Error("the stream carried the value")
	}
}
