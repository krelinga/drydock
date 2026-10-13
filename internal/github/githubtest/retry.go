package githubtest

import (
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/github"
)

// Attempts and delay for RetryPositiveControl: three tries, five seconds of
// waiting in all.
const (
	retryAttempts = 3
	retryDelay    = 2500 * time.Millisecond
)

// NotFoundLag reports whether err looks like GitHub's "not found" for a
// resource that exists: a 404 from the API, or git's "Repository not found"
// in its output. Right after a token is minted GitHub can answer this for a
// few seconds before the token's reach has propagated.
func NotFoundLag(err error) bool {
	return err != nil && (github.IsNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "not found"))
}

// RetryPositiveControl runs fn, a call EXPECTED TO SUCCEED, and retries it
// (up to three attempts, ~5 s of real time, each retry logged) only on the
// live backend and only when it fails with a not-found (NotFoundLag).
//
// NEVER wrap a negative assertion in this: a call expected to be refused or
// to find nothing is the scoping test itself, and a retry there could turn a
// real scoping bug into a pass. On the fake it is exactly one call and never
// sleeps.
func (b Backend) RetryPositiveControl(t testing.TB, what string, fn func() error) error {
	t.Helper()
	err := fn()
	for attempt := 2; b.Live && attempt <= retryAttempts && NotFoundLag(err); attempt++ {
		t.Logf("%s: %v; retrying (attempt %d of %d) in case GitHub's not-found lags a fresh token", what, err, attempt, retryAttempts)
		time.Sleep(retryDelay)
		err = fn()
	}
	return err
}
