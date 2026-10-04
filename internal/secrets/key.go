// Package secrets is the repository-secrets store (design §10): values sealed
// with XChaCha20-Poly1305 under a master key, the secret's id as associated
// data, granted per repository with nothing granted by default, and resolved
// for the broker's GET-SECRETS without a decryption per call.
//
// Three rules hold everywhere in this package, and each is an invariant
// rather than a style:
//
//   - **No value leaves except to the broker.** Nothing here returns a value
//     to an HTTP handler, an event, an error string, or a log line. Meta has
//     no field for one; Resolve is the only way out, and only the broker
//     calls it.
//   - **Validate on write** (§10.1). A value is single-line, printable, and
//     non-empty; a name is an environment variable name nobody else relies
//     on. Rejecting at write is the choice §10.1 argues for: the write is a
//     human with an error message, delivery is a shell prelude that can only
//     fail or guess.
//   - **Default deny.** No secret_grant row and no all_repos, no secret.
package secrets

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
)

// KeySize is the master key's length: XChaCha20-Poly1305 takes 256 bits.
const KeySize = 32

// Key is the master key. It formats as [redacted] and refuses to marshal, so
// a careless %v or a struct dumped to JSON cannot print it — the same
// discipline as github.Token.
type Key struct{ b [KeySize]byte }

func (Key) String() string   { return "[redacted]" }
func (Key) GoString() string { return "secrets.Key{[redacted]}" }
func (Key) MarshalJSON() ([]byte, error) {
	return nil, errors.New("secrets: the master key is never marshalled")
}
func (Key) MarshalText() ([]byte, error) {
	return nil, errors.New("secrets: the master key is never marshalled")
}

// Equal compares two keys in constant time.
func (k *Key) Equal(o *Key) bool { return subtle.ConstantTimeCompare(k.b[:], o.b[:]) == 1 }

// NewKey wraps raw key bytes; for tests and for LoadKey.
func NewKey(raw []byte) (*Key, error) {
	if len(raw) != KeySize {
		return nil, fmt.Errorf("secrets: a master key is %d bytes, not %d", KeySize, len(raw))
	}
	k := &Key{}
	copy(k.b[:], raw)
	return k, nil
}

// LoadKey reads the master key from its file, once, at startup (§10.2,
// §13.5). The file holds exactly 32 raw bytes — the installer writes them
// from /dev/urandom — and must be readable by its owner alone: a key file its
// group or anyone else can read is refused, as github.LoadKey refuses the App
// key. Never an environment variable: those reach /proc, crash reports, and
// every child process.
func LoadKey(path string) (*Key, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets master key: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secrets master key: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets master key %s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secrets master key %s is mode %#o: it must be readable by its owner alone (0400)", path, perm)
	}
	// One byte more than a key, so a longer file is told apart from a key.
	b, err := io.ReadAll(io.LimitReader(f, KeySize+1))
	if err != nil {
		return nil, fmt.Errorf("secrets master key: %w", err)
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("secrets master key %s must hold exactly %d raw bytes (head -c %d /dev/urandom)", path, KeySize, KeySize)
	}
	k, err := NewKey(b)
	clear(b)
	return k, err
}
