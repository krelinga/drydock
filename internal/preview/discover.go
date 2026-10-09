package preview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/sys"
)

// Discovery (PF §8.2, §13 step 5): what each running workspace's container is
// listening on, read from the host every few seconds, debounced, and merged
// onto the workspace's forwarded_port rows. Four rules, each with its test:
//
//   - **Discovery never enables anything.** The scan writes `observed`,
//     `bind_addr`, `observed_state` and the two timestamps — on a row it
//     creates, also the row itself, born off like every row. It has no path
//     to `enabled`: the container decides what it listens on, so a scanner
//     that could enable would hand that decision to the container (PF §10.7).
//   - **Discovery is not a prompt.** A port appearing is a row in the panel,
//     announced as any row change is (port.added, port.updated, port.retired,
//     whose `data.source` is "discovery"), at info level, and nothing else:
//     no toast, no badge, no count that asks for attention (PF §8.2, §12).
//   - **The container is resolved now, every scan** (PF §8.2, §10.7): the
//     ListenerSource resolves the workspace's container by label and its PID
//     from that same inspect, on every call. The scanner remembers no PID and
//     no address — only what it has seen, by port.
//   - **A restarting server does not churn the list.** A port appears after
//     AppearAfter consecutive scans see it at the same bind address, and goes
//     after Grace with no scan seeing it.
//
// A retired row stays retired: the merge is onto live rows only, so a port
// seen again after its row was retired is a new row with a new slug (PF §4).

// KindPortScanned answers a rescan (POST …/ports/rescan): one per workspace
// asked about, after the scan that began after the request, carrying
// `data.discovery` — "ok", or "unavailable" when the socket table could not be
// read. It describes no row change and moves nothing into reach.
const KindPortScanned = "port.scanned"

// SourceDiscovery is the `data.source` of every event the scan writes.
const SourceDiscovery = "discovery"

// Discovery's verdicts on a workspace's socket table, as port.scanned carries
// them.
const (
	DiscoveryOK          = "ok"
	DiscoveryUnavailable = "unavailable"
)

// Observed states (forwarded_port.observed_state).
const (
	StateListening = "listening"
	StateGone      = "gone"
)

// Discovery's bounds and cadence.
const (
	// DefaultScanInterval is the time between scans nobody asked for.
	DefaultScanInterval = 5 * time.Second
	// DefaultGrace is how long a listening port may go unseen before its
	// row says it is gone (PF §8.2's grace period).
	DefaultGrace = 15 * time.Second
	// AppearAfter is the consecutive scans, at the same bind address, that
	// make a port listening (PF §8.2: two).
	AppearAfter = 2
	// MaxObserved bounds the rows discovery alone holds per workspace —
	// observed, and neither declared, added by hand, enabled nor hidden — so
	// a test suite opening a socket per case cannot fill the 64 MaxPorts.
	MaxObserved = 32
	// maxTracked bounds the ports the scanner remembers per workspace.
	maxTracked = 4 * MaxPorts
)

// Listener is one listening socket in a workspace's container: its port and
// the address it is bound to (internal/container's Listener).
type Listener struct {
	Port int
	Addr netip.Addr
}

// ListenerSource reads a workspace's listening sockets now. In production it
// is internal/container's Listeners, which resolves the container by label —
// and its PID from the same inspect — on every call. Its errors:
// ErrNotRunning is "no running container", which the scan reads as nothing
// listening (PF §8.2: scanned as empty, never skipped with stale rows);
// ErrScanRaced is a read that raced the container stopping or restarting,
// which teaches the scan nothing; anything else is discovery unavailable for
// that workspace, which changes nothing either.
type ListenerSource interface {
	Listeners(ctx context.Context, workspaceID string) ([]Listener, error)
}

// ListenerFunc adapts a function to ListenerSource.
type ListenerFunc func(ctx context.Context, workspaceID string) ([]Listener, error)

// Listeners calls f.
func (f ListenerFunc) Listeners(ctx context.Context, workspaceID string) ([]Listener, error) {
	return f(ctx, workspaceID)
}

// ErrScanRaced: the socket table was read while the container stopped or
// restarted under it, so what was read may not have been the container's.
var ErrScanRaced = errors.New("preview: the container moved while its socket table was read")

// Scanner is the discovery scanner. Set the exported fields, then Start it
// under a life.Group; Rescan asks for a scan.
type Scanner struct {
	Registry *Service
	Source   ListenerSource
	Clock    sys.Clock
	// Interval is the time between scans nobody asked for, from the end of
	// one to the start of the next; zero is DefaultScanInterval, and a
	// negative one runs only the scans asked for (a test's).
	Interval time.Duration
	// Grace is DefaultGrace when zero.
	Grace time.Duration
	// Logf is the service log, told when a workspace's discovery becomes
	// unavailable and when it recovers — never every scan. Nil is standard
	// error.
	Logf func(string, ...any)

	w life.Coalescer[struct{}, string]
	// tracks is what the scans have seen, by workspace then port. Only the
	// worker touches it.
	tracks map[string]*wsTrack
}

type wsTrack struct {
	ports       map[int]*portTrack
	unavailable string // the reason last logged; "" while discovery works
}

type portTrack struct {
	bind     netip.Addr // the bind address the streak is at
	streak   int        // consecutive scans that saw it at bind
	lastSeen time.Time  // the last scan that saw it
}

// Start runs the scans under g until it stops: one now, then every Interval,
// and one for each Rescan.
func (s *Scanner) Start(g *life.Group) error {
	s.w.Work = s.run
	s.w.Clock = s.Clock
	s.w.Interval = s.Interval
	if s.w.Interval == 0 {
		s.w.Interval = DefaultScanInterval
	}
	if err := s.w.Start(g, "discovery"); err != nil {
		return err
	}
	_, _ = s.w.Trigger()
	return nil
}

// Rescan asks for a scan that begins after the call, and returns at once:
// POST …/ports/rescan answers 202 and is settled by the port.scanned that
// scan writes for this workspace. ErrNoWorkspace and ErrWorkspaceDeleting as
// the registry's writes refuse; life.ErrStopping once the scanner's group is
// stopping, and life.ErrNotStarted before Start.
func (s *Scanner) Rescan(ctx context.Context, workspaceID string) error {
	state, err := workspaceState(ctx, s.Registry.DB, workspaceID)
	if err != nil {
		return err
	}
	if state == "deleting" {
		return ErrWorkspaceDeleting
	}
	_, err = s.w.TriggerWith(workspaceID)
	return err
}

// Asked is how many scans have been asked for: a test's way to know a
// request has reached the worker without sleeping.
func (s *Scanner) Asked() life.Ticket { return s.w.Asked() }

func (s *Scanner) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
		return
	}
	fmt.Fprintf(os.Stderr, f+"\n", a...)
}

func (s *Scanner) grace() time.Duration {
	if s.Grace > 0 {
		return s.Grace
	}
	return DefaultGrace
}

// target is one workspace a round scans.
type target struct {
	id, state string
	listening map[int]string // live rows the registry says are listening, at which bind
}

// run is one round: every running workspace, every workspace with a row
// still listening (a stopped one is scanned as empty, so its rows go), and
// every one asked about.
func (s *Scanner) run(ctx context.Context, asks []string) (struct{}, error) {
	if s.tracks == nil {
		s.tracks = map[string]*wsTrack{}
	}
	asked := map[string]bool{}
	for _, id := range asks {
		asked[id] = true
	}
	targets, err := s.Registry.scanTargets(ctx, asked)
	if err != nil {
		if ctx.Err() == nil {
			s.logf("drydock: port discovery: listing workspaces: %v", err)
		}
		return struct{}{}, err
	}
	keep := map[string]bool{}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		keep[t.id] = true
		s.scanOne(ctx, t, asked[t.id])
	}
	for id := range s.tracks {
		if !keep[id] {
			delete(s.tracks, id)
		}
	}
	return struct{}{}, nil
}

func (s *Scanner) scanOne(ctx context.Context, t target, asked bool) {
	tr := s.tracks[t.id]
	if tr == nil {
		tr = &wsTrack{ports: map[int]*portTrack{}}
		s.tracks[t.id] = tr
	}
	var ls []Listener
	var err error
	if t.state == "running" {
		ls, err = s.Source.Listeners(ctx, t.id)
	} else {
		err = ErrNotRunning
	}
	if ctx.Err() != nil {
		return
	}
	var note *ScanNote
	if asked {
		note = &ScanNote{Discovery: DiscoveryOK}
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrNotRunning):
		ls = nil // scanned as empty: its rows go after their grace
	case errors.Is(err, ErrScanRaced):
		// Nothing learned; the next round reads again.
		s.observe(ctx, t.id, nil, note)
		return
	default:
		if tr.unavailable != err.Error() {
			s.logf("drydock: port discovery for workspace %s is unavailable: %v", t.id, err)
			tr.unavailable = err.Error()
		}
		if note != nil {
			note.Discovery = DiscoveryUnavailable
		}
		s.observe(ctx, t.id, nil, note)
		return
	}
	if tr.unavailable != "" {
		s.logf("drydock: port discovery for workspace %s works again", t.id)
		tr.unavailable = ""
	}
	sightings := s.debounce(tr, t, ls)
	if len(sightings) > 0 || note != nil {
		s.observe(ctx, t.id, sightings, note)
	}
}

func (s *Scanner) observe(ctx context.Context, id string, ss []Sighting, note *ScanNote) {
	if len(ss) == 0 && note == nil {
		return
	}
	if err := s.Registry.Observe(ctx, id, ss, note); err != nil && ctx.Err() == nil {
		s.logf("drydock: port discovery: recording what workspace %s listens on: %v", id, err)
	}
}

// debounce folds one scan into the workspace's track and returns what the
// registry should change: a port seen AppearAfter scans running at one bind
// address that the registry does not already list as listening there, and a
// listening port no scan has seen for Grace.
func (s *Scanner) debounce(tr *wsTrack, t target, ls []Listener) []Sighting {
	now := s.Clock.Now()
	seen := bindsByPort(ls)
	for port, bind := range seen {
		pt := tr.ports[port]
		if pt == nil {
			if len(tr.ports) >= maxTracked {
				continue
			}
			pt = &portTrack{}
			tr.ports[port] = pt
		}
		if pt.streak > 0 && pt.bind == bind {
			pt.streak++
		} else {
			pt.bind, pt.streak = bind, 1
		}
		pt.lastSeen = now
	}
	// A row the registry says is listening that no scan here has seen —
	// a restart of Drydock forgot its track — is given its grace from now.
	for port := range t.listening {
		if _, ok := tr.ports[port]; !ok {
			tr.ports[port] = &portTrack{lastSeen: now}
		}
	}
	var out []Sighting
	for port, pt := range tr.ports {
		_, isSeen := seen[port]
		bind, listed := t.listening[port]
		switch {
		case isSeen:
			if pt.streak >= AppearAfter && (!listed || bind != pt.bind.String()) {
				out = append(out, Sighting{Port: port, Listening: true, Bind: pt.bind.String(), At: now})
			}
		default:
			pt.streak = 0
			if !listed {
				// Nothing listed and nothing seen: nothing pending.
				delete(tr.ports, port)
				continue
			}
			if now.Sub(pt.lastSeen) >= s.grace() {
				out = append(out, Sighting{Port: port, Listening: false, At: pt.lastSeen})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// bindsByPort is each port's one bind address: the widest of the sockets on
// it — 0.0.0.0, then ::, then any other address, then loopback — so a port
// is loopback only when every socket on it is.
func bindsByPort(ls []Listener) map[int]netip.Addr {
	rank := func(a netip.Addr) int {
		switch {
		case a == netip.IPv4Unspecified():
			return 0
		case a.IsUnspecified():
			return 1
		case !a.Unmap().IsLoopback():
			return 2
		default:
			return 3
		}
	}
	out := map[int]netip.Addr{}
	for _, l := range ls {
		if l.Port < 1 || l.Port > 65535 || !l.Addr.IsValid() {
			continue
		}
		cur, ok := out[l.Port]
		if !ok || rank(l.Addr) < rank(cur) || rank(l.Addr) == rank(cur) && l.Addr.Less(cur) {
			out[l.Port] = l.Addr
		}
	}
	return out
}

// Sighting is the debounced scan's verdict on one port of a workspace.
type Sighting struct {
	Port int
	// Listening: seen at Bind, AppearAfter scans running. Otherwise gone:
	// unseen for the grace period, At being the last scan that saw it.
	Listening bool
	Bind      string
	At        time.Time
}

// ScanNote is what a rescan is owed: port.scanned, with the verdict.
type ScanNote struct {
	Discovery string
}

// scanTargets is the round's workspaces: every running one, every one with a
// live row still listening, and every one asked about — never one being
// deleted, whose rows are about to be retired with it.
func (s *Service) scanTargets(ctx context.Context, asked map[string]bool) ([]target, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.state, p.container_port, p.bind_addr
		FROM workspace w LEFT JOIN forwarded_port p
		  ON p.workspace_id = w.id AND p.retired_at IS NULL AND p.observed_state = 'listening'
		WHERE w.state <> 'deleting' ORDER BY w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*target{}
	var order []string
	for rows.Next() {
		var id, state string
		var port sql.NullInt64
		var bind sql.NullString
		if err := rows.Scan(&id, &state, &port, &bind); err != nil {
			return nil, err
		}
		t := byID[id]
		if t == nil {
			t = &target{id: id, state: state, listening: map[int]string{}}
			byID[id] = t
			order = append(order, id)
		}
		if port.Valid {
			t.listening[int(port.Int64)] = bind.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []target
	for _, id := range order {
		t := byID[id]
		if t.state == "running" || len(t.listening) > 0 || asked[id] {
			out = append(out, *t)
		}
	}
	return out, nil
}

// held reports whether something besides discovery keeps a row listed: the
// operator's switch, hand or hide, or the configuration's declaration.
func held(p Port) bool { return p.Enabled || p.Manual || p.Declared || p.Hidden }

// Observe writes one workspace's debounced scan onto its rows, in one
// transaction with the events that say so (events.Log.Commit), merged by
// container_port onto the live rows — declared, hand-added or discovered,
// never a retired one:
//
//   - A port listening that a live row names: that row is marked observed
//     and listening at its bind address (port.updated).
//   - A port listening that no live row names: a new row, observed and
//     **disabled**, with a freshly minted slug (port.added) — within
//     MaxPorts and MaxObserved, past which it is left unlisted.
//   - A port gone: a row something else holds (held) is marked gone
//     (port.updated); one only discovery held is retired, its slug spent
//     (port.retired), so the port seen again is a new row.
//
// Every event carries `data.source: "discovery"`. A note adds port.scanned.
// It never writes `enabled`. A workspace being deleted, or gone, is left
// alone.
func (s *Service) Observe(ctx context.Context, workspaceID string, ss []Sighting, note *ScanNote) error {
	var revoked []string
	err := s.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		revoked = nil
		state, err := workspaceState(ctx, tx, workspaceID)
		if errors.Is(err, ErrNoWorkspace) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if state == "deleting" {
			return nil, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+portCols+` FROM forwarded_port
			WHERE workspace_id = ? AND retired_at IS NULL`, workspaceID)
		if err != nil {
			return nil, err
		}
		live := map[int]Port{}
		observedOnly := 0
		for rows.Next() {
			p, err := s.scanPort(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			live[p.ContainerPort] = p
			if p.Observed && !held(p) {
				observedOnly++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var es []events.Event
		emit := func(kind, id, msg string) error {
			p, err := s.port(ctx, tx, workspaceID, id)
			if err != nil {
				return err
			}
			e, err := events.NewEvent(workspaceID, events.Info, kind, msg, map[string]any{"port": p, "source": SourceDiscovery})
			es = append(es, e)
			return err
		}
		for _, o := range ss {
			if o.Port < 1 || o.Port > 65535 {
				continue
			}
			cur, ok := live[o.Port]
			if o.Listening {
				if ok && cur.Observed && cur.ObservedState != nil && *cur.ObservedState == StateListening &&
					cur.BindAddr != nil && *cur.BindAddr == o.Bind {
					continue
				}
				id := cur.ID
				kind, msg := KindPortUpdated, fmt.Sprintf("Port %d is listening.", o.Port)
				if !ok {
					if len(live) >= MaxPorts || observedOnly >= MaxObserved {
						continue
					}
					id, err = s.insert(ctx, tx, workspaceID, o.Port, nil, "http", HostLocalhost, "observed")
					if err != nil {
						return nil, err
					}
					observedOnly++
					live[o.Port] = Port{ID: id}
					kind, msg = KindPortAdded, fmt.Sprintf("Port %d is listening. It is not previewed.", o.Port)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET observed = 1, observed_state = ?,
					bind_addr = ?, first_seen_at = coalesce(first_seen_at, ?), last_seen_at = ? WHERE id = ?`,
					StateListening, o.Bind, ts(o.At), ts(o.At), id); err != nil {
					return nil, err
				}
				if err := emit(kind, id, msg); err != nil {
					return nil, err
				}
				continue
			}
			if !ok || cur.ObservedState == nil || *cur.ObservedState != StateListening {
				continue
			}
			if held(cur) {
				if _, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET observed_state = ?, last_seen_at = ? WHERE id = ?`,
					StateGone, ts(o.At), cur.ID); err != nil {
					return nil, err
				}
				if err := emit(KindPortUpdated, cur.ID, fmt.Sprintf("Port %d stopped listening.", o.Port)); err != nil {
					return nil, err
				}
				continue
			}
			if err := retire(ctx, tx, cur.ID, s.Clock.Now()); err != nil {
				return nil, err
			}
			revoked = append(revoked, cur.ID)
			e, err := events.NewEvent(workspaceID, events.Info, KindPortRetired,
				fmt.Sprintf("Port %d stopped listening.", o.Port),
				map[string]any{"port_id": cur.ID, "container_port": cur.ContainerPort, "source": SourceDiscovery})
			if err != nil {
				return nil, err
			}
			es = append(es, e)
		}
		if note != nil {
			msg := "Ports scanned."
			if note.Discovery == DiscoveryUnavailable {
				msg = "Could not read what this workspace's container is listening on."
			}
			e, err := events.NewEvent(workspaceID, events.Info, KindPortScanned, msg,
				map[string]any{"discovery": note.Discovery, "source": SourceDiscovery})
			if err != nil {
				return nil, err
			}
			es = append(es, e)
		}
		return es, nil
	})
	if err == nil && s.Revoked != nil {
		for _, id := range revoked {
			s.Revoked(id)
		}
	}
	return err
}
