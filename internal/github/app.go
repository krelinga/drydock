// Package github is Drydock's GitHub App client: the JWT that authenticates
// as the App, the installation tokens minted with it, and the few REST calls
// the catalog needs (design §9).
//
// Three rules from the design shape it.
//
//   - **The App key is the only stored credential** (§4, §13.5). It is read
//     once, from a file nobody else can read, and never put in the
//     environment — which leaks into /proc, crash reports, and every child.
//   - **Tokens live in memory only.** Installation tokens expire in an hour;
//     they are cached here, keyed by what they grant, and never written to
//     the database, an event, or a log. Token's String is "[redacted]", so the
//     accidental %v in an error message prints nothing useful.
//   - **Each token asks for what its caller needs** (§9.3). The App holds a
//     superset; a token for listing repositories asks for metadata and
//     nothing else.
package github

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// LoadKey reads the App's private key. It refuses a file that its group or
// anyone else can read: §13.5 specifies mode 0400, and a key that has been
// readable by others should be treated as one that has been read.
func LoadKey(path string) (*rsa.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("github app key %s is mode %#o: it must be readable by its owner alone (0400)", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	return ParseKey(b)
}

// ParseKey parses a PEM private key: PKCS#1, which is what GitHub issues, or
// PKCS#8.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app key: not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app key: neither a PKCS#1 nor a PKCS#8 RSA key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA key")
	}
	return rk, nil
}

// JWT returns the token that authenticates as the App itself: RS256, issued a
// minute in the past to absorb clock skew between this host and GitHub, and
// expiring nine minutes from now — under GitHub's ten-minute ceiling.
func JWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token is an installation token. Its value is reachable only through Value,
// and every formatting path prints "[redacted]", so a token that ends up in
// an error string or a log line by accident leaks nothing (§13.5).
type Token struct {
	value     string
	ExpiresAt time.Time
}

// NewToken wraps a token value; for the fake and for tests.
func NewToken(value string, expires time.Time) Token { return Token{value: value, ExpiresAt: expires} }

// Value is the token itself, for an Authorization header and nothing else.
func (t Token) Value() string { return t.value }

func (Token) String() string   { return "[redacted]" }
func (Token) GoString() string { return "github.Token{[redacted]}" }

// MarshalJSON refuses: a token has no business in any JSON Drydock writes.
func (Token) MarshalJSON() ([]byte, error) {
	return nil, errors.New("github: refusing to marshal a token")
}
