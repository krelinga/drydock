package broker

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// Broker serves one socket per workspace.
type Broker struct {
	// Dir holds the sockets: /run/drydock/sock. It is 0700 and Drydock's
	// own, and that — not the socket's mode — is what keeps host users out;
	// see Open.
	Dir    string
	GitHub *github.Client
	DB     *sql.DB
	Events *events.Log
	Env    sys.Env
	// Secrets answers GET-SECRETS. Nil when no master key is configured,
	// and then there are no secrets: every workspace gets count=0.
	Secrets SecretSource

	mu        sync.Mutex
	listeners map[string]net.Listener
	minted    map[string]time.Time // workspace|scope → expiry last recorded
	wg        sync.WaitGroup
}

// requestTimeout bounds a connection: a client that connects and says
// nothing must not hold a goroutine forever.
const requestTimeout = 10 * time.Second

var workspaceID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// SocketPath is where a workspace's socket lives on the host — design §6
// step 5. Inside the container it is mounted at /run/drydock/broker.sock.
func (b *Broker) SocketPath(workspaceID string) string {
	return filepath.Join(b.Dir, workspaceID+".sock")
}

// Open starts the workspace's socket, replacing a stale one left by a crash.
//
// The socket is mode 0666 inside a 0700 directory, and both halves are
// deliberate. The container's user — uid 1000 in most dev containers — is
// neither Drydock nor in its group, so a 0660 socket would refuse it. The
// directory is what stops other users on the host: they cannot traverse it.
// The container is not stopped by the directory, because the bind mount
// hands it the socket's inode directly. So who can connect is decided by
// where the socket is mounted, which is the design's point (§9.1).
func (b *Broker) Open(ctx context.Context, wsID string) error {
	if !workspaceID.MatchString(wsID) {
		return fmt.Errorf("broker: %q is not a workspace id", wsID)
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(b.Dir, 0o700); err != nil {
		return err
	}
	path := b.SocketPath(wsID)

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, open := b.listeners[wsID]; open {
		return nil
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("broker: %s exists and is not a socket; refusing to replace it", path)
		}
		os.Remove(path) // a stale socket from a previous run
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("broker: %w", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return err
	}
	if b.listeners == nil {
		b.listeners = map[string]net.Listener{}
	}
	b.listeners[wsID] = ln
	b.wg.Add(1)
	go b.serve(wsID, ln)
	return nil
}

// Close removes a workspace's socket. A container holding the mount then
// gets ECONNREFUSED: GitHub access is gone at once, with nothing to revoke
// (§9.1, Fig 3).
func (b *Broker) Close(wsID string) error {
	b.mu.Lock()
	ln, ok := b.listeners[wsID]
	delete(b.listeners, wsID)
	b.mu.Unlock()
	if !ok {
		return nil
	}
	err := ln.Close()
	os.Remove(b.SocketPath(wsID))
	return err
}

// CloseAll stops every socket and waits for in-flight requests.
func (b *Broker) CloseAll() {
	b.mu.Lock()
	ids := make([]string, 0, len(b.listeners))
	for id := range b.listeners {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.Close(id)
	}
	b.wg.Wait()
}

func (b *Broker) serve(wsID string, ln net.Listener) {
	defer b.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // closed
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer conn.Close()
			b.handle(wsID, conn)
		}()
	}
}

func (b *Broker) handle(wsID string, conn net.Conn) {
	conn.SetDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(&limited{conn, maxLine + 2}).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	// A line cut off by the length limit fails Parse on its length.
	req, err := Parse(strings.TrimSuffix(line, "\n"))
	if err != nil {
		conn.Write([]byte(errLine(ReasonBadRequest)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	switch req.Verb {
	case "PING":
		conn.Write([]byte("OK\n"))
	case "GET-TOKEN":
		conn.Write([]byte(b.token(ctx, wsID, req.Scope)))
	case "GET-SECRETS":
		conn.Write([]byte(b.secrets(ctx, wsID)))
	}
}

// SecretSource is what GET-SECRETS needs from internal/secrets.
type SecretSource interface {
	Resolve(ctx context.Context, repositoryID int64) ([]secrets.Entry, error)
	RecordAccess(ctx context.Context, workspaceID string, delivered []secrets.Entry) error
}

// secrets answers one GET-SECRETS: the workspace's repository's grant set,
// read from the store's decrypted snapshot.
//
// This runs before every Bash command an agent issues (Spike 03), so it
// stays cheap: one indexed row read for the binding, a map lookup for the
// grant set, and the access rows — no GitHub request and no decryption. A
// failure is an ERR line, which the client turns into `exit 69`: a command
// that cannot have its secrets does not run (§10.3 constraint 4).
func (b *Broker) secrets(ctx context.Context, wsID string) string {
	bd, err := b.binding(ctx, wsID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errLine(ReasonRevoked)
	case err != nil:
		return errLine(ReasonUnavailable)
	case bd.state == string(workspace.Deleting):
		return errLine(ReasonRevoked)
	}
	// Archived or removed from the installation does not stop secrets: the
	// grant is the operator's decision, not GitHub's, and §12 keeps a
	// removed repository's workspace working on what it already has.
	if b.Secrets == nil {
		return secretsAnswer(nil, nil)
	}
	got, err := b.Secrets.Resolve(ctx, bd.repositoryID)
	if err != nil {
		return errLine(ReasonUnavailable)
	}
	// Recorded before it is sent, and not sent if it cannot be recorded:
	// "which workspaces ever held this?" (§10.4) has no other answer.
	if err := b.Secrets.RecordAccess(ctx, wsID, got); err != nil {
		return errLine(ReasonUnavailable)
	}
	names := make([]string, len(got))
	values := make([]string, len(got))
	for i, e := range got {
		names[i], values[i] = e.Name, e.Value
	}
	return secretsAnswer(names, values)
}

// binding is what the broker reads, per request, about the workspace a
// socket belongs to. Read fresh each time: a repository archived or removed
// from the installation since the socket opened must stop getting tokens.
type binding struct {
	repositoryID, installationID int64
	state                        string
	archived, removed            bool
}

func (b *Broker) binding(ctx context.Context, wsID string) (binding, error) {
	var bd binding
	err := b.DB.QueryRowContext(ctx, `
		SELECT w.repository_id, r.installation_id, w.state, coalesce(r.archived, 0), r.removed_at IS NOT NULL
		FROM workspace w JOIN repository r ON r.id = w.repository_id WHERE w.id = ?`, wsID).
		Scan(&bd.repositoryID, &bd.installationID, &bd.state, &bd.archived, &bd.removed)
	return bd, err
}

// token answers one GET-TOKEN. Every failure is an ERR line with a reason
// from the closed set, and none of them retries with anything broader: no
// fallback to an unscoped token, no other scope, no other credential (§12).
func (b *Broker) token(ctx context.Context, wsID string, scope Scope) string {
	bd, err := b.binding(ctx, wsID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errLine(ReasonRevoked)
	case err != nil:
		return errLine(ReasonUnavailable)
	case bd.state == string(workspace.Deleting) || bd.removed:
		return errLine(ReasonRevoked)
	case bd.archived:
		return errLine(ReasonRepoArchived)
	}

	tok, err := b.GitHub.InstallationToken(ctx, github.TokenRequest{
		InstallationID: bd.installationID,
		Permissions:    scope.Permissions(),
		RepositoryIDs:  []int64{bd.repositoryID}, // exactly one, and it is this workspace's
	})
	if err != nil {
		reason := reasonFor(err)
		b.Events.Emit(ctx, wsID, events.Warn, "token.refused",
			"GitHub refused a token for this workspace ("+reason+").", map[string]any{"scope": scope, "reason": reason})
		return errLine(reason)
	}
	b.record(ctx, wsID, bd.repositoryID, scope, tok)
	return okToken(tok.Value(), tok.ExpiresAt)
}

// record writes a token_grant row and a token.issued event the first time a
// token is served, not on every cache hit: the row says a token was minted
// (§4), and twenty `gh` calls on one cached token are one grant.
func (b *Broker) record(ctx context.Context, wsID string, repoID int64, scope Scope, tok github.Token) {
	k := wsID + "|" + string(scope)
	b.mu.Lock()
	if b.minted == nil {
		b.minted = map[string]time.Time{}
	}
	if b.minted[k].Equal(tok.ExpiresAt) {
		b.mu.Unlock()
		return
	}
	b.minted[k] = tok.ExpiresAt
	b.mu.Unlock()

	now := b.Env.Clock.Now().UTC()
	id, err := workspace.NewID(now, b.Env.Random)
	if err != nil {
		return
	}
	perms, _ := json.Marshal(scope.Permissions())
	b.DB.ExecContext(ctx, `INSERT INTO token_grant (id, workspace_id, repository_id, permissions, issued_at, expires_at, requested_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, wsID, repoID, string(perms),
		now.Format(time.RFC3339Nano), tok.ExpiresAt.UTC().Format(time.RFC3339Nano), scope.requestedBy())
	b.Events.Emit(ctx, wsID, events.Info, "token.issued", fmt.Sprintf("Issued a %s token.", scope),
		map[string]any{"scope": scope, "expires_at": tok.ExpiresAt.UTC()})
}

// reasonFor maps a GitHub failure to the protocol's closed set. A 404 or 422
// on the mint means the repository is not (or no longer) the installation's;
// a 429, or a 403 that says "rate limit" or "suspended", is GitHub refusing
// the App as a whole.
func reasonFor(err error) string {
	var ae *github.APIError
	if !errors.As(err, &ae) {
		return ReasonUnavailable
	}
	msg := strings.ToLower(ae.Message)
	switch {
	case ae.Status == http.StatusTooManyRequests,
		ae.Status == http.StatusForbidden && (strings.Contains(msg, "rate limit") || strings.Contains(msg, "suspended")):
		return ReasonRateLimited
	case ae.Status == http.StatusNotFound, ae.Status == http.StatusUnprocessableEntity:
		return ReasonRevoked
	}
	return ReasonUnavailable
}

// limited reads at most n bytes, so an endless line cannot grow a buffer.
type limited struct {
	c net.Conn
	n int
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("broker: request too long")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.c.Read(p)
	l.n -= n
	return n, err
}
