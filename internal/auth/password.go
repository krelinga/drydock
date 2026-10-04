// Package auth is Drydock's one credential check: an operator password, a
// session cookie, and the lockout in front of both (design §13.2).
//
// It is deliberately the least sophisticated design that is correct here — one
// operator, no enrollment, no second service — and the effort goes into the
// places that design §13.2 says matter: a password hash that is expensive for a
// guesser, a session store with nothing in it worth stealing, and a lockout that
// one noisy device cannot use to lock the operator out on its own.
package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id's cost parameters. They are encoded into every hash, so
// raising them later does not invalidate existing hashes — Verify reads the
// parameters a hash was made with, and reports when they are below the floor
// so the caller can rehash on the next successful sign-in.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams aims at design §13.2's "tune the cost high — 250 ms of CPU per
// sign-in is invisible to you and ruinous to a guesser". 64 MiB is the memory
// cost that does most of that work: it is what makes a GPU or ASIC guesser
// expensive, where iterations alone mostly cost the defender.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// Floor is the weakest parameter set Verify accepts without flagging a rehash,
// and the weakest Hash will produce. It exists so the cost cannot be lowered
// silently — testing §8.1: "argon2id cost is not silently lowered".
var Floor = Params{Memory: 64 * 1024, Time: 3, Threads: 1, SaltLen: 16, KeyLen: 32}

// ErrWeakParams is returned by Hash when asked to go below Floor.
var ErrWeakParams = errors.New("argon2id parameters are below the configured floor")

// Hash returns a PHC-format argon2id string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
//
// The password is never logged and never returned in an error.
func Hash(password string, p Params, random io.Reader) (string, error) {
	if !p.atLeast(Floor) {
		return "", ErrWeakParams
	}
	salt := make([]byte, p.SaltLen)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify checks a password against an encoded hash in constant time.
// needsRehash is true when the hash's parameters are below Floor, so the caller
// can upgrade it while it has the plaintext in hand.
func Verify(encoded, password string) (ok, needsRehash bool, err error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, !p.atLeast(Floor), nil
}

// ParamsOf reports the parameters an encoded hash was made with.
func ParamsOf(encoded string) (Params, error) {
	p, _, _, err := decode(encoded)
	return p, err
}

func decode(encoded string) (Params, []byte, []byte, error) {
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errors.New("not an argon2id hash")
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, fmt.Errorf("bad argon2 parameters %q", parts[3])
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errors.New("bad argon2 salt encoding")
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, errors.New("bad argon2 key encoding")
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}

func (p Params) atLeast(f Params) bool {
	return p.Memory >= f.Memory && p.Time >= f.Time && p.Threads >= f.Threads &&
		p.SaltLen >= f.SaltLen && p.KeyLen >= f.KeyLen
}
