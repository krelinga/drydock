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
	// Dir holds the sockets, one directory per workspace:
	// /run/drydock/sock/<id>/broker.sock. It is 0700 and Drydock's own, and
	// that — not the socket's mode — is what keeps host users out; see Open.
	Dir    string
	GitHub *github.Client
	DB     *sql.DB
	Events *events.Log
	Env    sys.Env
	// Secrets answers GET-SECRETS. Nil when no master key is configured,
	// and then there are no secrets: every workspace gets count=0.
	Secrets SecretSource
	// Logf, if set, is the journal: a grant or an access that could not be
	// recorded is written there (never the token or a value), since the
	// answer the client gets is only "unavailable".
	Logf func(format string, args ...any)

	mu        sync.Mutex
	listeners map[string]net.Listener
	// recordMu guards minted: workspace|scope → the expiry of the token
	// whose token_grant row is written.
	recordMu sync.Mutex
	minted   map[string]time.Time
	wg       sync.WaitGroup
}

// requestTimeout bounds a connection: a client that connects and says
// nothing must not hold a goroutine forever.
const requestTimeout = 10 * time.Second

var workspaceID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// SocketName is the socket's name inside its workspace's directory, on the
// host and in the container alike.
const SocketName = "broker.sock"

// maxSocketPath is the longest path a Unix socket can be reached by on
// Linux: sun_path's 108 bytes, less the terminating NUL.
const maxSocketPath = 107

// SocketDir is the workspace's own directory under Dir, holding its socket
// and nothing else. It — not the socket — is what is bind-mounted into the
// container (container.BrokerMountPoint; design §6 step 5, §9.1).
//
// A directory, because a bind mount pins the inode it was given. A socket
// mounted as a file is the inode of the socket that existed when the
// container started; a restart of Drydock unlinks it and listens again on a
// new inode, and the container's mount names the dead one for as long as the
// container lives — git, gh and every command's secrets prelude then fail in
// a workspace the UI still shows running. A mounted directory shows its
// current entries, so a socket recreated inside it is the container's at
// once. The directory itself must therefore outlive Drydock's process: Open
// never recreates one that exists, only Remove deletes it, and the unit keeps
// its runtime directory across restarts (RuntimeDirectoryPreserve,
// deploy/install.sh).
func (b *Broker) SocketDir(workspaceID string) string {
	return filepath.Join(b.Dir, workspaceID)
}

// SocketPath is where a workspace's socket lives on the host:
// <Dir>/<id>/broker.sock. Inside the container it is
// /run/drydock/broker.sock (container.BrokerSocketInContainer).
func (b *Broker) SocketPath(workspaceID string) string {
	return filepath.Join(b.SocketDir(workspaceID), SocketName)
}

// legacySocketPath is where a Drydock before the directory mount kept a
// workspace's socket: a file directly in Dir.
func (b *Broker) legacySocketPath(workspaceID string) string {
	return filepath.Join(b.Dir, workspaceID+".sock")
}

// stagingPath is where Open binds before moving the socket into place: in
// Dir, which no container sees.
func (b *Broker) stagingPath(workspaceID string) string {
	return filepath.Join(b.Dir, "."+workspaceID+".sock")
}

// Open starts the workspace's socket, replacing a stale one left by a crash
// or by the previous process.
//
// Three modes, each deliberate. Dir is 0700 and Drydock's own: that is what
// keeps other host users out, since they cannot traverse it. The workspace's
// directory inside it is 0755, because the container sees the mounted
// directory with the host's ownership and its user — uid 1000 in most dev
// containers — is neither Drydock nor in its group, and must reach the
// socket in it. The socket is 0666 for the same reason. Who can connect is
// decided by which directory is mounted where, which is the design's point
// (§9.1): each container gets its own workspace's directory and no other.
//
// The mount cannot be read-only (container.UpSpec.BrokerDir), so root in the
// container can write in that one directory, and whatever Open finds there
// is the container's. Hence the staging path: the socket is bound and
// chmodded in Dir, which no container sees, and renamed into place. The
// chmod never acts on a path a container can swap for a symlink; the socket
// appears whole, never for an instant with the umask's mode; and rename
// replaces whatever is at the name — a stale socket, or a file or symlink the
// container put there — without following it. Only a directory there is
// refused, since rename cannot replace one.
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
	dir, path, staging := b.SocketDir(wsID), b.SocketPath(wsID), b.stagingPath(wsID)
	// The socket is bound at the staging path, which is shorter, so the
	// kernel would not notice a final path too long for sun_path; a client
	// could then never connect to it.
	if len(path) > maxSocketPath {
		return fmt.Errorf("broker: socket path %s is %d bytes, more than a Unix socket's %d", path, len(path), maxSocketPath)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, open := b.listeners[wsID]; open {
		return nil
	}
	// A directory that exists is kept: a running container's mount names
	// this very inode, and a new directory would strand it exactly as a new
	// socket file did.
	switch fi, err := os.Lstat(dir); {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("broker: %w", err)
		}
	case err != nil:
		return fmt.Errorf("broker: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("broker: %s exists and is not a directory; refusing to replace it", dir)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		return fmt.Errorf("broker: %s is a directory, put there from inside the container; refusing to replace it", path)
	}
	os.Remove(staging) // one a crash left between bind and rename
	ln, err := net.Listen("unix", staging)
	if err != nil {
		return fmt.Errorf("broker: %w", err)
	}
	// Close removes the socket by its real path; the listener must not
	// unlink the staging name on close, which by then may be another Open's.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(staging, 0o666); err != nil {
		ln.Close()
		os.Remove(staging)
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		ln.Close()
		os.Remove(staging)
		return fmt.Errorf("broker: %w", err)
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
// finds no socket: GitHub access is gone at once, with nothing to revoke
// (§9.1, Fig 3). The directory stays, so the container's mount still names
// it when the socket is opened again — after a restart above all, when Close
// is CloseAll's. Remove deletes it with the workspace.
//
// A socket file this process is not serving — one left by an earlier
// process, for a workspace whose delete is being resumed at boot — is
// removed too, so a closed workspace has no socket on disk whichever process
// opened it; and so is one at the path a Drydock before the directory mount
// used. Only a socket: anything else at the path is not Drydock's.
func (b *Broker) Close(wsID string) error {
	if !workspaceID.MatchString(wsID) {
		return fmt.Errorf("broker: %q is not a workspace id", wsID)
	}
	b.mu.Lock()
	ln, ok := b.listeners[wsID]
	delete(b.listeners, wsID)
	b.mu.Unlock()
	var err error
	if ok {
		err = ln.Close()
	}
	for _, path := range []string{b.SocketPath(wsID), b.legacySocketPath(wsID)} {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSocket != 0 {
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}
	return err
}

// ErrLeftover: Remove could not delete everything in a workspace's
// directory. Only the container can have put anything there besides the
// socket — as its root, which owns what it makes — and Drydock cannot delete
// a non-empty directory root made. It is a tmpfs, so it goes at the next
// reboot; nothing else reads it, and the workspace's id is never reused.
var ErrLeftover = errors.New("broker: something the container left in its broker directory could not be removed")

// Remove is Close and then the workspace's directory with whatever is in it:
// a delete's broker_socket sub-step, which runs after the workspace's
// containers are gone, so no mount names the directory any more and nothing
// can be writing in it. os.RemoveAll removes a symlink, never what it names.
// What cannot be removed is ErrLeftover.
func (b *Broker) Remove(wsID string) error {
	if err := b.Close(wsID); err != nil {
		return err
	}
	dir := b.SocketDir(wsID)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("broker: %s is not a directory; refusing to remove it", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrLeftover, err)
	}
	return nil
}

// Serving reports whether this process has the workspace's socket open.
func (b *Broker) Serving(wsID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.listeners[wsID]
	return ok
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
		b.logf("drydock: broker: workspace %s: secret_access could not be recorded, so no secrets were served: %v", wsID, err)
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
		// A fixed sentence keyed by reason, and never GitHub's own message:
		// the event is what the operator reads, so it says what to check.
		// The permission set is the scope's constant, not a secret.
		b.Events.Emit(ctx, wsID, events.Warn, "token.refused", RefusedSentence(scope, reason),
			map[string]any{"scope": scope, "reason": reason, "permissions": scope.Permissions()})
		return errLine(reason)
	}
	if err := b.record(ctx, wsID, bd.repositoryID, scope, tok); err != nil {
		b.logf("drydock: broker: workspace %s: the %s token's token_grant could not be recorded, so it was not served: %v", wsID, scope, err)
		// Never served unrecorded (see record): the next request retries.
		return errLine(ReasonUnavailable)
	}
	return okToken(tok.Value(), tok.ExpiresAt)
}

func (b *Broker) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// record writes a token_grant row and a token.issued event the first time a
// token is served, not on every cache hit: the row says a token was minted
// (§4), and twenty `gh` calls on one cached token are one grant.
//
// The token is marked recorded only once its row is written, and record's
// error stops it being served (token answers unavailable). The row is the
// only answer to "which workspaces held a token for this repository, and
// when?" — GitHub's own log names the App, not the workspace — so a token
// served without one is a hole in that history, exactly as secret_access is
// for secrets (§10.4), which refuse to answer when they cannot record. The
// cost is small: a database that cannot take one row is failing everything
// else too, and the next request retries the write (the token itself is in
// the client's cache, so it costs GitHub nothing). recordMu makes the check,
// the write and the mark one step, so concurrent requests for one token
// write one row.
func (b *Broker) record(ctx context.Context, wsID string, repoID int64, scope Scope, tok github.Token) error {
	k := wsID + "|" + string(scope)
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	if b.minted[k].Equal(tok.ExpiresAt) {
		return nil
	}

	now := b.Env.Clock.Now().UTC()
	id, err := workspace.NewID(now, b.Env.Random)
	if err != nil {
		return err
	}
	perms, _ := json.Marshal(scope.Permissions())
	if _, err := b.DB.ExecContext(ctx, `INSERT INTO token_grant (id, workspace_id, repository_id, permissions, issued_at, expires_at, requested_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, wsID, repoID, string(perms),
		now.Format(time.RFC3339Nano), tok.ExpiresAt.UTC().Format(time.RFC3339Nano), scope.requestedBy()); err != nil {
		return err
	}
	if b.minted == nil {
		b.minted = map[string]time.Time{}
	}
	b.minted[k] = tok.ExpiresAt
	// The row is the record; the event is a courtesy to the live view, and
	// a failure to publish it does not unrecord the token.
	b.Events.Emit(ctx, wsID, events.Info, "token.issued", fmt.Sprintf("Issued a %s token.", scope),
		map[string]any{"scope": scope, "expires_at": tok.ExpiresAt.UTC()})
	return nil
}

// reasonFor maps a GitHub failure to the protocol's closed set. A 429, or a
// 403 that says "rate limit" or "suspended", is GitHub refusing the App as a
// whole. A 422 is two different refusals that only GitHub's message tells
// apart (the contract tests pin both against the real App): a permission the
// App lacks, or the installation has not accepted, is the operator's to fix;
// a repository the installation does not include is a revocation. A 404 is
// an installation that is gone. Anything else, a 422 included, is
// unavailable: never a guess.
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
	case github.IsPermissionNotGranted(err):
		return ReasonPermissionMissing
	case ae.Status == http.StatusNotFound, github.IsRepositoryNotIncluded(err):
		return ReasonRevoked
	}
	return ReasonUnavailable
}

// RefusedSentence is token.refused's message: design §12's sentence for the
// reason, naming the cause and what fixes it — and never GitHub's own words,
// nor a retry that will fail the same way. The UI renders the same reasons
// from the event's data (frontend §9); this is the feed's text.
func RefusedSentence(scope Scope, reason string) string {
	switch reason {
	case ReasonRateLimited:
		// §12, *GitHub rate limit or App suspended*: a token already issued
		// serves until it expires; then this. Nothing broader is tried.
		return fmt.Sprintf("GitHub is refusing requests: it would not issue a %s token because the App's rate limit is spent "+
			"or the App is suspended. Tokens already issued work until they expire. Check the App on GitHub; "+
			"nothing in Drydock can retry it sooner.", scope)
	case ReasonPermissionMissing:
		return fmt.Sprintf("GitHub refused a %s token for this workspace: the GitHub App lacks a permission this scope needs. "+
			"Check the App's permissions, and accept any pending permission request on its installation.", scope)
	case ReasonRevoked:
		// §12, *Repo removed from the installation*.
		return fmt.Sprintf("GitHub refused a %s token: this repository is no longer in the GitHub App's installation, "+
			"so the workspace is read-only. Unpushed work in the working tree survives; add the repository back to "+
			"the installation to restore access.", scope)
	}
	return fmt.Sprintf("GitHub did not issue a %s token for this workspace (%s). The next git or gh command asks again.", scope, reason)
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
