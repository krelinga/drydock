package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestSignInReportsFailedAttemptsOnce is design §12's *Repeated failed
// sign-ins*: after two bad passwords from one address, the next successful
// sign-in answers 200 naming the count and the address, still setting the
// cookie; the sign-in after that — nothing failed in between — is the plain
// 204, as is a first sign-in with no failures (the control).
func TestSignInReportsFailedAttemptsOnce(t *testing.T) {
	r := start(t)
	if err := r.srv.Auth.SetPassword(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	signIn := func(pw, ip string) (int, string, bool) {
		resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin, ip: ip,
			body: fmt.Sprintf(`{"password":%q}`, pw)})
		cookie := false
		for _, c := range resp.Cookies() {
			cookie = cookie || c.Name == "__Host-drydock"
		}
		return resp.StatusCode, strings.TrimSpace(readBody(t, resp)), cookie
	}
	if st, body, ok := signIn(password, "192.0.2.10"); st != 204 || body != "" || !ok {
		t.Fatalf("control, nothing failed: %d %q cookie=%v", st, body, ok)
	}
	for i := 0; i < 2; i++ {
		if st, _, _ := signIn("wrong", "192.0.2.66"); st != 401 {
			t.Fatalf("bad password: %d", st)
		}
	}
	st, body, ok := signIn(password, "192.0.2.10")
	if st != 200 || body != `{"failed_attempts":2,"failed_sources":["192.0.2.66"]}` || !ok {
		t.Errorf("after two failures: %d %s cookie=%v", st, body, ok)
	}
	if st, body, _ := signIn(password, "192.0.2.10"); st != 204 || body != "" {
		t.Errorf("the notice repeated: %d %q", st, body)
	}
}
