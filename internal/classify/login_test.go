package classify

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two authorize URLs in the corpus. They come from two separate `auth
// login` runs, so code_challenge and state differ; every other byte agrees.
const (
	// login-code-prompt, login-invalid-code, login-url-1000col.
	urlWide = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins&code_challenge=rO0cyuzCnLmBmE0vHYDtZezw4CsEqGSOC0MNYu8hNKA&code_challenge_method=S256&state=c9vvfNWNMJJ4gR7Pl5HTz4wRFCgdNciV4XLSYJ9yRz4"
	// login-url-80col.
	urlNarrow = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins&code_challenge=bHyHCpKFo7q6GzR-OVZz6FIUSZ0M4p6Hkn4nHs_c83o&code_challenge_method=S256&state=7csoCynZ_B7PgmJaZO7tISsL_oKv48jtn5V-p-pRIrY"
)

func loginFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "transcripts", "claude-"+ClaudeCodeVersion, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func phaseName(p LoginPhase) string {
	return [...]string{"Starting", "AwaitingCode", "InvalidCode", "Success", "TimedOut"}[p]
}

func TestClassifyLoginFixtures(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		phase   LoginPhase
		url     string
	}{
		{"login-code-prompt", LoginAwaitingCode, urlWide},
		{"login-url-80col", LoginAwaitingCode, urlNarrow},
		// login-url-1000col is byte-identical to login-invalid-code: the
		// recorder captured the badcode run, so its verdict is the rejection.
		{"login-url-1000col", LoginInvalidCode, urlWide},
		{"login-invalid-code", LoginInvalidCode, urlWide},
		{"login-success-plain", LoginSuccess, ""},
		{"login-success-period", LoginSuccess, ""},
		{"login-success-press", LoginSuccess, ""},
		{"login-success-after-prompt", LoginSuccess, urlWide},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			got, err := ClassifyLogin(loginFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if got.Phase != tc.phase {
				t.Errorf("phase = %s, want %s", phaseName(got.Phase), phaseName(tc.phase))
			}
			if got.AuthorizeURL != tc.url {
				t.Errorf("url = %q\nwant  %q", got.AuthorizeURL, tc.url)
			}
		})
	}
}

// The 80- and 1000-column recordings must yield the same complete URL. They
// are two runs, so the per-run PKCE challenge and state differ; the rest is
// compared exactly, and the length is the 465 Spike 01 measured.
func TestClassifyLoginURLIsWidthIndependent(t *testing.T) {
	var got [2]*url.URL
	for i, name := range []string{"login-url-80col", "login-url-1000col"} {
		l, err := ClassifyLogin(loginFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := len(l.AuthorizeURL); n != 465 {
			t.Errorf("%s: URL is %d characters, want 465", name, n)
		}
		u, err := url.Parse(l.AuthorizeURL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[i] = u
	}
	a, b := got[0].Query(), got[1].Query()
	if len(a) != 8 || len(a) != len(b) {
		t.Fatalf("parameter sets differ in size: %d vs %d", len(a), len(b))
	}
	for k, v := range a {
		w, ok := b[k]
		switch {
		case !ok:
			t.Errorf("%s present at 80 columns only", k)
		case k == "state" || k == "code_challenge":
			if v[0] == w[0] {
				t.Errorf("%s identical across two runs; the fixtures are not independent recordings", k)
			}
		case v[0] != w[0]:
			t.Errorf("%s differs: %q vs %q", k, v[0], w[0])
		}
	}
	if got[0].Scheme+got[0].Host+got[0].Path != got[1].Scheme+got[1].Host+got[1].Path {
		t.Errorf("origin or path differs")
	}
}

// The most important negative: a fragment is never a usable URL. Every prefix
// of a real recording is classified, so wherever a PTY read happens to end —
// mid-escape, mid-parameter, mid-percent-escape — the verdict is either "no
// URL yet" or the complete one.
func TestClassifyLoginTruncatedStreamNeverYieldsFragment(t *testing.T) {
	for _, name := range []string{"login-code-prompt", "login-url-80col"} {
		full := loginFixture(t, name)
		want := urlWide
		if name == "login-url-80col" {
			want = urlNarrow
		}

		// Positive control: the whole stream is a usable URL at the prompt.
		l, err := ClassifyLogin(full)
		if err != nil || l.Phase != LoginAwaitingCode || l.AuthorizeURL != want {
			t.Fatalf("%s full: %+v, %v", name, l, err)
		}

		sawPartial := 0
		for n := 0; n < len(full); n++ {
			l, err := ClassifyLogin(full[:n])
			if err != nil {
				t.Fatalf("%s[:%d]: error %v (a cut stream is still arriving, not broken)", name, n, err)
			}
			if l.AuthorizeURL != "" && l.AuthorizeURL != want {
				t.Fatalf("%s[:%d]: fragment reported as URL: %q", name, n, l.AuthorizeURL)
			}
			if l.Phase == LoginAwaitingCode && l.AuthorizeURL != want {
				t.Fatalf("%s[:%d]: awaiting code without the complete URL", name, n)
			}
			if l.Phase != LoginStarting && l.Phase != LoginAwaitingCode {
				t.Fatalf("%s[:%d]: phase %s", name, n, phaseName(l.Phase))
			}
			// Count prefixes whose *visible* text already holds a partial URL
			// — the cuts a `!= ""` check would accept.
			vis := stripTerminal(full[:n])
			if strings.Contains(vis, "https://claude.com/cai/oauth/authorize?") && !strings.Contains(vis, want) {
				sawPartial++
				if l.AuthorizeURL != "" {
					t.Fatalf("%s[:%d]: partial visible URL yielded %q", name, n, l.AuthorizeURL)
				}
			}
		}
		// The loop must actually have exercised fragments, or it proves nothing.
		if sawPartial < 400 {
			t.Errorf("%s: only %d prefixes held a partial URL; the truncation sweep is not reaching the URL", name, sawPartial)
		}

		// The named case: cut in the middle of the state value.
		cut := bytes.LastIndex(full, []byte("&state=")) + len("&state=") + 10
		l, err = ClassifyLogin(full[:cut])
		if err != nil || l.Phase != LoginStarting || l.AuthorizeURL != "" {
			t.Errorf("%s cut mid-state: %+v, %v; want Starting with no URL", name, l, err)
		}
	}
}

// A URL on a complete line that cannot be used is an error: no later byte
// fixes it. The edit is applied to both copies (OSC 8 target and label).
func TestClassifyLoginBrokenURLOnCompleteLine(t *testing.T) {
	full := loginFixture(t, "login-code-prompt")

	l, err := ClassifyLogin(full)
	if err != nil || l.Phase != LoginAwaitingCode || l.AuthorizeURL != urlWide {
		t.Fatalf("positive control: %+v, %v", l, err)
	}

	const state = "&state=c9vvfNWNMJJ4gR7Pl5HTz4wRFCgdNciV4XLSYJ9yRz4"
	const method = "&code_challenge_method=S256"
	for _, tc := range []struct{ name, old, new string }{
		{"no state", state, ""},
		{"empty state", state, "&state="},
		{"state twice", state, state + state},
		{"no code_challenge", "&code_challenge=rO0cyuzCnLmBmE0vHYDtZezw4CsEqGSOC0MNYu8hNKA", ""},
		{"method plain", method, "&code_challenge_method=plain"},
		{"no client_id", "&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e", ""},
		{"response_type token", "&response_type=code", "&response_type=token"},
		{"no code=true", "code=true&", ""},
		{"no scope", "&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins", ""},
		{"redirect not https", "redirect_uri=https%3A", "redirect_uri=http%3A"},
		{"truncated percent escape", state, "&state=c9vv%3"},
		{"bad percent escape", state, "&state=%ZZ"},
		{"wrapped mid-scope", "user%3Ainference", "user%3Ainfe\r\nrence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Contains(full, []byte(tc.old)) {
				t.Fatalf("fixture lacks %q", tc.old)
			}
			b := bytes.ReplaceAll(full, []byte(tc.old), []byte(tc.new))
			l, err := ClassifyLogin(b)
			if !errors.Is(err, ErrAuthorizeURL) {
				t.Errorf("err = %v, want ErrAuthorizeURL", err)
			}
			if l.AuthorizeURL != "" || l.Phase != LoginStarting {
				t.Errorf("got %+v; a broken URL must not be usable", l)
			}
			if err != nil && strings.Contains(err.Error(), "c9vvfNWN") {
				t.Errorf("error carries URL contents: %v", err)
			}
		})
	}
}

// Origin and path cannot be reached through the stream, because the pattern
// would not match them at all; validAuthorizeURL is checked directly.
func TestValidAuthorizeURL(t *testing.T) {
	if err := validAuthorizeURL(urlWide); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	q := urlWide[strings.Index(urlWide, "?"):]
	for _, raw := range []string{
		"http://claude.com/cai/oauth/authorize" + q,
		"https://claude.co/cai/oauth/authorize" + q,
		"https://evil@claude.com/cai/oauth/authorize" + q,
		"https://claude.com/cai/oauth/authorise" + q,
		urlWide + "#frag",
		"https://claude.com/cai/oauth/authorize",
		"https://claude.com/cai/oauth/authorize?code=true",
	} {
		if err := validAuthorizeURL(raw); !errors.Is(err, ErrAuthorizeURL) {
			t.Errorf("%q: err = %v, want ErrAuthorizeURL", raw, err)
		}
	}
}

// The phase is the latest event; markers count only at the start of a segment
// (line start, or right after the prompt), and only as whole words. Every
// near-miss row is paired with the real marker in the same position.
func TestClassifyLoginPhase(t *testing.T) {
	prompt := loginFixture(t, "login-code-prompt")   // URL + prompt, unterminated
	invalid := loginFixture(t, "login-invalid-code") // … prompted > Invalid code.\r\n
	rej := []byte("Invalid code. Please make sure the full code was copied.\r\n")
	header := prompt[:bytes.LastIndex(prompt, []byte("Paste code"))] // URL, no prompt

	for _, tc := range []struct {
		name  string
		in    []byte
		phase LoginPhase
	}{
		{"empty", nil, LoginStarting},
		{"URL without prompt", header, LoginStarting},
		{"prompt without URL", []byte("Paste code here if prompted > "), LoginStarting},
		{"prompt with URL", prompt, LoginAwaitingCode},

		// The prompt is not re-printed after a rejection.
		{"second rejection, no prompt between", join(invalid, rej), LoginInvalidCode},
		{"rejection on its own line after prompt", join(prompt, []byte("\r\n"), rej), LoginInvalidCode},
		{"rejection with no prompt ever", join(header, rej), LoginStarting},
		{"rejection mid-prose", join(prompt, []byte("error: Invalid code. Retry\r\n")), LoginAwaitingCode},
		{"rejection mid-prose control", join(prompt, []byte("Invalid code. Retry\r\n")), LoginInvalidCode},

		// Latest event wins.
		{"rejected then success", join(invalid, []byte("Login successful.\r\n")), LoginSuccess},
		{"success then rejected", join(prompt, []byte("Login successful.\r\n"), rej), LoginInvalidCode},

		// Success marker: a prefix, at a segment start, as a whole word.
		{"success press", join(prompt, []byte("Login successful. Press any key to continue\r\n")), LoginSuccess},
		{"success unterminated", join(prompt, []byte("Login successful")), LoginSuccess},
		{"success indented", join(prompt, []byte("\r\n  Login successful.\r\n")), LoginSuccess},
		{"success coloured", join(prompt, []byte("\x1b[32mLogin successful\x1b[39m\r\n")), LoginSuccess},
		{"success after CR repaint", join(prompt, []byte("\rLogin successful.")), LoginSuccess},
		{"success mid-line", join(prompt, []byte("\r\nError: Login successful but credentials not saved\r\n")), LoginAwaitingCode},
		{"longer word", join(prompt, []byte("Login successfully completed\r\n")), LoginAwaitingCode},
		{"lowercase", join(prompt, []byte("login successful.\r\n")), LoginAwaitingCode},
		{"partial marker", join(prompt, []byte("Login success")), LoginAwaitingCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ClassifyLogin(tc.in)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if l.Phase != tc.phase {
				t.Errorf("phase = %s, want %s", phaseName(l.Phase), phaseName(tc.phase))
			}
			if l.Phase == LoginTimedOut {
				t.Errorf("ClassifyLogin returned LoginTimedOut, which is the caller's clock decision")
			}
		})
	}
}

// leaks reports whether s carries any 6-byte window of secret.
func leaks(s, secret string) bool {
	if len(secret) < 6 {
		return strings.Contains(s, secret)
	}
	for i := 0; i+6 <= len(secret); i++ {
		if strings.Contains(s, secret[i:i+6]) {
			return true
		}
	}
	return false
}

// TestValidateCodeShapeMakesNoCopy: the code is a one-time credential held in
// a slice its caller zeroes. Converting it to a string makes an immutable
// copy no one can zero, so the check reads the bytes in place and allocates
// nothing — which is what this measures, on a code the length of a real one
// (past the 32 bytes a non-escaping conversion may keep on the stack). The
// control: the same measurement sees the copy a string conversion makes.
func TestValidateCodeShapeMakesNoCopy(t *testing.T) {
	code := []byte(strings.Repeat("Kq7vZ2mXwP9rT4bNe8Jd", 4) + "#" + strings.Repeat("Hs3jY8cLdF6gA1eUo5Wy", 3) + "\n")
	var sink string
	if n := testing.AllocsPerRun(100, func() { sink = string(code) }); n < 1 {
		t.Fatalf("control: a string conversion measured %v allocations", n)
	}
	_ = sink
	if n := testing.AllocsPerRun(100, func() {
		if ValidateCodeShape(code) != nil {
			t.Fatal("setup: the code is refused")
		}
	}); n != 0 {
		t.Errorf("ValidateCodeShape allocated %v times per call: a copy of the code it cannot zero", n)
	}
}

func TestValidateCodeShape(t *testing.T) {
	// High-entropy canaries: any 6-byte window of either found in an error
	// is a leak.
	const l, r = "Kq7vZ2mXwP9rT4bNe8Jd", "Hs3jY8cLdF6gA1eUo5Wy"

	// Positive control for the leak check itself: a naive error that echoes
	// the code is caught, so a clean result below is not vacuous.
	if naive := fmt.Errorf("bad code %q", l+"#"); !leaks(naive.Error(), l) {
		t.Fatal("leak check cannot see an echoed code")
	}

	for _, ok := range []string{
		l + "#" + r,
		l + "#" + r + "\n",
		l + "#" + r + "\r\n",
		"  " + l + "#" + r + "\t",
		"a#b",
	} {
		if err := ValidateCodeShape([]byte(ok)); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}

	for _, tc := range []struct {
		name string
		code string
		want error
	}{
		{"empty", "", ErrCodeEmpty},
		{"blank", " \n", ErrCodeEmpty},
		{"no hash", l + r, ErrCodeNoSeparator},
		{"left half missing", "#" + r, ErrCodeHalfMissing},
		{"right half missing", l + "#", ErrCodeHalfMissing},
		{"right half missing, newline", l + "#\n", ErrCodeHalfMissing},
		{"two hashes", l + "#" + r + "#" + l, ErrCodeExtraHash},
		{"trailing hash", l + "#" + r + "#", ErrCodeExtraHash},
		{"space inside left", l[:8] + " " + l[8:] + "#" + r, ErrCodeBadCharacter},
		{"tab inside right", l + "#" + r[:8] + "\t" + r[8:], ErrCodeBadCharacter},
		{"newline inside", l + "\n#" + r, ErrCodeBadCharacter},
		{"ctrl-c", l + "\x03#" + r, ErrCodeBadCharacter},
		{"escape", l + "#" + r + "\x1b[A", ErrCodeBadCharacter},
		{"del", l + "#\x7f" + r, ErrCodeBadCharacter},
		{"non-ascii", l + "é#" + r, ErrCodeBadCharacter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCodeShape([]byte(tc.code))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, msg := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if leaks(msg, l) || leaks(msg, r) {
					t.Errorf("error leaks the code: %s", msg)
				}
			}
		})
	}
}
