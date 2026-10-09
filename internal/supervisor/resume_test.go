//go:build linux

package supervisor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
)

// A sign-in that lands while a loop has read the identity as signed out but
// not yet parked is not lost: Resume finds the loop running, tells it, and
// the loop goes round again instead of parking — then reads the live login
// and serves. The loop is held between its read and its park; that window is
// where a check of running() alone (the old resumeWaiting) skipped the loop,
// which then parked, waiting for a sign-in that had already happened. The
// window matters more now that a resume can come as a busy job ends
// (internal/provision's owed resume), which is just after a step 8 or a
// restart launched the loop. The control is the same held loop released with
// no Resume in the window: it parks in awaiting_login and starts nothing.
func TestASignInBesideAParkIsNotLost(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			r := newRig(t)
			r.claude(claudetest.Step{Mode: claudetest.RCServe})
			var mu sync.Mutex
			state, calls := "blanked", 0
			entered, release := make(chan struct{}), make(chan struct{})
			r.m.Identity = func(context.Context) (string, bool) {
				mu.Lock()
				calls++
				n, st := calls, state
				mu.Unlock()
				if n == 2 { // the loop's first read; the first is the launch's
					close(entered)
					<-release
				}
				return st, true
			}
			r.start()
			<-entered
			if resume {
				mu.Lock()
				state = "ok"
				mu.Unlock()
				if err := r.m.Resume(context.Background(), wsID); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if !resume {
				r.waitState(AwaitingLogin, ReasonSignedOut)
				time.Sleep(200 * time.Millisecond)
				if n := r.invocations(); n != 0 {
					t.Fatalf("control: %d starts with no sign-in", n)
				}
				return
			}
			r.waitState(Serving, ReasonServing)
			if n := r.invocations(); n != 1 {
				t.Errorf("%d starts; want 1", n)
			}
		})
	}
}

// TestASignInDuringARefusedRunIsNotLost is round 1 of #112's review: a
// sign-in that lands while a server is running — starting, not
// awaiting_login, so AwaitingLogin does not list it and Resume leaves it
// alone — and that server then exits with the organization refusal (whose own
// sentence asks for a sign-in). SignedIn, which OnChange calls first, tells
// every running loop, so the refusal's park goes round again under the new
// login and the next server serves. The control is the same refusal with no
// sign-in: it parks in awaiting_login, no_organization, after one start.
func TestASignInDuringARefusedRunIsNotLost(t *testing.T) {
	for _, signIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("signIn=%v", signIn), func(t *testing.T) {
			r := newRig(t)
			r.claude(
				claudetest.Step{Mode: claudetest.RCRefuseNoOrganization, ExitAfter: dur(1500 * time.Millisecond), Times: 1},
				claudetest.Step{Mode: claudetest.RCServe},
			)
			r.m.Identity = func(context.Context) (string, bool) { return "ok", true }
			r.start()
			r.waitFor(10*time.Second, "the first server running", func() bool { return r.invocations() == 1 })
			if signIn {
				// As the server's OnChange does it: tell the running loops,
				// then resume the parked ones (none: it is starting).
				r.m.SignedIn()
				if got := r.m.AwaitingLogin(); len(got) != 0 {
					t.Fatalf("AwaitingLogin mid-run = %v, want none", got)
				}
				r.waitState(Serving, ReasonServing)
				if n := r.invocations(); n != 2 {
					t.Errorf("%d starts; want 2", n)
				}
				return
			}
			r.waitState(AwaitingLogin, ReasonNoOrganization)
			time.Sleep(500 * time.Millisecond)
			if n := r.invocations(); n != 1 {
				t.Errorf("control: %d starts; want 1", n)
			}
		})
	}
}
