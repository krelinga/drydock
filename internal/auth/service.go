package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// Errors SignIn returns. The HTTP layer maps all three of ErrBadPassword,
// ErrLockedOut and ErrNotConfigured to responses that say nothing about the
// password itself.
var (
	ErrBadPassword = errors.New("wrong password")
	// ErrNotConfigured means no operator password exists yet. There is no
	// HTTP route that can fix that, by design (§13.2): `drydock passwd` on
	// the host is the only way, which removes the whole class of
	// "unauthenticated bootstrap endpoint left enabled" bugs.
	ErrNotConfigured = errors.New("no operator password is set; run `drydock passwd` on the host")
)

// Service is the sign-in flow: lockout, password, session.
type Service struct {
	DB       *sql.DB
	Clock    sys.Clock
	Random   io.Reader
	Params   Params
	Sessions *Sessions
	Limiter  *Limiter
}

// New wires a Service with the production defaults.
func New(db *sql.DB, env sys.Env) *Service {
	return &Service{
		DB: db, Clock: env.Clock, Random: env.Random, Params: DefaultParams,
		Sessions: &Sessions{DB: db, Clock: env.Clock, Random: env.Random},
		Limiter:  &Limiter{DB: db, Clock: env.Clock, Policy: DefaultPolicy},
	}
}

// SignInResult is a successful sign-in plus the failed-attempt notice.
type SignInResult struct {
	Cookie  string
	Session Session
	// FailedSinceLastSignIn and FailedSources are bad-password attempts since
	// the previous successful sign-in from anywhere, shown once.
	FailedSinceLastSignIn int
	FailedSources         []string
}

// SignIn checks the lockout, then the password, then starts a session.
//
// The order matters: the lockout is consulted before the password is hashed,
// so a locked-out guesser cannot spend 250 ms of server CPU per attempt.
func (s *Service) SignIn(ctx context.Context, ip, password, label string) (SignInResult, error) {
	d, err := s.Limiter.Check(ctx, ip)
	if err != nil {
		return SignInResult{}, err
	}
	if !d.Allowed {
		if err := s.Limiter.Record(ctx, ip, OutcomeLockedOut); err != nil {
			return SignInResult{}, err
		}
		return SignInResult{}, ErrLockedOut{RetryAfter: d.RetryAfter, Global: d.Global}
	}

	var hash string
	switch err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM operator WHERE id = 1`).Scan(&hash); {
	case errors.Is(err, sql.ErrNoRows):
		return SignInResult{}, ErrNotConfigured
	case err != nil:
		return SignInResult{}, fmt.Errorf("read operator: %w", err)
	}

	ok, rehash, err := Verify(hash, password)
	if err != nil {
		return SignInResult{}, fmt.Errorf("stored password hash: %w", err)
	}
	if !ok {
		if err := s.Limiter.Record(ctx, ip, OutcomeBadPassword); err != nil {
			return SignInResult{}, err
		}
		return SignInResult{}, ErrBadPassword
	}

	// The notice covers failures since the previous success, so read the
	// previous success before recording this one.
	var prev sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT max(at) FROM auth_attempt WHERE outcome = 'ok'`).Scan(&prev); err != nil {
		return SignInResult{}, err
	}
	since := time.Time{}
	if prev.Valid {
		if t, err := parseTS(prev.String); err == nil {
			since = t
		}
	}
	if err := s.Limiter.Record(ctx, ip, OutcomeOK); err != nil {
		return SignInResult{}, err
	}
	failed, sources, err := s.Limiter.FailuresSince(ctx, since)
	if err != nil {
		return SignInResult{}, err
	}

	// Upgrade a hash made with weaker parameters while the plaintext is in
	// hand: the only moment that is possible.
	if rehash {
		if err := s.storeHash(ctx, password); err != nil {
			return SignInResult{}, err
		}
	}

	cookie, sess, err := s.Sessions.Create(ctx, label, ip)
	if err != nil {
		return SignInResult{}, err
	}
	return SignInResult{Cookie: cookie, Session: sess, FailedSinceLastSignIn: failed, FailedSources: sources}, nil
}

// SetPassword is `drydock passwd`. It also ends every session: a password is
// changed because it may be known to someone else, and a change that leaves
// that someone signed in has not done its job.
func (s *Service) SetPassword(ctx context.Context, password string) error {
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters: it is the only credential between the LAN and code execution on this host (§13.5)")
	}
	if err := s.storeHash(ctx, password); err != nil {
		return err
	}
	return s.Sessions.RevokeAll(ctx)
}

func (s *Service) storeHash(ctx context.Context, password string) error {
	h, err := Hash(password, s.Params, s.Random)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO operator (id, password_hash, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		h, ts(s.Clock.Now().UTC()))
	return err
}
