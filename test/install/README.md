# test/install — the installer against real systemd

Neither script is part of `go test`.

## `run.sh`

Runs `deploy/install.sh` against real systemd, the official Caddy package, Debian's Docker (a
nested daemon) and the devcontainer CLI in a privileged container. It installs from a local copy
of the release assets via `DRYDOCK_DOWNLOAD_BASE`, so it exercises the one-liner's path. CI runs
it on every PR.

What it covers:

- refusal without Docker or the CLI; a real `devcontainer up` inside the running service's own
  mount namespace (and again through the installed binary as the docker guard: refused with no
  policy, run with one);
- fresh install, no-op re-run, upgrade, rollback, previews on and off, an unreadable key, a
  foreign Caddyfile;
- the secrets master key (created, never printed, kept across re-run and upgrade, a damaged one
  refused);
- `--ca-cert` (kept; without it the trust failure is named as one; with Caddy cut off from the
  socket the check still fails, with or without it);
- a mixed-case `--ui-host` (lowercased, sign-in works, an old `drydock.env` repaired,
  `drydock serve` refusing one); a preview domain under or beside the UI host (refused, nothing
  changed);
- previews on — a preview certificate without the wildcard failing the final check, then every
  preview URL `401` over the wildcard verified with `--cacert` whatever it carries (the UI
  session, a token, a forged preview cookie, the right password), two labels deep no host at all —
  and off again (no preview site, no TLS answer);
- a first install *with* `--github-app-key` on a host with no `/etc/drydock` (every other App
  install finds the directory already made, which once hid a failure here);
- `--secrets-key` from nothing: a wrong size, base64, a missing file and a directory refused before
  anything is installed; the given bytes installed; the same key restarting nothing; a different
  key replacing it while no secret is stored, and refused with a secret stored — key
  byte-identical, drydock never stopped, the secret still served; a hex sweep of every installer
  output and the journal for the key bytes, the generated key included.

Then what a refusal or failure must not leave behind: a key refused during an upgrade (`v0.0.2`
stays installed and running from its own file; the runbook's re-run then upgrades), an interrupted
upgrade (a replaced binary under a running process: restarted and reported, and onto a broken
release rolled back to what was running), a failure after the key swap (drydock started again), a
`systemctl stop` that fails (a `PATH` shim: reported, no staged key left), and a 0-byte database
(refused, key unchanged).

## `live.sh`

- `live.sh vX.Y.Z` runs the README one-liner against a *published* release from GitHub. CI runs
  it.
- `live.sh --dir DIR vX.Y.Z` runs it against assets on disk, served the way GitHub serves them,
  and then as the README's **upgrade**: the release *Latest* on GitHub, with a password and a
  stored secret, upgraded by those assets with no flags (the session and the secret must survive).
  The release workflow's `verify` job runs this on the draft's assets.
