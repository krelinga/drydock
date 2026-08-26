# Drydock port forwarding

*Reaching a dev server running inside a workspace container from a phone or tablet on the LAN — without giving repository code a foothold on Drydock's own origin.*

**Status** design document, draft v1 · **Date** 26 August 2026

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
- **Explicit.** A port is reachable because someone enabled it, not because something was listening.
- **Cheap to reason about.** No new listener on the LAN, no new credential, no new trust boundary.

### Non-goals

- **Not a general reverse proxy.** The only upstream a preview can reach is *this workspace's container, on a port enabled for it*. There is no operator-supplied host field, because that field is server-side request forgery with a nicer name.
- **Not internet-facing.** Same answer as §13.1 of the overall design: Tailscale in front of the same Caddy, or nothing.
- **Not TCP forwarding.** HTTP and websockets only. Postgres on a phone is not the use case, and a raw TCP path would need a listener this design is built to avoid (§12).
- **Not a replacement for `devcontainer exec`.** Debugging a server that will not start is a terminal job.
- **Not a certificate manager.** Unchanged from §13.1 — but this design *does* add a requirement to the result, and §9 states it plainly.

## 2. What the base design fixes before we start

Four decisions in the overall document constrain this one, and it is worth naming them up front because three of them turn out to be load-bearing in ways their original authors did not need to care about.

| Constraint | Source | Consequence here |
|---|---|---|
| **Drydock binds no TCP port.** | §13.5 | The preview proxy is not allowed to open a listener either. It gets a second Unix socket (§3). |
| **Caddy is a dumb front door that knows nothing about Drydock.** | §3.1 | Caddy must not learn the workspace→port→container mapping. It forwards a whole wildcard to a socket and Drydock does the routing. |
| **`__Host-drydock`, `Secure`, `HttpOnly`, `SameSite=Strict`.** | §13.2 | The `__Host-` prefix was chosen because "a subdomain cannot set it". That property stops being a free win and becomes the thing preventing a hostile preview from performing cookie fixation on the UI (§10). |
| **`Origin` allowlist on every state-changing route.** | §13.3 | Described there as ten lines of belt-and-braces behind `SameSite`. Once previews exist it is promoted to the *primary* CSRF defense, because previews are same-site with the UI (§10). |

> [!WARNING]
> **The one that will surprise you**
>
> `preview.drydock.example.com` and `drydock.example.com` share a registrable domain, so a preview origin is **same-site** with the Drydock UI. `SameSite=Strict` therefore does *not* stop a previewed app from issuing authenticated requests to `/api/*` — the browser considers them same-site and attaches the cookie. CORS still prevents the app from *reading* the responses, but a state-changing `POST` does not need its response read.
>
> The `Origin` check in §13.3 is what stops this, and after this document ships it is no longer optional hardening. §10.2 makes that explicit and §13 of the overall document should be amended to say so.

## 3. Architecture — a second socket, not a second listener

The whole design is one idea: **extend socket-as-identity to the front door.** The overall design already uses "which socket did this arrive on" to decide which repository a broker request may touch (§9.2). The same trick answers "is this a control-plane request or untrusted repo content?"

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/01-two-sockets-dark.svg">
  <img alt="A LAN device reaches Caddy over TLS on two hostnames. Caddy holds two site blocks — one for the UI hostname, one for a preview wildcard — and forwards each to a different Unix socket. Inside the single Drydock process those sockets are served by two entirely separate muxes: the API mux, guarded by the session cookie and an Origin allowlist, and the preview mux, which holds only the proxy and no API route at all. The preview mux dials the workspace container's dev server directly over the Docker network; nothing is published on the host." src="diagrams/01-two-sockets-light.svg" width="100%">
</picture>

**Fig 1** — *Two Caddy site blocks, two Unix sockets, two `http.Server`s, two muxes, one process. Caddy still knows nothing: one block matches the UI hostname, the other a wildcard, and each forwards everything it receives to a socket. The separation is a file descriptor rather than a branch in a handler — which is the point, because the two origins are same-site and the browser will not separate them for you.*

The property that makes this worth the extra socket: **a preview request cannot reach an API handler even if the Host-parsing logic is wrong.** They are not routes in the same mux behind a branch — they are different servers on different files. §13.5's "auth is middleware around the whole mux, so a route added later is protected by forgetting to think about it" continues to hold, once per mux, rather than becoming "protected unless someone adds a route on the wrong side of an `if`."

### 3.1 Component responsibilities

Extending the table in §3.1 of the overall document:

| Component | Owns | Explicitly does not |
|---|---|---|
| **Preview proxy** | The `preview.sock` listener, host→port resolution, the upstream dial, websocket upgrade, preview session validation. | Serve any API route. Accept an operator-supplied upstream. Hold any credential. |
| **Port registry** | `forwarded_port` rows, discovery from the resolved devcontainer config, enable/disable. | Decide reachability on its own — a row is a *permission*, the workspace still has to be running. |
| **Container manager** *(extended)* | Resolving a workspace's current container IP, and invalidating that on every state transition. | Publish ports on the host. Nothing is bound on the dev server's network interfaces. |

Note what is *not* here: no change to the container. No agent, no injected feature, no published port, no `docker run -p`. Drydock reaches the container the way it already reaches everything else — from the host, over the Docker network — and the container never learns it is being previewed.

## 4. Naming and routing

One hostname per (workspace, port), from a wildcard:

```
https://<slug>.preview.drydock.example.com
        └─ e.g.  drydock-3000-k4x9  /  myapp-5173-p2mq
```

`slug` is `<sanitized-repo>-<container_port>-<4 random chars>`, generated once and stored on the `forwarded_port` row. It is readable enough to tell two open tabs apart, unique across workspaces on the same repo, and stable for the life of the row — a bookmark keeps working across container restarts and rebuilds.

The random suffix is not a security control. Previews are authenticated; it is there so that deleting and re-adding a port produces a *different* URL, which means a stale bookmark fails closed rather than silently landing on whatever now occupies port 3000.

| Alternative | Why not |
|---|---|
| **Path prefix** `drydock.example.com/preview/<ws>/<port>/` | Same origin as the UI. Repo code gets the session cookie and full API access. Also breaks every app that emits absolute asset paths. Disqualified on the first point alone. |
| **Port per preview** `drydock.example.com:3001` | Requires a listener per preview, which §13.5 forbids, and a cert that covers nothing new. |
| **Separate registrable domain** `*.drydock-preview.net` | **Strictly better security** — restores cross-site status so `SameSite` blocks preview→API by itself. Rejected only because it costs a second domain to own and renew. §14 records what would reopen it. |
| **Fixed SAN pool** `preview1..8.drydock.example.com` | Avoids DNS-01, at the cost of a hard ceiling and URLs that are not stable across restarts. The fallback if wildcard issuance ever becomes unavailable. |

## 5. Data model

One new table, plus one that exists to make revocation work.

```sql
-- A permission to reach one port on one workspace. Default-deny: no row, no preview.
forwarded_port(
  id TEXT PRIMARY KEY,               -- ULID
  workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
  container_port INTEGER NOT NULL,
  slug TEXT NOT NULL UNIQUE,         -- the DNS label; stable for the life of the row
  label TEXT,                        -- "vite dev server"
  upstream_scheme TEXT NOT NULL DEFAULT 'http',   -- http | https (rare; self-signed upstreams)
  host_header TEXT NOT NULL DEFAULT 'passthrough',-- passthrough | localhost  (§8.2)
  enabled INTEGER NOT NULL DEFAULT 0,
  discovered_from TEXT,              -- forwardPorts | appPort | manual
  created_at TEXT, last_used_at TEXT,
  UNIQUE(workspace_id, container_port)
)

-- A device's proof that it may view previews. Dies with the session that minted it.
preview_session(
  id TEXT PRIMARY KEY,               -- sha256 of the preview cookie value
  auth_session_id TEXT NOT NULL REFERENCES auth_session(id) ON DELETE CASCADE,
  preview_host TEXT NOT NULL,        -- host-only: this cookie is good for one preview
  created_at TEXT, last_seen_at TEXT
)
```

Three notes, in the spirit of §4's "what is deliberately absent".

There is **no upstream host column** — only a port. The upstream address is always derived from the workspace's own container, so there is no value an operator or an attacker could write that would make the proxy dial somewhere else.

`preview_session.auth_session_id` is a **cascading foreign key on purpose**. §13.2 promises that one button kills every session; without the cascade that promise would quietly stop covering previews, which are the most likely thing to be left open on a device you no longer have.

The row is **per preview host, not per device**. A single cookie covering `.preview.drydock.example.com` would let a previewed app fetch every other preview on the same device. Host-only cookies cost one extra redirect per preview, and §7 explains why that redirect is invisible.

## 6. API surface

Additions to §5 of the overall document. All on the API mux, all behind the session cookie and the `Origin` check.

| Method & path | Does | Returns |
|---|---|---|
| `GET /api/workspaces/:id/ports` | Every known port: discovered and manual, enabled and not, each with its URL and last reachability result. | Port list |
| `POST /api/workspaces/:id/ports` | Add a port. Body: `container_port`, optional `label`, `upstream_scheme`, `host_header`. Mints the slug. | `201` + port |
| `PATCH /api/workspaces/:id/ports/:port` | Enable, disable, or relabel. Enabling is the click that makes a URL live. | `200` + port |
| `DELETE /api/workspaces/:id/ports/:port` | Remove it. The slug is not reused. | `204` |
| `GET /api/workspaces/:id/ports/:port/probe` | Dial it now and report what happened, with the §11 diagnosis attached. | Probe result |
| `GET /preview/authorize` | The main-origin half of the handshake in §7. Query: `return`. Requires a session. | `302` |

On the **preview mux**, and nowhere else, two routes under a reserved prefix:

| Method & path | Does |
|---|---|
| `GET /.drydock/session` | Consume the one-time token, set the host-only preview cookie, redirect to the originally requested path. |
| `GET /.drydock/denied` | The human-readable dead end: not signed in, port disabled, workspace stopped, or upstream refused. |

`/.drydock/*` is the only path the preview mux handles itself; everything else is proxied verbatim. A repo that genuinely serves something at `/.drydock/` loses that path, which is a trade worth making once and documenting.

New events on the existing SSE stream: `port.enabled`, `port.disabled`, `port.unreachable`.

## 7. Preview authentication

The requirement is that a preview is behind the same sign-in as everything else, and the obstacle is that a cookie set for `drydock.example.com` is host-only and therefore never sent to a preview host. So each preview host needs its own cookie, and the only origin that can prove you are signed in is the main one.

1. Browser requests `https://myapp-5173-p2mq.preview.drydock.example.com/`. No preview cookie.
2. Preview mux redirects to `https://drydock.example.com/preview/authorize?return=<the original URL>`.
3. **The API cookie is sent on that navigation.** The two hosts share a registrable domain, so this is a same-site navigation and `SameSite=Strict` permits it — the same fact that creates the risk in §10.2 is what makes this handshake work at all.
4. `/preview/authorize` validates the session, checks the slug resolves to an enabled port on a running workspace, and mints a **single-use token, 60-second TTL, bound to that `auth_session.id` and that one preview host**.
5. Redirect to `https://myapp-5173-p2mq.preview.../.drydock/session?t=<token>`.
6. Preview mux consumes the token, writes `preview_session`, sets a host-only cookie (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`), and redirects to the path from step 1.

Steps 2–6 are four redirects with no user interaction, so in practice the first request to a new preview host renders the app. If you are not signed in, step 3 lands on the ordinary sign-in page and `return` carries you back afterwards.

> [!WARNING]
> **Strip the preview cookie before proxying**
>
> The browser attaches the preview cookie to every request to that host, including the ones the proxy forwards upstream. The container must never see it: a dev server that logs headers would write a live credential to a file, and a hostile one would simply exfiltrate it. **The proxy removes its own cookie from `Cookie` before dialing, and removes any `Set-Cookie` from upstream that tries to claim the same name.** Everything else passes through untouched, because the app's own cookies are the app's business.

`SameSite=Lax` rather than `Strict` on the preview cookie is deliberate and narrow: `Strict` would drop the cookie on the step-6 redirect, and a preview cookie is a read-only capability to view one app, not an API credential.

## 8. Reaching the container

### 8.1 Resolving the upstream

Drydock runs on the host, not in a container (§1, *not containerized itself*), so it can dial the container's address on the Docker network directly. There is nothing to publish and nothing bound on the dev server's interfaces.

The address comes from the container's `NetworkSettings`, looked up by the same `drydock.workspace=<id>` label that everything else uses. **Container IPs change on restart**, so the resolved address is a cache with exactly one invalidation rule: any workspace state transition clears it. This is the same posture as §6's reconciliation — *Docker is the truth, the database is the cache* — applied to one more field.

A dial is attempted only when the workspace is `running` and the port row is `enabled`. Either being false is a `/.drydock/denied` page naming which one, not a 502.

### 8.2 The `Host` header

Dev servers increasingly reject unexpected `Host` values — Vite's `server.allowedHosts`, Rails' `config.hosts`, Django's `ALLOWED_HOSTS`. Two behaviors, per port, because neither is right for everything:

| `host_header` | Sends upstream | Use when |
|---|---|---|
| `passthrough` *(default)* | The preview hostname | The app builds absolute URLs from `Host` and you want them to work. Requires adding the preview host to the app's allowlist. |
| `localhost` | `localhost:<port>` | The app's host allowlist cannot be changed and it does not generate absolute URLs. |

`X-Forwarded-Proto: https`, `X-Forwarded-Host: <preview host>`, and `X-Forwarded-For` are always set, so a framework that honors them produces correct absolute URLs even under `localhost`.

### 8.3 Websockets and streaming

HMR is the whole point of previewing a dev server, so `Upgrade` must survive the proxy, and the Caddy block needs `flush_interval -1` for the same reason the API block already has it (§13.1). Server-sent events from a previewed app work for free once that flag is set.

## 9. Caddy configuration

The one LAN-facing file, extended. **This is the only place this design touches §13.1.**

```
drydock.example.com {
    tls /etc/caddy/certs/drydock.pem /etc/caddy/certs/drydock.key
    encode zstd gzip
    reverse_proxy unix//run/drydock/http.sock {
        flush_interval -1
        header_up X-Forwarded-For {remote_host}
    }
}

*.preview.drydock.example.com {
    tls /etc/caddy/certs/preview.pem /etc/caddy/certs/preview.key   # wildcard, provisioned externally
    reverse_proxy unix//run/drydock/preview.sock {
        flush_interval -1                          # HMR websockets and SSE from the previewed app
        header_up X-Forwarded-For {remote_host}
        header_up X-Drydock-Preview-Host {host}
    }
}
# Still no site block for the bare IP, and the wildcard matches one level:
# a request for anything else — including drydock.example.com's own siblings — matches nothing.
```

> [!WARNING]
> **A new dependency on the certificate automation**
>
> §13.1 says certificate issuance is out of scope and Drydock has no opinion beyond needing the result. This design adds one requirement to that result: **a wildcard certificate for `*.preview.drydock.example.com`**, which in practice means DNS-01 rather than HTTP-01, plus a wildcard `A`/`AAAA` record. If that is not available, §4's fixed-SAN pool is the fallback and it changes the URL scheme but nothing else in this document.
>
> `encode` is deliberately absent from the preview block. Compressing a proxied dev server that is already compressing, or already streaming, buys nothing and has broken HMR in the wild.

## 10. Security

### 10.1 What a preview actually is

A preview is a **browser-side** exposure: repository code, rendered as a first-class web origin on your domain, on a device that is also signed in to Drydock. Nothing new is exposed to the network — no listener, no published port, no credential in the container — and a compromised container gains nothing it did not have, because it does not learn it is being previewed.

So the entire threat model is *what can that origin do to the other origin*, and there are exactly three answers.

### 10.2 Preview → API is same-site, so `Origin` carries the load

This is the finding that should change the parent document.

`drydock.example.com` and `*.preview.drydock.example.com` share the registrable domain `example.com`. A previewed app is therefore **same-site** with the UI, and:

- `SameSite=Strict` on `__Host-drydock` does **not** prevent the browser attaching it to requests aimed at `/api/*`. §13.3 describes `SameSite` as stopping CSRF "for essentially every current browser" — that sentence is true as written and false in the presence of previews.
- CORS still prevents the previewed page from *reading* API responses. It does not prevent the side effect. `POST /api/workspaces/:id/delete` does not need a readable response to have happened.
- Therefore **the `Origin` allowlist on every state-changing route is the primary CSRF defense**, not the backup. It is already required by §13.3 and §5; what changes is that removing it now opens a real hole rather than a theoretical one.

`GET` routes need the same care: any API `GET` with a side effect, or any that returns data worth stealing via a scripted same-site navigation, must be treated as state-changing. The existing rule that mutations are never `GET` is what makes this tractable.

### 10.3 Preview → UI cookie fixation is already blocked

A same-site subdomain can normally set cookies on its parent domain — classic cookie tossing, and a preview would be perfectly placed to do it. It cannot here, because §13.2 chose the `__Host-` prefix, and a browser refuses a `__Host-`-prefixed cookie that is not host-only. A preview can set `Domain=drydock.example.com` cookies under other names; the UI reads exactly one cookie and ignores the rest, which is the property to preserve rather than a new thing to build.

This is a decision made for a different reason in the parent document paying off here, and it is worth recording as such: had §13.2 picked a bare cookie name, this design would have needed a separate registrable domain.

### 10.4 Preview → preview is blocked by host-only cookies

Per-host preview cookies (§5, §7) mean a previewed app cannot read another preview even though they are same-site, because the browser will not send a host-only cookie for `a.preview...` to `b.preview...`. The cost is one invisible redirect per new preview host.

### 10.5 Additions to the blast-radius table

Extending §13.4 of the overall document:

| If this is compromised | Reachable | Not reachable |
|---|---|---|
| **A previewed app** (hostile or XSS'd repo code) | Side-effecting same-site requests to `/api/*` **if and only if the `Origin` check is missing or wrong**. Its own preview origin's storage. | The API cookie (host-only). API *responses* (CORS). Other previews (host-only preview cookies). Anything on the host — the proxy dials one container port and nothing else. |
| **The preview proxy** | Every enabled port on every running workspace. | The App key, the Docker socket, the API mux, any credential — it holds none. |

### 10.6 Additions to §13.5's non-negotiables

- **The preview mux serves no API route, ever.** It is a separate mux on a separate socket precisely so this is structural rather than remembered.
- **The upstream is derived, never supplied.** Workspace container IP plus an enabled port. No host field reaches the dialer from a request, a config file, or a database column.
- **The preview cookie never reaches the container**, and an upstream `Set-Cookie` may not claim its name.
- **Previews are default-deny.** No `forwarded_port` row with `enabled = 1`, no preview — the same rule, for the same reason, as `secret_grant` in §10.1.
- **The API's `Origin` allowlist is load-bearing.** Not defense in depth. See §10.2.
- **The UI sends `Content-Security-Policy: frame-ancestors 'none'`**, so a preview cannot frame the control plane for clickjacking.

## 11. Failure modes

Extending §12. The first row is the one that will actually happen, repeatedly.

| Failure | Detection | Response |
|---|---|---|
| **Dev server bound to `127.0.0.1` inside the container** | Dial to the container IP is refused, but `ss -ltn` over `devcontainer exec` shows the port listening on loopback | The single most common cause, and a generic 502 would send you debugging the proxy. Name it exactly: *"vite is listening on 127.0.0.1:5173, which is only reachable from inside the container. Start it with `--host 0.0.0.0`."* Include the flag for the detected server where known. |
| Port enabled, nothing listening | Connection refused, nothing on loopback either | *"Nothing is listening on port 5173"* — the server has not been started, or it crashed. Link to the workspace log. |
| Workspace stopped | State check before dialing | `/.drydock/denied` naming the state, with a start button. Never a proxy error. |
| Container restarted, IP changed | Dial fails against the cached address | Re-resolve once and retry transparently. Only a second failure is user-visible. |
| App rejects the `Host` header | Upstream returns 400/403 with a recognizable body (`Blocked request`, `Invalid HTTP_HOST`) | Detect the signature and suggest the fix for that framework, or switching the port to `host_header: localhost`. A raw 403 here reads as a Drydock bug. |
| Websocket upgrade fails | `Upgrade` request returns non-101 | Usually `flush_interval` or a buffering layer. Surface it as "live reload unavailable" rather than breaking the page. |
| Wildcard certificate missing or expired | TLS failure at Caddy, before Drydock | Previews fail; the UI is unaffected because it is a different block with a different cert. Health check warns on preview-cert expiry separately from the UI cert. |
| Slug collision | `UNIQUE` violation on insert | Regenerate the random suffix and retry. Four characters over a per-repo-per-port namespace makes this rare and harmless. |
| Preview left open on a lost device | You notice, as in §13.2 | *Revoke all sessions* cascades to `preview_session`, so every preview on every device dies with the same click. This is the reason for the foreign key. |

## 12. What this deliberately gives up

- **No non-HTTP forwarding.** A database client on a tablet would need a raw TCP listener on the LAN, which §13.5 forbids and which no amount of design here can make acceptable. Use `devcontainer exec`.
- **No port auto-exposure.** A repo's `forwardPorts` was written for VS Code, where forwarding lands on *your own* loopback. Promoting that to a LAN-reachable origin serving code to a phone is a different decision, so Drydock discovers those ports and pre-fills the list, but the enable is yours. One click, made once per port.
- **No preview without a session.** There is no share link, no anonymous read-only mode, no "just this one port". A preview is repo code on your domain; the sign-in is the only thing making that reasonable.
- **No editing through the preview.** It is a viewport. Changes happen through Remote Control, which is where the agent already is.
- **`/.drydock/*` is reserved** on every preview host. Rare, documented, and the alternative — a magic query parameter, or sniffing content — is worse.

## 13. Build plan

Small enough to be one phase, ordered so the risky part is first. This slots in after Phase 2 (walking skeleton) of §14 — it needs a running container and nothing else, and specifically does not need Claude, the broker, or secrets.

| Step | Deliverable | Done when |
|---|---|---|
| **1 — Front door** | Second socket, second mux, wildcard Caddy block, wildcard cert in place. Preview mux returns 401 for everything. | Every preview URL returns 401 from a device on the LAN, and no preview URL can reach an API route. |
| **2 — Handshake** | `/preview/authorize`, one-time tokens, `preview_session`, cookie stripping. | A signed-in device reaches a hardcoded upstream; an unsigned-in one is bounced to sign-in and returned. Revoke-all closes it. |
| **3 — Proxy** | Container IP resolution, dial, websocket upgrade, `Host` handling, `X-Forwarded-*`. | Vite with HMR works end to end on a phone. |
| **4 — Registry** | `forwarded_port`, discovery from the resolved config, the ports UI, probe endpoint. | Enable a port from the card, open it, disable it, and watch it close. |
| **5 — Diagnosis** | The §11 table, especially the loopback detection. | Binding a dev server to `127.0.0.1` produces the sentence that tells you to use `--host 0.0.0.0`, not a 502. |

Step 1 before anything else, for the same reason §14 puts the front door before the skeleton: retrofitting auth onto a proxy that already works is how open proxies happen.

## 14. Open questions

### 14.1 Still open

1. **Does the `Origin` check hold up as the sole CSRF defense?** §10.2 makes it load-bearing. Before step 2 ships, it is worth a deliberate test: a page served from a preview origin attempting a state-changing `POST` to `/api/*`, confirming it is refused, and confirming the refusal is logged. This is cheap and it is the one place where being wrong is quiet.
2. **Does `host_header: passthrough` want to be the default?** It is the correct behavior for apps that generate absolute URLs and the wrong one for apps with strict host allowlists, and the second group is growing. The answer is one afternoon of pointing it at the repos actually in the installation.

### 14.2 Deferred, and what would reopen each

| Deferred | Reopen when |
|---|---|
| **A separate registrable domain for previews** (`*.drydock-preview.net`). | Either the `Origin` check proves fragile in 14.1, or a preview needs to run code you did not write — a dependency's demo, a third-party template. At that point the same-site relationship stops being a manageable risk and the second domain becomes cheap by comparison. |
| **Raw TCP forwarding.** | Never, on this design. It would reopen §13.5's first bullet. If it is genuinely needed the answer is Tailscale to the host, not a Drydock feature. |
| **Sharing a preview with someone else.** | A second operator exists — at which point §1's "Operators: 1" is what actually needs revisiting, and this follows from it rather than leading. |
| **Auto-enabling ports on `forwardPorts`.** | The one-click enable proves to be friction you resent, measured in actual clicks rather than anticipated ones. The row already records `discovered_from`, so the switch is a default change, not a migration. |
| **Previewing a stopped workspace** by starting it on demand. | Opening a bookmark to a stopped workspace becomes a common enough annoyance to be worth the surprise of a container starting because you clicked a link. |

### 14.3 Decided while writing this

| Question | Answer |
|---|---|
| Path prefix or subdomain? | **Subdomain.** A path prefix puts repo code on the API's origin, which is disqualifying before the broken asset paths are even considered (§4). |
| One preview cookie or one per host? | **One per host.** Host-only cookies are what stop preview A reading preview B, and the extra redirect is invisible (§10.4). |
| Should Caddy route to workspaces? | **No.** It gets a wildcard and a socket. Teaching the front door the workspace map would make it stateful and couple it to Drydock, against §3.1. |
| Publish container ports on the host? | **No.** Drydock dials the container's Docker-network address from the host. Publishing would put listeners on the dev server's interfaces, which is the thing §13.5 exists to prevent. |

---

*Supplements `docs/design/overall/drydock-design.md` draft v3. The `Origin`-check promotion in §10.2 is a change to that document's §13.3 and §13.5, not merely an addition to it, and should be reflected there when this is built.*
