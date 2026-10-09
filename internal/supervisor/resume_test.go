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
