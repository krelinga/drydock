# deploy/ — what ships to the server

`package.sh` builds the release assets (the same script in CI and in the installer test). The
README's one-liner is the contract: change an asset name, a flag, or `/etc/drydock/drydock.env`
and an existing install's re-run is what breaks. **A change to the installer updates both
`docs/deploy/first-deployment.md` and `docs/deploy/first-deployment-ansible.md`** in the same
change.

## `Caddyfile`, `preview.caddy`

The entire LAN-facing surface, every value an env placeholder so the shipped files are the tested
files (`test/component`). The preview site is a separate, optional file imported by glob, so a
first deployment needs no wildcard certificate. It matches exactly one label under the preview
domain, passes the cookie and query through to `preview.sock` untouched (the preview mux
authenticates on them, and strips the cookie at the next hop, PF §7), and keeps access logging
off. The preview domain, its wildcard certificate and its LAN DNS are the operator's; the
repository names no real domain (`drydock-preview.test` in tests).

## `install.sh`

The installer and upgrader, one file in two modes: standalone (`curl … | sudo bash`) it downloads
and verifies the release tarball and runs the copy inside; from the tarball it installs.

- Settings persist in `/etc/drydock/drydock.env`, parsed, never `source`d.
- Idempotent: files are written only when they change, and only what changed is restarted.
- **Everything that can refuse runs before anything is replaced**: the new binary is staged as
  `/usr/local/bin/drydock.new`, the Caddy config validated from a staging directory, and the master
  key decided (its `count-secrets` asked of the staged binary) before the binary, a key or a file
  is installed. (Installing the binary first once left the old process running a deleted file
  after a refused key, and the re-run called it current.) An `EXIT` trap removes whatever was
  staged and, if the run stopped `drydock` for the key swap and did not reach `start_drydock`,
  starts it again.
- A `drydock` whose `/proc/<pid>/exe` is not the installed file (`-ef`: an interrupted run) is
  restarted, reported by the version it was really running, and copied as `drydock.previous` for
  the rollback; a run never says *current* unless the process runs the installed file. A rollback
  restores the previous unit with the previous binary.
- It requires Docker and the devcontainer CLI (Node 20+) on the service's `PATH` and installs
  neither. It adds `drydock` to the `docker` group — root by another name, design §13.4 — and
  gives the unit `ReadWritePaths=/srv/drydock/ws` and `RuntimeDirectoryPreserve=yes`, so
  `/run/drydock/sock/<id>/`, which running containers mount, survives a restart (`run.sh` checks
  its inode across one).
- **The secrets master key.** The first install creates `/etc/drydock/secrets.key` (32 bytes of
  `/dev/urandom`, `0400`, `drydock`'s, never printed), or installs the 32-byte file
  `--secrets-key PATH` names (a path, like `--github-app-key`, staged through a temp file in
  `/etc/drydock` and not kept in `drydock.env`). Later runs keep it and **refuse rather than
  replace** one that is not a 32-byte file. A *different* `--secrets-key` replaces the installed
  key **only while no secret is stored** — asked with `drydock count-secrets` as `drydock`, once
  with the service running and again after stopping it, so nothing is written under the old key
  in between — and is otherwise refused with nothing changed and nothing restarted, the binary
  included. A `systemctl stop` that fails is reported, never a silent exit. Nothing ever deletes or
  re-seals a secret, and there is no `--force`.
- `--ui-host` (and `--preview-domain`) are **lowercased**, which also repairs an older
  `drydock.env`. A preview domain on the UI host's registrable domain — child, parent or sibling —
  is refused before anything is installed, by asking the bundle's own binary
  (`drydock check-preview-domain`) rather than reimplementing the Public Suffix List in shell.
- `--vscode-ssh-host [user@]host[:port]` (the *Open in VS Code* link's SSH address) is kept in
  `drydock.env` as `DRYDOCK_VSCODE_SSH_HOST` and passed to `drydock serve --vscode-ssh-host` (empty is
  off); `--no-vscode-ssh-host` forgets it. A value serve would refuse is refused before anything is
  installed, by the bundle's own binary (`drydock check-vscode-ssh-host`), never a shell copy of
  the rule.
- **The final check** — a `401` from `/api/auth/session` through Caddy, and with previews on a
  `302` from `https://drydock-check.<preview-domain>/` to its own `/.drydock/denied` (curl's
  `%{redirect_url}`), since `drydock-check` is never a slug — verifies TLS against `--ca-cert` (a
  private CA's certificate, kept in `drydock.env`; `--no-ca-cert` forgets it) or the system store,
  never `-k`. With previews on it also asks `https://drydock-check.<preview-domain>/` over the same
  verification (`verify_site`), so a preview certificate without the wildcard's names fails the
  install naming the preview host and `--preview-cert`. When only verification failed (curl exit
  `60`) a second, unverified request decides whether to say *"installed and running, but this host
  could not verify the certificate"* or report a real failure, and never turns a failure into a
  pass. The undocumented `DRYDOCK_VERIFY_CACERT` it replaces is gone; only releases before
  `--ca-cert` read it.

Tested by `test/install/` (see its README) and, for the Ansible companion, `test/ansible/`.
