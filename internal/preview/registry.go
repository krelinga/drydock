package preview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// The port registry (PF §3.1, §5, §6; §13 step 4): forwarded_port rows as the
// API serves them, and every change to one written with its event in one
// transaction. A row is a permission, never a reachability: the workspace
// still has to be running, and the proxy reads the row on every request.
//
// Three rules the registry owns, each with its test:
//
//   - **Off until enabled.** A row is born disabled — added by hand or
//     declared by the configuration — and only Update with Enabled set
//     enables it. Nothing a container controls has a path to `enabled`: the
//     declared sync writes `declared` and labels, never the switch.
//   - **A disable or a retire ends the port's previews at once.** The rows of
//     preview_session go in the same transaction, and Revoked is told after
//     the commit so the server closes the port's open websockets then and
//     there (Proxy.CloseWhere), not at their next recheck.
//   - **A slug is minted once and never again.** Retire keeps the row; the
//     global UNIQUE on slug covers retired rows, and a mint that draws a spent
//     slug draws again rather than reuse it (PF §4, §11).

// Event kinds the registry writes, each on the port's workspace. Every one
// but port.retired carries the whole port as `data.port`, so the reducer
// upserts it by id; port.retired carries `data.port_id` and drops it.
const (
	KindPortAdded    = "port.added"
	KindPortEnabled  = "port.enabled"
	KindPortDisabled = "port.disabled"
	KindPortUpdated  = "port.updated"
	KindPortRetired  = "port.retired"
)

// MaxPorts bounds a workspace's live rows. A configuration — the container's
// to write — declares at most MaxDeclared of them; the rest are the
// operator's by hand.
const (
	MaxPorts    = 64
	MaxDeclared = 32
	// MaxLabel is a label's length in bytes.
	MaxLabel = 100
)

// Registry errors. Each is one refusal the API turns into a code.
var (
	ErrNoWorkspace       = errors.New("preview: no such workspace")
	ErrWorkspaceDeleting = errors.New("preview: the workspace is being deleted")
	ErrNoPort            = errors.New("preview: no such port on this workspace")
	ErrPortExists        = errors.New("preview: the workspace already lists that port")
	ErrTooManyPorts      = errors.New("preview: the workspace lists as many ports as it may")
	ErrPreviewsOff       = errors.New("preview: no preview domain is configured")
	// ErrLoopbackOnly refuses an enable of a port discovery sees listening
	// on loopback only (PF §11's first row): nothing outside the container
	// can reach it, so the switch could only make a preview that never
	// answers. The dev server restarted on 0.0.0.0 is seen within two scans
	// and the switch is offered again.
	ErrLoopbackOnly = errors.New("preview: the port is listening on loopback only")
)

// InvalidError is a request the registry refuses as malformed; Reason is a
// sentence the operator can act on.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "preview: " + e.Reason }

// Port is one live forwarded_port row as the API and the events carry it.
type Port struct {
	ID            string `json:"id"`
	WorkspaceID   string `json:"workspace_id"`
	ContainerPort int    `json:"container_port"`
	Slug          string `json:"slug"`
	// Host is the preview host the slug is served on, null when no preview
	// domain is configured. URL is set only while the port is enabled: the
	// panel shows the full host of an enabled row (frontend §6.3).
	Host           *string `json:"host"`
	URL            *string `json:"url"`
	Label          *string `json:"label"`
	UpstreamScheme string  `json:"upstream_scheme"`
	HostHeader     string  `json:"host_header"`
	Enabled        bool    `json:"enabled"`
	Hidden         bool    `json:"hidden"`
	Declared       bool    `json:"declared"`
	Observed       bool    `json:"observed"`
	Manual         bool    `json:"manual"`
	BindAddr       *string `json:"bind_addr"`
	// Loopback is discovery's classification of BindAddr (PF §8.2): a
	// server bound to loopback is listed, but only the container itself can
	// reach it, so no preview of it can answer. Derived, never stored.
	Loopback      bool    `json:"loopback"`
	ObservedState *string `json:"observed_state"`
	LastSeenAt    *string `json:"last_seen_at"`
	CreatedAt     *string `json:"created_at"`
}

// Observation is what discovery last recorded of the port, as the proxy and
// the probe read it.
func (p Port) Observation() Observation {
	var o Observation
	if p.ObservedState != nil {
		o.State = *p.ObservedState
	}
	if p.BindAddr != nil {
		o.Bind = *p.BindAddr
	}
	return o
}

// AddSpec is a port added by hand (POST …/ports).
type AddSpec struct {
	ContainerPort  int
	Label          string
	UpstreamScheme string // "" is http
	HostHeader     string // "" is localhost
}

// Change is PATCH …/ports/:port: each field set is changed, the rest kept.
// An empty Label clears it.
type Change struct {
	Enabled    *bool
	Hidden     *bool
	Label      *string
	HostHeader *string
}

// Declared is one port the resolved configuration names (forwardPorts,
// appPort), with the label portsAttributes gives it.
type Declared struct {
	Port  int
	Label string
}

const portCols = `id, workspace_id, container_port, slug, label, upstream_scheme, host_header,
	enabled, hidden, declared, observed, manual, bind_addr, observed_state, last_seen_at, created_at`

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func (s *Service) scanPort(r interface{ Scan(...any) error }) (Port, error) {
	var p Port
	var label, bind, state, seen, created sql.NullString
	err := r.Scan(&p.ID, &p.WorkspaceID, &p.ContainerPort, &p.Slug, &label, &p.UpstreamScheme, &p.HostHeader,
		&p.Enabled, &p.Hidden, &p.Declared, &p.Observed, &p.Manual, &bind, &state, &seen, &created)
	if err != nil {
		return Port{}, err
	}
	str := func(n sql.NullString) *string {
		if !n.Valid {
			return nil
		}
		v := n.String
		return &v
	}
	p.Label, p.BindAddr, p.ObservedState, p.LastSeenAt, p.CreatedAt = str(label), str(bind), str(state), str(seen), str(created)
	if a, err := netip.ParseAddr(bind.String); bind.Valid && err == nil {
		p.Loopback = a.Unmap().IsLoopback()
	}
	if s.Domain != "" {
		h := s.HostFor(p.Slug)
		p.Host = &h
		if p.Enabled {
			u := "https://" + h + "/"
			p.URL = &u
		}
	}
	return p, nil
}

// PreviewsOn reports whether a preview domain is configured: without one a
// port can be listed and declared, but not enabled.
func (s *Service) PreviewsOn() bool { return s.Domain != "" }

// Ports lists a workspace's live rows by port number; hidden ones only when
// asked. ErrNoWorkspace when there is no such workspace.
func (s *Service) Ports(ctx context.Context, workspaceID string, hidden bool) ([]Port, error) {
	if _, err := workspaceState(ctx, s.DB, workspaceID); err != nil {
		return nil, err
	}
	q := `SELECT ` + portCols + ` FROM forwarded_port WHERE workspace_id = ? AND retired_at IS NULL`
	if !hidden {
		q += ` AND hidden = 0`
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY container_port`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Port{}
	for rows.Next() {
		p, err := s.scanPort(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Port is one live row of a workspace's; ErrNoPort if it is not one.
func (s *Service) Port(ctx context.Context, workspaceID, portID string) (Port, error) {
	return s.port(ctx, s.DB, workspaceID, portID)
}

func (s *Service) port(ctx context.Context, q querier, workspaceID, portID string) (Port, error) {
	p, err := s.scanPort(q.QueryRowContext(ctx, `SELECT `+portCols+` FROM forwarded_port
		WHERE id = ? AND workspace_id = ? AND retired_at IS NULL`, portID, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Port{}, ErrNoPort
	}
	return p, err
}

func workspaceState(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}, id string) (string, error) {
	var state string
	err := q.QueryRowContext(ctx, `SELECT state FROM workspace WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoWorkspace
	}
	return state, err
}

// commit runs fn in one transaction with the events it returns — through the
// event log when there is one, so the row and its event are one fact.
func (s *Service) commit(ctx context.Context, fn func(tx *sql.Tx) ([]events.Event, error)) error {
	return s.commitThen(ctx, fn, nil)
}

// commitThen is commit with committed run after a successful commit and
// before its events are published (events.Log.CommitThen), told whether fn
// wrote any.
func (s *Service) commitThen(ctx context.Context, fn func(tx *sql.Tx) ([]events.Event, error), committed func(wrote bool)) error {
	if s.Events != nil {
		var after func([]events.Event)
		if committed != nil {
			after = func(es []events.Event) { committed(len(es) > 0) }
		}
		_, err := s.Events.CommitThen(ctx, fn, after)
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	es, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if committed != nil {
		committed(len(es) > 0)
	}
	return nil
}

// Add lists a port by hand: a new row, manual, disabled, with a freshly
// minted slug. A port the workspace already lists live is ErrPortExists — it
// is enabled where it stands.
func (s *Service) Add(ctx context.Context, workspaceID string, spec AddSpec) (Port, error) {
	if spec.ContainerPort < 1 || spec.ContainerPort > 65535 {
		return Port{}, &InvalidError{"A port is a number from 1 to 65535."}
	}
	if spec.UpstreamScheme == "" {
		spec.UpstreamScheme = "http"
	}
	if spec.UpstreamScheme != "http" && spec.UpstreamScheme != "https" {
		return Port{}, &InvalidError{"upstream_scheme is http or https."}
	}
	if spec.HostHeader == "" {
		spec.HostHeader = HostLocalhost
	}
	if spec.HostHeader != HostLocalhost && spec.HostHeader != HostPassthrough {
		return Port{}, &InvalidError{"host_header is localhost or passthrough."}
	}
	label, err := cleanLabel(spec.Label)
	if err != nil {
		return Port{}, err
	}
	var out Port
	err = s.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		if err := s.writable(ctx, tx, workspaceID); err != nil {
			return nil, err
		}
		if _, ok, err := s.livePort(ctx, tx, workspaceID, spec.ContainerPort); err != nil {
			return nil, err
		} else if ok {
			return nil, ErrPortExists
		}
		id, err := s.insert(ctx, tx, workspaceID, spec.ContainerPort, label, spec.UpstreamScheme, spec.HostHeader, "manual")
		if err != nil {
			return nil, err
		}
		if out, err = s.port(ctx, tx, workspaceID, id); err != nil {
			return nil, err
		}
		e, err := portEvent(KindPortAdded, out, fmt.Sprintf("Port %d added to the ports list.", out.ContainerPort))
		return []events.Event{e}, err
	})
	return out, err
}

// writable: the workspace exists, is not being deleted, and has room.
func (s *Service) writable(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	state, err := workspaceState(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if state == "deleting" {
		return ErrWorkspaceDeleting
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM forwarded_port WHERE workspace_id = ? AND retired_at IS NULL`,
		workspaceID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPorts {
		return ErrTooManyPorts
	}
	return nil
}

func (s *Service) livePort(ctx context.Context, q querier, workspaceID string, port int) (Port, bool, error) {
	p, err := s.scanPort(q.QueryRowContext(ctx, `SELECT `+portCols+` FROM forwarded_port
		WHERE workspace_id = ? AND container_port = ? AND retired_at IS NULL`, workspaceID, port))
	if errors.Is(err, sql.ErrNoRows) {
		return Port{}, false, nil
	}
	return p, err == nil, err
}

// slugAttempts bounds the mint's draws. A collision is one in 36⁴ per live or
// retired slug of the same repository and port, so eight draws all colliding
// is a broken random source, and that is an error rather than a reuse.
const slugAttempts = 8

// insert mints a slug and writes a new disabled row with one provenance flag
// set. A slug already spent — live or retired, any workspace — is never
// taken: the UNIQUE on slug refuses it and the mint draws again (PF §11).
func (s *Service) insert(ctx context.Context, tx *sql.Tx, workspaceID string, port int, label *string, scheme, hostHeader, flag string) (string, error) {
	var fullName string
	err := tx.QueryRowContext(ctx, `SELECT r.full_name FROM workspace w JOIN repository r ON r.id = w.repository_id
		WHERE w.id = ?`, workspaceID).Scan(&fullName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	now := s.Clock.Now()
	id, err := sys.NewULID(now, s.Random)
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < slugAttempts; attempt++ {
		suffix, err := randomSuffix(s.Random)
		if err != nil {
			return "", err
		}
		slug := MintSlug(fullName, port, suffix)
		// A savepoint, so a refused insert leaves the transaction usable
		// for the next draw.
		if _, err := tx.ExecContext(ctx, `SAVEPOINT mint`); err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO forwarded_port
			(id, workspace_id, container_port, slug, label, upstream_scheme, host_header, `+flag+`, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`, id, workspaceID, port, slug, label, scheme, hostHeader, ts(now))
		if err == nil {
			_, err = tx.ExecContext(ctx, `RELEASE mint`)
			return id, err
		}
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO mint`); rerr != nil {
			return "", rerr
		}
		if !strings.Contains(err.Error(), "forwarded_port.slug") {
			return "", err
		}
	}
	return "", errors.New("preview: every slug drawn was already spent; the random source is broken")
}

// slugAlphabet is the suffix's: lowercase letters and digits, one DNS label's.
const slugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomSuffix(r io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("preview slug: %w", err)
	}
	out := make([]byte, 4)
	for i, v := range b {
		out[i] = slugAlphabet[int(v)%len(slugAlphabet)]
	}
	return string(out), nil
}

// MintSlug is `<sanitized-repo>-<port>-<suffix>` (PF §4): the repository's
// short name lowercased with every run of anything that is not a letter or a
// digit made one hyphen, shortened so the whole is one DNS label, and "port"
// when nothing of the name survives.
func MintSlug(fullName string, port int, suffix string) string {
	name := fullName
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	hyphen := false
	for _, c := range strings.ToLower(name) {
		if c < 0x80 && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			b.WriteRune(c)
			hyphen = false
		} else if !hyphen && b.Len() > 0 {
			b.WriteByte('-')
			hyphen = true
		}
	}
	base := strings.Trim(b.String(), "-")
	tail := "-" + strconv.Itoa(port) + "-" + suffix
	if max := 63 - len(tail); len(base) > max {
		base = strings.Trim(base[:max], "-")
	}
	if base == "" {
		base = "port"
	}
	return base + tail
}

func cleanLabel(l string) (*string, error) {
	l = strings.TrimSpace(l)
	if l == "" {
		return nil, nil
	}
	if len(l) > MaxLabel || !utf8.ValidString(l) {
		return nil, &InvalidError{fmt.Sprintf("A label is at most %d bytes of text.", MaxLabel)}
	}
	for _, c := range l {
		if unicode.IsControl(c) {
			return nil, &InvalidError{"A label is one line of text."}
		}
	}
	return &l, nil
}

// Update applies a PATCH to one live port, in one transaction with its one
// event: port.enabled or port.disabled when the change names Enabled — even
// unchanged, so a second device's press still settles — and port.updated
// otherwise. A disable deletes the port's preview sessions in the same
// transaction (PF §5) and tells Revoked after the commit.
func (s *Service) Update(ctx context.Context, workspaceID, portID string, c Change) (Port, error) {
	if c.HostHeader != nil && *c.HostHeader != HostLocalhost && *c.HostHeader != HostPassthrough {
		return Port{}, &InvalidError{"host_header is localhost or passthrough."}
	}
	var label *string
	if c.Label != nil {
		var err error
		if label, err = cleanLabel(*c.Label); err != nil {
			return Port{}, err
		}
	}
	if c.Enabled != nil && *c.Enabled && !s.PreviewsOn() {
		return Port{}, ErrPreviewsOff
	}
	var out Port
	err := s.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		state, err := workspaceState(ctx, tx, workspaceID)
		if err != nil {
			return nil, err
		}
		cur, err := s.port(ctx, tx, workspaceID, portID)
		if err != nil {
			return nil, err
		}
		if c.Enabled != nil && *c.Enabled && state == "deleting" {
			return nil, ErrWorkspaceDeleting
		}
		if c.Enabled != nil && *c.Enabled && cur.Observation().LoopbackOnly() {
			// Refused, not allowed with a warning: an enabled loopback
			// port is a link the operator would open onto Drydock's own
			// failure page, and the proxy never dials it anyway. One
			// already enabled when its server moved to loopback stays
			// enabled — discovery has no path to the switch, either way
			// (PF §10.7) — and its preview says the sentence until the
			// server listens on 0.0.0.0 again.
			return nil, ErrLoopbackOnly
		}
		set := func(col string, v any) error {
			_, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET `+col+` = ? WHERE id = ?`, v, cur.ID)
			return err
		}
		if c.Hidden != nil {
			if err := set("hidden", *c.Hidden); err != nil {
				return nil, err
			}
		}
		if c.Label != nil {
			if err := set("label", label); err != nil {
				return nil, err
			}
		}
		if c.HostHeader != nil {
			if err := set("host_header", *c.HostHeader); err != nil {
				return nil, err
			}
		}
		kind := KindPortUpdated
		if c.Enabled != nil {
			if err := set("enabled", *c.Enabled); err != nil {
				return nil, err
			}
			kind = KindPortEnabled
			if !*c.Enabled {
				kind = KindPortDisabled
				// The sessions go with the switch, so a re-enable
				// revives none of them (PF §5).
				if _, err := tx.ExecContext(ctx, `DELETE FROM preview_session WHERE forwarded_port_id = ?`, cur.ID); err != nil {
					return nil, err
				}
			}
		}
		if out, err = s.port(ctx, tx, workspaceID, portID); err != nil {
			return nil, err
		}
		msg := fmt.Sprintf("Port %d updated.", out.ContainerPort)
		switch kind {
		case KindPortEnabled:
			msg = fmt.Sprintf("Port %d previewed.", out.ContainerPort)
			if out.URL != nil {
				msg = fmt.Sprintf("Port %d previewed at %s.", out.ContainerPort, *out.URL)
			}
		case KindPortDisabled:
			msg = fmt.Sprintf("Port %d no longer previewed.", out.ContainerPort)
		}
		e, err := portEvent(kind, out, msg)
		return []events.Event{e}, err
	})
	if err == nil && c.Enabled != nil && !*c.Enabled && s.Revoked != nil {
		s.Revoked(portID)
	}
	return out, err
}

// SetEnabled is Update with only the switch: enable, or disable — its preview
// sessions deleted in the same transaction and its open websockets closed
// after it (Revoked).
func (s *Service) SetEnabled(ctx context.Context, workspaceID, portID string, enabled bool) (Port, error) {
	return s.Update(ctx, workspaceID, portID, Change{Enabled: &enabled})
}

// Retire soft-deletes a port: disabled, retired_at set, its preview sessions
// gone, in one transaction with port.retired; then Revoked. The row stays so
// its slug stays spent (PF §4): a port listed again is a new row with a new
// slug.
func (s *Service) Retire(ctx context.Context, workspaceID, portID string) error {
	err := s.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		if _, err := workspaceState(ctx, tx, workspaceID); err != nil {
			return nil, err
		}
		cur, err := s.port(ctx, tx, workspaceID, portID)
		if err != nil {
			return nil, err
		}
		if err := retire(ctx, tx, cur.ID, s.Clock.Now()); err != nil {
			return nil, err
		}
		e, err := retiredEvent(cur)
		return []events.Event{e}, err
	})
	if err == nil && s.Revoked != nil {
		s.Revoked(portID)
	}
	return err
}

func retire(ctx context.Context, tx *sql.Tx, portID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM preview_session WHERE forwarded_port_id = ?`, portID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET enabled = 0, retired_at = ? WHERE id = ? AND retired_at IS NULL`,
		ts(now), portID)
	return err
}

func portEvent(kind string, p Port, msg string) (events.Event, error) {
	return events.NewEvent(p.WorkspaceID, events.Info, kind, msg, map[string]any{"port": p})
}

func retiredEvent(p Port) (events.Event, error) {
	return events.NewEvent(p.WorkspaceID, events.Info, KindPortRetired,
		fmt.Sprintf("Port %d removed from the ports list.", p.ContainerPort),
		map[string]any{"port_id": p.ID, "container_port": p.ContainerPort})
}

// DeclarePorts is the declared half of the registry (PF §8.2's merge, §13
// step 4): the ports the resolved configuration names, written onto the
// workspace's rows in one transaction. A port newly declared is a new row,
// declared and disabled; a live row the configuration names is marked
// declared, and given the configuration's label if it has none. A row the
// configuration no longer names loses the flag, and is retired if nothing
// else holds it — not enabled, not added by hand, not hidden, not listening
// now (discovery's observed_state) — so a declaration that comes back gets a
// new slug, which is the bookmark failing closed as it should. A row kept
// because it is listening is discovery's from then on: it is retired when it
// stops (Observe).
//
// It never touches `enabled`: the configuration is the container's to write,
// and what the container listens on or declares is never a decision to expose
// it (PF §10.7). At most MaxDeclared ports are taken, in the order given.
func (s *Service) DeclarePorts(ctx context.Context, workspaceID string, ds []Declared) error {
	want := map[int]string{}
	var order []int
	for _, d := range ds {
		if d.Port < 1 || d.Port > 65535 {
			continue
		}
		if _, dup := want[d.Port]; dup {
			continue
		}
		if len(order) == MaxDeclared {
			break
		}
		want[d.Port] = d.Label
		order = append(order, d.Port)
	}
	return s.commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		state, err := workspaceState(ctx, tx, workspaceID)
		if err != nil {
			return nil, err
		}
		if state == "deleting" {
			return nil, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+portCols+` FROM forwarded_port
			WHERE workspace_id = ? AND retired_at IS NULL ORDER BY container_port`, workspaceID)
		if err != nil {
			return nil, err
		}
		live := map[int]Port{}
		var all []Port
		for rows.Next() {
			p, err := s.scanPort(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			live[p.ContainerPort] = p
			all = append(all, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var es []events.Event
		emit := func(kind string, id, msg string) error {
			p, err := s.port(ctx, tx, workspaceID, id)
			if err != nil {
				return err
			}
			e, err := portEvent(kind, p, msg)
			es = append(es, e)
			return err
		}
		count := len(all)
		for _, port := range order {
			// A label the configuration gives that is not one line of
			// text is dropped, not refused: the port is still declared.
			label, _ := cleanLabel(want[port])
			if cur, ok := live[port]; ok {
				if cur.Declared && (cur.Label != nil || label == nil) {
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET declared = 1, label = coalesce(label, ?) WHERE id = ?`,
					label, cur.ID); err != nil {
					return nil, err
				}
				if err := emit(KindPortUpdated, cur.ID, fmt.Sprintf("Port %d is declared by the dev container configuration.", port)); err != nil {
					return nil, err
				}
				continue
			}
			if count >= MaxPorts {
				continue
			}
			id, err := s.insert(ctx, tx, workspaceID, port, label, "http", HostLocalhost, "declared")
			if err != nil {
				return nil, err
			}
			count++
			if err := emit(KindPortAdded, id, fmt.Sprintf("Port %d is declared by the dev container configuration.", port)); err != nil {
				return nil, err
			}
		}
		for _, p := range all {
			if _, still := want[p.ContainerPort]; still || !p.Declared {
				continue
			}
			listening := p.ObservedState != nil && *p.ObservedState == StateListening
			if !p.Enabled && !p.Manual && !p.Hidden && !listening {
				if err := retire(ctx, tx, p.ID, s.Clock.Now()); err != nil {
					return nil, err
				}
				e, err := retiredEvent(p)
				if err != nil {
					return nil, err
				}
				es = append(es, e)
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET declared = 0 WHERE id = ?`, p.ID); err != nil {
				return nil, err
			}
			if err := emit(KindPortUpdated, p.ID, fmt.Sprintf("Port %d is no longer declared by the dev container configuration.", p.ContainerPort)); err != nil {
				return nil, err
			}
		}
		return es, nil
	})
}

// Stopped reports whether a slug names an enabled, unretired port on a
// workspace that exists, is not running and is not being deleted — and which
// workspace and port. Authorize asks it, for a signed-in device only, after
// Resolve has refused the slug: such a device is sent to the workspace's page
// in the UI, which says the workspace is not running beside its Start (PF
// §11's *workspace stopped*), rather than to the preview host's constant dead
// end. Only the UI origin hears it, and only a signed-in device, which can
// read the workspace's page anyway.
func (s *Service) Stopped(ctx context.Context, slug string) (workspaceID, portID string, ok bool, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT fp.workspace_id, fp.id FROM forwarded_port fp JOIN workspace w ON w.id = fp.workspace_id
		WHERE fp.slug = ? AND fp.enabled = 1 AND fp.retired_at IS NULL AND w.state NOT IN ('running', 'deleting')`,
		slug).Scan(&workspaceID, &portID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return workspaceID, portID, err == nil, err
}

// Spent reports whether a slug names a port that was switched off or retired
// — any workspace, any state — and which. Authorize asks it, for a signed-in
// device only, after Resolve has refused the slug: such a host gets
// Clear-Site-Data (PF §10.3), since a page that ran there may have left a
// service worker or storage behind. A stopped workspace's enabled port is not
// spent; a stop is not a revocation (PF §13.2).
func (s *Service) Spent(ctx context.Context, slug string) (string, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM forwarded_port WHERE slug = ? AND (enabled = 0 OR retired_at IS NOT NULL)`,
		slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
