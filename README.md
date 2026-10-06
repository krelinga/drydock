# Drydock

A single Go binary for one dev server that turns a GitHub repository into a running dev container
with a supervised `claude remote-control` session inside it, driven from a web UI on the LAN.

> **Status: early.** What works today: the sign-in, the repository list from the GitHub App,
> cloning a repository into a running dev container, GitHub access scoped to that one repository
> inside it, repository secrets, and stop, rebuild and delete. Claude Code sessions (Phase 5) and
> previews are not built yet. See [the design](docs/design/overall/drydock-design.md) §14 for the
> build order.

## Install

**Deploying for the first time? Follow [the first-deployment runbook](docs/deploy/first-deployment.md)**:
a step-by-step checklist from cutting the release to your first workspace, including the
certificate options, getting the keys onto the server safely, and troubleshooting. If you manage
the server with Ansible, [its Ansible companion](docs/deploy/first-deployment-ansible.md) has the
server-side steps as copy-pastable tasks.

On a Linux server with systemd and [Caddy](https://caddyserver.com/docs/install) installed from its
official package, with a hostname for the UI and a certificate for it:

```sh
curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
  | sudo bash -s -- --ui-host drydock.example.com --cert /etc/ssl/drydock.pem --key /etc/ssl/drydock.key
```

It asks for the operator password on the first run. Then open `https://drydock.example.com`.

**To upgrade, run the same line again** — the flags can be left off, since the first run's settings
are kept in `/etc/drydock/drydock.env`. A re-run restarts only what changed, never asks for the
password again, and puts the previous binary back if the new one does not start.

What the installer needs, and what it will not do:

- **A hostname, not an IP**, and a certificate every client device already trusts. Drydock checks
  `Host` itself to turn away DNS rebinding, so there has to be a name to check. It is lowercased,
  since browsers send it that way and the `Origin` check is exact. The `caddy` user must be able
  to read the key (e.g. group `caddy`, mode `0640`). The installer does not obtain certificates.
- **`--ca-cert` for a private CA.** The installer ends by fetching the UI through Caddy and
  verifying its certificate; a server that does not trust your CA cannot, so give it the CA's
  certificate (never its key) with `--ca-cert /path/ca.pem`. It is used for that check alone and
  kept for later runs; `--no-ca-cert` forgets it. When the check fails only because the
  certificate could not be verified, the installer says so, apart from a real failure.
- **Caddy to itself.** Drydock's Caddyfile has a global options block — it moves Caddy's admin API
  off `localhost:2019` onto a `0600` socket — so it cannot be imported into someone else's. The
  package's placeholder Caddyfile is replaced (and backed up); any other one is refused unless you
  pass `--take-over-caddy`.
- **No TCP port of its own.** Drydock listens only on Unix sockets under `/run/drydock`, readable by
  the `drydock` group, of which Caddy is the only other member.
- **Docker and the devcontainer CLI, installed by you.** [Docker Engine](https://docs.docker.com/engine/install/)
  (or your distribution's `docker.io`), and `npm install -g @devcontainers/cli@0.89.0` with Node.js 20 or
  later, landing in `/usr/local/bin` or `/usr/bin` — the service's `PATH` has nothing else. The
  installer refuses to run without them, and adds the `drydock` user to the `docker` group, which is
  root-equivalent on the host: Drydock builds whatever a repository's `devcontainer.json` asks for.
  Clones live in `/srv/drydock/ws`, readable by Drydock alone.

Previews of web apps running in a workspace are served from a **separate registrable domain** with a
wildcard certificate, and are optional. Add them on any run with `--preview-domain`,
`--preview-cert` and `--preview-key`; take them away with `--no-preview`. `--version vX.Y.Z` installs
a specific release, and `--help` lists the rest.

**The repository list needs the GitHub App.** Create it as described in the design (§9.3 lists the
permissions), download its private key, and pass both to the installer once:

```sh
curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
  | sudo bash -s -- --github-app-id 123456 --github-app-key ~/drydock.private-key.pem
```

The installer copies the key to `/etc/drydock/github-app.pem`, readable by the `drydock` user alone,
and re-runs keep it. Use the numeric **App ID** from the App's settings page, not its Client ID.

**Back up the secrets master key.** The first install creates `/etc/drydock/secrets.key` — 32 random
bytes, readable by the `drydock` user alone — and every repository secret you store is encrypted
under it. **Lose it and every stored secret is unreadable**: there is no recovery but entering each
value again. Re-runs and upgrades keep it, and the installer refuses to replace one that is damaged
rather than quietly making a new one; restore it from your backup instead. Copy it somewhere safe
that is not the database's backup (`sudo cp /etc/drydock/secrets.key …`), since a backup holding
both is a backup holding every secret.

To change the password later: `sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db`.
It signs out every device.

## Development

The devcontainer carries the whole toolchain. The Go build needs no Node: the UI is committed,
built, in `internal/web/dist`.

```sh
go build ./... && go vet ./... && go test ./...   # the Go suite (the Caddy test needs `caddy`)
cd web && npm ci && npm run check                 # the UI, including that dist is current
test/install/run.sh                               # the installer, against real systemd and Caddy
test/install/live.sh latest                       # the one-liner, against the published release
```

CI runs all but the last on every pull request; the release workflow runs the last, as
`live.sh --dir`, against each release's assets while it is still a draft.

Releases are cut by [release-please](https://github.com/googleapis/release-please) from
[conventional commit](https://www.conventionalcommits.org/) subjects: `feat:` and `fix:` cut a
release, everything else (`docs:`, `test:`, `chore:` …) does not. Merging its release PR tags
`vX.Y.Z` and creates a draft release; its assets — `drydock_linux_amd64.tar.gz`, `SHA256SUMS`, and
the standalone `install.sh`, built by `deploy/package.sh` — are uploaded to the draft, installed by
the one-liner, and only then published as *Latest*.

`CLAUDE.md` and [`docs/design/`](docs/design/) explain why things are the way they are.
