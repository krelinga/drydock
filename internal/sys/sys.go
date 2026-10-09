// Package sys holds the three things Drydock must not read from the ambient
// environment: the time, the disk, and randomness.
//
// Testing-plan §5.2 is the reason. Almost every rule in the design is a timing
// rule — lockout backoff, the 30-day absolute and 14-day idle session
// ceilings, the three-day expiry warning, the 60-second preview token TTL, the
// five-minute login deadline, the supervisor's 2s→60s backoff, the discovery
// debounce. A suite that tests those by sleeping takes nine minutes and flakes,
// and a flaky gate is a disabled gate.
//
// The real implementations are the zero-value defaults, so nothing
// production-facing changes shape to get this.
//
// # Rules and details
//
// Never call time.Now() directly; take a Clock. NewTimer is the Clock's After
// with a stop, for a bound or a period that may not be needed — a
// life.Group's Wait deadline, a life.Coalescer's interval — so it neither
// runs on nor is counted by a FakeClock's Waiting once it is not. Goroutine
// lifecycles are internal/life's, which, unlike this package, the context
// rule scans.
//
// DiskUsage answers two questions — how full a filesystem is (Usage, statfs of
// the nearest existing ancestor) and how much one directory holds (Size:
// allocated blocks as du counts them, hard links once, no symlink followed, no
// other filesystem entered, partial when some of it could not be read) — and
// HostDisk is the real one, FakeDisk a test's.
package sys

import (
	"context"
	"crypto/rand"
	"io"
	"time"
)

// Clock is the only source of time in the codebase. `time.Now()` called
// directly anywhere outside this file is a bug, because it is a dependency a
// test cannot move.
type Clock interface {
	Now() time.Time
	// Since is here rather than left to callers so that a fake clock cannot
	// be half-used: Now() from the fake and Since() from the real package
	// is the inconsistency that produces a test which passes at 3am only.
	Since(time.Time) time.Duration
	// After is the timer seam. A supervisor backoff or a login deadline
	// waits on this, so a test can expire it without waiting.
	After(time.Duration) <-chan time.Time
}

// RealClock is the production Clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// DiskUsage is the disk, as two questions: how full is the filesystem holding
// a path (the pre-flight check in design §12, which refuses a clone before
// starting rather than failing a three-minute build), and how much does one
// directory hold (the card's per-workspace disk, design §6 *Resources*).
//
// An interface because the interesting tests are the boundary ones — just over
// and just under the threshold — and arranging a real disk to be nearly full is
// not a thing a test should attempt.
type DiskUsage interface {
	// Usage reports bytes used and total for the filesystem holding path.
	Usage(path string) (used, total uint64, err error)
	// Size reports the bytes allocated to everything under path, as du
	// counts them: blocks actually allocated, each hard-linked file once,
	// symlinks not followed, other filesystems not entered. partial is true
	// when some of the tree could not be read — a directory a container's
	// root made private, say — or a bound was reached (ctx's deadline
	// among them), so the figure is a lower bound, and a caller must say so
	// rather than show it as exact.
	Size(ctx context.Context, path string) (bytes uint64, partial bool, err error)
}

// Random is the seam for slug minting (port forwarding §4) and ULID generation.
//
// Slugs exist so that deleting and re-adding a port produces a *different*
// URL, which means a stale bookmark fails closed. A test that wants to prove a
// retired slug is never reissued needs to force a collision, and that is only
// possible if the source is injectable.
//
// Note the production implementation is crypto/rand, not math/rand seeded from
// the clock: a preview slug is a capability URL, and a predictable one would
// let a guess reach another workspace's dev server.
type Random interface {
	io.Reader
}

// CryptoRandom is the production Random.
type CryptoRandom struct{}

func (CryptoRandom) Read(p []byte) (int, error) { return rand.Read(p) }

// Env bundles the three so a component takes one parameter rather than three,
// and so adding a fourth seam later does not re-shape every constructor.
type Env struct {
	Clock  Clock
	Disk   DiskUsage
	Random Random
}

// Production returns the real implementations.
func Production() Env {
	return Env{
		Clock:  RealClock{},
		Disk:   HostDisk{},
		Random: CryptoRandom{},
	}
}
