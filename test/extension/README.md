# test/extension — the VS Code extension's client tier

vscode-extension §12.4. The extension's own client, stream and session code — on its production
Node transport, with no VS Code — against the real Drydock behind real Caddy on the shipped
Caddyfile. Run by `run.sh` (needs `go`, `node` with `npm ci` done in `extension/`, `caddy`,
`openssl`, `git`, and `drydock.test` resolving to `127.0.0.1`, which the script checks and never
writes; CI adds the `/etc/hosts` line as root). Mutation-checked.

## The stack

- **`stack/`** is a Go test package. `TestServeStack` (skipped unless `DRYDOCK_EXTENSION_STACK`
  names a config) runs `server.New` and `Serve` with the fake GitHub App (`githubtest`, which needs
  a `testing.TB`) and serves until SIGTERM, then returns so `t.Cleanup` shuts everything down. The
  harness builds it with `go test -c` and runs the binary itself, so the signal reaches the test.
- **No Go seam for containers.** `devcontainer` and `docker` are fake executables first on `PATH`
  — wrapper scripts that run the test binary with `DRYDOCK_STACK_FAKE` naming which (`TestMain`) —
  so every copy of the container manager runs them, and the argv Drydock really builds is what is
  answered (each logged to `<root>/fakestate/<name>-argv.log`). `read-configuration` answers from
  the recorded corpus: the docker-in-docker recording (`privileged`) for a clone carrying the
  `drydock-fixture/dind` marker, plus an `initializeCommand` while `fakestate/dind-variant` says
  `initialize`; `up` makes a container that `ps` and `inspect` then report by label, with the
  clone's bind mount, which is what the VS Code link is built from.
- **`TestStackControl`** is the stack's positive control, in the ordinary `go test`: the catalog
  lists both seeded repositories, a clone of `drydock-fixture/plain` runs with a link of the shape
  the clients accept, and `drydock-fixture/dind` stops for approval naming `privileged` before any
  `up`, then runs once approved.
- It does **not** test the docker guard: `--docker-path` names the test binary, which the fake
  `devcontainer` never runs. The guard is the container tier's.
- **`stack.ts`** starts it: the Go stack, a tap in front of its API socket (`test/harness/tap.ts`),
  and Caddy (`test/harness/caddy.ts`) on a port the UI origin carries
  (`https://drydock.test:<port>`), plus `direct` — the operator's cookie session on the socket
  itself, for minting tokens and starting, declining or seeding what the extension does not do.

## Trust

The CA is made by the client tier's global setup (`extension/test/client/global.ts`) **before**
the test processes fork, and `NODE_EXTRA_CA_CERTS` points at it: Node reads it once, at start,
for `https` and `fetch` alike, with validation fully on. Never `rejectUnauthorized: false`, never
`NODE_TLS_REJECT_UNAUTHORIZED` (`extension/src/boundary.spec.ts` bans both from the extension's
sources). The wrong-host control is a second Caddy serving a valid-chain certificate for another
name: the client fails with `ERR_TLS_CERT_ALTNAME_INVALID` and the tap records nothing.

## The extension-host tier

`run.sh host` runs `extension/test/host/`: the pinned VS Code (`extension/test/host/vscode.json`,
downloaded by URL and checked by SHA-256 before it is unpacked, cached in `~/.cache/drydock-vscode`)
under `xvfb-run`, running `inner/suite.ts` against this stack. VS Code's extension host does not
trust a CA named by `NODE_EXTRA_CA_CERTS` (Spike 05 #1) but does read the system store, so this
tier's CA must be put there first by `system-ca.sh` (as root; `remove` untrusts it and deletes its
directory). That CA is name-constrained (critical) to the tier's names alone, its key is deleted
once the leaves are signed, and its directory is made by `mktemp`. CI does it on a runner that lives
one job; `run.sh host` refuses until the system store verifies the CA, and refuses a CA without the
constraint or with its key on disk; it never edits the store. `DRYDOCK_SPIKE_05_NEGATIVE=1` runs the
tier on `NODE_EXTRA_CA_CERTS` alone, Spike 05 #1's negative, which is expected to fail.

## The specs

They live in `extension/test/client/` (Vitest resolves `vitest` and the extension's own modules
from there), configured by `extension/vitest.client.config.mts`: sign-in with a minted token and
`GET /api/meta`; `Origin` (absent → `forbidden_origin`, the client's → `202`); cookie plus
`Authorization` → `401`; the token never sent to a second site, never past a redirect, never to a
wrong-host certificate; a bearer stream, live and replayed, with no `secret.*`/`api_token.*` event
and not the secret's name (a cookie stream over the same window has them); a Caddy restart closed
by `Last-Event-ID`; a revoked token's reconnect ending on the 401 path, and Sign Out revoking its
own; clone → `settlesClone` → `running` → a refetched view whose link `remoteURI` opens; and
approval — `privileged` asked for, `403 forbidden_scope` without `approve` (token kept), a stale
hash `409 approval_stale`, the shown one approved and running.
