package events

// Kind is an event's kind: dotted, entity first (workspace.state,
// session.status, auth.identity). On the wire it is its string, unchanged.
//
// This file is the registry: every kind the server emits is a constant here,
// with its data shape and its scope, and nowhere else spells one.
// TestKindRegistry holds that by typechecking the module — a kind spelled
// outside this file, a Kind made from a variable (but the scan of a stored
// row), a constant here nothing emits, and a kind without a scope each fail
// by name.
type Kind string

// Scope is who may see a kind on the stream: a credential with only the
// `read` scope sees read kinds; session kinds are for the operator's own
// session alone, since their data names what a read credential may not learn
// from the routes either (a secret's name, reach and grants).
type Scope string

const (
	ScopeRead    Scope = "read"
	ScopeSession Scope = "session"
)

// Each kind: what writes it, its data, and (in Scopes) its scope.
const (
	// internal/workspace: the row's state moved. data: {state, from,
	// detail?}, plus {approval} on a run stopped for host access,
	// {container_id} on running, {repository_id, branch} on create. read.
	KindWorkspaceState Kind = "workspace.state"
	// internal/workspace: a §6 step's progress. data: {step, status:
	// started|done|failed|needs_approval, detail?}. read.
	KindWorkspaceStep Kind = "workspace.step"
	// internal/provision: a stop's or a delete's sub-step. data: {action:
	// stop|delete, step, status, detail?}. read.
	KindWorkspaceAction Kind = "workspace.action"
	// internal/workspace: the row is removed. data: {}. read.
	KindWorkspaceGone Kind = "workspace.gone"
	// internal/workspace: a workspace job ended, however it ended; it
	// settles the request that began it. data: {kind, outcome}. read.
	KindWorkspaceJob Kind = "workspace.job"
	// internal/reconcile: a known row matched to a container at boot.
	// data: {container_id}. read.
	KindWorkspaceAdopted Kind = "workspace.adopted"
	// internal/workspace: a host-access approval recorded. data:
	// {repository_id, hash}. read.
	KindConfigApproved Kind = "config.approved"

	// internal/supervisor: the session server's process state. data:
	// {state, from, reason, detail, restart_count}. read.
	KindSupervisorState Kind = "supervisor.state"
	// internal/supervisor: what the server's output announced. data:
	// {environment_id, url, capacity_used, capacity_total, sessions}. read.
	KindSessionStatus Kind = "session.status"

	// internal/provision: the boot sweep removed helper containers. data:
	// {count}. read.
	KindContainerHelpersSwept Kind = "container.helpers_swept"
	// internal/provision: an action unpaused a paused container to stop its
	// server. data: {count}. read.
	KindContainerUnpaused Kind = "container.unpaused"
	// internal/provision: the action paused it again, or could not. data:
	// {count, failed}. read.
	KindContainerRepaused Kind = "container.repaused"
	// internal/reconcile: a container with this instance's label matched
	// no workspace and was left alone. data: {container_id, reason}. read.
	KindContainerUnclaimed Kind = "container.unclaimed"

	// internal/preview: a port joined the ports list. data: {port,
	// source?} (source: "discovery" on what discovery writes). read.
	KindPortAdded Kind = "port.added"
	// internal/preview: a port's preview turned on. data: {port}. read.
	KindPortEnabled Kind = "port.enabled"
	// internal/preview: a port's preview turned off. data: {port}. read.
	KindPortDisabled Kind = "port.disabled"
	// internal/preview: a port's row changed otherwise. data: {port,
	// source?}. read.
	KindPortUpdated Kind = "port.updated"
	// internal/preview: a port left the list. data: {port_id,
	// container_port, source?}. read.
	KindPortRetired Kind = "port.retired"
	// internal/preview: a rescan's answer. data: {discovery, source}. read.
	KindPortScanned Kind = "port.scanned"
	// internal/preview: discovery's state changed unasked. data:
	// {discovery, source}. read.
	KindPortDiscovery Kind = "port.discovery"

	// internal/catalog: the repository list refreshed. data: {count, added,
	// removed}. read.
	KindRepoRefreshed Kind = "repo.refreshed"
	// internal/catalog: a refresh failed; the reason is the message. data:
	// {}. read.
	KindRepoRefreshFailed Kind = "repo.refresh_failed"
	// internal/workspace (with internal/store): repositories the
	// installation dropped were released by their last workspace. data:
	// {repository_ids}. read.
	KindRepoRemoved Kind = "repo.removed"

	// internal/secrets: a secret stored. data: {secret}. session.
	KindSecretCreated Kind = "secret.created"
	// internal/secrets: a secret's value changed. data: {secret, stale}.
	// session.
	KindSecretRotated Kind = "secret.rotated"
	// internal/secrets: a secret's reach or description changed, never its
	// value. data: {secret}. session.
	KindSecretUpdated Kind = "secret.updated"
	// internal/secrets: a secret's grants changed. data: {secret}. session.
	KindSecretGrants Kind = "secret.grants"
	// internal/secrets: a secret deleted. data: {name}. session.
	KindSecretDeleted Kind = "secret.deleted"
	// internal/secrets: delivery broke, or the broken set changed. data:
	// {undeliverable: {since, secrets: [{name, reason}]}}. session.
	KindSecretUndeliverable Kind = "secret.undeliverable"
	// internal/secrets: delivery recovered. data: {}. session.
	KindSecretDeliverable Kind = "secret.deliverable"

	// internal/broker: a GitHub token was issued to a workspace. data:
	// {scope, expires_at}. read: it names a scope, never the token.
	KindTokenIssued Kind = "token.issued"
	// internal/broker: GitHub refused a workspace's token. data: {scope,
	// reason, permissions}. read.
	KindTokenRefused Kind = "token.refused"

	// internal/auth: an API token minted (vscode-extension §4.2). data:
	// {token: {id, label, scopes, created_at, last_used_at, expires_at,
	// revoked_at}} — metadata only, never the token or its hash. session.
	KindAPITokenCreated Kind = "api_token.created"
	// internal/auth: an API token revoked, from Settings or by itself.
	// data: {token: {…}}, as api_token.created. session.
	KindAPITokenRevoked Kind = "api_token.revoked"

	// internal/identity: the stored Claude identity changed, or a failing
	// check recovered. data: {identity}. read.
	KindAuthIdentity Kind = "auth.identity"
	// internal/identity: a check could not read its inputs; the stored
	// state stands. data: {check_error: {at, problem, message}}. read.
	KindAuthIdentityCheckFailed Kind = "auth.identity_check_failed"
	// internal/identity: a check someone asked for found nothing to
	// announce. data: {identity}. read.
	KindAuthIdentityChecked Kind = "auth.identity_checked"
	// internal/login: the login handshake's every phase change. data:
	// {login}, which holds no code. read.
	KindAuthLogin Kind = "auth.login"

	// internal/server: boot reconciliation failed. data: none. read.
	KindSystemReconcile Kind = "system.reconcile"
)

// Scopes is every registered kind's scope. The stream filter for scoped
// credentials reads it, and must deny a kind missing from it (fail closed);
// a cookie session sees every kind. token.issued and token.refused are the
// broker's GitHub-token events, read: a scope name, never a token.
var Scopes = map[Kind]Scope{
	KindWorkspaceState:   ScopeRead,
	KindWorkspaceStep:    ScopeRead,
	KindWorkspaceAction:  ScopeRead,
	KindWorkspaceGone:    ScopeRead,
	KindWorkspaceJob:     ScopeRead,
	KindWorkspaceAdopted: ScopeRead,
	KindConfigApproved:   ScopeRead,

	KindSupervisorState: ScopeRead,
	KindSessionStatus:   ScopeRead,

	KindContainerHelpersSwept: ScopeRead,
	KindContainerUnpaused:     ScopeRead,
	KindContainerRepaused:     ScopeRead,
	KindContainerUnclaimed:    ScopeRead,

	KindPortAdded:     ScopeRead,
	KindPortEnabled:   ScopeRead,
	KindPortDisabled:  ScopeRead,
	KindPortUpdated:   ScopeRead,
	KindPortRetired:   ScopeRead,
	KindPortScanned:   ScopeRead,
	KindPortDiscovery: ScopeRead,

	KindRepoRefreshed:     ScopeRead,
	KindRepoRefreshFailed: ScopeRead,
	KindRepoRemoved:       ScopeRead,

	KindSecretCreated:       ScopeSession,
	KindSecretRotated:       ScopeSession,
	KindSecretUpdated:       ScopeSession,
	KindSecretGrants:        ScopeSession,
	KindSecretDeleted:       ScopeSession,
	KindSecretUndeliverable: ScopeSession,
	KindSecretDeliverable:   ScopeSession,

	KindTokenIssued:  ScopeRead,
	KindTokenRefused: ScopeRead,

	KindAPITokenCreated: ScopeSession,
	KindAPITokenRevoked: ScopeSession,

	KindAuthIdentity:            ScopeRead,
	KindAuthIdentityCheckFailed: ScopeRead,
	KindAuthIdentityChecked:     ScopeRead,
	KindAuthLogin:               ScopeRead,

	KindSystemReconcile: ScopeRead,
}
