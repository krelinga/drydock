// Package usage measures what each workspace is using — its container's
// memory and its disk — for the card (design §6, *Resources*; frontend §6.1),
// and how full the disk holding the workspaces is, for §12's *Disk full*.
//
// A measurement is not state. Nothing about a workspace changes when its
// memory moves, so nothing here writes the event log: persisted every
// half-minute per workspace, the samples would be most of the log and would
// push the events that matter out of the replay window. The latest values are
// kept in memory, carried by the workspace views, and published as one live,
// unpersisted `resources` frame per round (events.Broadcast). The browser's
// reducer is still their one writer; a missed frame is replaced by the next.
//
// Two loops, so neither waits on the other:
//
//   - Memory, every MemoryInterval (30 s): nothing at all while no workspace
//     is running; otherwise one `docker ps` by label, one `docker inspect`,
//     and one `docker stats --no-stream` naming every running container at
//     once. Memory moves on the scale of a build step or a test run, so half
//     a minute shows which workspace is the heavy one without making the
//     daemon compute stats continuously; `docker stats --no-stream` itself
//     takes about a second, since the daemon waits for a second CPU reading.
//     The filesystem holding the workspace root is read here too (statfs).
//   - Disk, checked on the same tick but due per workspace: every
//     DiskInterval (5 min) after its last attempt, at the next tick for a
//     workspace never attempted, and once more at the next tick after a
//     failure. Only the due workspaces are walked and only their containers
//     are asked for `docker inspect --size`, which makes the daemon walk
//     each layer. Each walk is bounded by WalkTimeout and by the walk's own
//     entry, depth and memory bounds (sys.HostDisk) — the tree is the
//     container's to shape — and ends at once when the sampler stops.
//
// Stopped workspaces keep their disk figure — they hold disk, and they are
// what an operator deletes to free some — but have no memory figure. A
// failed measurement keeps the last good value and marks it stale; it is never
// shown as zero, and never as current.
//
// Every frame and every view's copy carries a round number from this process
// and a boot id: the client orders copies by (boot, round), never by the wall
// clock, which can step backwards.
package usage

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	// DefaultMemoryInterval and DefaultDiskInterval are the two cadences;
	// the package comment gives the reasons.
	DefaultMemoryInterval = 30 * time.Second
	DefaultDiskInterval   = 5 * time.Minute
	// DefaultCallTimeout bounds each docker call, so a hung daemon makes a
	// stale reading rather than a stuck sampler.
	DefaultCallTimeout = 20 * time.Second
	// DefaultWalkTimeout bounds one workspace's directory walk. Past it the
	// figure is what was counted, partial.
	DefaultWalkTimeout = 30 * time.Second
	// FrameName is the stream's name for a round's frame.
	FrameName = "resources"
)

// Containers is what the sampler needs from container.Manager.
type Containers interface {
	List(ctx context.Context) ([]container.Found, error)
	Memory(ctx context.Context, ids []string) (map[string]uint64, error)
	WritableSizes(ctx context.Context, ids []string) (map[string]uint64, error)
}

// Workspaces lists the rows to measure.
type Workspaces interface {
	List(ctx context.Context) ([]workspace.Workspace, error)
}

// Frame is one round, as the stream carries it: every workspace with a row
// but a deleting one, by id, and the host's disk. Round and Boot order it
// against every other copy.
type Frame struct {
	Round      uint64                         `json:"round"`
	Boot       string                         `json:"boot"`
	At         time.Time                      `json:"at"`
	Workspaces map[string]workspace.Resources `json:"workspaces"`
	Host       *workspace.HostDisk            `json:"host"`
}

type diskState struct {
	sample    *workspace.DiskSample
	attempted time.Time
	failed    int // consecutive failed attempts
}

// Sampler measures on its two cadences until its context ends.
type Sampler struct {
	Workspaces Workspaces
	Containers Containers
	Disk       sys.DiskUsage
	Clock      sys.Clock
	// Random makes the boot id; nil uses crypto/rand.
	Random sys.Random
	// Root is the workspace root: a workspace's directory is Root/<id>.
	Root string
	// LimitPercent is config.DiskLimitPercent, reported with the host's
	// figures so the banner and the refusal use one number.
	LimitPercent int
	// Publish sends a round's frame to the stream (events.Log.Broadcast
	// under FrameName). Nil publishes nothing.
	Publish func(Frame)
	// Intervals and timeouts; zero means the defaults.
	MemoryInterval, DiskInterval, CallTimeout, WalkTimeout time.Duration
	Logf                                                   func(format string, args ...any)

	mu          sync.Mutex
	rows        map[string]bool // the live workspaces the last listing saw
	mem         map[string]*workspace.MemorySample
	disk        map[string]*diskState
	host        *workspace.HostDisk
	round       uint64
	boot        string
	listFailing bool
	pub         sync.Mutex // one snapshot-and-publish at a time, in round order
}

func (s *Sampler) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// failed logs a measurement's failure — unless the round was cut off by
// shutdown, which is no failure of the measurement.
func (s *Sampler) failed(ctx context.Context, f string, a ...any) {
	if ctx.Err() == nil {
		s.logf(f, a...)
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (s *Sampler) init() { // s.mu held
	if s.mem == nil {
		s.mem = map[string]*workspace.MemorySample{}
		s.disk = map[string]*diskState{}
		s.rows = map[string]bool{}
	}
	if s.boot == "" {
		b := make([]byte, 8)
		r := s.Random
		if r == nil {
			r = sys.CryptoRandom{}
		}
		if _, err := r.Read(b); err != nil {
			b = []byte(s.Clock.Now().Format("150405.000000")) // a fallback id, never a security value
		}
		s.boot = hex.EncodeToString(b)
	}
}

// Run samples at once and then every MemoryInterval until ctx ends, memory
// and disk each in their own loop. It returns only when both have stopped,
// and an in-flight walk ends with ctx.
func (s *Sampler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.loop(ctx, s.DiskRound)
	}()
	s.loop(ctx, s.MemoryRound)
	wg.Wait()
}

func (s *Sampler) loop(ctx context.Context, round func(context.Context) Frame) {
	for {
		round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(or(s.MemoryInterval, DefaultMemoryInterval)):
		}
	}
}

// Round is one memory round and one disk round, in that order: what a test
// drives directly rather than through the clock.
func (s *Sampler) Round(ctx context.Context) Frame {
	s.MemoryRound(ctx)
	return s.DiskRound(ctx)
}

// live lists the rows to measure, logging a failure once rather than every
// round, and saying when it clears.
func (s *Sampler) live(ctx context.Context) ([]workspace.Workspace, bool) {
	rows, err := s.Workspaces.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if err != nil {
		if !s.listFailing && ctx.Err() == nil {
			s.logf("drydock: usage: reading workspaces: %v (measurements stand until it recovers)", err)
		}
		s.listFailing = s.listFailing || ctx.Err() == nil
		return nil, false
	}
	if s.listFailing {
		s.logf("drydock: usage: reading workspaces works again")
		s.listFailing = false
	}
	var out []workspace.Workspace
	s.rows = map[string]bool{}
	for _, w := range rows {
		if w.State != workspace.Deleting {
			out = append(out, w)
			s.rows[w.ID] = true
		}
	}
	for id := range s.mem {
		if !s.rows[id] {
			delete(s.mem, id)
		}
	}
	for id := range s.disk {
		if !s.rows[id] {
			delete(s.disk, id)
		}
	}
	return out, true
}

// MemoryRound measures every running workspace's memory and the workspace
// filesystem, then publishes. It never waits on a walk.
func (s *Sampler) MemoryRound(ctx context.Context) Frame {
	now := s.Clock.Now()
	rows, ok := s.live(ctx)
	if !ok {
		return s.Snapshot()
	}
	running := []workspace.Workspace{}
	for _, w := range rows {
		if w.State == workspace.Running {
			running = append(running, w)
		}
	}
	next := map[string]*workspace.MemorySample{}
	if len(running) > 0 {
		cctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.CallTimeout, DefaultCallTimeout))
		found, err := s.Containers.List(cctx)
		var mem map[string]uint64
		var ids []string
		byWS := map[string][]container.Found{}
		if err == nil {
			for _, f := range found {
				byWS[f.WorkspaceID] = append(byWS[f.WorkspaceID], f)
			}
			for _, w := range running {
				for _, f := range byWS[w.ID] {
					if f.Running {
						ids = append(ids, f.ContainerID)
					}
				}
			}
			mem, err = s.Containers.Memory(cctx, ids)
		}
		cancel()
		if err != nil {
			s.failed(ctx, "drydock: usage: reading memory: %v", err)
		}
		s.mu.Lock()
		prev := s.mem
		s.mu.Unlock()
		for _, w := range running {
			if m := memoryOf(byWS[w.ID], mem, err, prev[w.ID], now); m != nil {
				next[w.ID] = m
			}
		}
	}
	var host *workspace.HostDisk
	if used, total, err := s.Disk.Usage(s.Root); err != nil {
		s.failed(ctx, "drydock: usage: the workspace filesystem: %v", err)
	} else {
		host = &workspace.HostDisk{UsedBytes: used, TotalBytes: total, LimitPercent: s.LimitPercent,
			Over: workspace.OverLimit(used, total, s.LimitPercent), At: now}
	}
	if ctx.Err() != nil {
		return s.Snapshot() // shutting down: keep the last whole round, publish nothing
	}
	s.mu.Lock()
	s.mem = next
	if host != nil {
		s.host = host
	}
	s.mu.Unlock()
	return s.publish(now)
}

// memoryOf is one running workspace's memory figure: the sum over its running
// containers (normally one), or — when the measurement failed — the last good
// figure marked stale. A running workspace whose container docker does not
// report as running has no figure: that is not a reading of zero.
func memoryOf(found []container.Found, mem map[string]uint64, err error,
	prev *workspace.MemorySample, now time.Time) *workspace.MemorySample {
	if err != nil {
		if prev == nil {
			return nil
		}
		c := *prev
		c.Stale = true
		return &c
	}
	var total uint64
	var ids []string
	for _, f := range found {
		if v, ok := mem[f.ContainerID]; ok && f.Running {
			total += v
			ids = append(ids, f.ContainerID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m := &workspace.MemorySample{Bytes: total, At: now}
	if len(ids) == 1 {
		// Which container it is, so a card whose workspace has moved to
		// another container since (a stop and a start inside one round)
		// does not show this one's figure as its own.
		m.ContainerID = ids[0]
	}
	return m
}

// due reports whether a workspace's disk should be measured now.
func (s *Sampler) due(st *diskState, now time.Time) bool {
	switch {
	case st == nil || st.attempted.IsZero():
		return true
	case st.failed == 1:
		return now.Sub(st.attempted) >= or(s.MemoryInterval, DefaultMemoryInterval)
	}
	return now.Sub(st.attempted) >= or(s.DiskInterval, DefaultDiskInterval)
}

// DiskRound measures the workspaces whose disk is due, then publishes if it
// measured any.
func (s *Sampler) DiskRound(ctx context.Context) Frame {
	now := s.Clock.Now()
	rows, ok := s.live(ctx)
	if !ok {
		return s.Snapshot()
	}
	var dueRows []workspace.Workspace
	s.mu.Lock()
	for _, w := range rows {
		if s.due(s.disk[w.ID], now) {
			dueRows = append(dueRows, w)
		}
	}
	s.mu.Unlock()
	if len(dueRows) == 0 {
		return s.Snapshot()
	}

	cctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.CallTimeout, DefaultCallTimeout))
	found, err := s.Containers.List(cctx)
	byWS := map[string][]container.Found{}
	var layers map[string]uint64
	if err == nil {
		var ids []string
		for _, f := range found {
			byWS[f.WorkspaceID] = append(byWS[f.WorkspaceID], f)
		}
		for _, w := range dueRows {
			for _, f := range byWS[w.ID] {
				ids = append(ids, f.ContainerID)
			}
		}
		layers, err = s.Containers.WritableSizes(cctx, ids)
	}
	cancel()
	if err != nil {
		s.failed(ctx, "drydock: usage: reading container sizes: %v", err)
	}

	for _, w := range dueRows {
		if ctx.Err() != nil {
			return s.Snapshot()
		}
		sample, ok := s.diskOf(ctx, w, byWS[w.ID], layers, err)
		if ctx.Err() != nil {
			return s.Snapshot() // a walk cut off by shutdown is no reading
		}
		s.mu.Lock()
		if s.rows[w.ID] {
			st := s.disk[w.ID]
			if st == nil {
				st = &diskState{}
				s.disk[w.ID] = st
			}
			st.attempted = now
			if ok {
				st.sample, st.failed = sample, 0
			} else {
				st.failed++
				if st.sample != nil {
					c := *st.sample
					c.Stale = true
					st.sample = &c
				}
			}
		}
		s.mu.Unlock()
	}
	return s.publish(now)
}

// diskOf is one workspace's disk: its directory and its containers' layers,
// and whether that is a reading. A workspace with no directory and no
// container (a create before step 1) is a reading of nothing: nil, true.
func (s *Sampler) diskOf(ctx context.Context, w workspace.Workspace, found []container.Found,
	layers map[string]uint64, layerErr error) (*workspace.DiskSample, bool) {
	wctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.WalkTimeout, DefaultWalkTimeout))
	dir, partial, err := s.Disk.Size(wctx, filepath.Join(s.Root, w.ID))
	cancel()
	missing := errors.Is(err, fs.ErrNotExist)
	if missing && len(found) == 0 {
		return nil, true
	}
	if err != nil && !missing {
		s.failed(ctx, "drydock: usage: workspace %s's directory: %v", w.ID, err)
		return nil, false
	}
	if layerErr != nil && len(found) > 0 {
		return nil, false
	}
	d := &workspace.DiskSample{DirectoryBytes: dir, Partial: partial, At: s.Clock.Now()}
	if len(found) > 0 {
		var c uint64
		for _, f := range found {
			v, ok := layers[f.ContainerID]
			if !ok {
				// Gone since the listing: what is left is a lower bound.
				d.Partial = true
				continue
			}
			c += v
		}
		d.ContainerBytes = &c
	}
	d.Bytes = d.DirectoryBytes
	if d.ContainerBytes != nil {
		d.Bytes += *d.ContainerBytes
	}
	return d, true
}

// publish numbers a snapshot of everything held and sends it. Under pub, so
// frames leave in round order whichever loop made them.
func (s *Sampler) publish(at time.Time) Frame {
	s.pub.Lock()
	defer s.pub.Unlock()
	s.mu.Lock()
	s.init()
	s.round++
	f := s.snapshotLocked(at)
	s.mu.Unlock()
	if s.Publish != nil {
		s.Publish(f)
	}
	return f
}

// Snapshot is the latest round: what the views carry.
func (s *Sampler) Snapshot() Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.snapshotLocked(time.Time{})
}

func (s *Sampler) snapshotLocked(at time.Time) Frame {
	f := Frame{Round: s.round, Boot: s.boot, At: at, Workspaces: map[string]workspace.Resources{}}
	for id := range s.rows {
		if r, ok := s.resourcesLocked(id); ok {
			f.Workspaces[id] = r
		}
	}
	if s.host != nil {
		h := *s.host
		h.Round, h.Boot = s.round, s.boot
		f.Host = &h
	}
	return f
}

func (s *Sampler) resourcesLocked(id string) (workspace.Resources, bool) {
	m, mok := s.mem[id]
	d, dok := s.disk[id]
	if !mok && !dok {
		return workspace.Resources{}, false
	}
	r := workspace.Resources{Round: s.round, Boot: s.boot}
	if mok {
		c := *m
		r.Memory = &c
	}
	if dok && d.sample != nil {
		c := *d.sample
		r.Disk = &c
	}
	return r, true
}

// Of is one workspace's latest resources, or nil when nothing was measured.
func (s *Sampler) Of(id string) *workspace.Resources {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	r, ok := s.resourcesLocked(id)
	if !ok {
		return nil
	}
	return &r
}

// HostDisk is the latest reading of the workspace filesystem, or nil.
func (s *Sampler) HostDisk() *workspace.HostDisk {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.host == nil {
		return nil
	}
	h := *s.host
	h.Round, h.Boot = s.round, s.boot
	return &h
}
