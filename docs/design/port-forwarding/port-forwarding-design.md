# Drydock port forwarding

*Reaching a dev server running inside a workspace container from a phone or tablet on the LAN — without giving repository code a foothold on Drydock's own origin.*

**Status** design document, draft v3 · **Date** 28 August 2026 · previews moved to a separate registrable domain (cross-site) per the [security review](../security-review.md)

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
| **Container manager** *(extended)* | Resolving a workspace's current container IP and PID, and invalidating both on every state transition. | Publish ports on the host. Nothing is bound on the dev server's network interfaces. |

Note what is *not* here: no change to the container. No agent, no injected feature, no published port, no `docker run -p`. Drydock reaches the container the way it already reaches everything else — from the host, over the Docker network — and the container never learns it is being previewed.

## 4. Naming and routing

One hostname per (workspace, port), from a wildcard:

```
https://<slug>.drydock-preview.net
        └─ e.g.  drydock-3000-k4x9  /  myapp-5173-p2mq
```

`slug` is `<sanitized-repo>-<container_port>-<4 random chars>`, generated once and stored on the `forwarded_port` row. It is readable enough to tell two open tabs apart, unique across workspaces on the same repo, and stable for the life of the row — a bookmark keeps working across container restarts and rebuilds.

The random suffix is not a security control. Previews are authenticated; it is there so that deleting and re-adding a port produces a *different* URL, which means a stale bookmark fails closed rather than silently landing on whatever now occupies port 3000.

The wildcard is on a **separate registrable domain** from the UI — `drydock-preview.net`, distinct from `drydock.example.com`. That is the crux of the whole security model, not a naming preference: because the two do not share a registrable domain, a previewed app is *cross-site* with the control plane, so `SameSite=Lax` stops it issuing authenticated requests to `/api/*` without an application check having to catch every one (§10.2). It costs one more domain to own and renew — the deliberate price for making the browser enforce the boundary rather than an `Origin` check on every route.

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
  workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
  container_port INTEGER NOT NULL,
  slug TEXT NOT NULL UNIQUE,         -- the DNS label; stable for the life of the row
  label TEXT,                        -- "vite dev server"
  upstream_scheme TEXT NOT NULL DEFAULT 'http',   -- http | https (rare; self-signed upstreams)
  host_header TEXT NOT NULL DEFAULT 'passthrough',-- passthrough | localhost  (§8.2)
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

`enabled` and `observed` are **deliberately independent**. A port being listened on has no bearing on whether it is reachable, and a port being enabled does not require anything to be listening yet. Conflating them is how a discovery feature turns into an exposure feature by accident.

`preview_session.auth_session_id` is a **cascading foreign key on purpose**. §13.2 promises that one button kills every session; without the cascade that promise would quietly stop covering previews, which are the most likely thing to be left open on a device you no longer have.

The row is **per preview host, not per device**. A single cookie covering `.drydock-preview.net` would let a previewed app fetch every other preview on the same device. Host-only cookies cost one extra redirect per preview, and §7 explains why that redirect is invisible.

Two lifetimes, both made explicit rather than left implicit. The preview cookie gets its **own** idle TTL keyed on `last_seen_at`, shorter than the auth session's — a captured preview cookie should not stay live for the auth session's full 30-day ceiling. And a `preview_session` is deleted when its port is disabled or removed, not only when the auth session is: without that, disabling a port dial-blocks new requests (§8.1) but leaves the minted rows lying around to reactivate silently on re-enable. The cascade on `auth_session` is the backstop for the lost-device case; per-port cleanup is the routine one.

## 6. API surface

Additions to §5 of the overall document. All on the API mux, all behind the session cookie and the `Origin` check.

| Method & path | Does | Returns |
|---|---|---|
| `GET /api/workspaces/:id/ports` | Every known port — declared, observed, manual — with its provenance flags, `bind_addr`, `observed_state`, `last_seen_at`, and URL if enabled. Hidden rows only with `?hidden=true`. | Port list |
| `POST /api/workspaces/:id/ports` | Add a port by hand. Body: `container_port`, optional `label`, `upstream_scheme`, `host_header`. Mints the slug. | `201` + port |
| `PATCH /api/workspaces/:id/ports/:port` | Enable, disable, hide, unhide, or relabel. Enabling is the click that makes a URL live. | `200` + port |
| `DELETE /api/workspaces/:id/ports/:port` | Remove it. The slug is not reused. A still-listening port reappears on the next scan as a fresh, disabled row. | `204` |
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

## 7. Preview authentication

The requirement is that a preview is behind the same sign-in as everything else, and the obstacle is that a cookie set for `drydock.example.com` is host-only and therefore never sent to a preview host. So each preview host needs its own cookie, and the only origin that can prove you are signed in is the main one.

1. Browser requests `https://myapp-5173-p2mq.drydock-preview.net/`. No preview cookie.
2. Preview mux redirects to `https://drydock.example.com/preview/authorize?return=<the original URL>`.
3. **The session cookie rides that top-level navigation.** The preview host and the UI are on *different* registrable domains, so this is a cross-site navigation — but `SameSite=Lax` sends the cookie on a top-level GET even cross-site, which is exactly why the session cookie is `Lax` and not `Strict` (§13.2). `Strict` would withhold it here and a signed-in user would be bounced to sign-in on every new preview.
4. `/preview/authorize` validates the session, checks the slug resolves to an enabled port on a running workspace, and mints a **single-use token, 60-second TTL, bound to that `auth_session.id` and that one preview host**.
5. Redirect to `https://myapp-5173-p2mq.drydock-preview.net/.drydock/session?t=<token>`.
6. Preview mux consumes the token, writes `preview_session`, sets a host-only cookie (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`), and redirects to the path from step 1.

Steps 2–6 are four redirects with no user interaction, so in practice the first request to a new preview host renders the app. If you are not signed in, step 3 lands on the ordinary sign-in page and `return` carries you back afterwards.

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

#### Discovery is not a prompt

The tempting next step is a notification — *"port 8080 appeared, approve?"* — and this design deliberately refuses it, for the reason §13.5 of the overall document gives for refusing a re-auth prompt on delete: **a prompt you see often enough buys habituation rather than safety.** A toast that fires whenever a test run opens a socket trains exactly one reflex, and it is the wrong one.

So discovery is **ambient, not interruptive**. The workspace card carries a quiet count — *"4 listening · 1 previewed"* — and the ports panel is where decisions get made, at a moment the operator chose. No port changing state ever moves anything into reach.

That the list is now live is itself an argument that default-deny was the right call in §12. A static list makes auto-exposure merely unwise; a live one would make it dangerous, because a debugger would become reachable at the instant it opened.

### 8.3 The `Host` header

Dev servers increasingly reject unexpected `Host` values — Vite's `server.allowedHosts`, Rails' `config.hosts`, Django's `ALLOWED_HOSTS`. Two behaviors, per port, because neither is right for everything:

| `host_header` | Sends upstream | Use when |
|---|---|---|
| `passthrough` *(default)* | The preview hostname | The app builds absolute URLs from `Host` and you want them to work. Requires adding the preview host to the app's allowlist. |
| `localhost` | `localhost:<port>` | The app's host allowlist cannot be changed and it does not generate absolute URLs. |

`X-Forwarded-Proto: https`, `X-Forwarded-Host: <preview host>`, and `X-Forwarded-For` are always set, so a framework that honors them produces correct absolute URLs even under `localhost`.

### 8.4 Websockets and streaming

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
| Container restarted, IP changed | Dial fails against the cached address | Re-resolve once and retry transparently. Only a second failure is user-visible. |
| App rejects the `Host` header | Upstream returns 400/403 with a recognizable body (`Blocked request`, `Invalid HTTP_HOST`) | Detect the signature and suggest the fix for that framework, or switching the port to `host_header: localhost`. A raw 403 here reads as a Drydock bug. |
| Websocket upgrade fails | `Upgrade` request returns non-101 | Usually `flush_interval` or a buffering layer. Surface it as "live reload unavailable" rather than breaking the page. |
| Wildcard certificate missing or expired | TLS failure at Caddy, before Drydock | Previews fail; the UI is unaffected because it is a different block with a different cert. Health check warns on preview-cert expiry separately from the UI cert. |
| Slug collision | `UNIQUE` violation on insert | Regenerate the random suffix and retry. Four characters over a per-repo-per-port namespace makes this rare and harmless. |
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
| **1 — Front door** | Second socket, second mux, wildcard Caddy block, wildcard cert in place. Preview mux returns 401 for everything. | Every preview URL returns 401 from a device on the LAN, and no preview URL can reach an API route. |
| **2 — Handshake** | `/preview/authorize`, one-time tokens, `preview_session`, cookie stripping. | A signed-in device reaches a hardcoded upstream; an unsigned-in one is bounced to sign-in and returned. Revoke-all closes it. |
| **3 — Proxy** | Container IP resolution, dial, websocket upgrade, `Host` handling, `X-Forwarded-*`. | Vite with HMR works end to end on a phone. |
| **4 — Registry** | `forwarded_port`, declared-port parsing from the resolved config, the ports UI, probe endpoint. | Enable a port from the card, open it, disable it, and watch it close. |
| **5 — Discovery** | The `/proc/<pid>/net/*` scanner, debounce, merge onto declared rows, the loopback classification, the ambient count on the card. | Start a server on an undeclared port with the panel already open; it appears, disabled, correctly labelled — and nothing is pushed at you. |
| **6 — Diagnosis** | The §11 table, wired to what §8.2 already knows. | Binding a dev server to `127.0.0.1` produces the sentence that tells you to use `--host 0.0.0.0`, and no dial is ever attempted. |

Step 1 before anything else, for the same reason §14 puts the front door before the skeleton: retrofitting auth onto a proxy that already works is how open proxies happen.

## 14. Open questions

### 14.1 Still open

1. **Does the cross-site boundary hold end to end?** With previews on a separate registrable domain, `SameSite=Lax` should refuse a preview-origin state-changing `POST` to `/api/*` before the `Origin` check is even reached — worth confirming in a real browser rather than assuming, because it depends on the cookie attribute *and* the domain split both being right. The test: from a preview origin, (a) a state-changing `POST` to `/api/*` is refused and logged; (b) it is still refused with `Origin` stripped (the belt behind `SameSite`); and (c) a data `GET` cannot be *read* cross-origin (no reflected CORS). Cheap, and it catches a misconfigured cookie or a preview domain that accidentally shares a registrable suffix with the UI.
2. **Does `host_header: passthrough` want to be the default?** It is the correct behavior for apps that generate absolute URLs and the wrong one for apps with strict host allowlists, and the second group is growing. The answer is one afternoon of pointing it at the repos actually in the installation.
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

---

*Supplements `docs/design/overall/drydock-design.md` draft v5. Previews are served from a separate registrable domain, which keeps them cross-site with the UI; the one change that pushes into the parent document is the session cookie moving from `SameSite=Strict` to `Lax` (overall §13.2) so the cross-site authorize redirect still carries the session, with the `Origin` allowlist kept as belt-and-braces (§13.3, §13.5).*
