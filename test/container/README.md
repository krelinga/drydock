# test/container — the container tier

Real Docker (the devcontainer's DinD, or the CI runner's) and the real `devcontainer` CLI. Each
test runs under its own random label prefix. Tests skip without Docker unless
`DRYDOCK_REQUIRE_DOCKER` is set, which CI does.

What it covers:

- **Reconciliation**: adopt-an-orphan and died-unobserved against real containers; the cleanup
  helper sweep beside a workspace container, one carrying both labels, and another prefix's helper.
- **Credentials** (Phase 3): a real `devcontainer up` with the Feature from this checkout and the
  workspace's broker directory bind-mounted. Inside, git pushes a `drydock/` branch through the
  helper; a push to `main` is refused, the other repository is unreachable, and there is no socket
  but its own (with another workspace's open beside it on the host) and no Docker socket.
- **A Drydock restart under the running container** — `CloseAll` and a fresh `Broker` on the same
  directory, a new socket inode (the old one held by a hard link, as a file mount would hold it, so
  a reused inode number cannot fake the check) — after which `drydock-broker PING`, a push and the
  prelude still work, and again over a symlink root in the container planted at the socket's name;
  mounting the socket file instead fails it with exit 69. `TestLegacyBrokerMountAsDockerReportsIt`
  checks the legacy-mount reading against real `docker inspect`.
- **Secrets** (Phase 4): a suite run through the Feature's `CLAUDE_ENV_FILE` passes while a secret
  is granted and fails on the next command once it is not; a hostile value runs nothing; no value
  is in any `/proc/*/cmdline` or `docker inspect`.
- **The walking skeleton** (Phase 2): `POST /api/workspaces` through the real server, signed in,
  for a repository with a config and one without, both reaching `running` with the published
  Feature, the clones untouched and no token anywhere in the tree or the database; a secret
  granted to one reaches its `CLAUDE_ENV_FILE` prelude and not the other's. The configured one's
  `forwardPorts`, read by the real CLI, become the port registry's declared rows — labelled, off —
  and the minimal configuration declares none.
- **Claude** (Phase 5): five workspaces share one credential volume and each runs the pinned
  `claude`; the volume owner helper; fakeclaude's login in a real container, where the PTY
  semantics Spike 01 relied on are measured through `docker run -t`.
- **Previews** (`TestViteHMRThroughThePreviewProxy`): a workspace from `node:22-bookworm-slim`
  (the pinned Claude base, with `remoteUser: node`), Vite installed from npm and serving the clone,
  nothing published, reached through the real preview socket after the handshake — Vite sees
  `Host: localhost:5173` and refuses the preview host under `passthrough`, an event stream
  streams, and the HMR socket says `connected`, echoes a custom event and delivers the hot update
  for a file edited on the host; then `docker stop` behind Drydock's back and the denied page.
- **The host-access gate** (design §6): the tier's own `runArgs: --network=host` is pre-approved
  per repository (`approveHostNetwork`, hashed by the same function); a docker-in-docker repository
  stops `needs_approval` with no container, is approved through the API and comes up privileged;
  and the lifecycle test rewrites `devcontainer.json` from inside a running container with an
  `initializeCommand` that touches a host canary — the rebuild stops, the canary absent and a stale
  hash refused, and once approved it runs and the canary appears.
- **The docker guard**: the test binary is the guard (`main_test.go`), and
  `TestTheGuardRefusesAMovedTag` is the tag race for real — a Feature from the tier's registry
  whose tag moves to one declaring `privileged` when a `devcontainer` wrapper on `PATH` starts
  `up`; step 3 sees nothing, `up` is refused naming privileged and no container carries the label;
  approved, the same workspace comes up `Privileged=true`; and with the approval narrowed and the
  tag moved back, `docker start` of that privileged container is refused.
- **Lifecycle**: provision → stop → start → rebuild → delete.
