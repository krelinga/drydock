# Drydock security review

*An adversarial read of the Drydock design as specified, with the port-forwarding supplement folded in. The job here is to try to break it on paper — not to restate its security section approvingly.*

**Status** review, draft v1 · **Date** 27 August 2026 · **Reviewer** adversarial pass

**Reviews** [`overall/drydock-design.md`](overall/drydock-design.md) draft v3 · [`port-forwarding/port-forwarding-design.md`](port-forwarding/port-forwarding-design.md) draft v1

**Nature of this review** Both documents are design-only; there is no code. "Severity" below is therefore *design risk* — what the exposure becomes if the system is built exactly as written — and every recommendation is a design change, not a patch. Where the design already names a risk honestly, this review says so rather than re-discovering it.

---

## 0. Summary

The core architecture is genuinely strong, and most of it earns the confidence its authors have in it — socket-as-identity, the two-mux separation, the `__Host-` payoff, default-deny secrets, and a schema with no column to leak are all load-bearing in the right way. §7 credits Spike 00 correctly. The honesty in §10.4 (secrets are fully readable by the agent and there is no technical control) is exactly the right posture.

The concentration of new risk is in the **port-forwarding supplement**, and it traces to one decision:

> Previews are served from `*.preview.drydock.example.com`, **same-site** with the UI, and the alternative — a separate registrable domain — is rejected only on the cost of owning a second domain, despite the design's own §4 table calling it *"strictly better security."*

That decision is defensible, but it converts a belt-and-braces control (`Origin` checking) into the *only* thing standing between hostile repo code and the control plane, and the design specifies that control in prose without nailing the three details that decide whether it actually holds. Everything in §2 below follows from that.

| # | Severity | Finding | Where |
|---|---|---|---|
| F1 | **High** | Same-site previews downgrade CSRF posture to a single control the design admits is second-best; also enables credible operator-password phishing on a legitimate-looking origin, with no second factor. | PF §4, §10.2 |
| F2 | **High** | The `Origin` allowlist — now the sole CSRF defense — is under-specified: exact-match vs. suffix, fail-closed on absent `Origin`, and the CORS policy §10.2 leans on are all unstated. Any one wrong = full CSRF. | PF §10.2; Overall §13.3 |
| F3 | **High** | Volatile container IP/PID caches + boot-only reconciliation → a preview can dial, or the scanner can read the netns of, a *different* workspace's container (or a foreign process) after IP/PID reuse. | PF §8.1, §8.2; Overall §6 |
| F4 | Medium | The single-use preview token rides in a URL query string, colliding with "redact by default" (Caddy access log, `Referer`, history). | PF §7 |
| F5 | Medium | Internal contradiction: the `up` command writes an on-disk plaintext `--secrets-file`, which §10.3 argues against and claims never happens. | Overall §6 vs §10.3 |
| F6 | Low | A previewed origin can install a Service Worker and set non-`__Host-` cookies on the parent domain; bounded, but unacknowledged. | PF §10.3, §10.4 |
| F7 | Low | "The proxy dials one container port and nothing else" understates that the port's handler is repo code and can relay the container's own egress back through the preview. | PF §10.5 |
| F8 | Low | `preview_session` has no independent TTL and is not cleaned up on port delete; the pending one-time-token store is unspecified and must be atomically single-use. | PF §5, §7 |
| F9 | Low | No DoS / resource-exhaustion consideration on the preview mux (unbounded proxied connections, unauth redirect work per request from LAN devices). | PF §3, §11 |

Nothing here argues against building the feature. F2 and F3 are the two that would turn into real holes silently, and both are cheap to close in the design now.

---

## 1. Scope, method, threat model

### 1.1 What was reviewed

The overall design (§1–§15) and the port-forwarding supplement (§1–§14). Diagrams were read for the claims in their captions, not audited as artifacts.

### 1.2 Threat actors considered

| Actor | Has | Wants |
|---|---|---|
| **Internet page the operator visits** | The operator's browser, briefly | CSRF into `/api/*`, DNS-rebinding to the LAN, phishing the password |
| **Other LAN device** (IoT, guest laptop, a printer) | Network path to Caddy, no cookie | Any authenticated action; a foothold |
| **Hostile / compromised repository** | Its `devcontainer.json` runs on the host at build; its code runs in the container; its dev server becomes a **same-site web origin** once previewed | Escape the container's blast radius; reach the API or another workspace; harvest the operator password |
| **Compromised container** (rogue agent via prompt injection, or repo code) | Its own broker socket, its secrets, the shared Claude credential | Another repo's token, another workspace, the host |
| **Holder of a leaked SQLite file / backup** | The database at rest | Live credentials, usable cookies |
| **Local non-Caddy process on the host** | A shell as some other uid | Drydock's socket, the broker socket |
| **Root on the host** | Everything | — *(explicitly out of scope; the host is the trust boundary, §13.4)* |

### 1.3 Method

For each boundary the design claims, I assumed the implementation is the *most natural wrong one* a competent engineer would write from the prose, and asked whether the boundary still holds. That is where F2 and F3 come from: the design's reasoning is correct, but the prose admits an implementation that quietly isn't.

---

## 2. High-severity findings

### F1 — Same-site previews are a self-inflicted downgrade, and they enable password phishing

**The decision.** PF §4 and §10.2 put previews on `*.preview.drydock.example.com`, same-site with the UI. PF §4's own alternatives table says a separate registrable domain is **"strictly better security — restores cross-site status so `SameSite` blocks preview→API by itself,"** and rejects it *"only because it costs a second domain to own and renew."*

**Why this is the headline.** The overall design's one-sentence self-summary (§13.5) is *"the only thing standing between a device on your wifi and code execution on your dev server."* The port-forwarding feature deliberately parks **arbitrary, possibly-hostile repository code** as a first-class web origin next to that control plane, and pays for it by demoting `SameSite=Strict` from "works for essentially every browser" to "does not apply here." The compensating control (F2) is real but singular. Trading a structural, browser-enforced boundary for a single application-level check — to save roughly the price of a domain — is the most lopsided cost/benefit in either document. The design reasons its way to this honestly; it just lands on the wrong side of its own analysis.

**The phishing vector the docs don't mention.** Because previews live on `*.preview.drydock.example.com`, a hostile previewed app is served from what a human reads as *"part of Drydock."* Nothing stops that app from rendering a pixel-perfect Drydock sign-in form and asking for the password. The operator, seeing a trusted-looking origin under the real domain, is far more likely to type it than they would be at `evil.example`. There is a **single factor** (§13.2) and no second one, so a harvested password is the whole system. The separate-domain option removes the "looks like us" trust that makes this credible; the same-site option leans entirely on the operator never typing their password anywhere but the bare host.

**Recommendation.**
1. Reverse the decision, or set a concrete trigger to reverse it that is cheaper than being wrong: PF §14.2 already lists "a preview needs to run code you did not write" as a reopen condition — but *all* repo code is code the operator did not write in the security-relevant sense (an agent reads untrusted issue text and dependency READMEs, per §10.4). The trigger is met on day one.
2. If same-site stays, treat F2 as a hard gate on shipping step 2, and add an explicit UX invariant: the sign-in form is served *only* from the bare origin, the UI teaches the operator that a login prompt on any `*.preview` host is hostile, and the preview responses never share visual chrome with the UI.

---

### F2 — The `Origin` allowlist is now sole CSRF defense and is under-specified in the three ways that matter

PF §10.2 correctly promotes the `Origin` check to *primary* CSRF defense once previews exist. The reasoning is right. The specification is missing the parts that decide whether it holds, and each omission admits a natural implementation that reopens full CSRF from a hostile preview.

**(a) Exact-match, not suffix or registrable-domain match.** The allowlist must be the exact string `https://drydock.example.com`. The one thing it must *never* do is match on "ends with `drydock.example.com`" or "same registrable domain" — because `evil.preview.drydock.example.com` satisfies both, and that origin is now attacker-controlled content. This is the single most likely implementation mistake, precisely because same-site subdomains are a new and non-obvious hazard, and a suffix check is the intuitive way to write "belongs to us." The design must state: exact-string allowlist, previews explicitly excluded.

**(b) Fail closed on absent `Origin`.** Browsers omit `Origin` on some requests (notably top-level GET navigations, and historically some same-origin requests). A middleware that treats "no `Origin` header" as "allow" is trivially bypassed by a request shaped to omit it. Every state-changing route must reject a *missing* `Origin` as hard as a wrong one. This is safe here because the only legitimate API clients are the browser UI (always sends `Origin` on fetch) and the SSE stream (a GET, non-mutating); there is no non-browser POST client to accommodate.

**(c) The CORS policy §10.2 leans on is never stated.** §10.2's second pillar is *"CORS still prevents the previewed page from reading API responses."* That guarantee is **entirely** a property of the response headers Drydock emits, and the design never specifies them. If any handler emits `Access-Control-Allow-Origin: *`, or — worse and more common — *reflects* the request `Origin` with credentials, a same-site preview can read every API response and the "reading is blocked" pillar collapses silently. The design must state: the API emits **no** permissive CORS header and **never** reflects `Origin`; cross-origin reads fail by default. Given previews are same-site, this is load-bearing, not hygiene.

**Recommendation.** Fold all three into §13.3 of the overall doc and §10.2/§10.6 of the supplement as explicit non-negotiables. PF §14.1.1 already proposes a browser-level integration test that a preview-origin `POST` to `/api/*` is refused and logged — keep it, and extend it to cover (b) a stripped-`Origin` request and (c) a cross-origin read attempt against a data `GET`. This is the one place §14 itself flags as "cheap, and the one place where being wrong is quiet." It is right about that.

---

### F3 — Stale IP/PID caches can misroute a preview or a scan across workspace boundaries

The overall design's reconciliation is **one-directional and boot-only** (§6): "Docker is the truth, the database is the cache," reconciled at startup. The port-forwarding subsystem builds two live, latency-sensitive mechanisms on top of volatile Docker facts and inherits a cache-invalidation rule that is too coarse for them.

**The preview dial (PF §8.1).** The container IP is "a cache with exactly one invalidation rule: any workspace state transition clears it." Container IPs are reassigned by Docker on restart. Consider: workspace X's container dies in a way that produces **no Drydock-observed state transition** — an OOM kill, a `dockerd` restart, a crash the supervisor hasn't yet noticed. The DB still says X is `running`; the cached IP still points where X's container used to be. Docker hands that IP to a new container — another workspace's, or a non-Drydock one. A preview request for X, which passes the "workspace running AND port enabled" check because the DB is stale, now dials **another container's** dev server. The requester is authorized for X and is shown Y — including whatever Y's dev server renders, which may itself be a view onto Y's granted secrets.

**The discovery scan (PF §8.2).** Same failure, sharper. The scanner reads `/proc/<container-pid>/net/tcp`. If X's container died and the kernel reused `.State.Pid` for an unrelated **host** process, a scan that trusts the cached PID enumerates the *host's* (or another process's) network namespace and surfaces host-local listeners as previewable rows for workspace X. Enable one and F3's dial path (above) sends a browser at it.

Both are the classic hazard of keying a live data path on cached volatile identifiers with a coarse invalidation cadence. The preview subsystem needs fresher truth than "reconcile on boot."

**Recommendation.** Re-resolve the container from Docker **by `drydock.workspace=<id>` label immediately before every dial and every scan**, and treat "no live container for this label right now" as `stopped` (a `/.drydock/denied`), never as a cache miss to be filled from a stale row. Bind the dial to the resolved container identity, not to a remembered IP string. This is the same "Docker is the truth" rule the overall design already commits to — the finding is only that boot-cadence reconciliation does not satisfy a proxy and a poller that run continuously against IPs and PIDs that churn under them.

---

## 3. Medium-severity findings

### F4 — The single-use preview token travels in a URL, against "redact by default"

PF §7 step 5 redirects to `https://<host>/.drydock/session?t=<token>`. The token is a live (if 60-second, single-use) capability, and it is in a **query string** — the most-logged, most-leaked part of any request:

- **Caddy access log.** The overall §13.5 non-negotiable is that session tokens and the login code "never reach Caddy's access log." A query-string credential on the preview vhost is captured by default access logging. If access logging is on anywhere for `*.preview`, the token is logged.
- **`Referer`.** The previewed app is arbitrary repo code with no CSP enforced by Drydock. Sub-resource or outbound requests it triggers can carry a `Referer`; depending on redirect handling the token URL can appear there.
- **Browser history and any intermediary.**

The 60-second TTL and single-use consumption make *replay* hard, and by the time the app has loaded the token is spent — so this is a confidentiality/consistency-with-invariants finding, not a standing account takeover. But it directly contradicts a stated non-negotiable.

**Recommendation.** State explicitly that the preview vhost disables access logging (or redacts query strings), and set `Referrer-Policy: no-referrer` on the `/.drydock/session` response. Prefer a carrier that isn't the URL if one is workable (the handshake already sets a cookie one hop later); if the URL is kept, document the TTL + single-use + logging-off trio as the compensating controls so the tension with §13.5 is a decision on the record rather than an oversight.

### F5 — The `up` command writes an on-disk plaintext secrets file, which §10.3 says never happens

The overall §6 step-6 invocation includes `--secrets-file /run/drydock/secrets/$WS.json`. §10.3 then argues *against* `--secrets-file` ("Baked in at container creation. Rotating a secret means rebuilding") and states as a virtue of the socket-delivery design that secrets "never touch a file, so there is nothing to leave behind on container stop." These cannot both be true as written. Either the `up` file is for a different class of value (build-time secrets, which the docs never distinguish from the §10 repository secrets) or it is a plaintext at-rest secret store the §10 design was explicitly built to avoid.

Whichever it is, its lifecycle is unspecified: what writes it, where (`tmpfs`? mode?), when it is deleted, and whether it survives a crash between `up` and cleanup. An unspecified plaintext secrets file on the host is exactly the "the file ended up somewhere its contents were not thought about" case §10.2's encryption-at-rest is meant to prevent — undone at the `up` boundary.

**Recommendation.** Reconcile the two sections. If `--secrets-file` is genuinely needed at `up` (e.g., for feature installation), name that value class, keep it disjoint from repository secrets, put the file on `tmpfs` mode `0600`, and specify its deletion. If it is not needed, remove it from the §6 invocation so the document has one delivery story.

---

## 4. Low-severity findings

### F6 — A previewed origin can plant a Service Worker and set parent-domain cookies

The `__Host-` prefix (§13.2) correctly blocks a preview from *tossing* a cookie the UI will read (PF §10.3). Two residuals the supplement doesn't name:

- A previewed app can set `Domain=drydock.example.com` cookies under *other* names. The design says the UI reads only `__Host-drydock` and ignores the rest — true, and the property to keep. Worth stating as an invariant ("the UI reads exactly one cookie by exact name") rather than leaving it implicit, because a future feature that reads a second cookie by a guessable name would inherit a hostile writer.
- A hostile previewed app can register a **Service Worker** scoped to its preview host. Within that origin's own blast radius this grants nothing new (the preview cookie is `HttpOnly` and host-only; the first token-bearing navigation predates any SW). But a SW persists past the container and can intercept later re-auth handshakes on that host. Bounded to the preview's own origin, so not an escalation — but it means a retired slug's host should be considered "possibly still running attacker JS in some browser" until the browser evicts it.

**Recommendation.** Note the one-cookie-by-exact-name invariant in §13.2/§10.3. Consider emitting `Clear-Site-Data` when a port is disabled or a slug retired, and accept SW persistence as within the preview origin's own (already-hostile-assumed) blast radius.

### F7 — "The proxy dials one container port and nothing else" understates the relay

PF §10.5's blast-radius row for the preview proxy reads *"one container port and nothing else."* True of the **proxy**. But the thing on the other end of that port is repo code, and it can relay: a previewed handler can proxy the browser's bytes onward to anything the *container* can reach on the Docker network or its egress. The preview does not expand the container's reach, but it does give a LAN browser a driven channel into whatever the container chooses to relay.

**Recommendation.** Refine the claim to "the proxy dials one container port; what that port serves is repo code and can relay the container's own network reach." It does not change the design — the container's egress is governed elsewhere — but the table currently reads as a stronger guarantee than it is.

### F8 — `preview_session` lifetime and the pending-token store

- `preview_session` cascades on `auth_session` delete (correct, and the reason for the FK). It has **no independent TTL** and is **not** cascaded by `forwarded_port` delete or disable — so a captured preview cookie is valid until the whole auth session dies (up to the 30-day absolute ceiling), and deleted-port rows linger (inert, since the dial-check fails closed and slugs aren't reused, but they leak).
- The **pending one-time token** (minted in PF §7 step 4, consumed in step 6) has no table and no specified store. Assume in-memory; the design must state that consumption is **atomic** (compare-and-delete) so two racing requests can't both spend it, and that a Drydock restart between mint and consume fails the handshake closed (it does — the user simply retries).

**Recommendation.** Give the preview cookie a shorter independent TTL keyed on `last_seen`, delete `preview_session` rows on port disable/delete, and specify the pending-token store as in-memory with atomic single-use.

### F9 — No resource bound on the preview mux

The API has sign-in lockout; the preview mux has no stated cap on concurrent proxied connections, held-open HMR websockets, or the redirect/authorize work an unauthenticated LAN device can drive per request. A noisy or hostile LAN device (no cookie, so it can't proxy) can still make Drydock do redirect work indefinitely, and a signed-in-but-buggy client can pin connections. Low severity on a small LAN, but the overall design otherwise bounds its resources deliberately (§1 caps, §8 `--capacity 4`).

**Recommendation.** State a concurrent-preview-connection cap and an idle timeout on proxied upgrades, consistent with the hand-managed-capacity posture of §1.

---

## 5. Things that are right and should not be "fixed"

An adversarial review that only lists faults misleads about the balance. These are load-bearing and correct:

- **Socket-as-identity, extended to the front door.** The two-socket / two-mux split (PF §3) makes "control-plane vs. untrusted content" a file-descriptor boundary rather than a branch in a handler. A preview request genuinely cannot reach an API handler even if host parsing is wrong. This is the strongest structural idea in the supplement and the right answer to same-site's dangers.
- **The `__Host-` payoff.** §13.2 chose `__Host-` for a different reason; PF §10.3 shows it blocks preview→UI cookie fixation for free. Recording *why* a past decision pays off (PF §10.3's closing note) is exactly the discipline that keeps a design maintainable.
- **Default-deny everywhere.** No `secret_grant` row → no secret; no `enabled` port → no preview. The required `reach` field (§10.4) as a *control* rather than documentation is a genuinely good idea.
- **A schema with nothing to steal.** No `value` column, `auth_session.id = sha256(cookie)`, `token_grant` records issuance not tokens, secrets are AEAD ciphertext with the id as AAD. A leaked DB file yields no live credential — the F-list above never once turns the SQLite file into an account, and that is by design.
- **§10.4's honesty.** The refusal to pretend there is a technical control over what a granted secret does once inside the container, and the explicit call-out that the transcript is an exfil channel Drydock can't redact, is the right way to document a residual risk. Previews do **not** widen it: a previewed app is browser-side and cannot read the container's secrets that a compromised container couldn't already exfiltrate.
- **The DNS-rebinding treatment** (§13.3) validating `Host` in *both* Caddy and Drydock, so the defense doesn't live in one config file. The preview mux inherits this for free — its routing key *is* the `Host`, so an unresolvable host is denied by construction.
- **PF §14.1.1** already proposing the exact CSRF integration test F2 needs. Credit where due; this review's contribution there is to make it a gate and widen it to the `Origin`-absent and CORS-read cases.

---

## 6. Recommendations, ordered

1. **Re-open the separate-registrable-domain decision (F1).** It is the one change that dissolves F1 and demotes F2 back to belt-and-braces. Its stated cost is a domain registration; its stated benefit, in the design's own words, is "strictly better security." For a control plane that is an arbitrary-code-execution surface, that trade should go the other way, or carry a trigger that is actually reachable.
2. **If same-site stays, specify the `Origin`/CORS contract precisely and gate step 2 on the test (F2).** Exact-match allowlist with previews excluded; fail-closed on absent `Origin`; no permissive or reflected CORS; the browser-level test from §14.1.1 extended to all three.
3. **Re-resolve containers by label at every dial and scan (F3).** Boot-cadence reconciliation does not serve a live proxy and poller.
4. **Reconcile §6's `--secrets-file` with §10.3 (F5).** One delivery story, or two clearly-separated value classes.
5. **Get the preview token out of the log path (F4),** and tidy the `preview_session` lifetime and pending-token store (F8).
6. Fold F6/F7/F9 in as clarifying invariants and a resource cap.

None of these blocks the feature. F2 and F3 are the two that fail silently if built from the current prose, and both are a paragraph of design to close.

---

*This review supplements the two documents it names and does not alter them. Its F1/F2 recommendations are the concrete form of the amendment the port-forwarding supplement's own closing note already anticipates ("the `Origin`-check promotion in §10.2 is a change to that document's §13.3 and §13.5") — this review's position is that the promotion is not enough on its own, and the same-site decision that forces it should be revisited first.*
