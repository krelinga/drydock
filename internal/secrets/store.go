package secrets

import (
	"context"
	"crypto/cipher"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// ErrNotFound is a name with no secret.
var ErrNotFound = errors.New("secrets: no such secret")

// ErrExists is Create's refusal: a secret by that name is already stored.
// A create must never replace one — the old value cannot be shown again, so
// it could not be recovered (frontend §6.4).
var ErrExists = errors.New("secrets: a secret by that name already exists")

// StaleKind is which kind of stale a live workspace is after a rotation
// (§10.3, frontend §4.5 #5). The UI must not infer it: guessing wrong is the
// twenty minutes of confusion the §10.3 warning is about.
type StaleKind string

const (
	// StaleNewCommands: the next Bash command picks the new value up on its
	// own, because CLAUDE_ENV_FILE runs `drydock-secrets export` per
	// command (Spike 03). Nothing to restart.
	StaleNewCommands StaleKind = "new_commands"
	// StaleNeedsSupervisorRestart: something in the workspace holds the
	// environment it was started with — an MCP server, which Claude Code
	// launches itself — and only re-exec'ing the remote-control server
	// re-launches it (§10.3's warning: ending a session is not enough).
	StaleNeedsSupervisorRestart StaleKind = "needs_supervisor_restart"
)

// Store is the secret, secret_grant and secret_access tables, plus the
// decrypted snapshot the broker reads.
type Store struct {
	DB     *sql.DB
	Key    *Key
	Events *events.Log // may be nil in tests
	Env    sys.Env
	// StaleKind decides, per live workspace, which kind of stale a rotation
	// leaves it. Nil means StaleNewCommands for every one — which is the
	// truth until Phase 5: with no supervised remote-control server there is
	// no Drydock-started process holding a frozen environment, and the
	// prelude reaches every command. Phase 5 supplies one that answers
	// needs_supervisor_restart for a workspace whose resolved configuration
	// declares MCP servers and whose supervisor is running.
	StaleKind func(ctx context.Context, workspaceID string) StaleKind

	aeadOnce sync.Once
	aead     cipher.AEAD
	aeadErr  error

	mu   sync.Mutex
	snap *snapshot // nil: rebuild on the next Resolve
	// undeliverable is the delivery condition the last non-transient build
	// found, nil when every row can be delivered; deliveryKnown is false
	// until a build has looked. Guarded by mu, like snap.
	undeliverable *Undeliverable
	deliveryKnown bool

	decrypts atomic.Int64
}

// Entry is one secret as delivered: the only type in Drydock that carries a
// value, and the broker is the only reader of one.
type Entry struct {
	ID, Name, Value string
}

// snapshot is every secret decrypted, and who may have it. Built once per
// write rather than per GET-SECRETS: the prelude asks on every Bash command,
// so the broker must not decrypt per call (§10.3 constraint 4, testing §8.2
// "the broker is cheap per call").
type snapshot struct {
	all    []Entry           // all_repos
	byRepo map[int64][]Entry // granted
	err    error
	// bad is every row that fails delivery, by name; non-empty exactly
	// when err is set and not transient.
	bad []UndeliverableSecret
	// transient marks a failed read, which is retried on the next call
	// rather than cached until the next write.
	transient bool
}

// Decrypts counts decryptions, for the test that N exports make none.
func (s *Store) Decrypts() int64 { return s.decrypts.Load() }

func (s *Store) cipher() (cipher.AEAD, error) {
	s.aeadOnce.Do(func() {
		if s.Key == nil {
			s.aeadErr = errors.New("secrets: no master key")
			return
		}
		s.aead, s.aeadErr = chacha20poly1305.NewX(s.Key.b[:])
	})
	return s.aead, s.aeadErr
}

// seal encrypts value under a fresh nonce, with the secret's id as the
// associated data: a row's ciphertext moved onto another row fails to open
// (§10.2), so a database edit cannot swap one secret's value for another's.
func (s *Store) seal(id, value string) (ct, nonce []byte, err error) {
	a, err := s.cipher()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, a.NonceSize())
	if _, err := io.ReadFull(s.Env.Random, nonce); err != nil {
		return nil, nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return a.Seal(nil, nonce, []byte(value), []byte(id)), nonce, nil
}

func (s *Store) open(id string, ct, nonce []byte) (string, error) {
	a, err := s.cipher()
	if err != nil {
		return "", err
	}
	s.decrypts.Add(1)
	if len(nonce) != a.NonceSize() {
		return "", fmt.Errorf("secrets: secret %s has a malformed nonce", id)
	}
	pt, err := a.Open(nil, nonce, ct, []byte(id))
	if err != nil {
		// Never wrap the AEAD error with anything from the row.
		return "", fmt.Errorf("secrets: secret %s does not decrypt under this master key", id)
	}
	return string(pt), nil
}

// Invalidate drops the decrypted snapshot, so the next Resolve rebuilds it
// from the tables. Every write here calls it; so must anything else that
// deletes secret_grant rows — the catalog and a workspace's removal, when a
// repository dropped from the installation is released — or the broker
// keeps serving the deleted grants from memory to a repository re-added
// under the same id.
func (s *Store) Invalidate() {
	s.mu.Lock()
	s.snap = nil
	s.mu.Unlock()
}

func (s *Store) now() time.Time { return s.Env.Clock.Now().UTC() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}

// ---- metadata ---------------------------------------------------------------

// Meta is everything about a secret except its value — and there is no field
// a value could go in (§13.5: no route returns one).
type Meta struct {
	Name        string  `json:"name"`
	Reach       string  `json:"reach"`
	Description string  `json:"description"`
	AllRepos    bool    `json:"all_repos"`
	Grants      []Grant `json:"grants"`
	// CreatedAt is when the name was first stored; RotatedAt when its value
	// last changed, null if never.
	CreatedAt *time.Time `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at"`
	// LastAccessAt and AccessedBy answer "which workspaces ever held this?"
	// (§10.4) from secret_access.
	LastAccessAt *time.Time `json:"last_access_at"`
	AccessedBy   []string   `json:"accessed_by"`
}

// Grant is one repository that may receive the secret.
type Grant struct {
	RepositoryID int64  `json:"repository_id"`
	FullName     string `json:"full_name"`
}

// List returns every secret's metadata, by name.
func (s *Store) List(ctx context.Context) ([]Meta, error) {
	return s.meta(ctx, "")
}

// Get returns one secret's metadata.
func (s *Store) Get(ctx context.Context, name string) (Meta, error) {
	ms, err := s.meta(ctx, name)
	if err != nil {
		return Meta{}, err
	}
	if len(ms) == 0 {
		return Meta{}, ErrNotFound
	}
	return ms[0], nil
}

func (s *Store) meta(ctx context.Context, only string) ([]Meta, error) {
	q := `SELECT id, name, reach, coalesce(description, ''), all_repos, created_at, rotated_at FROM secret`
	var args []any
	if only != "" {
		q += ` WHERE name = ?`
		args = append(args, only)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	var out []Meta
	var ids []string
	for rows.Next() {
		var m Meta
		var id string
		var created, rotated sql.NullString
		if err := rows.Scan(&id, &m.Name, &m.Reach, &m.Description, &m.AllRepos, &created, &rotated); err != nil {
			rows.Close()
			return nil, err
		}
		m.CreatedAt, m.RotatedAt = parseTS(created), parseTS(rotated)
		m.Grants, m.AccessedBy = []Grant{}, []string{}
		out = append(out, m)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, id := range ids {
		g, err := s.DB.QueryContext(ctx, `
			SELECT g.repository_id, coalesce(r.full_name, '') FROM secret_grant g
			LEFT JOIN repository r ON r.id = g.repository_id
			WHERE g.secret_id = ? ORDER BY r.full_name, g.repository_id`, id)
		if err != nil {
			return nil, err
		}
		for g.Next() {
			var gr Grant
			if err := g.Scan(&gr.RepositoryID, &gr.FullName); err != nil {
				g.Close()
				return nil, err
			}
			out[i].Grants = append(out[i].Grants, gr)
		}
		g.Close()
		a, err := s.DB.QueryContext(ctx, `
			SELECT workspace_id, max(at) FROM secret_access WHERE secret_id = ?
			GROUP BY workspace_id ORDER BY max(at) DESC`, id)
		if err != nil {
			return nil, err
		}
		for a.Next() {
			var ws string
			var at sql.NullString
			if err := a.Scan(&ws, &at); err != nil {
				a.Close()
				return nil, err
			}
			out[i].AccessedBy = append(out[i].AccessedBy, ws)
			if t := parseTS(at); t != nil && (out[i].LastAccessAt == nil || t.After(*out[i].LastAccessAt)) {
				out[i].LastAccessAt = t
			}
		}
		a.Close()
	}
	return out, nil
}

// ---- writes -----------------------------------------------------------------

// PutResult is what PUT /api/secrets/:name answers.
type PutResult struct {
	Secret  Meta `json:"secret"`
	Created bool `json:"created"`
	// Rotated is true when the value changed. A PUT that changes only the
	// reach or description rotates nothing and leaves nothing stale.
	Rotated bool  `json:"rotated"`
	Stale   Stale `json:"stale"`
}

// Stale is the workspaces a rotation reached, split by what it costs them.
type Stale struct {
	NewCommands            []StaleWorkspace `json:"new_commands"`
	NeedsSupervisorRestart []StaleWorkspace `json:"needs_supervisor_restart"`
}

// StaleWorkspace names one.
type StaleWorkspace struct {
	WorkspaceID  string `json:"workspace_id"`
	RepositoryID int64  `json:"repository_id"`
	FullName     string `json:"full_name"`
}

// Put creates a secret, or replaces one's value, reach and description.
// Every field is validated before anything is written; a refused write
// changes nothing.
func (s *Store) Put(ctx context.Context, name, value, reach, description string) (PutResult, error) {
	return s.put(ctx, name, &value, reach, description, false)
}

// Create stores a new secret and refuses, with ErrExists and nothing
// written, a name that is already stored: the PUT with If-None-Match: *.
// The check is inside the write's transaction, so two devices creating the
// same name at once cannot both land; the second is refused rather than
// silently replacing the first's value, reach and description.
func (s *Store) Create(ctx context.Context, name, value, reach, description string) (PutResult, error) {
	return s.put(ctx, name, &value, reach, description, true)
}

// PutProse replaces an existing secret's reach and description and keeps its
// stored value — the PUT with no `value` (frontend §4.5 #13). Rewriting what
// a secret reaches is the §10.4 control, and asking for the value again to do
// it would handle a credential for no reason. It is never a rotation. A name
// with no secret is refused with CodeValueRequired: a new secret has no value
// to keep.
func (s *Store) PutProse(ctx context.Context, name, reach, description string) (PutResult, error) {
	return s.put(ctx, name, nil, reach, description, false)
}

// put is both: value nil keeps the stored one, and is not the same as "",
// which ValidateValue refuses. createOnly refuses a stored name (Create).
func (s *Store) put(ctx context.Context, name string, value *string, reach, description string, createOnly bool) (PutResult, error) {
	checks := []error{ValidateName(name), nil, ValidateReach(reach), ValidateDescription(description)}
	if value != nil {
		checks[1] = ValidateValue(*value)
	}
	for _, err := range checks {
		if err != nil {
			return PutResult{}, err
		}
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return PutResult{}, err
	}
	defer tx.Rollback()

	var res PutResult
	var id string
	var oldCT, oldNonce []byte
	switch err := tx.QueryRowContext(ctx, `SELECT id, ciphertext, nonce FROM secret WHERE name = ?`, name).Scan(&id, &oldCT, &oldNonce); {
	case errors.Is(err, sql.ErrNoRows):
		if value == nil {
			return PutResult{}, &Invalid{Code: CodeValueRequired, Message: "A new secret needs a value.",
				Detail: "There is no secret by this name, so there is no stored value to keep."}
		}
		if id, err = workspace.NewID(now, s.Env.Random); err != nil {
			return PutResult{}, err
		}
		ct, nonce, err := s.seal(id, *value)
		if err != nil {
			return PutResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret (id, name, ciphertext, nonce, reach, description, all_repos, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, ?)`, id, name, ct, nonce, reach, description, ts(now)); err != nil {
			return PutResult{}, err
		}
		res.Created = true
	case err != nil:
		return PutResult{}, err
	case createOnly:
		return PutResult{}, ErrExists
	default:
		// No value, or an unchanged one: only the prose moves, and nothing
		// is stale. A row that no longer opens (a replaced master key) is
		// overwritten by a value: putting it again is exactly how that is
		// repaired. With no value nothing is opened at all — the row keeps
		// its ciphertext, whether or not it opens.
		same := value == nil
		if !same {
			old, oerr := s.open(id, oldCT, oldNonce)
			same = oerr == nil && subtle.ConstantTimeCompare([]byte(old), []byte(*value)) == 1
			old = ""
		}
		if same {
			_, err = tx.ExecContext(ctx, `UPDATE secret SET reach = ?, description = ? WHERE id = ?`, reach, description, id)
		} else {
			ct, nonce, serr := s.seal(id, *value)
			if serr != nil {
				return PutResult{}, serr
			}
			_, err = tx.ExecContext(ctx, `UPDATE secret SET ciphertext = ?, nonce = ?, reach = ?, description = ?, rotated_at = ? WHERE id = ?`,
				ct, nonce, reach, description, ts(now), id)
			res.Rotated = true
		}
		if err != nil {
			return PutResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PutResult{}, err
	}
	s.Invalidate()

	res.Stale = Stale{NewCommands: []StaleWorkspace{}, NeedsSupervisorRestart: []StaleWorkspace{}}
	if res.Rotated {
		if res.Stale, err = s.stale(ctx, id); err != nil {
			return PutResult{}, err
		}
	}
	if res.Secret, err = s.Get(ctx, name); err != nil {
		return PutResult{}, err
	}
	switch {
	case res.Created:
		s.emit(ctx, "secret.created", "Stored the secret "+name+". It is granted to nothing yet.", map[string]any{"secret": res.Secret})
	case res.Rotated:
		s.emit(ctx, "secret.rotated", fmt.Sprintf("Rotated the secret %s; %d running workspaces hold it.",
			name, len(res.Stale.NewCommands)+len(res.Stale.NeedsSupervisorRestart)),
			map[string]any{"secret": res.Secret, "stale": res.Stale})
	default:
		s.emit(ctx, "secret.updated", "Updated the reach and description of the secret "+name+"; its value is unchanged.", map[string]any{"secret": res.Secret})
	}
	s.recheck(ctx)
	return res, nil
}

// stale lists the running workspaces a secret reaches, by kind. Running only:
// a stopped or building workspace has no command shell yet, and its next
// start reads current values anyway.
func (s *Store) stale(ctx context.Context, secretID string) (Stale, error) {
	out := Stale{NewCommands: []StaleWorkspace{}, NeedsSupervisorRestart: []StaleWorkspace{}}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT w.id, w.repository_id, r.full_name FROM workspace w JOIN repository r ON r.id = w.repository_id
		WHERE w.state = 'running' AND (
		  (SELECT all_repos FROM secret WHERE id = ?) = 1 OR
		  w.repository_id IN (SELECT repository_id FROM secret_grant WHERE secret_id = ?))
		ORDER BY w.id`, secretID, secretID)
	if err != nil {
		return out, err
	}
	var list []StaleWorkspace
	for rows.Next() {
		var w StaleWorkspace
		if err := rows.Scan(&w.WorkspaceID, &w.RepositoryID, &w.FullName); err != nil {
			rows.Close()
			return out, err
		}
		list = append(list, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, w := range list {
		kind := StaleNewCommands
		if s.StaleKind != nil {
			kind = s.StaleKind(ctx, w.WorkspaceID)
		}
		if kind == StaleNeedsSupervisorRestart {
			out.NeedsSupervisorRestart = append(out.NeedsSupervisorRestart, w)
		} else {
			out.NewCommands = append(out.NewCommands, w)
		}
	}
	return out, nil
}

// Delete removes a secret and, by the foreign key's cascade, every grant. Its
// secret_access rows stay: "which workspaces ever held it" outlives it.
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM secret WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.Invalidate()
	s.emit(ctx, "secret.deleted", "Deleted the secret "+name+".", map[string]any{"name": name})
	s.recheck(ctx)
	return nil
}

// SetGrants replaces the set of repositories that may receive a secret, and
// its all_repos flag. Changing a grant is not a rotation: the grant set is
// resolved in the broker on every call, so the next command sees the change
// with nothing to restart and nothing marked stale (frontend §6.4).
func (s *Store) SetGrants(ctx context.Context, name string, repoIDs []int64, allRepos bool) (Meta, error) {
	want := slices.Clone(repoIDs)
	slices.Sort(want)
	want = slices.Compact(want)
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Meta{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM secret WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNotFound
	} else if err != nil {
		return Meta{}, err
	}
	for _, r := range want {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM repository WHERE id = ?`, r).Scan(&n); err != nil {
			return Meta{}, err
		}
		if n == 0 {
			return Meta{}, &Invalid{Code: CodeUnknownRepo, Message: "That repository is not in the catalog.",
				Detail: fmt.Sprintf("No repository has id %d. Refresh the repository list and try again.", r)}
		}
	}
	have := map[int64]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT repository_id FROM secret_grant WHERE secret_id = ?`, id)
	if err != nil {
		return Meta{}, err
	}
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return Meta{}, err
		}
		have[r] = true
	}
	rows.Close()
	for r := range have {
		if !slices.Contains(want, r) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM secret_grant WHERE secret_id = ? AND repository_id = ?`, id, r); err != nil {
				return Meta{}, err
			}
		}
	}
	for _, r := range want {
		if !have[r] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO secret_grant (secret_id, repository_id, granted_at) VALUES (?, ?, ?)`, id, r, ts(now)); err != nil {
				return Meta{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE secret SET all_repos = ? WHERE id = ?`, allRepos, id); err != nil {
		return Meta{}, err
	}
	if err := tx.Commit(); err != nil {
		return Meta{}, err
	}
	s.Invalidate()
	m, err := s.Get(ctx, name)
	if err != nil {
		return Meta{}, err
	}
	msg := fmt.Sprintf("Granted the secret %s to %d repositories.", name, len(m.Grants))
	if allRepos {
		msg = "Granted the secret " + name + " to every repository."
	}
	s.emit(ctx, "secret.grants", msg, map[string]any{"secret": m})
	s.recheck(ctx)
	return m, nil
}

func (s *Store) emit(ctx context.Context, kind, msg string, data any) {
	if s.Events != nil {
		s.Events.Emit(ctx, "", events.Info, kind, msg, data)
	}
}

// ---- delivery -----------------------------------------------------------------

// Resolve returns the secrets a repository may receive, by name, from the
// decrypted snapshot — building it first if a write invalidated it. This is
// the broker's per-command path, so after the first call it touches neither
// the database nor the cipher.
//
// A snapshot that holds anything undeliverable — a row that does not open, or
// a name or value the write-time rules would refuse, which only a database
// edit can produce — is an error for every repository, not a partial answer:
// the prelude then fails the command loudly (§10.3 constraint 4), which beats
// running a test suite against half an environment.
func (s *Store) Resolve(ctx context.Context, repositoryID int64) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked(ctx)
	if err := s.snap.err; err != nil {
		if s.snap.transient {
			s.snap = nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	var out []Entry
	for _, list := range [][]Entry{s.snap.all, s.snap.byRepo[repositoryID]} {
		for _, e := range list {
			if !seen[e.ID] {
				seen[e.ID] = true
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Undeliverable is why stored secrets cannot be delivered right now: the
// rows that fail delivery, by name, and since when. While it holds, Resolve
// refuses every repository (frontend §4.5 #12). It carries names and reasons
// and nothing else — no value, no ciphertext, no id a caller could act on.
type Undeliverable struct {
	// Since is when this process first found the condition. A restart
	// finds it again and starts the clock again.
	Since   time.Time             `json:"since"`
	Secrets []UndeliverableSecret `json:"secrets"`
}

// UndeliverableSecret is one row that fails delivery.
type UndeliverableSecret struct {
	Name string `json:"name"`
	// Reason is ReasonDoesNotOpen or ReasonBreaksRules.
	Reason string `json:"reason"`
}

const (
	// ReasonDoesNotOpen: the row does not decrypt under this master key —
	// a replaced key, or ciphertext moved from another row. Storing the
	// value again (a PUT with a value) repairs it.
	ReasonDoesNotOpen = "does_not_open"
	// ReasonBreaksRules: it opens, but its name or value is one the
	// write-time rules refuse (§10.1), which only a database edit can
	// produce. Deleting it is the repair; a PUT of a name the rules refuse
	// is refused too.
	ReasonBreaksRules = "breaks_write_rules"
)

// Undeliverable reports whether every stored secret can be delivered: nil
// when they can, and the condition when they cannot. It reads the same
// snapshot the broker does, building it if a write invalidated it, so what
// GET /api/secrets says is what the next GET-SECRETS will do. The error is a
// failed read, which says nothing either way.
func (s *Store) Undeliverable(ctx context.Context) (*Undeliverable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked(ctx)
	if s.snap.transient {
		err := s.snap.err
		s.snap = nil
		return nil, err
	}
	if s.undeliverable == nil {
		return nil, nil
	}
	u := *s.undeliverable
	u.Secrets = slices.Clone(u.Secrets)
	return &u, nil
}

// recheck rebuilds the snapshot after a write, rather than on the next
// fetch, so a write that repairs the last undeliverable row announces it at
// once (secret.deliverable) instead of whenever a workspace next runs a
// command. A write can only repair: every write is validated, and the key
// does not change while the process runs.
func (s *Store) recheck(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked(ctx)
	if s.snap.transient {
		s.snap = nil
	}
}

// ensureLocked builds the snapshot if there is none, and records what it
// found about delivery. A transient failure (a failed read) records nothing:
// it is not evidence about the rows.
func (s *Store) ensureLocked(ctx context.Context) {
	if s.snap != nil {
		return
	}
	s.snap = s.build(ctx)
	if !s.snap.transient {
		s.noteLocked(ctx, s.snap.bad)
	}
}

// noteLocked moves the delivery state and announces each change on the
// stream — secret.undeliverable when it breaks or the set of broken rows
// changes, secret.deliverable when it is fixed — so a client that saw one
// learns of the other (frontend §4.5 #12). Unchanged, it says nothing: a
// grant edit while broken is not a new report.
//
// A process that has not yet looked starts from the event log, not from
// "fine": a restart that finds the rows repaired (the old key restored)
// announces the repair to clients that saw the last process's report.
func (s *Store) noteLocked(ctx context.Context, bad []UndeliverableSecret) {
	wasBad := s.undeliverable != nil
	if !s.deliveryKnown {
		wasBad = s.lastReportedUndeliverable(ctx)
		s.deliveryKnown = true
	}
	if len(bad) > 0 {
		if s.undeliverable != nil && slices.Equal(s.undeliverable.Secrets, bad) {
			return
		}
		since := s.now()
		if s.undeliverable != nil {
			since = s.undeliverable.Since
		}
		s.undeliverable = &Undeliverable{Since: since, Secrets: bad}
		if s.Events != nil {
			// The message names secrets and rules, never a value.
			s.Events.Emit(ctx, "", events.Error, "secret.undeliverable",
				"Stored secrets cannot be delivered, so every workspace's commands will fail until this is fixed: "+s.snap.err.Error(),
				map[string]any{"undeliverable": s.undeliverable})
		}
		return
	}
	s.undeliverable = nil
	if wasBad && s.Events != nil {
		s.Events.Emit(ctx, "", events.Info, "secret.deliverable",
			"Stored secrets can be delivered again.", map[string]any{})
	}
}

// lastReportedUndeliverable is whether the newest delivery event in the log
// is a secret.undeliverable — by this process or an earlier one.
func (s *Store) lastReportedUndeliverable(ctx context.Context) bool {
	var kind string
	err := s.DB.QueryRowContext(ctx, `SELECT kind FROM event WHERE kind IN ('secret.undeliverable', 'secret.deliverable')
		ORDER BY id DESC LIMIT 1`).Scan(&kind)
	return err == nil && kind == "secret.undeliverable"
}

func (s *Store) build(ctx context.Context) *snapshot {
	snap := &snapshot{byRepo: map[int64][]Entry{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, ciphertext, nonce, all_repos FROM secret ORDER BY name`)
	if err != nil {
		return &snapshot{err: fmt.Errorf("secrets: read: %w", err), transient: true}
	}
	byID := map[string]Entry{}
	var all []string
	var bad []UndeliverableSecret
	var why []string
	for rows.Next() {
		var id, name string
		var ct, nonce []byte
		var allRepos bool
		if err := rows.Scan(&id, &name, &ct, &nonce, &allRepos); err != nil {
			rows.Close()
			return &snapshot{err: err, transient: true}
		}
		// Every row is checked, not just the first that fails, so the
		// report names every secret that needs repairing.
		v, err := s.open(id, ct, nonce)
		if err != nil {
			bad = append(bad, UndeliverableSecret{Name: name, Reason: ReasonDoesNotOpen})
			why = append(why, err.Error())
			continue
		}
		// Write-time rules, again, at the one place a value leaves. Only a
		// row written around Put can fail them; such a row is never sent.
		if ValidateName(name) != nil || ValidateValue(v) != nil {
			bad = append(bad, UndeliverableSecret{Name: name, Reason: ReasonBreaksRules})
			why = append(why, fmt.Sprintf("secrets: secret %s fails the write-time rules; delete it and store it again", id))
			continue
		}
		byID[id] = Entry{ID: id, Name: name, Value: v}
		if allRepos {
			all = append(all, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return &snapshot{err: err, transient: true}
	}
	if len(bad) > 0 {
		// Nothing decrypted is kept: the snapshot that refuses holds no value.
		return &snapshot{err: errors.New(strings.Join(why, "; ")), bad: bad}
	}
	for _, id := range all {
		snap.all = append(snap.all, byID[id])
	}
	g, err := s.DB.QueryContext(ctx, `SELECT secret_id, repository_id FROM secret_grant`)
	if err != nil {
		return &snapshot{err: err, transient: true}
	}
	defer g.Close()
	for g.Next() {
		var id string
		var repo int64
		if err := g.Scan(&id, &repo); err != nil {
			return &snapshot{err: err, transient: true}
		}
		if e, ok := byID[id]; ok {
			snap.byRepo[repo] = append(snap.byRepo[repo], e)
		}
	}
	if err := g.Err(); err != nil {
		return &snapshot{err: err, transient: true}
	}
	return snap
}

// RecordAccess writes one secret_access row per secret delivered, in one
// transaction (§10.4: "which workspaces ever held this?" has no other answer).
func (s *Store) RecordAccess(ctx context.Context, workspaceID string, delivered []Entry) error {
	if len(delivered) == 0 {
		return nil
	}
	at := ts(s.now())
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range delivered {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_access (secret_id, workspace_id, at) VALUES (?, ?, ?)`, e.ID, workspaceID, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}
