package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// Outcome is what an attempt recorded in auth_attempt was.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeBadPassword Outcome = "bad_password"
	// OutcomeLockedOut is an attempt refused before the password was even
	// checked. It is logged, because design §13.2 says to log every attempt,
	// but it never counts as a failure — otherwise a locked-out device would
	// extend its own lockout forever just by retrying.
	OutcomeLockedOut Outcome = "locked_out"
)

// Policy is the lockout's tuning. Design §13.2 fixes the shape — exponential
// backoff per source IP *plus* a global cap — and leaves the numbers, so they
// are named here rather than buried in arithmetic.
//
// Why both: a per-IP counter alone is defeated by a botnet, and a global cap
// alone lets one noisy device (a phone with a stale saved password) lock the
// operator out. With both, one device runs into its own backoff long before
// it can move the global count, and only many sources at once trip the global
// cap.
type Policy struct {
	FreeAttempts int           // consecutive failures from one IP before backoff starts
	BaseDelay    time.Duration // the first enforced wait
	MaxDelay     time.Duration // the backoff's ceiling
	Window       time.Duration // failures older than this are forgotten per IP
	GlobalCap    int           // failures across every IP within GlobalWindow
	GlobalWindow time.Duration
}

// DefaultPolicy: five free tries per device, then 1s, 2s, 4s … up to 15 minutes;
// and a global lock if fifty failures land within fifteen minutes from anywhere.
var DefaultPolicy = Policy{
	FreeAttempts: 5,
	BaseDelay:    time.Second,
	MaxDelay:     15 * time.Minute,
	Window:       time.Hour,
	GlobalCap:    50,
	GlobalWindow: 15 * time.Minute,
}

// Limiter reads and writes auth_attempt. Its state is the table, not memory,
// so a restart does not reset a lockout in progress.
type Limiter struct {
	DB     *sql.DB
	Clock  sys.Clock
	Policy Policy
}

// Decision is the limiter's answer for one attempt.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	// Global reports that the global cap, not this IP's backoff, refused it.
	Global bool
}

// Check decides whether an attempt from ip may test a password now.
func (l *Limiter) Check(ctx context.Context, ip string) (Decision, error) {
	now := l.Clock.Now().UTC()
	p := l.Policy

	// Global first: when it is tripped, it applies to every source.
	var global int
	if err := l.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM auth_attempt WHERE outcome = 'bad_password' AND at > ?`,
		ts(now.Add(-p.GlobalWindow))).Scan(&global); err != nil {
		return Decision{}, fmt.Errorf("global attempt count: %w", err)
	}
	if global >= p.GlobalCap {
		// Lifts when the oldest failure inside the window ages out.
		var oldest string
		if err := l.DB.QueryRowContext(ctx,
			`SELECT at FROM auth_attempt WHERE outcome = 'bad_password' AND at > ?
			 ORDER BY at DESC LIMIT 1 OFFSET ?`,
			ts(now.Add(-p.GlobalWindow)), p.GlobalCap-1).Scan(&oldest); err != nil {
			return Decision{}, fmt.Errorf("global lockout horizon: %w", err)
		}
		t, err := parseTS(oldest)
		if err != nil {
			return Decision{}, err
		}
		return Decision{RetryAfter: positive(t.Add(p.GlobalWindow).Sub(now)), Global: true}, nil
	}

	// Per IP: consecutive failures since this IP's last success, within the
	// window. A success wipes the slate, which is what "consecutive" means.
	var lastOK sql.NullString
	if err := l.DB.QueryRowContext(ctx,
		`SELECT max(at) FROM auth_attempt WHERE source_ip = ? AND outcome = 'ok'`, ip).Scan(&lastOK); err != nil {
		return Decision{}, fmt.Errorf("last success: %w", err)
	}
	since := now.Add(-p.Window)
	if lastOK.Valid {
		if t, err := parseTS(lastOK.String); err == nil && t.After(since) {
			since = t
		}
	}
	var failures int
	var lastFail sql.NullString
	if err := l.DB.QueryRowContext(ctx,
		`SELECT count(*), max(at) FROM auth_attempt
		 WHERE source_ip = ? AND outcome = 'bad_password' AND at > ?`, ip, ts(since)).
		Scan(&failures, &lastFail); err != nil {
		return Decision{}, fmt.Errorf("per-IP attempt count: %w", err)
	}
	if failures < p.FreeAttempts || !lastFail.Valid {
		return Decision{Allowed: true}, nil
	}
	t, err := parseTS(lastFail.String)
	if err != nil {
		return Decision{}, err
	}
	wait := backoff(p, failures-p.FreeAttempts)
	if left := t.Add(wait).Sub(now); left > 0 {
		return Decision{RetryAfter: left}, nil
	}
	return Decision{Allowed: true}, nil
}

// Record logs one attempt. Every attempt is logged, refused ones included —
// design §12's "repeated failed sign-ins" notice is read from this table.
func (l *Limiter) Record(ctx context.Context, ip string, o Outcome) error {
	_, err := l.DB.ExecContext(ctx,
		`INSERT INTO auth_attempt (source_ip, outcome, at) VALUES (?, ?, ?)`,
		ip, string(o), ts(l.Clock.Now().UTC()))
	return err
}

// FailuresSince reports bad-password attempts after t, for the notice the
// sign-in screen shows once after a successful sign-in (frontend §8): a stale
// saved password on a forgotten device is the usual cause, and the operator
// wants to see it either way.
func (l *Limiter) FailuresSince(ctx context.Context, t time.Time) (count int, sources []string, err error) {
	rows, err := l.DB.QueryContext(ctx,
		`SELECT source_ip, count(*) FROM auth_attempt
		 WHERE outcome = 'bad_password' AND at > ? GROUP BY source_ip ORDER BY count(*) DESC`, ts(t))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip string
		var n int
		if err := rows.Scan(&ip, &n); err != nil {
			return 0, nil, err
		}
		count += n
		sources = append(sources, ip)
	}
	return count, sources, rows.Err()
}

// backoff is BaseDelay·2^n, capped at MaxDelay without overflowing.
func backoff(p Policy, n int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < n; i++ {
		if d >= p.MaxDelay/2 {
			return p.MaxDelay
		}
		d *= 2
	}
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

func positive(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return d
}

// ErrLockedOut is returned by SignIn when the limiter refuses an attempt.
type ErrLockedOut struct {
	RetryAfter time.Duration
	Global     bool
}

func (e ErrLockedOut) Error() string {
	return fmt.Sprintf("too many failed sign-ins; retry after %s", e.RetryAfter.Round(time.Second))
}

// IsLockedOut reports whether err is a lockout refusal.
func IsLockedOut(err error) (ErrLockedOut, bool) {
	var e ErrLockedOut
	ok := errors.As(err, &e)
	return e, ok
}
