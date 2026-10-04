package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// CookieName is fixed by design §13.2. The __Host- prefix makes a browser
// refuse the cookie unless it is Secure, host-only, and Path=/, so no other
// host can set it. Spike 04 verified Chromium enforces all three.
const CookieName = "__Host-drydock"

// Lifetimes from design §13.2: long enough that a phone stays signed in
// between uses, short enough that a device you stopped carrying falls off.
const (
	AbsoluteLifetime = 30 * 24 * time.Hour
	IdleLifetime     = 14 * 24 * time.Hour
)

// tokenBytes is the cookie's entropy: 32 random bytes.
const tokenBytes = 32

// Session is one signed-in device. It carries no credential: ID is the SHA-256
// of the cookie, which is safe to show in a device list and useless as a
// cookie.
type Session struct {
	ID         string
	Label      string
	CreatedIP  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute ceiling; idle expiry is derived from LastSeenAt
}

// Sessions is the session store.
type Sessions struct {
	DB     *sql.DB
	Clock  sys.Clock
	Random io.Reader
}

// ErrNoSession means the cookie names no live session — absent, revoked, or
// expired. Callers must not distinguish these to the client: all three are 401.
var ErrNoSession = errors.New("no valid session")

// Create starts a session and returns the cookie value to set. That value
// exists only here and in the browser: what is stored is its SHA-256, so a
// stolen database file yields no usable cookie (§4).
func (s *Sessions) Create(ctx context.Context, label, ip string) (cookie string, _ Session, _ error) {
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(s.Random, raw); err != nil {
		return "", Session{}, fmt.Errorf("session token: %w", err)
	}
	cookie = base64.RawURLEncoding.EncodeToString(raw)
	now := s.Clock.Now().UTC()
	sess := Session{
		ID: hashToken(cookie), Label: label, CreatedIP: ip,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(AbsoluteLifetime),
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO auth_session (id, label, created_ip, created_at, last_seen_at, absolute_expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Label, sess.CreatedIP, ts(sess.CreatedAt), ts(sess.LastSeenAt), ts(sess.ExpiresAt))
	if err != nil {
		return "", Session{}, fmt.Errorf("store session: %w", err)
	}
	return cookie, sess, nil
}

// Lookup resolves a cookie to a live session and slides its idle window.
// An expired session is deleted on sight, so the table does not accumulate
// dead rows and a device list never shows one.
func (s *Sessions) Lookup(ctx context.Context, cookie string) (Session, error) {
	if cookie == "" {
		return Session{}, ErrNoSession
	}
	id := hashToken(cookie)
	sess, err := s.get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	now := s.Clock.Now().UTC()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(IdleLifetime)) {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM auth_session WHERE id = ?`, id)
		return Session{}, ErrNoSession
	}
	sess.LastSeenAt = now
	if _, err := s.DB.ExecContext(ctx, `UPDATE auth_session SET last_seen_at = ? WHERE id = ?`, ts(now), id); err != nil {
		return Session{}, fmt.Errorf("slide session: %w", err)
	}
	return sess, nil
}

// List returns every live session, newest first, for the device list.
func (s *Sessions) List(ctx context.Context) ([]Session, error) {
	now := s.Clock.Now().UTC()
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, coalesce(label,''), coalesce(created_ip,''), created_at, last_seen_at, absolute_expires_at
		 FROM auth_session ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		if now.Before(sess.ExpiresAt) && now.Before(sess.LastSeenAt.Add(IdleLifetime)) {
			out = append(out, sess)
		}
	}
	return out, rows.Err()
}

// Revoke ends one session by its ID (the hash, as shown in the device list).
func (s *Sessions) Revoke(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_session WHERE id = ?`, id)
	return err
}

// RevokeAll ends every session: design §13.2's one-click response to a lost
// phone.
func (s *Sessions) RevokeAll(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_session`)
	return err
}

func (s *Sessions) get(ctx context.Context, id string) (Session, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, coalesce(label,''), coalesce(created_ip,''), created_at, last_seen_at, absolute_expires_at
		 FROM auth_session WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	return sess, err
}

type scanner interface{ Scan(...any) error }

func scanSession(r scanner) (Session, error) {
	var sess Session
	var created, seen, expires string
	if err := r.Scan(&sess.ID, &sess.Label, &sess.CreatedIP, &created, &seen, &expires); err != nil {
		return Session{}, err
	}
	var err error
	if sess.CreatedAt, err = parseTS(created); err != nil {
		return Session{}, err
	}
	if sess.LastSeenAt, err = parseTS(seen); err != nil {
		return Session{}, err
	}
	if sess.ExpiresAt, err = parseTS(expires); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// hashToken is the only transformation between a cookie and a row. Hex, so
// the canary sweep can look for the expected hit and assert the plaintext is
// absent (testing §4.2).
func hashToken(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])
}

// Timestamps are RFC 3339 with nanoseconds, UTC, so they sort as text.
func ts(t time.Time) string               { return t.UTC().Format(time.RFC3339Nano) }
func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
