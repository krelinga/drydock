package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestTheNoticeCountsLockedOutAttemptsAndCapsItsSources: an attempt the
// lockout refused before the password was checked is a failed sign-in too,
// and is counted; the addresses named are the busiest MaxFailedSources, while
// the count covers every one. The control is the existing notice test: bad
// passwords alone, one address, named.
func TestTheNoticeCountsLockedOutAttemptsAndCapsItsSources(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.svc.SetPassword(ctx, pw); err != nil {
		t.Fatal(err)
	}
	busy := "192.0.2.66"
	for i := 0; i < DefaultPolicy.FreeAttempts; i++ {
		if _, err := r.svc.SignIn(ctx, busy, "wrong", ""); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := r.svc.SignIn(ctx, busy, "wrong", ""); !errors.As(err, new(ErrLockedOut)) {
		t.Fatalf("past the free attempts: %v; want locked out", err)
	}
	const others = 12
	for i := 0; i < others; i++ {
		if _, err := r.svc.SignIn(ctx, fmt.Sprintf("198.51.100.%d", i+1), "wrong", ""); !errors.Is(err, ErrBadPassword) {
			t.Fatal(err)
		}
	}
	res, err := r.svc.SignIn(ctx, "192.0.2.1", pw, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultPolicy.FreeAttempts + 1 + others; res.FailedSinceLastSignIn != want {
		t.Errorf("counted %d; want %d, the locked-out attempt included", res.FailedSinceLastSignIn, want)
	}
	if len(res.FailedSources) != MaxFailedSources || res.FailedSources[0] != busy {
		t.Errorf("sources %v; want %d, busiest first", res.FailedSources, MaxFailedSources)
	}
}
