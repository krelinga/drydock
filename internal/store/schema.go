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
}
