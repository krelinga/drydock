package store

// migrations are applied in order; the index plus one is the schema version.
// Append, never edit: a migration that has run on someone's database is
// history, and changing it makes two databases at the same version disagree.
var migrations = []string{
	// 1 — design §4. Stricter than its text in one respect: §4 states each
	// enumeration (workspace.state, supervisor.state, claude_identity.state,
	// auth_attempt.outcome, token_grant.requested_by) only in a comment, and
	// here they are CHECK constraints. That turns "a 409 is a wait, not a
	// failure" from a convention into something the database refuses to
	// violate: supervisor.state has no 'failed', so code that reaches for it
	// fails loudly at the write. secret.reach is likewise refused when blank,
	// because §10.4 calls the field a control, not documentation.
	//
	// Four deliberate absences are part of this
	// migration as much as the columns are: no plaintext secret value, no
	// plaintext session token (auth_session.id is a SHA-256), no
	// github_token column, and no token in token_grant. The schema tests in
	// store_test.go assert each one, because "a column nobody added" is not
	// something a reviewer reliably notices.
	`
CREATE TABLE operator (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  password_hash TEXT NOT NULL,   -- argon2id, params encoded in the hash
  updated_at    TEXT
);

CREATE TABLE auth_session (
  id                  TEXT PRIMARY KEY,  -- sha256 of the cookie token; never the token
  label               TEXT,
  created_ip          TEXT,
  created_at          TEXT NOT NULL,
  last_seen_at        TEXT NOT NULL,
  absolute_expires_at TEXT NOT NULL
);

CREATE TABLE auth_attempt (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  source_ip TEXT NOT NULL,
  outcome   TEXT NOT NULL CHECK (outcome IN ('ok','bad_password','locked_out')),
  at        TEXT NOT NULL
);
CREATE INDEX auth_attempt_ip_at ON auth_attempt (source_ip, at);
CREATE INDEX auth_attempt_at    ON auth_attempt (at);

CREATE TABLE repository (
  id               INTEGER PRIMARY KEY,
  installation_id  INTEGER NOT NULL,
  full_name        TEXT NOT NULL,
  default_branch   TEXT NOT NULL,
  has_devcontainer INTEGER,
  private          INTEGER,
  archived         INTEGER,
  pushed_at        TEXT,
  refreshed_at     TEXT
);

CREATE TABLE workspace (
  id             TEXT PRIMARY KEY,
  repository_id  INTEGER NOT NULL REFERENCES repository(id),
  host_path      TEXT NOT NULL,
  branch         TEXT NOT NULL,
  config_path    TEXT,
  state          TEXT NOT NULL CHECK (state IN
                   ('pending','cloning','building','running','stopped','failed','deleting')),
  state_detail   TEXT,
  container_id   TEXT,
  remote_user    TEXT,
  environment_id TEXT,
  created_at     TEXT,
  last_active_at TEXT
);

CREATE TABLE supervisor (
  id                TEXT PRIMARY KEY,
  workspace_id      TEXT NOT NULL REFERENCES workspace(id),
  state             TEXT NOT NULL CHECK (state IN
                      ('starting','waiting_registration','awaiting_login','serving','degraded','exited')),
  pid               INTEGER,
  restart_count     INTEGER NOT NULL DEFAULT 0,
  capacity          INTEGER NOT NULL,
  last_error        TEXT,
  started_at        TEXT,
  last_heartbeat_at TEXT
);

CREATE TABLE rc_session (
  id            TEXT PRIMARY KEY,
  supervisor_id TEXT NOT NULL REFERENCES supervisor(id) ON DELETE CASCADE,
  name          TEXT,
  is_primary    INTEGER,
  first_seen_at TEXT,
  last_seen_at  TEXT
);

CREATE TABLE claude_identity (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  volume_name     TEXT NOT NULL,
  account_email   TEXT,
  logged_in_at    TEXT,
  state           TEXT NOT NULL CHECK (state IN ('ok','expiring','expired','blanked','absent')),
  expires_at      TEXT,
  last_checked_at TEXT
);

CREATE TABLE token_grant (
  id            TEXT PRIMARY KEY,
  workspace_id  TEXT NOT NULL,
  repository_id INTEGER NOT NULL,
  permissions   TEXT NOT NULL,
  issued_at     TEXT,
  expires_at    TEXT,
  requested_by  TEXT CHECK (requested_by IN ('git-credential','gh','api'))
);

CREATE TABLE secret (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  ciphertext   BLOB NOT NULL,
  nonce        BLOB NOT NULL,
  reach        TEXT NOT NULL CHECK (length(trim(reach)) > 0),
  description  TEXT,
  all_repos    INTEGER NOT NULL DEFAULT 0,
  inject_hosts TEXT,
  created_at   TEXT,
  rotated_at   TEXT
);

CREATE TABLE secret_grant (
  secret_id     TEXT NOT NULL REFERENCES secret(id) ON DELETE CASCADE,
  repository_id INTEGER NOT NULL REFERENCES repository(id),
  granted_at    TEXT,
  PRIMARY KEY (secret_id, repository_id)
);

CREATE TABLE secret_access (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  secret_id    TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  at           TEXT
);

CREATE TABLE event (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT,
  level        TEXT,
  kind         TEXT,
  message      TEXT,
  at           TEXT
);
`,

	// 2 — event.data, a JSON object: what changed, in a form the frontend's
	// reducer can apply. §4 had only `message`, which is prose for a human,
	// and a reducer that parsed prose to learn a workspace's new state is
	// the string-matching the error envelope exists to avoid. The design doc
	// records the column (§4). Never a credential, like every other column:
	// the canary sweep reads this file's raw bytes.
	`
ALTER TABLE event ADD COLUMN data TEXT;
CREATE INDEX event_workspace ON event (workspace_id, id);
`,

	// 3 — the label prefix, recorded at first run (§6, §13.5). Reconciliation
	// adopts and deletes by label, so the prefix a database was created with
	// is the one whose containers it owns; starting it later under another
	// prefix would orphan all of them and adopt someone else's.
	`
CREATE TABLE instance (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  label_prefix TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
`,

	// 4 — the catalog's other half. installation records the accounts the
	// App is installed on, because the settings link the UI gives for a
	// missing repository (§9.4) differs for a user and an organization.
	// repository.removed_at marks a repository the installation no longer
	// covers but a workspace still holds: §12 keeps that workspace and its
	// unpushed work, so the row cannot simply be deleted.
	`
CREATE TABLE installation (
  id           INTEGER PRIMARY KEY,
  account      TEXT NOT NULL,
  account_type TEXT NOT NULL,
  refreshed_at TEXT NOT NULL
);
ALTER TABLE repository ADD COLUMN removed_at TEXT;
`,

	// 5 — secrets as built (§10). GET-SECRETS writes a secret_access row per
	// secret per fetch, and the prelude fetches before every Bash command
	// (Spike 03), so this table grows with use; GET /api/secrets reads each
	// secret's last access and the workspaces that ever held it, which
	// without an index is a scan of every row ever written.
	`
CREATE INDEX secret_access_secret ON secret_access (secret_id, workspace_id, at);
`,

	// 6 — host-access approvals (design §6, "What a configuration may ask of
	// the host"). A start, rebuild or create whose configuration reaches
	// outside the container runs only when the operator has approved exactly
	// that host-access subset for the repository. config_approval is the
	// history, never deleted: a new approval supersedes the last rather than
	// replacing it, so "who let this repository run privileged, and when?"
	// stays answerable. approved_by is the approving session's id, which is
	// itself a SHA-256 of the cookie (§4), never a usable credential.
	// workspace.pending_approval is the request a run stopped at — the subset,
	// its hash, and how it differs from the approved one — kept on the row so
	// a reloaded page shows what the live events showed; any move clears it.
	`
CREATE TABLE config_approval (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id INTEGER NOT NULL,
  hash          TEXT NOT NULL,
  settings      TEXT NOT NULL,  -- the approved subset, canonical JSON
  workspace_id  TEXT,           -- the workspace it was approved from
  approved_by   TEXT NOT NULL,  -- auth_session.id: a SHA-256, never the cookie
  approved_at   TEXT NOT NULL,
  superseded_at TEXT
);
CREATE INDEX config_approval_repository ON config_approval (repository_id, superseded_at);
ALTER TABLE workspace ADD COLUMN pending_approval TEXT;
`,

	// 7 — the login's own expiry (design §7.3). expires_at is the ACCESS
	// token's, which a real login sets about eight hours out (measured
	// 2026-10-08) and every refresh moves; the login ends when the refresh
	// token does, which Claude Code records as refreshTokenExpiresAt.
	// login_expires_at is that, NULL when the file carries none. 'expiring'
	// meant "the access token ends within three days" before this migration
	// — true of every login — and means "the login ends within three days"
	// after it, so a row stored under the old meaning becomes 'ok' (it was a
	// live login) until the boot check, which runs at once, rewrites it.
	`
ALTER TABLE claude_identity ADD COLUMN login_expires_at TEXT;
UPDATE claude_identity SET state = 'ok' WHERE state = 'expiring';
`,

	// 8 — previews' two tables (port forwarding §5, §7; §13 step 2).
	//
	// forwarded_port is a permission to reach one port on one workspace:
	// default-deny, so no enabled row, no preview. A row is retired, never
	// deleted, so its slug stays spent and the global UNIQUE on slug is what
	// keeps a stale bookmark from resolving to another workspace (PF §4,
	// testing §15.4). For the same reason workspace_id has no foreign key:
	// §5's ON DELETE CASCADE would delete the row with its workspace and free
	// the slug for reissue, so workspace.Remove retires a workspace's ports
	// instead. The slug's CHECKs are the DNS label PF §4 describes, and
	// 'drydock-check' — the installer's probe name — can never be one.
	//
	// preview_session is a device's proof that it may view one preview host.
	// Its id is the SHA-256 of the cookie, never the cookie, as auth_session's
	// is. It cascades from auth_session, so a revoke — one device or all —
	// ends every preview it minted (§13.2), and from forwarded_port, though a
	// port is retired rather than deleted: disabling or retiring deletes the
	// rows explicitly (preview.Store), so a re-enable never revives one.
	`
CREATE TABLE forwarded_port (
  id              TEXT PRIMARY KEY,
  workspace_id    TEXT NOT NULL,
  container_port  INTEGER NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
  slug            TEXT NOT NULL UNIQUE CHECK (
                    length(slug) BETWEEN 1 AND 63
                    AND slug NOT GLOB '*[^a-z0-9-]*'
                    AND slug NOT GLOB '-*' AND slug NOT GLOB '*-'
                    AND slug <> 'drydock-check'),
  label           TEXT,
  upstream_scheme TEXT NOT NULL DEFAULT 'http' CHECK (upstream_scheme IN ('http','https')),
  host_header     TEXT NOT NULL DEFAULT 'localhost' CHECK (host_header IN ('localhost','passthrough')),
  enabled         INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
  hidden          INTEGER NOT NULL DEFAULT 0 CHECK (hidden IN (0,1)),
  declared        INTEGER NOT NULL DEFAULT 0 CHECK (declared IN (0,1)),
  observed        INTEGER NOT NULL DEFAULT 0 CHECK (observed IN (0,1)),
  manual          INTEGER NOT NULL DEFAULT 0 CHECK (manual IN (0,1)),
  bind_addr       TEXT,
  observed_state  TEXT CHECK (observed_state IN ('listening','gone','never_seen')),
  first_seen_at   TEXT,
  last_seen_at    TEXT,
  created_at      TEXT,
  last_used_at    TEXT,
  retired_at      TEXT
);
CREATE UNIQUE INDEX forwarded_port_live
  ON forwarded_port (workspace_id, container_port) WHERE retired_at IS NULL;

CREATE TABLE preview_session (
  id                TEXT PRIMARY KEY,  -- sha256 of the preview cookie; never the cookie
  auth_session_id   TEXT NOT NULL REFERENCES auth_session(id) ON DELETE CASCADE,
  forwarded_port_id TEXT NOT NULL REFERENCES forwarded_port(id) ON DELETE CASCADE,
  preview_host      TEXT NOT NULL,
  created_at        TEXT NOT NULL,
  last_seen_at      TEXT NOT NULL
);
CREATE INDEX preview_session_auth ON preview_session (auth_session_id);
CREATE INDEX preview_session_port ON preview_session (forwarded_port_id);
`,
}
