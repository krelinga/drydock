# Drydock port forwarding

*Reaching a dev server running inside a workspace container from a phone or tablet on the LAN — without giving repository code a foothold on Drydock's own origin.*

**Status** living design document — its history is the git log (`git log -p -- docs/design/port-forwarding/port-forwarding-design.md`).

**Supplements** [`../overall/drydock-design.md`](../overall/drydock-design.md) · **Depends on** §3, §6, §13 of that document

**Out of scope** non-HTTP forwarding · internet exposure · certificate issuance

## 1. Problem & goals

An agent finishes a change to a web app. To look at it you currently need a terminal, an SSH tunnel, and a laptop — which is precisely the workflow §1 of the overall design set out to delete. The clone button already produces a running container with a dev server in it; what is missing is a URL you can open on the device in your hand.

**The goal is one link on the workspace card that opens the running app on any signed-in device on the LAN.**

That sounds like a five-line reverse proxy, and it very nearly is. The reason it needs a document is that a preview serves **arbitrary code from the repository, into your browser** — and the overall design has spent considerable effort ensuring that the only thing reachable at Drydock's hostname is Drydock. A careless implementation hands repo code a same-origin foothold next to a control plane whose §13.5 summary is *"the only thing standing between a device on your wifi and code execution on your dev server."*

### Functional requirements

- **Preview a port.** Open a chosen container port at a stable URL, reachable from any device that can sign in to Drydock.
- **Authenticated.** A preview is behind the same sign-in as everything else. Signing out of all devices closes every preview with it.
- **Works with real dev servers.** Absolute asset paths, HMR websockets, and streaming responses all have to survive the trip.
- **Finds ports by itself.** `devcontainer.json` must not have to be an exhaustive list — dev servers pick ports at runtime, and an agent starting a second server picks whatever it likes.
- **Explicit.** A port is reachable because someone enabled it, not because something was listening. Discovery and exposure are different decisions, and only the second one is yours to make.
- **Cheap to reason about.** No new listener on the LAN, no new credential, no new trust boundary.

### Non-goals

- **Not a general reverse proxy.** The only upstream a preview can reach is *this workspace's container, on a port enabled for it*. There is no operator-supplied host field, because that field is server-side request forgery with a nicer name.
- **Not internet-facing.** Same answer as §13.1 of the overall design: Tailscale in front of the same Caddy, or nothing.
- **Not TCP forwarding.** HTTP and websockets only. Postgres on a phone is not the use case, and a raw TCP path would need a listener this design is built to avoid (§12).
- **Not a replacement for `devcontainer exec`.** Debugging a server that will not start is a terminal job.
- **Not a certificate manager.** Unchanged from §13.1 — but this design *does* add a requirement to the result, and §9 states it plainly.

## 2. What the base design fixes before we start

Four decisions in the overall document constrain this one. Two carry over unchanged; the other two — the session cookie and the `Origin` allowlist — interact with the cross-site split this design introduces, and the cookie changes slightly to accommodate it (§7, overall §13.2).

| Constraint | Source | Consequence here |
|---|---|---|
| **Drydock binds no TCP port.** | §13.5 | The preview proxy is not allowed to open a listener either. It gets a second Unix socket (§3). |
| **Caddy is a dumb front door that knows nothing about Drydock.** | §3.1 | Caddy must not learn the workspace→port→container mapping. It forwards a whole wildcard to a socket and Drydock does the routing. |
| **`__Host-drydock`, `Secure`, `HttpOnly`, `SameSite=Lax`.** | §13.2 | Previews live on a separate registrable domain, so a preview is cross-site with the UI and cannot set cookies for it at all — `__Host-` stays good hygiene rather than the load-bearing thing it would be under same-site. The cookie is `SameSite=Lax`, not `Strict`, so the cross-site authorize redirect (§7) still carries the session; §13.2 was changed to match. |
| **`Origin` allowlist on every state-changing route.** | §13.3 | Stays belt-and-braces behind `SameSite`, which does the primary CSRF work because a preview is cross-site (§4, §10.2). Kept exact-match and fail-closed anyway — cheap, testable in CI, and the safety net if the two domains are ever collapsed by mistake. |

> [!NOTE]
> **The decision that shapes everything below: previews are cross-site**
>
> Previews are served from a **separate registrable domain** (`*.drydock-preview.net`), not a subdomain of `drydock.example.com`. That one choice is what lets `SameSite=Lax` keep a previewed app — arbitrary repository code — from issuing authenticated requests to `/api/*`: a cross-site `POST` never carries the session cookie, so the browser enforces the boundary rather than an application check having to catch every route. It costs a second domain to own and renew, and it forces exactly one adjustment elsewhere: the session cookie moves from `SameSite=Strict` to `Lax` (§7, overall §13.2) so the cross-site authorize redirect can still carry it. §4 and §10 work through the consequences; the short version is that the browser, not the `Origin` check, is now the thing standing between repo code and the control plane.

## 3. Architecture — a second socket, not a second listener

The whole design is one idea: **extend socket-as-identity to the front door.** The overall design already uses "which socket did this arrive on" to decide which repository a broker request may touch (§9.2). The same trick answers "is this a control-plane request or untrusted repo content?"

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/01-two-sockets-dark.svg">
  <img alt="A LAN device reaches Caddy over TLS on two hostnames. Caddy holds two site blocks — one for the UI hostname, one for a preview wildcard — and forwards each to a different Unix socket. Inside the single Drydock process those sockets are served by two entirely separate muxes: the API mux, guarded by the session cookie and an Origin allowlist, and the preview mux, which holds only the proxy and no API route at all. The preview mux dials the workspace container's dev server directly over the Docker network; nothing is published on the host." src="diagrams/01-two-sockets-light.svg" width="100%">
</picture>

**Fig 1** — *Two Caddy site blocks, two Unix sockets, two `http.Server`s, two muxes, one process. Caddy still knows nothing: one block matches the UI hostname, the other a wildcard, and each forwards everything it receives to a socket. The separation is a file descriptor rather than a branch in a handler — which is the point: the browser now treats the two origins as cross-site, but Drydock does not lean on that alone, and a preview request lands on a different server that has no API route to reach.*

The property that makes this worth the extra socket: **a preview request cannot reach an API handler even if the Host-parsing logic is wrong.** They are not routes in the same mux behind a branch — they are different servers on different files. §13.5's "auth is middleware around the whole mux, so a route added later is protected by forgetting to think about it" continues to hold, once per mux, rather than becoming "protected unless someone adds a route on the wrong side of an `if`."

### 3.1 Component responsibilities

Extending the table in §3.1 of the overall document:

| Component | Owns | Explicitly does not |
|---|---|---|
| **Preview proxy** | The `preview.sock` listener, host→port resolution, the upstream dial, websocket upgrade, preview session validation. | Serve any API route. Accept an operator-supplied upstream. Hold any credential. |
| **Port registry** | `forwarded_port` rows, merging declared and observed ports, enable/disable/hide. | Decide reachability on its own — a row is a *permission*, the workspace still has to be running. |
| **Discovery scanner** | Reading each running container's socket table from the host, debouncing, classifying loopback binds (§8.2). | Execute anything inside a container. Enable a port. Notify anyone. |
| **Container manager** *(extended)* | Resolving a workspace's current container IP and PID from Docker by label, each time they are needed (*as built*, §13.4: `Address` and `Confirm`; nothing is cached, so there is nothing to invalidate). | Publish ports on the host. Nothing is bound on the dev server's network interfaces. |

Note what is *not* here: no change to the container. No agent, no injected feature, no published port, no `docker run -p`. Drydock reaches the container the way it already reaches everything else — from the host, over the Docker network — and the container never learns it is being previewed.

## 4. Naming and routing

One hostname per (workspace, port), from a wildcard:

```
https://<slug>.drydock-preview.net
        └─ e.g.  drydock-3000-k4x9  /  myapp-5173-p2mq
```

`slug` is `<sanitized-repo>-<container_port>-<4 random chars>`, generated once and stored on the `forwarded_port` row. It is readable enough to tell two open tabs apart, unique across workspaces on the same repo, and stable for the life of the row — a bookmark keeps working across container restarts and rebuilds.

The random suffix is not a security control. Previews are authenticated; it is there so that deleting and re-adding a port produces a *different* URL, which means a stale bookmark fails closed rather than silently landing on whatever now occupies port 3000.

**That only holds if a retired slug is never minted again**, and four random characters do not guarantee it on their own — a slug freed by a delete could later be drawn for a different workspace, at which point the stale bookmark stops failing closed and starts resolving to somebody else's preview. It is unlikely and the consequence is the cross-workspace exposure this design otherwise works to prevent, which is the wrong side of that trade. So `DELETE` **retires** the row rather than removing it (§5): the slug stays spent, the global `UNIQUE` on it does the enforcing, and "a stale bookmark fails closed" becomes a property of the schema instead of a property of the odds.

The wildcard is on a **separate registrable domain** from the UI — `drydock-preview.net`, distinct from `drydock.example.com`. That is the crux of the whole security model, not a naming preference: because the two do not share a registrable domain, a previewed app is *cross-site* with the control plane, so `SameSite=Lax` stops it issuing authenticated requests to `/api/*` without an application check having to catch every one (§10.2). It costs one more domain to own and renew — the deliberate price for making the browser enforce the boundary rather than an `Origin` check on every route. *As built:* "separate" is checked as the browser reckons it — eTLD+1 by the Public Suffix List — so a parent (`example.com`) or a sibling (`preview.example.com`) of `drydock.example.com` is refused exactly as a subdomain is, by `drydock serve` at startup and by the installer before it installs anything (overall §13.3).

| Alternative | Why not |
|---|---|
| **Path prefix** `drydock.example.com/preview/<ws>/<port>/` | Same origin as the UI. Repo code gets the session cookie and full API access. Also breaks every app that emits absolute asset paths. Disqualified on the first point alone. |
| **Same-domain wildcard** `*.preview.drydock.example.com` | Cheaper — no second domain — but *same-site* with the UI, so `SameSite` no longer separates a preview from `/api/*` and the `Origin` check becomes the *sole* CSRF defense: one application-level check, untestable in its browser half and one suffix-match bug from wide open. That concentration of risk is exactly what the separate domain buys its way out of. Recorded because it is the tempting shortcut. |
| **Port per preview** `drydock.example.com:3001` | Requires a listener per preview, which §13.5 forbids, and a cert that covers nothing new. |
| **Fixed SAN pool** `preview1..8.drydock-preview.net` | Avoids DNS-01, at the cost of a hard ceiling and URLs that are not stable across restarts. The fallback if wildcard issuance ever becomes unavailable. |

## 5. Data model

One new table, plus one that exists to make revocation work.

```sql
-- A permission to reach one port on one workspace. Default-deny: no row, no preview.
forwarded_port(
  id TEXT PRIMARY KEY,               -- ULID
  workspace_id TEXT NOT NULL,        -- no foreign key: see "As built" below
  container_port INTEGER NOT NULL,
  slug TEXT NOT NULL UNIQUE,         -- the DNS label; stable for the life of the row
  label TEXT,                        -- "vite dev server"
  upstream_scheme TEXT NOT NULL DEFAULT 'http',   -- http | https (rare; self-signed upstreams)
  host_header TEXT NOT NULL DEFAULT 'localhost',  -- localhost | passthrough  (§8.3)
  enabled INTEGER NOT NULL DEFAULT 0,
  hidden INTEGER NOT NULL DEFAULT 0,  -- muted from the panel; the escape hatch for noise

  -- provenance is a set, not a choice: a port can be both declared and observed
  declared INTEGER NOT NULL DEFAULT 0,  -- appears in forwardPorts / appPort
  observed INTEGER NOT NULL DEFAULT 0,  -- has been seen listening at least once
  manual   INTEGER NOT NULL DEFAULT 0,  -- added by hand

  -- last observation from the discovery scan (§8.2)
  bind_addr TEXT,                    -- 0.0.0.0 | :: | 127.0.0.1 | …
  observed_state TEXT,               -- listening | gone | never_seen
  first_seen_at TEXT, last_seen_at TEXT,

  created_at TEXT, last_used_at TEXT,
  retired_at TEXT                    -- soft delete. The row stays so its slug stays
                                     -- spent: `slug UNIQUE` is then what makes
                                     -- non-reuse structural rather than probabilistic
)

-- One *live* row per (workspace, port). Retiring rather than deleting would otherwise
-- block re-adding a port, so the uniqueness that matters is partial; the global
-- UNIQUE on `slug` keeps applying to retired rows, which is the entire point.
CREATE UNIQUE INDEX forwarded_port_live
  ON forwarded_port(workspace_id, container_port) WHERE retired_at IS NULL;

-- A device's proof that it may view previews. Dies with the session that minted it.
preview_session(
  id TEXT PRIMARY KEY,               -- sha256 of the preview cookie value
  auth_session_id TEXT NOT NULL REFERENCES auth_session(id) ON DELETE CASCADE,
  forwarded_port_id TEXT NOT NULL REFERENCES forwarded_port(id) ON DELETE CASCADE,
  preview_host TEXT NOT NULL,        -- host-only: this cookie is good for one preview
  created_at TEXT, last_seen_at TEXT
)
```

Three notes, in the spirit of §4's "what is deliberately absent".

There is **no upstream host column** — only a port. The upstream address is always derived from the workspace's own container, so there is no value an operator or an attacker could write that would make the proxy dial somewhere else.

`enabled` and `observed` are **deliberately independent**. A port being listened on has no bearing on whether it is reachable, and a port being enabled does not require anything to be listening yet. Conflating them is how a discovery feature turns into an exposure feature by accident.

`preview_session.auth_session_id` is a **cascading foreign key on purpose**. §13.2 promises that one button kills every session; without the cascade that promise would quietly stop covering previews, which are the most likely thing to be left open on a device you no longer have.

The row is **per preview host, not per device**. A single cookie covering `.drydock-preview.net` would let a previewed app fetch every other preview on the same device. Host-only cookies cost one extra redirect per preview, and §7 explains why that redirect is invisible.

Two lifetimes, both made explicit rather than left implicit. The preview cookie gets its **own** idle TTL keyed on `last_seen_at`, shorter than the auth session's — a captured preview cookie should not stay live for the auth session's full 30-day ceiling. And a `preview_session` is deleted when its port is disabled or retired, not only when the auth session is: without that, disabling a port dial-blocks new requests (§8.1) but leaves the minted rows lying around to reactivate silently on re-enable. The cascade on `auth_session` is the backstop for the lost-device case; per-port cleanup is the routine one.

*As built (§13 step 2, migration 8):* two departures from the block above, both found by building it. **`forwarded_port.workspace_id` has no foreign key.** `ON DELETE CASCADE` would delete a workspace's ports with it and free their slugs, which is exactly the reissue the global `UNIQUE` exists to prevent (§4), so removing a workspace *retires* its ports in the same transaction instead (`workspace.Remove`), and the resolver's join on a running workspace is what makes a removed one unreachable. **`preview_session` carries `forwarded_port_id`**, so a disable or retire deletes its rows by key rather than by matching a host string the preview domain could change under. The slug is checked in the schema too — one DNS label, lowercase, never `drydock-check`, the installer's probe name — and each enumeration (`upstream_scheme`, `host_header`, `observed_state`, the flags) is a `CHECK`. The preview cookie's idle window is **12 hours**; a preview session also dies with its auth session's own expiry (30-day absolute, 14-day idle), which is checked on every request because the auth row is only deleted lazily. Disabling and retiring are `preview.Service.SetEnabled` and `Retire`, each deleting the port's preview sessions in the same transaction; step 4 built their routes (§6, §13.5).

## 6. API surface

Additions to §5 of the overall document. All on the API mux, all behind the session cookie and the `Origin` check.

| Method & path | Does | Returns |
|---|---|---|
| `GET /api/workspaces/:id/ports` | Every known port — declared, observed, manual — with its provenance flags, `bind_addr`, `observed_state`, `last_seen_at`, and URL if enabled. Hidden rows only with `?hidden=true`. | Port list |
| `POST /api/workspaces/:id/ports` | Add a port by hand. Body: `container_port`, optional `label`, `upstream_scheme`, `host_header`. Mints the slug. | `201` + port |
| `PATCH /api/workspaces/:id/ports/:port` | Enable, disable, hide, unhide, or relabel. Enabling is the click that makes a URL live. | `200` + port |
| `DELETE /api/workspaces/:id/ports/:port` | **Retire** it — a soft delete that keeps the slug spent forever (§4, §5). Its `preview_session` rows go immediately. A still-listening port reappears on the next scan as a fresh, disabled row with a *new* slug. | `204` |
| `POST /api/workspaces/:id/ports/rescan` | Force a discovery scan now instead of waiting for the interval. | `200` + port list |
| `GET /api/workspaces/:id/ports/:port/probe` | Dial it now and report what happened, with the §11 diagnosis attached. Rarely needed once §8.2 is running — discovery usually knows the answer already. | Probe result |
| `GET /preview/authorize` | The main-origin half of the handshake in §7. Query: `return`. Requires a session. | `302` |

On the **preview mux**, and nowhere else, two routes under a reserved prefix:

| Method & path | Does |
|---|---|
| `GET /.drydock/session` | Consume the one-time token, set the host-only preview cookie, redirect to the originally requested path. |
| `GET /.drydock/denied` | The human-readable dead end: not signed in, port disabled, workspace stopped, or upstream refused. |

`/.drydock/*` is the only path the preview mux handles itself; everything else is proxied verbatim. A repo that genuinely serves something at `/.drydock/` loses that path, which is a trade worth making once and documenting.

New events on the existing SSE stream: `port.enabled`, `port.disabled`, `port.unreachable`.

Discovery deliberately emits **no** event of its own. A port appearing or disappearing changes the ports panel the next time it is read; it does not push anything at anyone, for the reason in §8.2.

*As built (§13 step 4, §13.5):* the table above with four departures, each a rule the rest of the API already keeps. **Every mutation answers `202`**, not `201`/`200`/`204`, and is settled by its event (overall §5's async shape; frontend §2.1: the body is never applied). **`:port` is the row's id**, not the port number: a port retired and listed again is a new row with a new slug, and a press still in flight for the old row must not land on the new one. The list is `{ports, previews}` — `previews` false when no preview domain is configured, so the panel says why there is no switch — and an enable without a domain is `503 previews_not_configured`; the other refusals are `port_exists`, `too_many_ports` (64 live rows) and `in_progress` (a workspace being deleted takes no new port and no enable). The events are `port.added`, `port.enabled`, `port.disabled`, `port.updated` (hide, label, `host_header`, the declared flag) — each carrying the whole row as `data.port` — and `port.retired` (`port_id`, `container_port`); `port.unreachable` is §11's, so step 6's. `POST …/rescan` is step 5's (below). Declared ports emit `port.added` and `port.updated` like a hand-added one: a row a configuration declares is not discovery, and the panel learns of it as of any other.

*As built (§13 step 5, §13.6):* **discovery writes the same `port.*` events as every other writer, never one of its own kind** — the paragraph above this one said none at all, but the done-when (*"with the panel already open; it appears"*) needs the open panel to learn of the row, and a row change without its event would break the row rule. So a row the scan creates is `port.added`, an observation of a listed row (listening, its bind address, gone) is `port.updated`, and a row only discovery held that stops listening is `port.retired`, each carrying `data.source: "discovery"` at `info` level. They are rows, reduced like any other, and nothing in the UI turns them into a notice: the reason in §8.2 stands, and is now a test. `POST …/rescan` answers `202` and is settled by `port.scanned` — written only for a workspace asked about, `data.discovery` being `ok` or `unavailable` — which changes no entity; the refusals are `not_found`, `in_progress` (deleting) and `503 unavailable` once the scanner is stopping.

## 7. Preview authentication

The requirement is that a preview is behind the same sign-in as everything else, and the obstacle is that a cookie set for `drydock.example.com` is host-only and therefore never sent to a preview host. So each preview host needs its own cookie, and the only origin that can prove you are signed in is the main one.

1. Browser requests `https://myapp-5173-p2mq.drydock-preview.net/`. No preview cookie.
2. Preview mux redirects to `https://drydock.example.com/preview/authorize?return=<the original URL>`.
3. **The session cookie rides that top-level navigation.** The preview host and the UI are on *different* registrable domains, so this is a cross-site navigation — but `SameSite=Lax` sends the cookie on a top-level GET even cross-site, which is exactly why the session cookie is `Lax` and not `Strict` (§13.2). `Strict` would withhold it here and a signed-in user would be bounced to sign-in on every new preview.
4. `/preview/authorize` validates the session, checks the slug resolves to an enabled port on a running workspace, and mints a **single-use token, 60-second TTL, bound to that `auth_session.id` and that one preview host**.
5. Redirect to `https://myapp-5173-p2mq.drydock-preview.net/.drydock/session?t=<token>`.
6. Preview mux consumes the token, writes `preview_session`, sets a host-only cookie (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`), and redirects to the path from step 1.

Steps 2–6 are four redirects with no user interaction, so in practice the first request to a new preview host renders the app. If you are not signed in, step 3 lands on the ordinary sign-in page and `return` carries you back afterwards.

*As built, §13 step 2 (§13.2):* exactly the six steps above — three redirects, four requests — with these specifics. The cookie is **`__Host-drydock-preview`**: two preview hosts are same-site with each other (§10.4), so without the prefix a hostile preview could `Set-Cookie` one with `Domain=<preview domain>` at every other preview — not a credential, since a session is bound to its host, but a way to wedge another preview's handshake; a browser refuses a `__Host-` cookie that names a `Domain`. Step 2's redirect carries the URL rebuilt from the canonical (lowercase, port-less) host. The landing path is kept *with the token*, server-side, so nothing in the token URL can change where step 6 lands, and step 6's `Location` is absolute on the preview host, so a path like `//evil.example/` is a path there and not another host. A `return` that is not an `https` URL on one preview host is a `400`, never a redirect. Whether a well-formed slug names an enabled port is decided only at step 4, after sign-in: an unauthenticated caller gets the same redirect for a real slug as for an invented one. (Step 1 answered every request with one uniform `401`; that answer is gone, and uniformity now holds per refusal, below.)

> [!WARNING]
> **Strip the preview cookie before proxying — at the second hop, not the first**
>
> The browser attaches the preview cookie to every request to that host, including the ones that get forwarded upstream. The container must never see it: a dev server that logs headers would write a live credential to a file, and a hostile one would simply exfiltrate it.
>
> There are **two** proxy hops, and the cookie has to survive the first one. Caddy forwards it to `preview.sock` untouched, because Drydock's preview mux is the thing that validates it — a Caddy-side strip would make preview authentication impossible. The strip belongs to the **preview proxy component** (§3.1), in the hop from Drydock to the container, *after* the session has been checked and immediately before the dial:
>
> | Hop | Preview cookie | Because |
> |---|---|---|
> | LAN → Caddy → `preview.sock` | **passes through** | The preview mux authenticates on it. |
> | preview proxy → container | **removed** | The container has no business seeing a Drydock credential. |
>
> Concretely: delete the preview cookie from `Cookie` in the outbound request, and drop any upstream `Set-Cookie` that tries to claim the same name. Everything else passes through untouched, because the app's own cookies are the app's business.

`SameSite=Lax` rather than `Strict` on the preview cookie is deliberate: the cookie has to survive arriving via the cross-site handshake chain and via a top-level link from the UI (now a *different* registrable domain), which `Lax` guarantees and `Strict` makes browser-dependent. It is safe because a preview cookie is a read-only capability to view one app, not an API credential — and it stays host-only, so §10.4's preview→preview isolation holds regardless.

The pending token from step 4 lives **in memory only**, is consumed with an atomic compare-and-delete so two racing requests cannot both spend it, and is never persisted. A Drydock restart between mint and consume simply fails the handshake closed, and the browser retries from step 1 — there is nothing on disk to leak and nothing to replay.

> [!WARNING]
> **The one-time token is in the URL, so keep it out of the logs**
>
> Step 5 carries the token as a `?t=` query parameter, because a redirect from `drydock.example.com` to a *different* host is the only way to cross origins — the main origin cannot set the preview host's cookie for it. A query-string credential is the most-logged thing there is, which collides with the overall design's "session tokens never reach Caddy's access log" (§13.5). The token is single-use and expires in 60 seconds, so replay is already hard; the remaining job is to stop it being *written down*:
>
> - The preview vhost keeps Caddy access logging **off** (Caddy's default), and if a global `log` directive is ever added it redacts query strings for this host. A live credential must not land in the access log even for 60 seconds.
> - `/.drydock/session` responds with `Referrer-Policy: no-referrer`, so the token URL cannot leak onward in a `Referer` header from the app that loads next.
> - The step-6 redirect lands on the clean path, so the token URL never becomes the app's own address or a bookmark.
>
> Single-use, 60-second TTL, no access log, no referrer: that quartet is what makes a credential-in-a-URL tolerable here, and it is a decision on the record rather than an oversight.

## 8. Finding and reaching the container

### 8.1 Resolving the upstream

Drydock runs on the host, not in a container (§1, *not containerized itself*), so it can dial the container's address on the Docker network directly. There is nothing to publish and nothing bound on the dev server's interfaces.

The address comes from the container's `NetworkSettings`, looked up by the same `drydock.workspace=<id>` label that everything else uses. **Container IPs change on restart, and a container can die without Drydock noticing** — an OOM kill, a `dockerd` restart, a crash the supervisor has not yet seen — after which Docker is free to hand that IP to a different container. A cached IP is therefore not safe to dial on the strength of a `running` row alone: the row can be stale in exactly the window where the IP now points at *another* workspace's container, and a request authorized for X would be shown Y — including whatever Y's dev server renders from Y's own secrets.

So the address is not merely cached-with-invalidation; it is **re-resolved from Docker by label immediately before every dial**, and the dial is bound to that freshly-resolved container identity, not to a remembered IP string. If the label resolves to no live container right now, the workspace is treated as stopped — a `/.drydock/denied` — never as a cache miss to be filled from the last known address. This is the same *Docker is the truth, the database is the cache* rule as §6, made per-request because a proxy runs continuously against a fact — *which container, at which IP* — that churns under it, and §6's boot-time reconciliation cadence is far too coarse to keep a live data path from crossing a workspace boundary.

A dial is attempted only when the label resolves to a `running` container and the port row is `enabled`. Either being false is a `/.drydock/denied` page naming which one, not a 502.

*As built (§13.4):* the denied page names neither — it is step 2's one constant dead end, which says nothing a caller did not already know — and the resolution is `docker ps` for the one running container carrying the workspace's label and `docker inspect` of it, before every dial, with the same container inspected again after the connect: a connection is kept only if that container still holds the address it was made to.

### 8.2 Discovering what is listening

Requiring `forwardPorts` to be exhaustive pushes the cost of this feature onto every repository, and it does not even work: Vite takes 5174 when 5173 is busy, Next.js does the same, and an agent that decides to start a second server picks whatever it likes. A hand-written list is stale the first time something moves.

Drydock does not have to ask. A container's listening sockets are visible from the host as an ordinary file:

```
/proc/<container-pid>/net/tcp        # and net/tcp6

  sl  local_address rem_address   st ...
   0: 0100007F:2382 00000000:0000 0A ...   ->  127.0.0.1:9090   LISTEN
   1: 00000000:1F90 00000000:0000 0A ...   ->  0.0.0.0:8080     LISTEN
```

`<container-pid>` is `.State.Pid` on the container Drydock already tracks by label (§8.1). Reading that file enumerates every socket in the container's network namespace — **which is only true while that PID is still that container.** A dead container's PID is reused by the kernel like any other, so a scan that trusts a cached `.State.Pid` can end up reading an unrelated host process's namespace and surfacing *host* listeners as previewable rows for a workspace. The PID is therefore re-resolved from Docker by label at the start of every scan, exactly as the dial re-resolves the IP (§8.1); a label that resolves to no live container is scanned as empty, not skipped-with-stale-rows. Enabling a phantom row would otherwise feed the misrouted dial in §8.1, so the two live paths share one rule: **trust the label resolved now, never a remembered PID or IP.**

Three properties make this the right mechanism rather than merely a working one, and all three were measured rather than assumed:

**No agent, and no cooperation.** Nothing executes inside the container — no injected process, no `exec`, no dependency on `ss`, `netstat`, or `lsof` being present in the image. §3.1's promise that the container never learns it is being previewed survives discovery intact, which it would not if discovery were a shell command.

**It is a file read.** No process spawn, so scanning fifteen workspaces every few seconds costs approximately nothing. An `exec`-based scan is a container round-trip per workspace per interval, which is the kind of cost that gets a feature quietly disabled.

**It is unprivileged.** A non-root user — uid 1000, `docker` group, `yama/ptrace_scope=1` — reads the socket table of a container process running as uid 0. Unlike `/proc/<pid>/mem` or `/proc/<pid>/fd`, `/proc/<pid>/net/` sits outside the ptrace access check, so Drydock needs no capability it does not already have. *If a hardened host ever changes that, the fallback is a throwaway container sharing the target's namespaces (`--network=container:<id>`), which costs a spawn but still asks nothing of the image.*

#### The bind address is the diagnosis

The scan distinguishes `0.0.0.0` from `127.0.0.1`. That single fact turns the most common failure in §11 from a guess made after a refused connection into something known before anyone clicks: a server on `127.0.0.1:5173` is listed, greyed, and labelled *"listening on loopback — start it with `--host 0.0.0.0` to preview it."* The preview is never offered, so the 502 never happens.

#### Turning sockets into rows

A raw socket list is not a useful list. A workspace running a Drydock session has the remote-control process, possibly MCP servers, maybe a debugger, and — somewhere in there — the dev server you actually wanted.

| Rule | Why |
|---|---|
| Only state `0A` (LISTEN), over `tcp` and `tcp6`. | An established connection is not an offer. |
| Loopback binds are listed but never previewable. | They are the diagnosis above, not candidates. |
| Two consecutive scans before a row appears; a grace period before it goes. | A restarting dev server must not churn the list or the event stream. |
| Suppress Drydock's own ports and a small known-noise denylist. | The remote-control process is not a preview. |
| Merge onto `container_port`; never duplicate. | A port both declared and observed is **one row with both flags** — the declaration supplies the label and the intent, the scan supplies the truth. |
| Any row can be hidden. | The escape hatch for the thing you never want to see again, and cheaper than a cleverer denylist. |

Declared-but-not-listening ports stay visible and greyed, which is what makes the panel useful on a workspace that is running while its dev server is not.

*As built (§13 step 5, §13.6):* the scan is `internal/container`'s `Listeners` — `Address` (so a container with no bridge address of its own, whose namespace may be the host's, is never read), the PID from that same `docker inspect`, `/proc/<pid>/net/tcp` and `tcp6` read as the unprivileged Drydock user, then the same container inspected again and the read thrown away unless it is still running under that PID — and `preview.Scanner`, every 5 s, debouncing by port (two scans at one bind address to appear, 15 s unseen to go) and merging through `preview.Service.Observe`. There is no denylist yet: Drydock runs no TCP listener in a container (the broker is a Unix socket), so there is nothing of its own to suppress, and **Hide** is the escape hatch. A row only discovery holds is **retired** when its port goes, so ports a test suite opened do not accumulate; one the operator enabled, added or hid, or the configuration declares, is kept and marked gone.

#### Discovery is not a prompt

The tempting next step is a notification — *"port 8080 appeared, approve?"* — and this design deliberately refuses it, for the reason §13.5 of the overall document gives for refusing a re-auth prompt on delete: **a prompt you see often enough buys habituation rather than safety.** A toast that fires whenever a test run opens a socket trains exactly one reflex, and it is the wrong one.

So discovery is **ambient, not interruptive**. The workspace card carries a quiet count — *"4 listening · 1 previewed"* — and the ports panel is where decisions get made, at a moment the operator chose. No port changing state ever moves anything into reach.

That the list is now live is itself an argument that default-deny was the right call in §12. A static list makes auto-exposure merely unwise; a live one would make it dangerous, because a debugger would become reachable at the instant it opened.

### 8.3 The `Host` header

Dev servers increasingly reject unexpected `Host` values — Vite's `server.allowedHosts`, Rails' `config.hosts`, Django's `ALLOWED_HOSTS`. Two behaviors, per port, because neither is right for everything:

| `host_header` | Sends upstream | Use when |
|---|---|---|
| `localhost` *(default)* | `localhost:<port>` | Almost always. The app's host allowlist accepts it without being edited, and a framework that honours `X-Forwarded-Host` still builds absolute URLs on the preview host. |
| `passthrough` | The preview hostname | The app builds absolute URLs from `Host` itself and ignores `X-Forwarded-*`. Requires adding the preview host to the app's allowlist. |

`X-Forwarded-Proto: https`, `X-Forwarded-Host: <preview host>`, and `X-Forwarded-For` are always set, so a framework that honors them produces correct absolute URLs even under `localhost`.

*Decided 8 October 2026 (owner, §14.3):* the default is `localhost`, not the `passthrough` earlier drafts chose. The allowlist group is the one that grows — Vite now refuses an unknown `Host` out of the box — and a preview that answers *"Blocked request"* on first open looks like a Drydock bug, while an app that needs its own hostname is the rarer case and is one switch on its port. Step 3 (§13) built it (§13.4).

### 8.4 Websockets and streaming

HMR is the whole point of previewing a dev server, so `Upgrade` must survive the proxy, and the Caddy block needs `flush_interval -1` for the same reason the API block already has it (§13.1). Server-sent events from a previewed app work for free once that flag is set.

*As built (§13.4):* the proxy flushes every write too, and relays an upgrade both ways through the hijacked connection, closing it after `--preview-idle-timeout` with no byte either way. Measured with Vite 8.3.2: its HMR socket connects through Caddy and the proxy, carries a custom event up and back, and delivers the hot update for a file edited on the host.

## 9. Caddy configuration

The one LAN-facing file, extended. **This is the only place this design touches §13.1.** The shipped version is `deploy/Caddyfile` plus `deploy/preview.caddy` — the preview site is a separate file imported by glob, so a deployment without the wildcard certificate yet simply omits it — and the main file also moves Caddy's admin API off its default `localhost:2019` onto a `0600` Unix socket — see the warning under the overall design's §13.1; the block below omits that global option for brevity, not because it is optional.

```
drydock.example.com {
    tls /etc/caddy/certs/drydock.pem /etc/caddy/certs/drydock.key
    encode zstd gzip
    reverse_proxy unix//run/drydock/http.sock {
        flush_interval -1
        header_up X-Forwarded-For {remote_host}
    }
}

*.drydock-preview.net {
    tls /etc/caddy/certs/preview.pem /etc/caddy/certs/preview.key   # wildcard on a separate domain, provisioned externally
    reverse_proxy unix//run/drydock/preview.sock {
        flush_interval -1                          # HMR websockets and SSE from the previewed app
        header_up X-Forwarded-For {remote_host}
    }
    # No cookie handling here — deliberately. See the note below.
    # Access logging stays OFF on this block (Caddy's default): the step-5
    # handshake puts a one-time token in the query string (§7). If a global
    # log directive is ever added, redact query strings for this host.
}
# Two site blocks, two registrable domains, two certs. The preview wildcard
# matches one label under drydock-preview.net; a request for anything else —
# a bare IP, or a drydock.example.com sibling — matches no block and is refused.
```

> [!WARNING]
> **A new dependency on the certificate automation**
>
> §13.1 says certificate issuance is out of scope and Drydock has no opinion beyond needing the result. This design adds one requirement to that result: **a wildcard certificate for `*.drydock-preview.net`** — a separate registrable domain from the UI's, which in practice means DNS-01 rather than HTTP-01, plus a wildcard `A`/`AAAA` record in that domain's zone pointing at the same Caddy. If that is not available, §4's fixed-SAN pool is the fallback and it changes the URL scheme but nothing else in this document.
>
> `encode` is deliberately absent from the preview block. Compressing a proxied dev server that is already compressing, or already streaming, buys nothing and has broken HMR in the wild.

> [!NOTE]
> **Why there is no cookie stripping in this file**
>
> §7 requires the preview cookie never to reach the container, and the natural place to look for that rule is here. It is not here, and it must not be: Caddy's hop ends at `preview.sock`, where Drydock still has to *read* that cookie to authenticate the request. Stripping it in Caddy would leave every preview permanently unauthenticated. The strip happens one hop later, in the preview proxy, between Drydock and the container — see the table in §7.
>
> Two behaviours this block relies on, both verified against Caddy rather than assumed:
>
> - **`Host` survives the proxy to a Unix socket.** Caddy passes the client's `Host` through unchanged, so `myapp-5173-p2mq.drydock-preview.net` arrives intact and is the routing key the preview mux resolves the slug from. Nothing needs to carry it separately — an earlier draft of this block set an `X-Drydock-Preview-Host` header, which was redundant and is now removed. A second source of truth for the routing key is a liability, not a convenience.
> - **`header_up` replaces rather than appends**, so a client-supplied `X-Forwarded-For` cannot survive alongside the real one. This is the same property §13.2 of the overall document relies on when it says the forwarded address is as trustworthy as Caddy is.
> - **Routing is by `Host`, not by the TLS name, and `strict_sni_host` stays off — deliberately.** A request whose SNI names a preview host but whose `Host` is the UI's reaches the API socket, and the reverse reaches the preview socket (measured, and pinned by `TestRoutingIsByHostAcrossSNI`). Turning `strict_sni_host` on would answer `421` instead, and browsers coalesce HTTP/2 connections across every name one certificate covers: two tabs on two preview slugs share the wildcard, so the second would get `421`s, which HMR and SSE handle badly. Off costs nothing a browser can use — neither certificate covers the other site's names, so no browser sends one site's `Host` on the other's connection — a raw client can choose any SNI anyway, and the API checks `Host` itself (overall §13.3).

## 10. Security

### 10.1 What a preview actually is

A preview is a **browser-side** exposure: repository code, rendered as a first-class web origin on a domain of yours kept *separate* from the control plane's, on a device that is also signed in to Drydock. Nothing new is exposed to the network — no listener, no published port, no credential in the container — and a compromised container gains nothing it did not have, because it does not learn it is being previewed.

So the threat model is mostly *what can that origin do to the other origin*, and there are three answers — plus a fourth (§10.5) aimed at the human at the keyboard rather than at another origin.

### 10.2 Preview → API is cross-site, so `SameSite` carries the load

`drydock.example.com` and `*.drydock-preview.net` are *different* registrable domains, so a previewed app is **cross-site** with the UI. That is the whole reason for the second domain (§4), and it means the browser enforces the CSRF boundary rather than an application check having to catch every request:

- `SameSite=Lax` on `__Host-drydock` is **not** attached to a cross-site `POST`, so a previewed app cannot issue a state-changing request to `/api/*` at all — the browser withholds the cookie. This is the primary CSRF defense, and it is a browser behaviour rather than a line of Drydock code that has to be right on every route.
- `Lax` still sends the cookie on a *top-level GET navigation*, which is what makes the authorize handshake (§7) work. That is harmless because mutations are never `GET` (overall §5), and CORS still prevents a cross-site page from reading any response it navigates to.
- The `Origin` allowlist on every state-changing route stays as **belt-and-braces** behind `SameSite`: cheap, testable in the CI half that `SameSite` is not, and the safety net that keeps a future mistake — collapsing the two domains back onto one registrable domain — from being an instant hole.

Keep the `Origin` check exact and fail-closed even though it is no longer the sole defense — the cost is nil and the failure it guards is quiet:

- **Exact-string allowlist.** Compare `Origin` against the literal `https://drydock.example.com`, never a suffix or registrable-domain match. This matters most precisely if previews are ever mis-hosted under this domain again.
- **Fail closed on an absent `Origin`.** A missing header is refused as hard as a wrong one; every legitimate API caller is a browser `fetch` (which always sends `Origin`) or the SSE `GET` (non-mutating).
- **No permissive CORS, and never a reflected `Origin`.** Cross-origin reads fail by default, so nothing depends on the caller's honesty.

The move to a separate domain is what let this section shrink from *"one application check is now the whole defense"* to *"the browser separates them, and the `Origin` check is the belt."* That is the trade the second domain buys.

### 10.3 Preview → UI cookie fixation is structurally impossible

A same-site subdomain can set cookies on its parent domain — classic cookie tossing. A preview on a *separate registrable domain* cannot: it shares no cookie scope with `drydock.example.com`, so there is no `Domain` it could set that the UI would receive. The `__Host-` prefix (§13.2) remains as hygiene — it would block fixation even under same-site — but the separate domain means the attack is not available in the first place.

Two properties still worth holding as the surface grows. First, **the UI reads exactly one cookie, `__Host-drydock`, by its exact name**, so even a future same-registrable-domain arrangement could not slip in a cookie the UI reads. Second, a hostile preview can register a **Service Worker** scoped to its own host, which outlives the container and can intercept later handshakes on that host; this grants nothing the preview origin did not already have (the preview cookie is `HttpOnly` and host-only, and the first token-bearing navigation predates any worker), but a retired slug's host should be assumed to still be running attacker code in some browser until it is evicted. Emitting `Clear-Site-Data: "*"` when a port is disabled or its slug retired is the cheap countermeasure; either way the residue is bounded to the preview's own already-hostile origin.

*As built (§13 step 4):* the header can only be sent from the preview host, and only to a device that comes back to it, so it rides the handshake. A device whose preview session the disable deleted is sent to `/preview/authorize` as any other; authorize, finding the slug's row switched off or retired (`preview.Service.Spent`), mints a single-use token marked *clear* instead of sending it to the plain denied page, and that host's `/.drydock/session` answers it with the denied page itself — `403`, never a redirect, so no engine has to honour the header on a `3xx` — carrying `Clear-Site-Data: "cache", "storage"` and setting no cookie. **Not `"cookies"`, and so not `"*"`** (review round 1): the cookies type is defined over the whole registrable domain, not the origin, so one visit to a switched-off port would clear every other preview's session cookie and every previewed app's own — preview-to-preview isolation (§10.4) undone for nothing, since the port's preview sessions are already deleted on the server and its cookie is dead. Cache and storage are origin-scoped, and storage covers service workers, the residue this section is about. Only a signed-in device that was shown the switch reaches it, so telling a switched-off port from a stopped workspace there tells it nothing new; an unauthenticated caller, and every token refusal, still gets the uniform answers of §13.2. A stopped workspace's enabled port is not spent and clears nothing: a stop is not a revocation. Chromium, through real Caddy, was measured clearing what the app had put in `localStorage` while another preview host's cookie survived — and, with `"cookies"` added back, wiping that cookie.

### 10.4 Preview → preview is blocked by host-only cookies

Two preview slugs are both under `drydock-preview.net`, so they are same-site *with each other* even though each is cross-site with the UI. Per-host preview cookies (§5, §7) are therefore still needed: a previewed app cannot read another preview because the browser will not send a host-only cookie for `a.drydock-preview.net` to `b.drydock-preview.net`. The cost is one invisible redirect per new preview host.

### 10.5 Preview → operator: the residual phishing surface

The three subsections above are origin-against-origin. There is a fourth target aimed at the human, and the separate domain shrinks it rather than removing it: a previewed app can still render a fake sign-in page and ask for the password.

What the separate domain changes is how convincing that is. A preview on `myapp-5173-p2mq.drydock-preview.net` is *not* the UI's domain — the address bar shows a different registrable domain from where sign-in actually lives — so this is ordinary lookalike phishing, the kind any external site can attempt, rather than a page served from Drydock's own name. There is a single factor (§13.2) and no second one, so the mitigations still earn their small cost:

- **Sign-in is served only from the bare UI origin.** `drydock.example.com` is the one and only place a password field ever appears, and it shares no registrable domain with any preview, so a password prompt anywhere under `drydock-preview.net` is categorically not the real thing.
- **The UI says so, where it is seen.** The device list and the ports panel state that a login prompt on any preview host is hostile by construction.
- **Previews carry no Drydock chrome.** Nothing Drydock renders is shared with the proxied response stream (§3.1), so a preview cannot borrow real Drydock UI to look convincing.

> [!NOTE]
> **Why this is now a note and not a warning**
>
> An earlier draft served previews same-site, on `*.preview.drydock.example.com`, where a hostile preview *was* Drydock's own domain and the phishing surface was the sharp edge of that choice. Moving to a separate registrable domain (§4) is what demoted it: the browser now separates the origins, and a preview is a lookalike rather than the real domain. The controls above are ordinary anti-phishing hygiene, not a compensating control holding up a risky decision.

### 10.6 Additions to the blast-radius table

Extending §13.4 of the overall document:

| If this is compromised | Reachable | Not reachable |
|---|---|---|
| **A previewed app** (hostile or XSS'd repo code) | Nothing on the API by default: a cross-site `POST` never carries the session cookie (`SameSite=Lax`), so state-changing requests to `/api/*` are refused by the browser, with the `Origin` check as backup. Its own preview origin's storage. | The API cookie (host-only, cross-site). API *responses* (CORS). Other previews (host-only preview cookies). Anything on the host directly — the proxy dials one container port and nothing else (but the note below bounds what that means). |
| **The preview proxy** | Every enabled port on every running workspace. | The App key, the Docker socket, the API mux, any credential — it holds none. |

> [!NOTE]
> **"One container port" bounds the proxy, not the response.** The preview proxy dials exactly one port and adds no network reach of its own. But the process listening on that port is repo code, and it can relay: bytes coming back through the preview may have been fetched by the container from anywhere the container itself can reach. A preview is a driven channel into whatever the container chooses to serve or relay — which is the container's existing egress (governed elsewhere), never a new capability the preview grants. The table row is a guarantee about the *proxy*, not about what the port serves.

### 10.7 Additions to §13.5's non-negotiables

- **The preview mux serves no API route, ever.** It is a separate mux on a separate socket precisely so this is structural rather than remembered.
- **The upstream is derived, never supplied.** Workspace container IP plus an enabled port. No host field reaches the dialer from a request, a config file, or a database column.
- **The container is re-resolved by label at every dial and every scan, never trusted from cache.** A `running` row is not permission to dial a remembered IP: the container may have died unobserved and Docker may have handed that IP, or that PID, to someone else (§8.1, §8.2). Trust the `drydock.workspace=<id>` label resolved *now*, or treat the workspace as stopped — never a stale address.
- **The preview cookie never reaches the container**, and an upstream `Set-Cookie` may not claim its name. This is the preview proxy's job on the hop to the container, *not* Caddy's on the hop to `preview.sock` — where the cookie still has to arrive for the request to authenticate at all (§7, §9).
- **Previews are default-deny.** No `forwarded_port` row with `enabled = 1`, no preview — the same rule, for the same reason, as `secret_grant` in §10.1.
- **A retired slug is never reissued.** `DELETE` retires the row and the global `UNIQUE` on `slug` keeps covering it, so a hostname is spent for good and a stale bookmark always fails closed. Reusing one would point an old link at a different workspace's preview, which is the cross-workspace exposure §8.1 exists to prevent, arrived at by coincidence instead of by a stale cache.
- **Discovery never enables anything.** The scanner writes `observed`, `bind_addr`, and timestamps. It has no path to `enabled`, and it is worth keeping that as a property of the code rather than of the current implementation: the container decides what it listens on, so a scanner that could enable would hand that decision to the container.
- **Previews are served from a separate registrable domain from the UI.** This is what keeps them cross-site, so `SameSite` — not one application check — is the primary CSRF boundary (§4, §10.2). Collapsing them onto one registrable domain is a security regression, not a naming change.
- **The API's `Origin` allowlist is exact-match and fail-closed.** Defense in depth behind `SameSite=Lax`, which does the primary CSRF work now that previews are cross-site (§10.2) — but kept exact (never a suffix), fail-closed on an absent `Origin`, and free of reflected CORS, because the cost is nil and it is the safety net if the domains are ever collapsed.
- **Sign-in appears only on the bare UI origin, never a preview host.** A password field anywhere under the preview domain — a different registrable domain from the UI — is hostile by construction (§10.5), and the UI teaches this rather than trusting the operator to notice.
- **The UI sends `Content-Security-Policy: frame-ancestors 'none'`**, so a preview cannot frame the control plane for clickjacking.
- **The preview mux is resource-bounded.** A concurrent-connection cap and an idle timeout on proxied upgrades, so a held-open HMR socket or an unauthenticated redirect flood cannot exhaust the process (§3, §11) — consistent with the hand-managed capacity of the overall §1.

## 11. Failure modes

Extending §12. The first row is the one that will actually happen, repeatedly.

| Failure | Detection | Response |
|---|---|---|
| **Dev server bound to `127.0.0.1` inside the container** | The discovery scan (§8.2) reads `bind_addr` directly — this is known *before* anyone tries | The single most common cause, and a generic 502 would send you debugging the proxy. The port is listed, greyed, and not previewable: *"listening on 127.0.0.1:5173, which is only reachable from inside the container. Start it with `--host 0.0.0.0`."* Include the flag for the detected server where known. **The dial never happens, so the 502 never happens.** |
| Port enabled, nothing listening | `observed_state = gone`, and the dial is refused | *"Nothing is listening on port 5173"* — the server has not been started, or it crashed. Link to the workspace log. |
| Discovery unavailable (cannot read `/proc/<pid>/net/*`) | Scan returns an error rather than an empty set | **Fail visibly, never silently.** An empty port list and a broken scanner look identical, and the difference matters. Fall back to declared ports only, badge the panel *"discovery unavailable"*, and keep manual add working. |
| A port flaps (test run opens and closes sockets) | Repeated appear/disappear within the debounce window | The two-scan threshold and the disappearance grace period absorb it. Nothing is emitted, so nothing is noticed. |
| Workspace stopped | State check before dialing | `/.drydock/denied` naming the state, with a start button. Never a proxy error. |
| Container restarted, IP changed | Dial fails against the address resolved for it — there is no cached one (§8.1) | Re-resolve once and retry transparently, if the answer changed. Only a second failure is user-visible. *As built:* a refused or timed-out connect is resolved again and retried only when the container or address differs; otherwise Drydock's `502` (refused) or `504` (timed out) page, naming the port and nothing internal. |
| App rejects the `Host` header | Upstream returns 400/403 with a recognizable body (`Blocked request`, `Invalid HTTP_HOST`) | Detect the signature and suggest the fix for that framework, or switching a `passthrough` port back to the default, `host_header: localhost`. A raw 403 here reads as a Drydock bug. |
| Websocket upgrade fails | `Upgrade` request returns non-101 | Usually `flush_interval` or a buffering layer. Surface it as "live reload unavailable" rather than breaking the page. |
| Wildcard certificate missing or expired | TLS failure at Caddy, before Drydock | Previews fail; the UI is unaffected because it is a different block with a different cert. Health check warns on preview-cert expiry separately from the UI cert. |
| Slug collision, live or retired | `UNIQUE` violation on insert | Regenerate the random suffix and retry. The constraint covers retired rows too, which is what makes the retry mandatory rather than cosmetic: without it the collision would be resolved by handing a spent hostname to a new port. |
| Preview left open on a lost device | You notice, as in §13.2 | *Revoke all sessions* cascades to `preview_session`, so every preview on every device dies with the same click. This is the reason for the foreign key. |
| Too many concurrent previews or held-open HMR sockets | Live connection count against the cap (§10.7) | Connections beyond the cap are refused with a plain 503; upgrades idle past the timeout are closed. A buggy client, or an unauthenticated redirect flood from a LAN device, cannot pin the process — the preview mux is bounded like the rest of the system (§1). |

## 12. What this deliberately gives up

- **No non-HTTP forwarding.** A database client on a tablet would need a raw TCP listener on the LAN, which §13.5 forbids and which no amount of design here can make acceptable. Use `devcontainer exec`.
- **No port auto-exposure.** A repo's `forwardPorts` was written for VS Code, where forwarding lands on *your own* loopback. Promoting that to a LAN-reachable origin serving code to a phone is a different decision, so Drydock finds ports for you and the enable is yours. One click, made once per port. Live discovery (§8.2) makes this *more* important rather than less: with a static list auto-exposure is merely unwise, but against a scanner it would publish a debugger at the moment it opened.
- **No notification when a port appears.** Discovery is ambient (§8.2). The cost is that you have to look at the panel; the benefit is that the approval click never becomes a reflex.
- **No preview without a session.** There is no share link, no anonymous read-only mode, no "just this one port". A preview is repo code on your domain; the sign-in is the only thing making that reasonable.
- **No editing through the preview.** It is a viewport. Changes happen through Remote Control, which is where the agent already is.
- **`/.drydock/*` is reserved** on every preview host. Rare, documented, and the alternative — a magic query parameter, or sniffing content — is worse.

## 13. Build plan

Small enough to be one phase, ordered so the risky part is first. This slots in after Phase 2 (walking skeleton) of §14 — it needs a running container and nothing else, and specifically does not need Claude, the broker, or secrets.

| Step | Deliverable | Done when |
|---|---|---|
| **1 — Front door** | Second socket, second mux, wildcard Caddy block, wildcard cert in place. Preview mux returns 401 for everything. **Done** — see *As built* below. | ✅ Every preview URL returns 401 from a device on the LAN, and no preview URL can reach an API route. |
| **2 — Handshake** | `/preview/authorize`, one-time tokens, `preview_session`, cookie stripping. **Done** — see *As built* below. | ✅ A signed-in device reaches a hardcoded upstream; an unsigned-in one is bounced to sign-in and returned. Revoke-all closes it. |
| **3 — Proxy** | Container IP resolution, dial, websocket upgrade, `Host` handling, `X-Forwarded-*`. **Done** — see *As built* below. | ✅ Vite with HMR works end to end — in the container tier through the real preview socket, and in Chromium through real Caddy (§13.4). On a phone it waits for step 4, which can enable a port. |
| **4 — Registry** | `forwarded_port`, declared-port parsing from the resolved config, the ports UI, probe endpoint. **Built** — see *As built* below; the real-Safari gate is not yet passed. | ✅ Enable a port from the card, open it, disable it, and watch it close — in Chromium through real Caddy (§13.5). ⏳ A real iOS and macOS Safari run the handshake end to end (§13.3): the runbook step is written (first deployment §8.8), and the run is the owner's. |
| **5 — Discovery** | The `/proc/<pid>/net/*` scanner, debounce, merge onto declared rows, the loopback classification, the ambient count on the card. **Built** — see *As built* below. | ✅ Start a server on an undeclared port with the panel already open; it appears, disabled, correctly labelled — and nothing is pushed at you: in a spec against the mock (held to the server's events by a golden file), over the real server's socket with a fake socket table, and through a real container's `/proc` (§13.6). |
| **6 — Diagnosis** | The §11 table, wired to what §8.2 already knows. | Binding a dev server to `127.0.0.1` produces the sentence that tells you to use `--host 0.0.0.0`, and no dial is ever attempted. |

Step 1 before anything else, for the same reason §14 puts the front door before the skeleton: retrofitting auth onto a proxy that already works is how open proxies happen.

### 13.1 As built — step 1

- **The preview socket is `api.PreviewFrontDoor`, not `api.Build`.** It mounts only the preview routes (§6) that have a written handler, each behind its gate as on the API mux, and answers every other request with `api.PreviewUnauthorized`: `401`, the API's own `unauthenticated` envelope, `Referrer-Policy: no-referrer` and `Cache-Control: no-store` — the request may carry a `?t=` token — and no CSP or framing rule, because the preview origin's headers are the previewed app's own. Nothing is written yet, so that is every request: any path (an API path, `/.drydock/session?t=…`, `/preview/authorize`), any method, any `Host`, and any credential — a forged preview cookie, a token, the UI's own session cookie, the right password on the sign-in `POST`. All get the same bytes, so a refusal says nothing about what was tried. "Every request" means every request that reaches the handler: `net/http` itself answers `OPTIONS *` (`200`), malformed request bytes or an HTTP/1.1 request with no `Host` (`400`) and over 1 MB of headers (`431`) before it runs — none of which depends on a route or a credential, so none is an oracle.
- **An unwritten preview route is not a `501`.** On the API socket a declared, unbuilt route answers `501` behind its gate. On the preview socket nobody is signed in yet, so a `501` — or `ServeMux`'s own `404` and `405`, which is what the socket answered before — would only tell the LAN which `/.drydock/` paths exist. When step 2 writes `/.drydock/session`, it is mounted behind its token gate and the rest keeps the `401`.
- **"No preview URL can reach an API route" is a property of which routes the door can mount**: `PreviewRoutes()` and nothing else. Its test hands the door a handler for every route of both muxes and asserts no API handler runs; the control is the same handlers on the API mux, where every one does. Over the real server's sockets, a live session reads `GET /api/auth/session` on the API socket and the same cookie reads the `401` on the preview socket, under the preview host, the UI host and a foreign one.
- **Caddy, under real Caddy** (testing §3.2): a preview host — any slug, any path including `/api/*` and `/preview/authorize` — reaches the preview socket and never the API socket; the UI host — `/.drydock/*` included — never reaches the preview socket; on the wildcard's TLS name, a `Host` two labels deep, the bare preview domain, a suffix trick or a foreign name matches no site; the preview cookie, the query and an ordinary header arrive intact while `X-Forwarded-For` is replaced; the token is in nothing Caddy writes; and nothing listens on `:2019`. Each was mutation-checked against `deploy/preview.caddy` or `deploy/Caddyfile`.
- **The wildcard certificate is proved at install.** With `--preview-domain`, the installer's final check also asks `https://drydock-check.<domain>/` for its `401`, verified against `--ca-cert` or the system store exactly as the UI host is, so a preview certificate without the wildcard's names fails the install naming the preview host and `--preview-cert`. The certificate, the domain and its LAN wildcard record are the operator's (§14.3); Drydock issues nothing.
- **In a browser** (testing §10.4): a signed-in Chromium clicking through to a preview host arrives cross-site with no session cookie and gets the `401`; a forged host-only preview cookie and a `?t=` token reach the preview socket through Caddy and change nothing; and another slug is sent no cookie.

What step 2 inherits: the fallback is the one place to turn into §7's redirect; `PreviewTokenValid` is still `false`; the uniform answer has to survive the redirect (a spent, expired and forged token alike); and the preview socket has no host check of its own yet — today every `Host` is the same `401`, and step 2's slug resolution is where an unknown host becomes `/.drydock/denied`. §10.7's connection cap and upgrade idle timeout are step 3's, with the proxy they bound. Three traps, found in review:

- **The installer's final check expects `401` from `drydock-check.<domain>/`.** Step 2's redirect (or an unknown slug's `/.drydock/denied`) makes that a `302`, and every install with previews on would fail its check — `test/install` will say so, but it is the one coupling outside `internal/api`, so change `verify_site`'s expectation for the preview host in the same change. `drydock-check` cannot parse as a slug (a slug's middle field is a numeric port), and step 2's parser can reserve it outright.
- **A refused token gets `wrap`'s headers, not the front door's.** Once `preview.session` is written, a spent, expired or forged token gets `302 /.drydock/denied` from the gate — uniform across the three, as §7 wants — but without `no-referrer` or `no-store`, and path-cleaning on a written route answers a `307` likewise. A redirect makes no document whose URL could leak as a referrer, so the risk is small, but decide it: set both headers on the gate's preview refusals, or record why not.
- **Step 3's proxy lives in the fallback, outside the route table**, so the table-walking meta-tests (gate before handler, no CORS, Origin) cannot see its preview-cookie gate. Declare it as a catch-all table entry, or give the fallback meta-tests of its own.

### 13.2 As built — step 2

- **The chain.** `https://<slug>.<domain>/p?q` with no valid preview cookie → `302 https://<ui-host>/preview/authorize?return=<that URL>` → (signed in) `302 https://<slug>.<domain>/.drydock/session?t=<token>` → `302 https://<slug>.<domain>/p?q` with `Set-Cookie: __Host-drydock-preview=…; Path=/; HttpOnly; Secure; SameSite=Lax` (no `Domain`, no `Max-Age`) → the upstream. Not signed in, authorize is `302 /signin?return=/preview/authorize?return=…`, and the sign-in view *loads* that path rather than routing to it (`isServerPath` in `web/src/lib/returnPath.ts`), so the device comes back through authorize. Measured in Chromium through real Caddy (`preview.spec.ts`).
- **The token** (`internal/preview`): 32 random bytes, kept only as its SHA-256 in an in-memory map with its grant — auth session, preview host, port, landing path — and a 60-second expiry. Consuming is one lookup-and-delete under one lock, and the delete happens whatever the outcome, so a token shown late or on the wrong host is spent too: of a hundred concurrent consumes, exactly one wins. At most 64 are pending per auth session and 4,096 in all; a mint past either is `503`, so one tab looping on authorize refuses only its own device. A restart forgets them all, which fails a handshake in flight closed.
- **The preview session** is written only if the grant's port still resolves and its auth session is still alive (and the foreign key refuses one revoked in between). Each request validates the cookie against `preview_session` joined to its auth session, its port (enabled, unretired) and its workspace (`running`), checks the `Host` it arrived on, the 12-hour idle window and the auth session's own expiry, and slides `last_seen_at` at most once a minute. Any failure is "no session". Only the **first** cookie of the name is tried — one lookup per request: a browser never sends two (the `__Host-` prefix forces `Path=/` and no `Domain`, so a second replaces the first), and trying each let ~3,000 forged cookies buy ~45 ms of database work per unauthenticated request (review round 1).
- **A stop does not delete preview sessions; it refuses them.** While the workspace is not `running` every request is refused like any other, and after a start, within the 12-hour window, the device's session works again with no handshake. Decided, not overlooked: a stop is not a revocation — the device's auth session would mint a new preview session on its next request anyway, so deleting buys nothing — while everything that *is* a revocation (disable, retire, workspace removal, sign-out, revoke-all, a password change) deletes. Step 4 should not assume a stop clears them.
- **Uniformity, per refusal.** Every cookie refusal — none, forged, another preview host's, a revoked or idle session's, a stopped workspace's, the UI's own cookie — is byte-for-byte the redirect no cookie gets. Every token refusal — none, empty, doubled, forged, spent, expired, another host's — is byte-for-byte `302 /.drydock/denied`. Neither sets or clears a cookie, which would itself be a tell.
- **Trap 1, closed.** Every answer Drydock makes on a preview host says `Referrer-Policy: no-referrer` and `Cache-Control: no-store`: the token gate's refusal, the handlers', the fallback's redirects, and ServeMux's path-cleaning `307` (whose `Location` keeps the query, so the token), which `api.PreviewFrontDoor` covers by setting both before dispatching to any mounted route. A preview URL too long for authorize to carry (`preview.MaxReturn`, 8 KiB) ends on its own host's `/.drydock/denied` before the round trip, not on a `400` on the UI origin; `return` refuses an empty port (`host:`) as well as a real one. The `/.drydock/session` handler sets them itself too, on success and refusal, and `TestPreviewSessionSendsNoReferrer` calls it directly on both paths.
- **The fallback's order**, outside the route table (trap 3, half-closed): a `Host` that is not one well-formed label under the preview domain, or is `drydock-check`, → `/.drydock/denied`; an unmounted path under `/.drydock/` → `/.drydock/denied`, whatever the cookie; no valid preview cookie → the handshake; otherwise the upstream. `TestPreviewFallbackOrder` drives it under the stub gate one gate at a time. The table meta-tests still cannot see it, which is step 3's to close if the proxy grows a gate of its own.
- **`/.drydock/denied`** is `403` and one constant page — no workspace, port, state or reason, and no link to the UI — with a "Try again" link to `/`, which restarts the handshake. No CSP and no framing rule: a preview origin's headers are the app's, and the browser tier frames it as its frameable control.
- **The upstream is a seam**: `preview.Upstream`, handed the request with the preview cookie removed from `Cookie` (every other cookie kept, in order) and a `preview.CookieGuard` writer that drops any `Set-Cookie` naming the preview cookie (case-insensitively) before **every** header block is sent — a `1xx` included, latching only once the final block goes (≥ `200`, a `101`, or an implicit `200`). Review round 1 found the first version latched on the first `WriteHeader`, so a `103 Early Hints` let a cookie added after it ride the final response; `httputil.ReverseProxy` forwards an upstream's `1xx` by default, so that was step 3's obvious proxy. Tested through a real `ReverseProxy`. Production serves `preview.Placeholder`, a fixed page inside Drydock naming the port and the *names* of the cookies the request carried for the app — never a value — so a browser can see the strip. `server.PreviewUpstream` is the test seam.
- **Revoke-all.** The cascade deletes every preview session of a revoked auth session — *Sign out everywhere*, signing one device out, a password change, an auth session expiring on sight — and validation joins the auth session besides, so either alone closes the preview on its next request. Settings now says so in its confirmation, and says a sign-in form on a preview is not Drydock's (§10.5).
- **The installer** asks `https://drydock-check.<domain>/` for `302` to `https://drydock-check.<domain>/.drydock/denied` (curl's resolved redirect URL) instead of `401`; `drydock-check` is refused as a slug by the parser and by the schema.

**What step 3 should know.**

- **Replace `preview.Placeholder`, keep the seam's contract.** The real proxy receives the request already stripped and writes through `CookieGuard`; it must keep both true for a **websocket upgrade**, where a `101` written through a hijacked connection bypasses the guard — filter the upstream's response headers in the proxy itself (`httputil.ReverseProxy`'s `ModifyResponse` runs for a `101`). A `1xx` is already covered: `ReverseProxy` forwards it through `WriteHeader`, which the guard filters (`TestCookieGuardFiltersInformationalResponses`); keep that test passing against whatever proxy replaces `Placeholder`.
- **`Target` carries no address.** It has the workspace id, port, scheme and `host_header`; the dial re-resolves the container by label every time (§8.1). A workspace whose label resolves to nothing is a `/.drydock/denied`, as the resolver's `running` check already is for the row.
- **The `localhost` Host rewrite, `X-Forwarded-*`, §10.7's connection cap and upgrade idle timeout** are all still step 3's. The cap has to count the redirects too: an unauthenticated redirect flood is one of the things it bounds.
- **The fallback does not clean the path.** A request for `/a/../b` reaches the upstream as sent; whether the proxy cleans it is step 3's decision, not an accident.
- **Firefox and WebKit are not in the handshake tests, and cannot be made to be without disabling certificate validation.** The handshake needs two real hostnames under trusted TLS. Playwright's Linux WebKit verifies through the system's glib-networking GnuTLS backend, whose only anchor here is GnuTLS's compiled-in `/etc/ssl/certs/ca-certificates.crt`: this GnuTLS has no p11-kit trust module to read a per-user anchor, and nothing in the stack reads a trust variable. Measured on 8 October 2026 (Playwright WebKit 2359, glib-networking 2.80.0, GnuTLS 3.8.3): with a throwaway CA that Node verifies against as its only anchor (the control), WebKit refused the leaf with *"Unacceptable TLS certificate"* under no hint and under each of `SSL_CERT_FILE`, `SSL_CERT_DIR`, `GIO_USE_TLS=openssl` (no OpenSSL backend is installed), `CURL_CA_BUNDLE`, and a per-run `HOME` with p11-kit user anchors. Bind-mounting a bundle over the system file in a private mount namespace would keep validation on, but unprivileged user namespaces are refused here (`unshare -rm`: *write failed /proc/self/uid_map: Operation not permitted*), and editing the system file is the system state the tier exists to avoid. `ignoreHTTPSErrors` stays banned, scoped or not. Playwright's WebKit is libsoup, not CFNetwork, in any case — it would be evidence about Safari, not Safari.
- **So a real-Safari handshake is a gate (§13.3).** Nothing in production reaches `/.drydock/session` yet — no port can be enabled — so no device has run the cookie half of the chain.

### 13.3 Gates before previews are declared done

- **A real Safari runs the handshake end to end** — iOS Safari and macOS Safari, on the deployed preview domain, a click from the UI to an enabled port's preview: through `/preview/authorize`, `/.drydock/session` and the landing, holding the preview cookie and served on the next request without a handshake; and after *Sign out everywhere*, bounced. It runs at **step 4**, when a port can be enabled from the UI, and the runbook gains its step then. Until it passes, previews are not done, whatever the steps' own *Done when* say. The reason it is a gate rather than a hope: the redirect chain's last hop — a cookie set on a cross-site-initiated redirect and sent back on the next — is exactly where engines differ, and `SameSite` behaviour Chromium never shows has shipped broken once already (v0.2.1's `Origin: null`).

  **Status: not yet passed.** Step 4 built what it needs, and the runbook's step is
  [first deployment §8.8](../../deploy/first-deployment.md#88-preview-a-port-and-the-real-safari-check):
  on each Safari, *Preview this port* from the workspace's Ports panel, open the address, two
  reloads served with no trip through the UI host (Web Inspector shows the cookie on the Mac),
  *Turn off preview* ending on the denied page, and *Sign out everywhere* bouncing the next
  request to sign-in. Record each Safari's version and result here when it has run.

### 13.4 As built — step 3

- **The proxy is `preview.Proxy`**, behind the `preview.Upstream` seam step 2 left: an `httputil.ReverseProxy` per request over one shared `http.Transport` whose only dial is the proxy's own. The request's URL host is a *dial key* — the workspace id and container port, hex-encoded under `.invalid` — never an address, and it is all the transport pools on. The transport takes no proxy from the environment (`HTTP_PROXY` cannot route a preview), adds no compression of its own and decodes none (`Accept-Encoding` and the body pass as sent), speaks HTTP/1.1 to the upstream, and keeps an idle upstream connection 30 seconds.
- **Resolve, dial, confirm (§8.1).** Before every dial, `container.Manager.Address`: `docker ps --quiet --no-trunc --filter status=running --filter label=<prefix>.workspace=<id>`, which must name exactly one container — none is *not running*, two is refused as ambiguous rather than guessed between — then `docker inspect --type container -- <full id>`: still running, still carrying the label with this id, and an address on one of its Docker networks, IPv4 before IPv6 and networks by name. An address no Docker network assigns — loopback and unspecified (the host itself, PF §10.6), link-local, multicast, a zone — and a container with none (`--network=host` or `none`) are *not running* too: there is nothing to dial that is the container's own. *Review round 1:* so are the network's own `Gateway`/`IPv6Gateway` (the host's side of a bridge), any address this host holds itself (`net.InterfaceAddrs`, asked at every resolution), and an address on any network whose driver is not `bridge` — the candidates' networks are read with one `docker network inspect -- <full ids>`. Bridge only, because it is the one driver whose containers sit behind a host-side bridge Docker made for them; macvlan and ipvlan put a container on the LAN beside the host, where an operator-approved `--ip` can name the host's own address or any other machine there (reproduced in review: an ipvlan on `eth0` with `--ip` the host's address came back from `docker inspect` as the container's), and overlay reaches other hosts. A Compose devcontainer's network is a bridge too, so nothing a dev container normally uses is lost. After the connect, `Confirm` inspects that same container again, and the connection is kept only if it is still running at that address — had it died and its address gone to another workspace's container in between, it would not be. *Not running*, by either, is the denied page and no request reaches anything. A refused or timed-out connect is resolved once more and retried only if the answer changed (§11's restarted container), so a dev server that is simply not listening costs one dial. At most eight resolutions run at once, so a first page load of hundreds of modules does not start hundreds of `docker` processes; a dial waits for a slot and then resolves, immediately before it dials, as ever.
- **A kept-alive upstream connection is reused without resolving again — decided, not overlooked.** A TCP connection is bound to the container it was made to: one that dies takes its connections with it (its process's sockets close as it goes), and an address Docker hands to another container cannot answer a connection that container never accepted. What must never happen — a request dialled to an address remembered from before — cannot, because nothing remembers one. The per-request check is the preview session's own (step 2's join on a `running` workspace and an enabled port), which runs before the proxy on every request.
- **`Host`** is `localhost:<port>` (`HostLocalhost`, the default and anything that is not `passthrough`) or, for a `passthrough` port, the canonical preview host (§8.3). Read per request from the port's row, so a change applies to the next request. Measured in the container tier: Vite answers the preview host with its own *"Blocked request"* `403` and `localhost:5173` with the page.
- **Forwarding headers.** `Forwarded`, every `X-Forwarded-*` and `X-Real-Ip` the request arrived with are dropped — a client's and Caddy's alike — and the proxy sets its own: `X-Forwarded-Proto: https`, `X-Forwarded-Host: <preview host>`, and `X-Forwarded-For`: the last entry of the incoming one, which is the address Caddy's `header_up` put there (the preview socket admits Caddy alone), and only if it parses as an IP address; anything else is dropped, never passed on.
- **The path and query are not cleaned, and go upstream as the device sent them** — `..`, an encoded slash, `//`, a `;` in the path and in the query (`ReverseProxy` alone would drop the query's unparsable parameters; the proxy restores the raw query). Drydock makes no decision on either past the front door's `/.drydock/` check, so cleaning would protect nothing of Drydock's and would change what the app's own router sees. That check (`reservedPath`) does read the path as an app's router might — dot segments and doubled slashes cleaned, a `;parameter` dropped from the first segment, any case — so `//.drydock/x`, `/a/../.drydock/x` and `/.drydock;/x` are Drydock's dead end, never the app's (review round 1). The app is the authority on its own paths, and an authorized device may already reach every path it serves.
- **The preview cookie.** The request arrives stripped (step 2's `StripCookie`) and every response header block is filtered: the `1xx`s and the final response by the `CookieGuard` the front door wraps the writer in, and the one the guard cannot see — a `101`, which `ReverseProxy` writes through the hijacked connection — in `ModifyResponse`, which runs for a `101` before the hijack and filters every response besides. Tested with a hostile upstream planting `__Host-drydock-preview` on a `101` and on a `103` beside an app cookie that passes. `StripCookie` removes the UI's `__Host-drydock` too: a browser never sends it to a preview host, so it costs nothing and holds even if the domains were ever collapsed (§10.3).
- **Streaming and upgrades.** `FlushInterval: -1`, and the writer the proxy hands `ReverseProxy` passes `Flush` and `Hijack` through: an SSE event and a chunk of a chunked response reach the device before the app writes the next. An upgrade is relayed both ways; each side is wrapped so a byte either way counts as activity, and a watch on the injected clock closes both after `--preview-idle-timeout` (default 30 minutes, at least a minute) of none. Vite's client pings its socket every 30 s, so a live tab never idles out; the timeout is for the tab a phone abandoned — and is therefore **no bound on a revoked one**: a tab left open on a lost device keeps pinging. So an open upgrade is ended with its preview session (review round 1). The gate attaches its own question to the request (`preview.WithRecheck`: does this cookie's preview session still hold?), and the upgrade's watch asks it again every `RecheckEvery` (30 s), closing both sides the first time it does not — which covers every way a session ends that the proxy is not told of: `drydock passwd` (another process), a disabled or retired port, a stopped workspace, an expiry. And a sign-out is told: `api.SessionRoutes.Revoked` calls `Proxy.CloseWhere` with the auth session it revoked, or every one for *Sign out everywhere*, so those close at once (each upgrade carries `Target.AuthSessionID`). Shutdown closes every open upgrade (`Proxy.Close`), which `http.Server.Shutdown` does not track.
- **The cap (§10.7)** is `preview.Limit` around the whole preview handler, outside the front door: every request counts while it is served — the handshake's redirects, Drydock's own pages, the app's — and an upgraded connection for as long as it is open, since the handler serving it does not return until it closes. Past `--preview-max-connections` (default 512, at least 1) a request is a plain `503` with `Retry-After`, at once rather than queued. 512 because one tab's first page load fans out from a single HTTP/2 connection to Caddy into a few hundred concurrent module requests. **A known limit, recorded rather than fixed (review round 1):** the cap is shared, so one preview can hold all of it — a hostile app open in the owner's own browser opening websockets, or long-polls that never send headers (the transport has no `ResponseHeaderTimeout`, because a dev server's first compile of a page can legitimately take a minute) — and every other preview and every handshake then gets `503`s until that tab closes. It is availability only, it needs the owner to keep the hostile app open, and closing the tab ends it; a per-host sub-cap small enough to protect the others would refuse an honest first page load, whose few hundred concurrent requests all go to one host. The UI's own socket is a different listener and is unaffected.
- **Failure answers (§11)**, each Drydock's own page with `no-store` and `no-referrer`, naming the port and nothing internal — no address, container id or error text: no running container (or one that moved) → `/.drydock/denied`, the same dead end as a stopped workspace's row; refused or unroutable → `502`, *"Nothing is answering on port N … listening on 0.0.0.0 rather than 127.0.0.1?"*; timed out (5 s connect) → `504`, the same sentence *"in time"*; Docker could not be asked → `503` and one journal line with the reason. `CONNECT` is `405` and an upgrade to anything but a websocket `400`: a tunnel is TCP forwarding by another name (§1).
- **Trap 3, closed.** `TestTheProxyIsReachedOnlyThroughEveryGate` drives the real `preview.Proxy` behind `api.PreviewFrontDoor` with a resolver that counts: every route of both muxes, with every gate open but the preview session, or but the preview host, never resolves, dials or reaches the app; with all three open, an API route's path reaches the *app* — it is the app's path on a preview host — and no API handler runs; a `/.drydock/` path never reaches the proxy. `TestPreviewFallbackOrder` and `TestPreviewFrontDoorReachesNoAPIHandler` still pin the order and the mount.
- **Tests.** Unit, in `internal/preview` and `internal/container`: `Host` both ways, the forwarding headers with every spoof dropped, the raw path and query, re-resolution per dial (two "containers" on one port at two addresses, every answer `Connection: close`: the second request dials the new one while the old still answers), the denied page with no dial, the confirm, the three failure pages, a restarted container retried once, SSE and chunked flushing, the `101` and the `103`, the idle watch on a fake clock, `Close`, the cap; `Address`'s argv and every address it refuses. Component (`internal/server`, the real sockets): the whole of it with a real dev server behind the proxy and a canary sweep — the preview cookie and token in nothing the app received, the proxy logged, any file under the temp root (the database and its events included) or any `/proc/*/cmdline` — and the cap counting a redirect behind an open websocket. Container tier (`TestViteHMRThroughThePreviewProxy`): §13 step 3's done-when — a workspace Drydock made from `node:22-bookworm-slim` (the base it already pins), Vite 8.3.2 installed from npm and serving the repository, nothing published, reached through the real preview socket after the handshake: the page, Vite's client and the module; the forwarding headers as Vite saw them; *"Blocked request"* under `passthrough`; an event stream; the HMR socket's `connected`, a custom event's echo, and the hot update for `main.js` edited on the host; then `docker stop` behind Drydock's back and the denied page. Browser tier (`preview.spec.ts`): Chromium through real Caddy loads a real Vite app proxied from this host, its HMR websocket upgrades through Caddy, the tap and the proxy (`101`, no `Set-Cookie`), a custom event goes up and comes back, an edit updates the page in place (a marker on `window` survives, so not a reload), and with the dev server gone and Docker reporting no container the next request is the denied page. The stand-in `docker` (`harness.ts`) answers the proxy's calls with this host's non-loopback address on a bridge network; since a release refuses any address this host holds, the tier builds `drydock` with `-tags browsertier`, whose one difference (`internal/server/localaddrs_browsertier.go`) is that it lists no local address — loopback, gateways and non-bridge networks are refused as in a release, it says so on startup, and `deploy/package.sh` passes no tags (`TestReleaseBuildRefusesLocalAddresses`). Review round 1 added: `Address` refusing the host's own addresses (with fakes, and against this machine's real interfaces), the gateway and every non-bridge driver, and in the container tier an ipvlan container refused beside a default-bridge control (`TestAddressDialsOnlyABridge`); an upgrade closed at the recheck after its session ends, on a fake clock; `CloseWhere` closing one session's socket and not another's; and over the real sockets (`TestRevocationClosesOpenWebsockets`), *Sign out everywhere* closing an open websocket at once and a password change closing one at the next recheck, with another device's sign-out leaving it open; the reserved path read cleaned; the UI cookie stripped. Mutation-checked — each fails its tests: remembering the first resolution, skipping the confirm, `Host` always `passthrough` or never rewritten, the cookie strip a no-op, the `101` unfiltered, the forwarding headers kept or `X-Forwarded-For` passed unexamined, the query cleaned, the cap never refusing or wrapping only the upstream, the idle watch never closing or traffic not counting, `Flush` not passed through, *not running* answered as a `502`, an error string on the failure page, the proxy reached before the cookie check, `Address` accepting loopback or the first of two containers, a non-websocket upgrade let through; and from round 1, `Address` accepting the host's own address, the gateway or any driver, the recheck never closing or never attached, the sign-out hook not called, `CloseWhere` closing every socket, the reserved path read uncleaned, the UI cookie kept.

**What step 4 should know.**

- **Enable writes `forwarded_port.enabled`; nothing else is needed for a preview to work.** The proxy reads the port, scheme and `host_header` per request from the session's row (`preview.Target`), so the registry's `PATCH` applies to the next request. Tests seed rows directly, after `<-srv.reconciled`, and so does the browser tier — with a ULID workspace id now, since `container.Manager` refuses any other.
- **A disabled or retired port's open websockets close within `RecheckEvery` (30 s)** — the recheck finds no session — but not at once. Sign-outs close theirs at once through `Proxy.CloseWhere`; step 4, which owns disable and retire, should do the same as it deletes the port's sessions (`CloseWhere(func(t Target) bool { return t.PortID == id })`). §10.3's `Clear-Site-Data` on disable is step 4's too.
- **The probe endpoint** (`GET …/ports/:port/probe`) is `Address`, a connect and `Confirm`, exactly the proxy's dial — call the same code rather than a second resolver, and give it the same answers.
- **Step 5's scanner** must re-resolve the PID the same way (§8.2): add it to what `addressOf` reads from the one `docker inspect` (`.State.Pid`), never from a remembered one.
- **Step 6's diagnosis** goes where `Proxy.fail` writes its pages; the failure kinds (`ErrNotRunning`, unreachable, timed out, lookup failed) are already told apart there.
- **The real-Safari gate (§13.3) is unchanged and is step 4's**: nothing in production can enable a port yet, so no phone has reached the proxy.

### 13.5 As built — step 4

- **The registry is `preview.Service`** (`registry.go`), the handshake's own type, so a row and the sessions it permits have one owner. `Ports`, `Port`, `Add`, `Update` (`SetEnabled` is `Update` with only the switch), `Retire` and `DeclarePorts` each run in one transaction **with their one event** (`events.Log.Commit`): a row's change and the event that says so are one fact, so commit order is publish order, as for every other writer. An `Update` naming `enabled` writes `port.enabled` or `port.disabled` even when nothing changed — a second device's press still settles — and any other change `port.updated`. The views and events carry the whole row: id, slug, `host` (null with no preview domain), `url` (only while enabled), label, `host_header`, the switches and the provenance flags; never an address.
- **Off until enabled, structurally.** A row is born `enabled = 0` — added by hand (`manual`) or declared — and nothing but `Update` writes the switch. `DeclarePorts` has no path to it: what a configuration (the container's to write) declares is listed, never exposed (§10.7, §12).
- **Disable and retire end the port's previews at once.** The port's `preview_session` rows go in the same transaction (step 2's rule), and after the commit `Service.Revoked` is told the port's id; the server wires it to `Proxy.CloseWhere(func(t Target) bool { return t.PortID == id })`, so an open HMR websocket closes then and there rather than at its next 30-second recheck. Measured both ways: over the real sockets with the recheck an hour away (`TestPortRegistryOverTheSockets`), and in Chromium through real Caddy, where the page's websocket closes within the test's 15 s with the recheck at its 30 s default. Two races are closed rather than left to that recheck (review round 1): `StartSession` writes the preview session in one `INSERT … SELECT` that requires the port still enabled, unretired and on a running workspace, so a disable committed between its checks and its insert leaves no session (`TestADisableDuringStartSessionLeavesNoSession`); and an upgrade asks its session once as soon as it registers, so one the gate passed before a disable but that registered after `CloseWhere` closes at once (`TestAnUpgradeRevokedBeforeItRegisteredClosesAtOnce`).
- **`Clear-Site-Data` on the way back (§10.3).** A device returning to a switched-off or retired port's host is cleared by its own host's `/.drydock/session`, through a single-use token authorize mints for a signed-in device only — see §10.3's *as built* for why there and only there.
- **A slug is minted once.** `MintSlug` is `<repo>-<port>-<4 of [a-z0-9]>`, the repository's short name lowercased with each run of anything else made one hyphen, shortened to keep the label within 63 characters, `port` when nothing survives. The insert runs under a savepoint and, refused by the `UNIQUE` on `slug` — which covers retired rows — draws again, at most eight times, and then fails rather than reuse anything (§11's *slug collision*). Retire sets `retired_at` and keeps the row, so the port listed again is a new row with a new slug.
- **Declared ports come from step 3.** `container.DeclaredPorts` reads the resolved configuration and the merged one (a Feature's or the image's declaration counts): `forwardPorts` — a number, a string of digits, or `localhost:N`; `service:N` names another Compose service and is skipped — then `appPort`'s container half (`"8080:3000"` is 3000, `/udp` skipped), each port once, labelled from an exact-number `portsAttributes` key, the repository's own label over a Feature's. Malformed entries are skipped, never refused: a declaration is only a row. `provision`'s step 3 hands them to `Provisioner.DeclarePorts` once the configuration is cleared to run (never for one stopped for an approval), and a registry that cannot write them is logged, never the step's failure. `DeclarePorts` adds a newly declared port, marks a listed one declared (and labels it if it had none), and takes the flag off a port no longer declared — retiring it if nothing else holds it (not enabled, not manual, not observed), so a declaration that comes back gets a new slug. At most 32 declared ports, 64 rows in all.
- **The probe is the proxy's dial.** `Proxy.Probe` calls `dial` — the same function the transport dials with: `Resolver.Resolve`, the connect, `Resolver.Confirm`, the one retry for a moved container — and closes the connection unused. Its outcomes (`answering`, `not_running`, `refused`, `timed_out`, `lookup_failed`) are the ones `Proxy.fail` already told apart, and each sentence is `OutcomeSentence`, the same function the failure pages print, so the panel and a preview tab never disagree. Any live row may be probed, enabled or not; a probe connects and shows nothing the port serves. The server's route reaches the proxy itself (`proxyProber`), read per request, so it is the resolver a test configures — which is how `TestPortRegistryOverTheSockets` proves there is no second one.
- **The UI** is `components/PortsPanel.vue` on the workspace's page, under its card (frontend §6.3): each row's number, label and provenance (*declared*, *added by hand*); *Not previewed* and **Preview this port** while off; the full preview host as a link opening a new tab (`noopener noreferrer`) and **Turn off preview** while on; under **More**, the `Host` the dev server is sent with a switch (**Send the preview host instead** / **Send localhost again**), **Check the port**, **Hide from the list**, and **Remove…** behind a confirm that says the address is retired for good; an add-by-number form that refuses a bad or listed number before sending; and, with no preview domain, a sentence instead of any switch. The panel says a sign-in form on a preview is not Drydock's (§10.5). Its state is the reducer's: `ports` (by row id), versioned per row by event id; a `port.retired` tombstone, so neither a late event nor an older list revives a row; the list (`GET …?hidden=true`, so "show hidden" is a filter) the only input that drops a live row, as the workspace list is for workspaces; `workspace.gone` taking a workspace's rows. Each switch is an `ActionButton`, in flight until its event — never the `202` — or a later snapshot showing it over, by one predicate (`OVER_PORT`) for both paths. The mock backend serves the same routes with the same refusals and events.
- **Tests.** Unit (`internal/preview`): off until enabled, one event per call, enable refused with no domain and on a deleting workspace while a disable is not, `Revoked` told of every disable and retire and of no enable or hide, the declared sync (labels, idempotence, an enabled or hand-added port kept when its declaration goes, a bare declared one retired, the 32 cap, a deleting workspace untouched), `MintSlug`, a retired slug never reissued even when the random draw lands on it, and the probe through a counting resolver — answering at the resolved address, moved after the connect, no container and no dial, the restarted container's retry, and refused, timed out and Docker-unreachable with the proxy's own sentences. `internal/container`: `DeclaredPorts` over every shape. `internal/provision`: step 3 hands the declaration over and a failing registry fails nothing. `internal/api`: each route's body rules and refusals over a real registry, the rescan's gated `501`, and the clearing token — `403`, `Clear-Site-Data`, the denied body, no cookie, spent after one use — for a port born off, one disabled and one retired, with a stopped workspace's enabled port as the control that clears nothing; the table meta-tests enumerate the six new routes with no edit. Component (`internal/server`, the real sockets): enable, the handshake and an HMR socket, the probe through the proxy's resolver, disable closing the socket at once, the next load cleared, retire, and the port listed again on a new slug. Vitest: the reducer's port cases and the panel against the mock — enable held in flight past its `202` until the event is delivered, the link's target and `rel`, another device's change arriving by event alone, add, remove, probe, `Host` and hide, previews off. Browser tier (`preview.spec.ts`): Chromium through real Caddy enables a port from the panel, opens it in a new tab onto real Vite and its HMR socket, disables it, sees the socket close at once, and on the next load lands on the cleared denied page with what the app stored in `localStorage` gone. Mutation-checked — each fails its tests: `Revoked` not called on a disable (the socket stays open over the sockets and in Chromium), the probe through a second resolver of its own (the counts and the moved container), `Retire` deleting the row (the reissue test draws the spent slug), the 202 ending the mark (the panel spec), `Clear-Site-Data` not sent (the API test and Chromium's `localStorage`); and from review round 1, `"cookies"` added back to it (the API test, and Chromium wiping another preview host's cookie), the session insert not conditioned on the port (a disable landed in between leaves a session), and no recheck at registration (a late-registered upgrade stays open).

**What step 5 should know.**

- **Discovery writes `observed`, `bind_addr`, `observed_state` and the two timestamps, and nothing else** — never `enabled` (§10.7). Add it as a `preview.Service` method beside `DeclarePorts`, in one `events.Commit`; the merge onto a declared or hand-added row is by `container_port` on the live index, as `DeclarePorts` does it. §6 says discovery emits no event of its own: keep `port.added` for a row the scan creates only if the panel must learn of it live (it would, open on screen — decide it), and never anything that notifies.
- **`DeclarePorts` retires a declared row nothing else holds; `observed` is one of the things that hold one.** Once the scanner sets it, a port that is both declared and listening survives its declaration going, as an enabled one does.
- **The PID comes from the same `docker inspect` the dial reads** (§13.4's note): add `.State.Pid` to what `addressOf` returns, re-resolved per scan, never remembered.
- **`POST …/rescan` is in the table and answers `501`.** Write its handler into `api.PortRoutes`; the meta-tests already cover it. The ambient count on the card (*"4 listening · 1 previewed"*) needs the workspace list to carry per-workspace counts, or the home view to fetch each running workspace's ports: decide which, since the reducer's `portsLoaded` is per workspace today.
- **The panel's provenance badges already render *observed*,** and the loopback row (§8.2: listed, greyed, no switch, the `--host 0.0.0.0` sentence) is step 6's diagnosis; `bind_addr` is in the view for it.

### 13.6 As built — step 5

- **The read is `container.Manager.Listeners`** (`internal/container/listeners.go`): `Address` — `docker ps` for the one running container with the workspace's label, `docker inspect` of it, `docker network inspect` for its bridge — whose inspect now also carries `.State.Pid` (`Address.Pid`; `Confirm` still compares only the container and its address, so the proxy's dial is unchanged); then `<ProcRoot>/<pid>/net/tcp` and `tcp6`, parsed by `ParseNetTCP` (state `0A` only, each address once, every 32-bit word in host byte order, the port in network order; a malformed line or a header that is not the kernel's is an error, never a partial table; more than 4,096 listeners, or 32 MiB of table, is refused rather than cut); then `docker inspect` of the same container again, and the read is `ErrMoved` unless it is still running under the same PID — a container that died in between gave its PID back to the kernel, and what was read may be another process's namespace. The PID is resolved on every call and kept nowhere. A container with no bridge address of its own (`--network=host`, `none`, `container:`) is `ErrNoAddress` and never read: its namespace is the host's or another's, and listing the host's listeners as a workspace's would offer the host for preview. Four docker calls per running workspace per scan. Measured in the container tier: `/proc/<pid>/net/tcp` of a container's root process is readable by the unprivileged user, as §8.2 says.
- **The scanner is `preview.Scanner`** (`internal/preview/discover.go`), a `life.Coalescer` in `Serve`'s `life.Group` (`work.Child("discovery")`, stopped and waited for before the database closes): one round at once, every `DefaultScanInterval` (5 s), and one for each `Rescan`. A round scans every running workspace, every workspace with a live row still `listening` (a stopped one is read as empty, so its rows go — §8.2's *scanned as empty, never skipped with stale rows*), and every one asked about; never one being deleted. Its memory is per workspace, per port: the bind address a streak is at, the streak, and the last scan that saw it — never a PID, never an address. A port appears after **two consecutive scans at one bind address** (`AppearAfter`), a bind change likewise; it goes after **15 s unseen** (`DefaultGrace`), and a row the registry lists as listening that the scanner has no memory of (Drydock restarted) is given its grace from then. Only a change reaches the database: a port already listed listening at that bind writes nothing, so a quiet workspace costs no transaction. Each port's one bind address is the widest of its sockets — `0.0.0.0`, then `::`, then any other, then loopback — so a port is loopback only when every socket on it is. The source's errors: no running container is nothing listening; a read that raced a restart (`ErrScanRaced`) teaches nothing; anything else (Docker, `/proc`, a host-network container, two containers) is **discovery unavailable**, which changes no row however long it lasts, answers a rescan `unavailable`, and is logged once as it breaks and once as it recovers.
- **The merge is `Service.Observe`**, one `events.Commit` per workspace per scan that found a change, by `container_port` onto **live rows only**: a listening port a row names is marked `observed`, `listening`, its `bind_addr`, `first_seen_at` (once) and `last_seen_at` (`port.updated`); one no row names is a new row, `observed`, **off**, a freshly minted slug (`port.added`) — so a port the operator removed while it still listens comes back as a new row with a new slug, and a retired row is never revived; a port gone is marked `gone`, `last_seen_at` the last scan that saw it (`port.updated`), when something else holds the row — enabled, added by hand, declared or hidden (hidden, so a row the operator muted does not come back visible) — and is otherwise retired (`port.retired`, `Revoked` told as for any retire). Every event carries `data.source: "discovery"`. **`Observe` has no path to `enabled`.** At most 32 rows discovery alone holds per workspace (`MaxObserved`), within the 64 of `MaxPorts`. `DeclarePorts`' retire rule changed to match: a row whose declaration goes is kept while it is enabled, hand-added, hidden or **listening now** (it was "ever observed"), and becomes discovery's — retired when it stops.
- **The loopback classification is `Port.loopback`**, derived from `bind_addr` in every view and event (a mapped `::ffff:127.0.0.1` included), never stored. The panel says it — *"Listening on 127.0.0.1 only, which nothing outside the container can reach."* — and still offers the switch: greying the row, withholding the switch and the `--host 0.0.0.0` sentence are step 6's.
- **`POST …/rescan`** is `api.PortRoutes.rescan`: `Scanner.Rescan` checks the workspace (`not_found`, `in_progress` while deleting) and asks for a round that begins after the request (`TriggerWith(workspaceID)`); `202 {}`; settled by the `port.scanned` that round writes for that workspace. `503 unavailable` once the scanner's group is stopping. A rescan is one scan like any other — it does not bypass the debounce, so a server started a moment ago may take the next interval to appear.
- **The UI.** The reducer reads `bind_addr`, `loopback` and `observed_state` into the port entity and applies discovery's events as any `port.*` (a `port.scanned` changes nothing). The panel badges a found row *discovered*, says *Listening on 0.0.0.0.*, the loopback sentence, or *Not listening now.*, and offers **Look for listening ports now** while the workspace runs (an `ActionButton`, settled by `settlesRescan`: that workspace's `port.scanned` newer than the press, or its going). **The ambient count** is `components/PortCount.vue` on the card, home and detail alike: *"3 listening · 1 previewed"* in the card's resource-line type, no button, no link, nothing else that moves. *Decided:* the home view loads each running workspace's port list (`GET …/ports?hidden=true`, the same snapshot the panel takes) rather than the workspace list carrying counts — one source for the rows, so the count is counted from entities the stream keeps current and can never disagree with the panel; the cost is one request per running workspace on a cold home view, which the cap bounds. Hidden rows do not count as listening.
- **The mock** (`scanPorts`) runs the same round over `sockets`, with the grace as three scans (15 s at 5 s), and is held to the server by a golden file: `TestDiscoveryEventsGolden` records the events — kind, level, the data's keys, the row's fields and which scan wrote each — of one scenario through the real scanner and registry in `internal/preview/testdata/discovery-events.json`, and `web/src/mocks/discovery.spec.ts` replays it on the mock and demands the same.
- **Tests.** `internal/container`: `ParseNetTCP` over a synthetic table (listeners, an established and a `TIME_WAIT` socket, a `SO_REUSEPORT` duplicate, loopback, wildcard, IPv6, mapped and global) and one the kernel wrote (four `python3 -m http.server`s at 127.0.0.1, 0.0.0.0, `::` and `::1` beside established connections), every malformed shape refused; `Listeners` following a restart's new PID against a fake `/proc`, throwing away a read the PID changed under or whose process is gone, and never reading a host-network container's table; the argv. `internal/preview`: off and never enabled (an enabled row seen stays enabled, a new one is off), the debounce (one scan lists nothing; a restart inside the grace writes nothing; a bind change takes two scans; gone after the grace), the merge onto a declared row (one row, both flags, the label kept, kept through its declaration going while it listens), a retired discovered port coming back with a new slug even when the random draw lands on the spent one, what holds a row and `MaxObserved`, a stopped workspace and a vanished container scanned as empty and a stopped one never asked of Docker, an unreadable table and a raced read changing nothing (logged once each way), `Rescan` answered by `port.scanned` and refused unknown, deleting and stopped, every discovery event `info` with `source: "discovery"`, and the golden file. `internal/api`: the rescan route's `202`, its refusals, and `501` with no scanner. Component (`internal/server`, the real sockets): two rescans over the API socket, each settled by its `port.scanned`, list an undeclared server and a loopback one, off, classified; a rescan with no `Origin` refused; a stopped workspace never read. Container tier (`TestDiscoveryReadsARealContainer`): a container serving on an undeclared `0.0.0.0:8000` and on `127.0.0.1:9229`, read from the host and listed by the real scanner, off, the second loopback; `docker restart` and the new PID followed; a host-network container never read. Vitest: the count's rules; the reducer's discovery cases and `settlesRescan`; the mock against the golden file; and in the app, the done-when — a server started with the panel open is absent after one scan, then a row, off, *discovered*, *Listening on 0.0.0.0.*, the loopback one said so, the declared row merged — with no alert, no `aria-live` change, no title change and no press in flight; a stopped server *Not listening now.* and a discovery-only row gone after the grace; the rescan in flight until its `port.scanned`, not another workspace's; and the home card's count as passive text. Mutation-checked — each fails its tests: a PID cached per workspace in `Listeners` (the restart reads the old table), the after-read PID check removed (the raced read is returned), `Observe` inserting a row enabled (the never-enabled test and the golden file), `Observe` reviving a retired row (the new-slug test), `AppearAfter` 1 and a zero grace (the debounce test), and in the mock, listing after one scan, dropping after one and a row born enabled (the golden replay and the app specs).

**What step 6 should know.**

- **The bind address is already in every view and event**: `bind_addr`, `loopback`, `observed_state` and `last_seen_at`. §11's first row is a function of them — a `loopback` row listening: greyed, no switch, the `--host 0.0.0.0` sentence, and the dial never attempted (the proxy and the probe should refuse a loopback-only port before `Address`, not after a refused connect). Today the panel says the loopback sentence and still offers **Preview this port**; removing the switch, and refusing an enable of a loopback-only port in `Service.Update`, are step 6's decisions.
- **"Discovery unavailable" is known but not shown.** A workspace whose table cannot be read keeps its rows and answers a rescan `port.scanned {discovery: "unavailable"}`; the scanner holds the reason in memory per workspace (`wsTrack.unavailable`) and logs it once. §11 wants the panel badged *discovery unavailable*: the state is there to expose — in the port list's body, say, beside `previews` — but nothing carries it to the UI yet, and a host-network workspace (the container tier's own repositories use `--network=host`) is permanently unavailable.
- **`observed_state = gone` is §11's *"Port enabled, nothing listening"***: an enabled row is kept, marked gone, with `last_seen_at`. The proxy's `502` page and the probe could say *"Nothing has listened on port N since …"* from it without a dial.
- **No denylist.** §8.2's *suppress Drydock's own ports and a small known-noise denylist* found nothing to suppress (Drydock listens on no TCP port in a container); if a real one appears — a language server, an MCP server — add it beside `bindsByPort`, never as a hidden row, which the operator owns.
- **The cost is four docker calls per running workspace per scan**, sequential, every 5 s. One `docker ps` for every workspace's label at once and one batched `docker inspect` would make it two per round whatever the count, but would be a second resolution path beside `Address`; do it only with the same per-container PID check after the read.
- **The debounce is in memory.** A Drydock restart forgets every streak: a port listed listening keeps its row, and one not yet listed takes two scans again.

## 14. Open questions

### 14.1 Still open

1. **Does the cross-site boundary hold end to end?** With previews on a separate registrable domain, `SameSite=Lax` should refuse a preview-origin state-changing `POST` to `/api/*` before the `Origin` check is even reached — worth confirming in a real browser rather than assuming, because it depends on the cookie attribute *and* the domain split both being right. The test: from a preview origin, (a) a state-changing `POST` to `/api/*` is refused and logged; (b) it is still refused with `Origin` stripped (the belt behind `SameSite`); and (c) a data `GET` cannot be *read* cross-origin (no reflected CORS). Cheap, and it catches a misconfigured cookie or a preview domain that accidentally shares a registrable suffix with the UI.
2. ~~**Does `host_header: passthrough` want to be the default?** It is the correct behavior for apps that generate absolute URLs and the wrong one for apps with strict host allowlists, and the second group is growing. The answer is one afternoon of pointing it at the repos actually in the installation.~~ **Decided** — `localhost` is the default, `passthrough` a per-port switch (§8.3, §14.3).
3. **What actually belongs on the discovery denylist?** §8.2 asserts that a workspace's socket table is mostly noise, which is true, but the specific noise is an empirical question — the remote-control process is certain, MCP servers and language servers are likely, and the rest is guesswork until a real workspace has been running for a week. Ship the `hidden` flag first and let the denylist be whatever people keep hiding. Getting this wrong is cosmetic, which is why it is not worth designing in advance.

### 14.2 Deferred, and what would reopen each

| Deferred | Reopen when |
|---|---|
| **Raw TCP forwarding.** | Never, on this design. It would reopen §13.5's first bullet. If it is genuinely needed the answer is Tailscale to the host, not a Drydock feature. |
| **Sharing a preview with someone else.** | A second operator exists — at which point §1's "Operators: 1" is what actually needs revisiting, and this follows from it rather than leading. |
| **Auto-enabling declared ports.** | The one-click enable proves to be friction you resent, measured in actual clicks rather than anticipated ones. The row already carries `declared` / `observed` / `manual` separately, so the switch is a default change rather than a migration — and it should only ever apply to *declared* ports, never observed ones (§12). |
| **Process attribution on discovered ports** — showing "vite (node)" rather than a bare number. | Bare port numbers prove genuinely ambiguous in practice. It is available from a sidecar sharing both namespaces (`--network=container:<id> --pid=container:<id>`, then `ss -ltnp`), but that costs a container spawn, so it belongs on panel-open rather than on the poll — an enrichment, never the scan itself. |
| **Previewing a stopped workspace** by starting it on demand. | Opening a bookmark to a stopped workspace becomes a common enough annoyance to be worth the surprise of a container starting because you clicked a link. |

### 14.3 Decided while writing this

| Question | Answer |
|---|---|
| Path prefix, same-domain subdomain, or separate domain? | **Separate-domain subdomain.** A path prefix puts repo code on the API's origin; a same-domain subdomain keeps it same-site, so `SameSite` stops separating it from `/api/*` (§4, §10.2). A wildcard on a separate registrable domain is the only option that is both origin-isolated and same-site-isolated from the control plane. |
| One preview cookie or one per host? | **One per host.** Host-only cookies are what stop preview A reading preview B, and the extra redirect is invisible (§10.4). |
| Should Caddy route to workspaces? | **No.** It gets a wildcard and a socket. Teaching the front door the workspace map would make it stateful and couple it to Drydock, against §3.1. |
| Publish container ports on the host? | **No.** Drydock dials the container's Docker-network address from the host. Publishing would put listeners on the dev server's interfaces, which is the thing §13.5 exists to prevent. |
| Discover ports with an in-container agent, like VS Code does? | **No — read the netns from the host.** VS Code can afford an agent because it already runs a server inside the container. Drydock does not, and `/proc/<pid>/net/tcp` gives the same answer as an unprivileged file read: no exec, no image dependency, no cost per poll, and the container stays unaware it is being previewed (§8.2). |
| Should a newly discovered port notify the operator? | **No.** Ambient count on the card, decisions in the panel. A prompt that fires whenever a test run opens a socket trains a click-through reflex — the same argument §13.5 of the overall document uses to refuse a re-auth prompt on delete (§8.2). |
| Which `Host` does the upstream see by default? *(owner, 8 October 2026)* | **`localhost:<port>`**, with `passthrough` a per-port toggle (§5's `host_header`, §8.3). Strict host allowlists are the growing group, and an app that rejects the preview host on first open reads as a Drydock bug; `X-Forwarded-Host` still carries the preview host for frameworks that honour it. Was §14.1's second question. |
| Who provides the preview domain, its certificate and its DNS? *(owner, 8 October 2026)* | **The operator.** A second registrable domain, a wildcard certificate for it and a LAN wildcard record pointing at Caddy are the operator's infrastructure, provisioned like the UI's certificate; Drydock never issues a certificate (§1's out-of-scope line, §9). The repository names no real domain: tests use `drydock-preview.test`. |
| Do the product defaults stand? *(owner, 8 October 2026)* | **Yes, as designed.** Ports are off until enabled; discovered ports are listed but never auto-exposed and never notified (§8.2, §12); there is no sharing (§12, §14.2); HTTP and websockets only (§1). |
| Delete a `forwarded_port` row, or retire it? | **Retire it.** A deleted row frees its slug, and a reissued slug makes a stale bookmark resolve to a different workspace rather than failing closed (§4). Soft-deleting costs one column and a partial index, and it moves that guarantee from the odds into the schema. |

---

*Supplements `docs/design/overall/drydock-design.md` (written against its draft v5). Previews are served from a separate registrable domain, which keeps them cross-site with the UI; the one change that pushes into the parent document is the session cookie moving from `SameSite=Strict` to `Lax` (overall §13.2) so the cross-site authorize redirect still carries the session, with the `Origin` allowlist kept as belt-and-braces (§13.3, §13.5).*
