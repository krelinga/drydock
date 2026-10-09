//go:build linux

package login

import (
	"context"
	"os"
	"time"

	"bytes"
	"crypto/rand"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/sys"
	"sync"
	"testing"
)

// chunk is n bytes of PTY output, none of them zero, so a chunk that reads
// all zero was zeroed and not merely never written.
func chunk(t *testing.T, n int) []byte {
	t.Helper()
	c := make([]byte, n)
	rand.Read(c)
	for i := range c {
		c[i] |= 1
	}
	return c
}

func zeroed(c []byte) bool { return bytes.Count(c, []byte{0}) == len(c) }

// TestStreamStopZeroesWhatIsBuffered: chunks the reader handed over that
// drive had not received when it returned are zeroed by its stop, not left
// in a channel nobody reads. The control is that they were whole before.
func TestStreamStopZeroesWhatIsBuffered(t *testing.T) {
	st := newStream()
	var put [][]byte
	for i := 0; i < 10; i++ {
		c := chunk(t, 64)
		put = append(put, c)
		st.put(c)
	}
	for i, c := range put {
		if zeroed(c) {
			t.Fatalf("control: chunk %d was zeroed before stop", i)
		}
	}
	st.stop()
	for i, c := range put {
		if !zeroed(c) {
			t.Errorf("chunk %d, buffered when drive returned, was not zeroed", i)
		}
	}
	if len(st.c) != 0 {
		t.Errorf("%d chunks still buffered after stop", len(st.c))
	}
}

// TestStreamPutAfterStopZeroes: a chunk read after drive has returned is
// zeroed by put, whichever way its select goes — refused at quit, or sent
// into the buffer's room after stop's drain had run, which is the case only
// put's own look at quit catches. Each way is a coin toss, so the test
// tosses it many times. The control is a put before stop, which is handed
// over whole.
func TestStreamPutAfterStopZeroes(t *testing.T) {
	st := newStream()
	before := chunk(t, 32)
	st.put(before)
	if got := <-st.c; !bytes.Equal(got, before) || zeroed(got) {
		t.Fatal("control: a chunk put before stop was not handed over whole")
	}
	st.stop()
	for i := 0; i < 1000; i++ {
		c := chunk(t, 32)
		st.put(c)
		if !zeroed(c) {
			t.Fatalf("put %d after stop left its chunk unzeroed", i)
		}
		if len(st.c) != 0 {
			t.Fatalf("put %d after stop left a chunk in the buffer", i)
		}
	}
}

// TestStreamStopRacingTheReader: a reader putting while drive stops, with
// nothing receiving, leaves every chunk zeroed however the two interleave.
func TestStreamStopRacingTheReader(t *testing.T) {
	for round := 0; round < 200; round++ {
		st := newStream()
		var put [][]byte
		for i := 0; i < 8; i++ {
			put = append(put, chunk(t, 16))
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range put {
				st.put(c)
			}
		}()
		st.stop()
		wg.Wait()
		for i, c := range put {
			if !zeroed(c) {
				t.Fatalf("round %d: chunk %d was not zeroed", round, i)
			}
		}
	}
}

// TestDriveStopsItsStreamOnReturn: once drive has returned, its reader keeps
// draining and zeroing, so a PTY that keeps writing never blocks and the
// reader ends with the group. Without drive's deferred stop the reader parks
// in put after 16 buffered chunks, the pipe fills, and the writer below times
// out. The control is that the same writes would otherwise need a reader: the
// pipe holds far less than is written.
func TestDriveStopsItsStreamOnReturn(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	g := life.NewGroup(context.Background())
	m := &Manager{Clock: sys.RealClock{}}
	s := &session{m: m, g: g, submit: make(chan submission), cancelReq: make(chan struct{}),
		view: View{ID: "lg_test", Phase: Starting}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, out := s.drive(ctx, &Proc{Master: r}, nil)
	if out.problem != ProblemShutdown {
		t.Fatalf("drive ended %+v, want a shutdown", out)
	}

	// 64 KiB of pipe + 17 chunks of 4 KiB would be all that fits with nobody
	// reading; write well beyond it, with the write end still open.
	const total = 512 << 10
	data := bytes.Repeat([]byte{'x'}, 4096)
	if err := w.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for sent := 0; sent < total; sent += len(data) {
		if _, err := w.Write(data); err != nil {
			t.Fatalf("after %d bytes the writer stalled, so nothing reads the PTY after drive returned: %v", sent, err)
		}
	}

	w.Close()
	if left := g.Wait(time.After(5 * time.Second)); len(left) != 0 {
		t.Fatalf("goroutines left in the group: %v", left)
	}
	r.Close()
}
