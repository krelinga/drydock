# test/browser — the browser tier

Testing §10, §10.4. Playwright's Chromium against the real `drydock serve` and real Caddy on the
shipped Caddyfile. Run by `run.sh`, which resolves `@playwright/test` from `web/node_modules` via
`NODE_PATH` and type-checks first (it needs `web/`'s `npm ci`). Mutation-checked.

## The stack

- `drydock serve` runs with a **stand-in `docker`** on its PATH (fakeclaude where the login
  container's claude would be) and its own `--claude-volume`, so the tier drives the login
  handshake through a reload (`login.spec.ts`, §10.2 item 12) and can never reach the host's Docker
  or its credential volume.
- A throwaway CA is trusted through NSS in a per-run `HOME` — **never `ignoreHTTPSErrors`**. It
  does not break cookie semantics (Spike 04), but it, and `--ignore-certificate-errors-spki-list`,
  stop the tier noticing a *misissued* certificate; only real NSS trust refuses a cert served for
  the wrong host.
- A tap between Caddy and each socket records what arrived, so "no cookie" is asserted at the
  server. The tap relays upgrades.

## What it covers

The `__Host-` cookie and its illegal variants, the `SameSite=Lax` split, the `Origin` belt,
cross-origin reads, framing both ways, the CSP as served, SSE through Caddy's `encode`, the
browser's own `Last-Event-ID` replay, sign-in with `return`, the mid-session `401`, a wrong-host
certificate refused, and the preview handshake (`preview.spec.ts`: a signed-in click to a preview
host lands, after three redirects observed at both taps, on the hardcoded upstream with only the
host-only preview cookie in the jar there and the token in no file under the root; not signed in,
sign-in and back; *Sign out everywhere* bounces the next preview request; forged cookies and
tokens change nothing; an unknown slug is denied).

A spec that only needs a document on a preview origin loads `PREVIEW_PAGE` (`/.drydock/denied`),
since a preview root starts the handshake.

`preview.spec.ts` runs a **real Vite** (web/'s own) on this host as the previewed app: the
stand-in `docker` reports it as the seeded workspace's container, on a bridge, at this host's
non-loopback address (`previewContainer()`). A release refuses any address the host holds, so the
tier builds `drydock` with **`-tags browsertier`** (`internal/server/localaddrs_browsertier.go`: no
local addresses listed, nothing else changed, a startup line saying so; `deploy/package.sh` passes
no tags). HMR is thus tested through Caddy, the tap and the proxy. So is port forwarding §13 step
4's done-when: the ports panel enables a seeded port, its link opens the app in a new tab, and
*Turn off preview* closes its HMR socket at once; the next load ends on the host's own denied page
with `Clear-Site-Data`, and what the app put in `localStorage` is gone.

## Not Chromium-only

`engines.spec.ts` also runs in Playwright's Firefox and WebKit (the `firefox` and `webkit`
projects), driving the real sign-in form and Settings' sign-out and asserting the `Origin` the
server received — a client once sent `Origin: null` from Safari and Firefox, which Chromium never
shows. Those two engines cannot trust the tier's CA, so they run against a **loopback front**
(`Stack.front()`: the real `drydock serve` over plain HTTP on 127.0.0.1) that maps only its own
exact origin to the UI origin and passes `null` through, which the spec's controls prove from Node.
Firefox and WebKit do not run the preview handshake: it needs two real hostnames under trusted
TLS.

## Fixtures

The `signIn` fixture is a bare `fetch` (mode `cors`, so it sends the right Origin whatever the
client does) and is for tests where how the sign-in was made is not the subject. It asks once more
only when the tap shows the first request never reached the server: Chromium aborts in-flight
requests with `ERR_NETWORK_CHANGED` when Docker adds or removes a veth beside the tier (testing
§10.4).

## CI

CI's `browser` job runs this inside Playwright's own image as a non-root user — see `CLAUDE.md`'s
CI section.
