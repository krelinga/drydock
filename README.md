# Drydock

A single Go binary for one dev server that turns a GitHub repository into a running dev container
with a supervised `claude remote-control` session inside it, driven from a web UI on the LAN.

> **Status: early.** What ships today is the front door: the sign-in, the session and device
> handling, and the route table every later phase plugs into. Workspaces are not implemented yet.
> See [the design](docs/design/overall/drydock-design.md) §14 for the build order.

## Install

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
  `Host` itself to turn away DNS rebinding, so there has to be a name to check. The `caddy` user
  must be able to read the key (e.g. group `caddy`, mode `0640`). The installer does not obtain
  certificates.
- **Caddy to itself.** Drydock's Caddyfile has a global options block — it moves Caddy's admin API
  off `localhost:2019` onto a `0600` socket — so it cannot be imported into someone else's. The
  package's placeholder Caddyfile is replaced (and backed up); any other one is refused unless you
  pass `--take-over-caddy`.
- **No TCP port of its own.** Drydock listens only on Unix sockets under `/run/drydock`, readable by
  the `drydock` group, of which Caddy is the only other member.

Previews of web apps running in a workspace are served from a **separate registrable domain** with a
wildcard certificate, and are optional. Add them on any run with `--preview-domain`,
`--preview-cert` and `--preview-key`; take them away with `--no-preview`. `--version vX.Y.Z` installs
a specific release, and `--help` lists the rest.

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

CI runs all but the last on every pull request; the release workflow runs the last against each
release it publishes.

Releases are cut by [release-please](https://github.com/googleapis/release-please) from
[conventional commit](https://www.conventionalcommits.org/) subjects: `feat:` and `fix:` cut a
release, everything else (`docs:`, `test:`, `chore:` …) does not. Merging its release PR tags
`vX.Y.Z` and publishes `drydock_linux_amd64.tar.gz`, `SHA256SUMS`, and the standalone
`install.sh` — built by `deploy/package.sh`.

`CLAUDE.md` and [`docs/design/`](docs/design/) explain why things are the way they are.
