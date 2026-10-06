//go:build linux

package supervisor

import (
	"context"
	"testing"
	"time"
)

// Serve starts Watch beside serving, so a shutdown quick enough closes the
// event log before Watch has subscribed — which is what the v0.4.0 release's
// test run hit: Watch subscribed to a closed log, and its deferred Cancel
// closed the subscription a second time and panicked the process. Here the
// order is fixed rather than raced: the log closes, then Watch runs. It must
// return (the subscription is already over) and must not panic.
//
// The control is Watch on an open log, which keeps following it until its
// context ends — so the returning above is the closed log's doing.
func TestWatchOnAClosedLogReturns(t *testing.T) {
	r := newRig(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.m.Watch(ctx) }()
	select {
	case <-done:
		t.Fatal("control: Watch on an open log returned before its context ended")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("control: Watch did not return when its context ended")
	}

	r.log.Close()
	done = make(chan struct{})
	go func() { defer close(done); r.m.Watch(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch on a closed log is still running")
	}
}
