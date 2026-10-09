package preview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/sys"
)

// Discovery (PF §8.2, §13 step 5): what each running workspace's container is
// listening on, read from the host every few seconds, debounced, and merged
// onto the workspace's forwarded_port rows. The rules, each with its test:
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
//     after Grace with no scan seeing it. A row discovery alone holds is
//     marked gone then, and retired only after RetireAfter more, so a server
//     that comes back keeps its row and its slug.
//   - **Nothing a container does grows the log without bound.** Each
//     workspace has two token buckets: every row discovery writes spends a
//     change (ChangeBurst, then one per ChangeEvery), and a new row a mint
//     as well (MintBurst, then one per MintEvery), the last few of each
//     (MintReserve, ChangeReserve) only for ports below EphemeralFrom. A
//     change past its budget waits for the next scan that can afford it;
//     nothing is lost, because each scan derives what to change afresh from
//     the rows.
//   - **A broken or held-back scanner fails visibly** (PF §11, §13 step 6).
//     An empty list and an unreadable table must not look alike, nor a port
//     listening and one withheld by a budget: each workspace's state — ok,
//     unavailable, limited — is reported by port.scanned to a rescan and by
//     port.discovery unasked when it changes, and Discovery is the last
//     report, which the port list carries.
//   - **One workspace cannot stall the others.** Each read is bounded by
//     Timeout on the injected clock; one cut off is that workspace's
//     discovery unavailable for that round, and the round goes on.
//
// A retired row stays retired: the merge is onto live rows only, so a port
// seen again after its row was retired is a new row with a new slug (PF §4).

// KindPortScanned answers a rescan (POST …/ports/rescan): one per workspace
// asked about, after the scan that began after the request, carrying
// `data.discovery` — "ok"; "unavailable" when the socket table could not be
// read; "limited" when a budget held back a change the scan found (PF §11).
// It describes no row change and moves nothing into reach.
const KindPortScanned = "port.scanned"

// KindPortDiscovery says a workspace's discovery changed between ok,
// unavailable and limited without anyone asking — `data.discovery` as
// port.scanned carries it — so an open ports panel says so (PF §11's
// *discovery unavailable*, and a budget holding changes back). Both kinds are
// reports of one per-workspace state, and the port list carries its latest
// value as `discovery`. At most StatusBurst, then one per StatusEvery, per
// workspace: a Docker that fails every other round cannot flood the log.
const KindPortDiscovery = "port.discovery"

// SourceDiscovery is the `data.source` of every event the scan writes.
const SourceDiscovery = events.SourceDiscovery

// Discovery's verdicts on a workspace's socket table, as port.scanned carries
// them.
const (
	DiscoveryOK          = "ok"
	DiscoveryUnavailable = "unavailable"
	// DiscoveryLimited: the socket table was read, and a budget held back
	// at least one change it called for — a port listening but not listed
	// yet, or one gone but not marked so.
	DiscoveryLimited = "limited"
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
	// RetireAfter is how long a row discovery alone holds stays listed,
	// gone, before it is retired and its slug spent: a server that comes
	// back within it gets the same row.
	RetireAfter = 10 * time.Minute
	// MintBurst and MintEvery are each workspace's budget of new rows:
	// MintBurst at once, then one per MintEvery (twelve an hour), so a
	// container cycling ports spends at most that many slugs — about 300 a
	// day, as before PF §13 step 6 raised the burst from 8.
	MintBurst = 12
	MintEvery = 5 * time.Minute
	// MintReserve of those tokens only a port below EphemeralFrom may
	// spend. A server a test binds to port 0 gets an ephemeral port, and a
	// dev server a fixed low one (3000, 5173, 8000, 8080), so a test run
	// that lists eight short-lived servers leaves four mints for the dev
	// server started after it, which appears at once rather than up to
	// MintEvery later (#120's review, round 2).
	MintReserve = 4
	// ChangeBurst and ChangeEvery are each workspace's budget of discovery
	// writes — every row change, so every event: ChangeBurst at once, then
	// one per ChangeEvery (120 an hour, under 3,000 a day).
	ChangeBurst = 64
	ChangeEvery = 30 * time.Second
	// ChangeReserve of those only a port below EphemeralFrom may spend, for
	// the same reason as MintReserve.
	ChangeReserve = 8
	// EphemeralFrom is the bottom of Linux's default ip_local_port_range,
	// where a server that binds port 0 is put.
	EphemeralFrom = 32768
	// StatusBurst and StatusEvery bound KindPortDiscovery per workspace. A
	// change of state past the budget is reported when it allows, as the
	// state is then.
	StatusBurst = 4
	StatusEvery = 5 * time.Minute
	// DefaultScanTimeout bounds one workspace's read: its four docker calls
	// and the table.
	DefaultScanTimeout = 15 * time.Second
	// IdleInterval is the most often a round nobody asked for runs while
	// Watched says nobody is looking.
	IdleInterval = time.Minute
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
	// Timeout bounds one workspace's read, on Clock; zero is
	// DefaultScanTimeout. A read cut off is discovery unavailable for that
	// workspace and that round, and the round goes on to the next.
	Timeout time.Duration
	// Watched reports whether anyone is looking — the server's is "an SSE
	// stream is open". While nobody is, a round nobody asked for runs at
	// most every IdleInterval, so an unwatched server does not exec docker
	// several times a second around the clock. Nil is always watched.
	Watched func() bool
	// Logf is the service log, told when a workspace's discovery becomes
	// unavailable and when it recovers, and when its budget first holds a
	// change back — never every scan. Nil is standard error.
	Logf func(string, ...any)

	w life.Coalescer[struct{}, string]
	// tracks is what the scans have seen, by workspace then port, and each
	// workspace's budgets. Only the worker touches it.
	tracks map[string]*wsTrack
	// lastFull is when the last round ran, for Watched's back-off.
	lastFull time.Time

	// mu guards reports, which the port list reads (Discovery) while the
	// worker writes it.
	mu sync.Mutex
	// reports is each workspace's discovery state as last reported — by a
	// port.scanned or a port.discovery — and the budget of the latter. It
	// outlives a workspace's track (a stopped workspace keeps what was last
	// said of it, so a start that finds discovery working says so) and goes
	// only with the workspace.
	reports map[string]*report

	// afterObserve, when set, runs after each Registry write a round makes
	// returns: a test's way to read the port list in the window between an
	// event's publication and the end of the round.
	afterObserve func()
}

type report struct {
	state  string // DiscoveryOK, DiscoveryUnavailable or DiscoveryLimited
	budget bucket // StatusEvery, StatusBurst
	// raced: state is a raced rescan's "unavailable", which read nothing
	// and is not discovery's state; the next round that reads the table
	// reports what it finds whatever the budget says.
	raced bool
}

// Discovery is a workspace's discovery state as the events last reported it:
// DiscoveryOK until something else has been said. The port list carries it,
// so a panel opened after the event says the same as one open before it.
func (s *Scanner) Discovery(workspaceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.reports[workspaceID]; r != nil {
		return r.state
	}
	return DiscoveryOK
}

type wsTrack struct {
	ports       map[int]*portTrack
	unavailable string // the reason last logged; "" while discovery works
	limited     bool   // a change was held back by a budget, and logged
	mints       bucket // new rows (MintEvery, MintBurst)
	changes     bucket // every row write (ChangeEvery, ChangeBurst)
}

type portTrack struct {
	bind     netip.Addr // the bind address the streak is at
	streak   int        // consecutive scans that saw it at bind
	lastSeen time.Time  // the last scan that saw it
}

// bucket is a token bucket on the injected clock: full (burst) at first,
// one token back every `every`.
type bucket struct {
	tokens float64
	at     time.Time
	init   bool
}

func (b *bucket) refill(now time.Time, every time.Duration, burst int) {
	if !b.init {
		b.tokens, b.at, b.init = float64(burst), now, true
		return
	}
	if d := now.Sub(b.at); d > 0 {
		b.tokens += float64(d) / float64(every)
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.at = now
	}
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

func (s *Scanner) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultScanTimeout
}

// liveRow is what a round needs of one live forwarded_port row.
type liveRow struct {
	state        string // observed_state, "" when never seen
	bind         string
	observedOnly bool      // observed, and nothing else holds it (held)
	lastSeen     time.Time // last_seen_at; zero when unset
}

// target is one workspace a round scans, with its live rows by port.
type target struct {
	id, state string
	rows      map[int]liveRow
}

// pending reports whether a row waits on discovery without a running
// container: still listening (to go), or discovery's own and gone (to be
// retired after RetireAfter).
func (r liveRow) pending() bool {
	return r.state == StateListening || r.observedOnly && r.state == StateGone
}

// run is one round: every running workspace, every workspace with a row
// still waiting on discovery (a stopped one is scanned as empty, so its rows
// go), and every one asked about.
func (s *Scanner) run(ctx context.Context, asks []string) (struct{}, error) {
	if s.tracks == nil {
		s.tracks = map[string]*wsTrack{}
	}
	now := s.Clock.Now()
	if len(asks) == 0 && s.Watched != nil && !s.Watched() && !s.lastFull.IsZero() && now.Sub(s.lastFull) < IdleInterval {
		return struct{}{}, nil // nobody is looking: the next round is soon enough
	}
	s.lastFull = now
	asked := map[string]bool{}
	for _, id := range asks {
		asked[id] = true
	}
	targets, exist, err := s.Registry.scanTargets(ctx, asked)
	if err != nil {
		if ctx.Err() == nil {
			s.logf("drydock: port discovery: listing workspaces: %v", err)
		}
		return struct{}{}, err
	}
	s.mu.Lock()
	for id := range s.reports {
		if !exist[id] {
			delete(s.reports, id)
		}
	}
	s.mu.Unlock()
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
		// Bounded per workspace, so a docker call that hangs for one
		// workspace costs that workspace this round and no other.
		rctx, cancel := sys.WithTimeout(ctx, s.Clock, s.timeout())
		ls, err = s.Source.Listeners(rctx, t.id)
		cutOff := rctx.Err() != nil && ctx.Err() == nil
		cancel()
		if cutOff {
			// Whatever the source made of its cancelled context — even a
			// table read just before — a read past its bound is no read.
			ls, err = nil, fmt.Errorf("reading the socket table took longer than %s", s.timeout())
		}
	} else {
		err = ErrNotRunning
	}
	if ctx.Err() != nil {
		return
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrNotRunning):
		ls = nil // scanned as empty: its rows go after their grace
	case errors.Is(err, ErrScanRaced):
		// Nothing learned, so nothing read: a rescan is not told "ok",
		// and nobody else is told anything. The next round reads again,
		// and corrects a rescan's "unavailable" whatever the budget.
		s.observe(ctx, t.id, nil, DiscoveryUnavailable, asked, false)
		return
	default:
		if tr.unavailable != err.Error() {
			s.logf("drydock: port discovery for workspace %s is unavailable: %v", t.id, err)
			tr.unavailable = err.Error()
		}
		s.observe(ctx, t.id, nil, DiscoveryUnavailable, asked, true)
		return
	}
	if tr.unavailable != "" {
		s.logf("drydock: port discovery for workspace %s works again", t.id)
		tr.unavailable = ""
	}
	sightings, held := s.budget(tr, t.id, s.debounce(tr, t, ls))
	verdict := DiscoveryOK
	if held > 0 {
		verdict = DiscoveryLimited
	}
	s.observe(ctx, t.id, sightings, verdict, asked, true)
}

// observe writes one workspace's round: its sightings, and the report its
// discovery state is owed — port.scanned when it was asked about, which
// carries the verdict whatever it is; otherwise port.discovery when the
// verdict differs from what was last reported and its budget allows
// (`settles`: false for a raced read, which teaches nothing to anyone who did
// not ask). What was reported is remembered only once it is committed, and
// before the event is published.
func (s *Scanner) observe(ctx context.Context, id string, ss []Sighting, verdict string, asked, settles bool) {
	now := s.Clock.Now()
	s.mu.Lock()
	if s.reports == nil {
		s.reports = map[string]*report{}
	}
	r := s.reports[id]
	if r == nil {
		r = &report{state: DiscoveryOK}
		s.reports[id] = r
	}
	last := r.state
	r.budget.refill(now, StatusEvery, StatusBurst)
	var note *ScanNote
	switch {
	case asked:
		note = &ScanNote{Discovery: verdict}
	case settles && verdict != last && (r.raced || r.budget.tokens >= 1):
		// A raced rescan's "unavailable" is corrected by the first round
		// that reads the table, without the status budget: a race is
		// transient, and the budget could leave it on the panel for
		// StatusEvery. It costs no token; only a rescan can owe one.
		note = &ScanNote{Status: verdict}
	}
	if note == nil && settles && verdict == last {
		// What was last reported is true now, whatever a raced rescan
		// said: nothing is owed to correct it.
		r.raced = false
	}
	s.mu.Unlock()
	if len(ss) == 0 && note == nil {
		return
	}
	var committed func(bool)
	if note != nil {
		// Set after the commit and before the event is published, under the
		// event log's lock (events.Log.CommitThen): a port list fetched after
		// the event reads what the event said, never what it replaced — the
		// reducer would let that older value overwrite the newer event, and
		// it would stay until the next report.
		committed = func(wrote bool) {
			if !wrote {
				return // the workspace is going: nothing was reported
			}
			s.mu.Lock()
			if note.Status != "" && !r.raced {
				r.budget.tokens--
			}
			r.state = verdict
			// A raced read's "unavailable" is owed a correction the moment a
			// round reads the table, budget or not (see the switch above).
			r.raced = !settles && (verdict != last || r.raced)
			s.mu.Unlock()
		}
	}
	err := s.Registry.observe(ctx, id, ss, note, committed)
	if s.afterObserve != nil {
		s.afterObserve()
	}
	if err != nil && ctx.Err() == nil {
		s.logf("drydock: port discovery: recording what workspace %s listens on: %v", id, err)
	}
}

// budget lets through, in port order, the changes the workspace's budgets
// allow: every change spends a ChangeEvery token, and a new row a MintEvery
// token as well — an ephemeral port's only while more than the reserve is
// left (MintReserve, ChangeReserve). A change held back is not lost — the next
// scan derives it again from what the registry says — so a budget delays, it
// never forgets. It returns what it let through and how many it held, which
// makes the round's verdict DiscoveryLimited. The first hold is logged; so is
// the end of a spell of holding.
func (s *Scanner) budget(tr *wsTrack, id string, ss []Sighting) ([]Sighting, int) {
	now := s.Clock.Now()
	tr.mints.refill(now, MintEvery, MintBurst)
	tr.changes.refill(now, ChangeEvery, ChangeBurst)
	var out []Sighting
	held := 0
	for _, o := range ss {
		needChange, needMint := 1.0, 1.0
		if o.Port >= EphemeralFrom {
			needChange, needMint = 1+ChangeReserve, 1+MintReserve
		}
		if tr.changes.tokens < needChange || o.Mint && tr.mints.tokens < needMint {
			held++
			continue
		}
		tr.changes.tokens--
		if o.Mint {
			tr.mints.tokens--
		}
		out = append(out, o)
	}
	switch {
	case held > 0 && !tr.limited:
		s.logf("drydock: port discovery for workspace %s is holding back %d port changes: its ports change faster than discovery records them", id, held)
		tr.limited = true
	case held == 0 && tr.limited:
		s.logf("drydock: port discovery for workspace %s is recording every change again", id)
		tr.limited = false
	}
	return out, held
}

// debounce folds one scan into the workspace's track and returns what the
// registry should change, by port: a port seen AppearAfter scans running at
// one bind address that the registry does not already list as listening
// there (on its row, or on a new one — Mint — within MaxPorts and
// MaxObserved, decided here so a workspace at the cap costs no transaction);
// a listening port no scan has seen for Grace; and a row only discovery
// holds that has been gone for RetireAfter and is not being seen again.
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
	live, observedOnly := len(t.rows), 0
	for port, r := range t.rows {
		if r.observedOnly {
			observedOnly++
		}
		if _, ok := tr.ports[port]; !ok && r.state == StateListening {
			tr.ports[port] = &portTrack{lastSeen: now}
		}
	}
	ports := make([]int, 0, len(tr.ports))
	for port := range tr.ports {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	var out []Sighting
	for _, port := range ports {
		pt := tr.ports[port]
		r, listed := t.rows[port]
		if _, isSeen := seen[port]; isSeen {
			bind := pt.bind.String()
			if pt.streak < AppearAfter || listed && r.state == StateListening && r.bind == bind {
				continue
			}
			if !listed {
				if live >= MaxPorts || observedOnly >= MaxObserved {
					continue
				}
				live++
				observedOnly++
			}
			out = append(out, Sighting{Port: port, Kind: SightListening, Bind: bind, At: now, Mint: !listed})
			continue
		}
		pt.streak = 0
		if !listed || r.state != StateListening {
			delete(tr.ports, port) // nothing seen and nothing listed listening: nothing pending
			continue
		}
		if now.Sub(pt.lastSeen) >= s.grace() {
			out = append(out, Sighting{Port: port, Kind: SightGone, At: pt.lastSeen})
		}
	}
	// Discovery's own rows, gone for RetireAfter, unless a scan is seeing
	// the port again: a server that comes back within it gets its row back,
	// slug and all, rather than a new one.
	var retire []int
	for port, r := range t.rows {
		if _, isSeen := seen[port]; isSeen || !r.observedOnly || r.state != StateGone || r.lastSeen.IsZero() {
			continue
		}
		if now.Sub(r.lastSeen) >= RetireAfter {
			retire = append(retire, port)
		}
	}
	sort.Ints(retire)
	for _, port := range retire {
		out = append(out, Sighting{Port: port, Kind: SightRetire, At: now})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Port < out[j].Port })
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

// SightKind is what a sighting asks of a row.
type SightKind int

// The three verdicts.
const (
	// SightListening: seen at Bind, AppearAfter scans running; At is now.
	SightListening SightKind = iota
	// SightGone: unseen for the grace period; At is the last scan that saw it.
	SightGone
	// SightRetire: a row only discovery holds, gone for RetireAfter.
	SightRetire
)

// Sighting is the debounced scan's verdict on one port of a workspace.
type Sighting struct {
	Port int
	Kind SightKind
	Bind string
	At   time.Time
	// Mint: no live row names the port, so listing it makes one.
	Mint bool
}

// discoveryMessage is the event's sentence for a discovery state. The UI says
// its own, keyed by `data.discovery`; this is the log's.
func discoveryMessage(state string) string {
	switch state {
	case DiscoveryUnavailable:
		return "Could not read what this workspace's container is listening on."
	case DiscoveryLimited:
		return "This workspace's ports are changing faster than discovery records them; some changes are held back."
	}
	return "Port discovery records what this workspace's container listens on."
}

// ScanNote is the report a round owes: port.scanned with the verdict, for a
// rescan (Discovery), or port.discovery with a changed state (Status).
type ScanNote struct {
	Discovery string
	Status    string
}

// scanTargets is the round's workspaces, each with its live rows: every
// running one, every one with a row still waiting on discovery (pending),
// and every one asked about — never one being deleted, whose rows are about
// to be retired with it. exist names every workspace not being deleted.
func (s *Service) scanTargets(ctx context.Context, asked map[string]bool) (out []target, exist map[string]bool, err error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.state, p.container_port, p.observed_state, p.bind_addr,
		  p.observed, p.enabled, p.manual, p.declared, p.hidden, p.last_seen_at
		FROM workspace w LEFT JOIN forwarded_port p ON p.workspace_id = w.id AND p.retired_at IS NULL
		WHERE w.state <> 'deleting' ORDER BY w.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byID := map[string]*target{}
	var order []string
	for rows.Next() {
		var id, state string
		var port sql.NullInt64
		var ostate, bind, seen sql.NullString
		var observed, enabled, manual, declared, hidden sql.NullBool
		if err := rows.Scan(&id, &state, &port, &ostate, &bind, &observed, &enabled, &manual, &declared, &hidden, &seen); err != nil {
			return nil, nil, err
		}
		t := byID[id]
		if t == nil {
			t = &target{id: id, state: state, rows: map[int]liveRow{}}
			byID[id] = t
			order = append(order, id)
		}
		if port.Valid {
			r := liveRow{state: ostate.String, bind: bind.String,
				observedOnly: observed.Bool && !(enabled.Bool || manual.Bool || declared.Bool || hidden.Bool)}
			if at, err := parseTS(seen.String); seen.Valid && err == nil {
				r.lastSeen = at
			}
			t.rows[int(port.Int64)] = r
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	exist = map[string]bool{}
	for _, id := range order {
		exist[id] = true
	}
	for _, id := range order {
		t := byID[id]
		pending := false
		for _, r := range t.rows {
			pending = pending || r.pending()
		}
		if t.state == "running" || pending || asked[id] {
			out = append(out, *t)
		}
	}
	return out, exist, nil
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
//   - A port gone: its row is marked gone (port.updated).
//   - A row to retire (RetireAfter gone, and only discovery holds it): it is
//     retired, its slug spent (port.retired), so the port seen again later
//     is a new row.
//
// Every event carries `data.source: "discovery"`. A note adds port.scanned.
// It never writes `enabled`. A workspace being deleted, or gone, is left
// alone.
func (s *Service) Observe(ctx context.Context, workspaceID string, ss []Sighting, note *ScanNote) error {
	return s.observe(ctx, workspaceID, ss, note, nil)
}

// observe is Observe with committed run after its commit and before its
// events are published, told whether it wrote any (events.Log.CommitThen):
// how the scanner's reported state changes with the event that reports it.
func (s *Service) observe(ctx context.Context, workspaceID string, ss []Sighting, note *ScanNote, committed func(wrote bool)) error {
	var revoked []string
	err := s.commitThen(ctx, func(tx *sql.Tx) ([]events.Event, error) {
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
			if o.Kind == SightListening {
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
			if !ok || cur.ObservedState == nil {
				continue
			}
			if o.Kind == SightGone {
				if *cur.ObservedState != StateListening {
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET observed_state = ?, last_seen_at = ? WHERE id = ?`,
					StateGone, ts(o.At), cur.ID); err != nil {
					return nil, err
				}
				if err := emit(KindPortUpdated, cur.ID, fmt.Sprintf("Port %d stopped listening.", o.Port)); err != nil {
					return nil, err
				}
				continue
			}
			// SightRetire, checked again against the row as it is now.
			if *cur.ObservedState != StateGone || !cur.Observed || held(cur) {
				continue
			}
			if err := retire(ctx, tx, cur.ID, s.Clock.Now()); err != nil {
				return nil, err
			}
			revoked = append(revoked, cur.ID)
			e, err := events.NewEvent(workspaceID, events.Info, KindPortRetired,
				fmt.Sprintf("Port %d removed from the ports list: nothing has listened on it for a while.", o.Port),
				map[string]any{"port_id": cur.ID, "container_port": cur.ContainerPort, "source": SourceDiscovery})
			if err != nil {
				return nil, err
			}
			es = append(es, e)
		}
		if note != nil && note.Status != "" {
			e, err := events.NewEvent(workspaceID, events.Info, KindPortDiscovery, discoveryMessage(note.Status),
				map[string]any{"discovery": note.Status, "source": SourceDiscovery})
			if err != nil {
				return nil, err
			}
			es = append(es, e)
		}
		if note != nil && note.Discovery != "" {
			msg := "Ports scanned."
			if note.Discovery != DiscoveryOK {
				msg = "Ports scanned. " + discoveryMessage(note.Discovery)
			}
			e, err := events.NewEvent(workspaceID, events.Info, KindPortScanned, msg,
				map[string]any{"discovery": note.Discovery, "source": SourceDiscovery})
			if err != nil {
				return nil, err
			}
			es = append(es, e)
		}
		return es, nil
	}, committed)
	if err == nil && s.Revoked != nil {
		for _, id := range revoked {
			s.Revoked(id)
		}
	}
	return err
}
