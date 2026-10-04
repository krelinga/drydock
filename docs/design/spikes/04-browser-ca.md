# Spike 04 — A local CA, a headless Chromium, and `__Host-` semantics

**Question** ([testing plan](../testing/testing-design.md) §10.1 and §16.1, the one spike that plan
still wanted): *can a headless Chromium under Playwright be made to trust a locally-generated CA,
such that a `__Host-` cookie set over the local HTTPS listener is accepted and replayed — without
`ignoreHTTPSErrors`?*

**Answer: yes, cleanly, via Chromium's NSS trust store.** One `certutil -A` and the browser does
full certificate validation against a throwaway CA. All fourteen assertions pass, including the three
cross-site cookie questions [port forwarding §14.1](../port-forwarding/port-forwarding-design.md)
asked and the testing plan could not answer. **The browser tier is viable as designed and keeps its
nine assertions** — §16.1's fallback (grow a real domain, or shrink the tier) is not needed.

Two findings beyond the question. **`ignoreHTTPSErrors` does not break cookie semantics** — it passes
the same fourteen — so the testing plan's stated reason for banning it is wrong, while the ban itself
is right for a reason the plan does not give. And the obvious no-system-state alternative,
`--ignore-certificate-errors-spki-list`, buys its convenience by going blind to a wrong certificate.

- **Verified against:** Playwright `1.63.0`, bundled Chromium (headless shell, `153.0.8010.12`),
  OpenSSL 3.0.13, `libnss3-tools` 3.98, Ubuntu noble.
- **Date:** 2026-10-04.
- **Harness:** [`harness-04-browser-ca/`](harness-04-browser-ca/) — re-runnable; see *Reproducing*.

---

## Results

### 1 — The matrix

Four trust routes against the same listener, same assertions. `none` is the control that proves the
harness can fail; the wrong-cert column is explained in result 3.

| Trust route | Loads | 14 assertions | Notices a wrong cert | System state |
|---|---|---|---|---|
| **`nss`** — `certutil -A -t "C,,"` into `~/.pki/nssdb` | yes | **14 / 14** | **yes** — `ERR_CERT_COMMON_NAME_INVALID` | one user-level NSS entry |
| `spki` — `--ignore-certificate-errors-spki-list` | yes | 14 / 14 | **no** | none |
| `ignore` — Playwright `ignoreHTTPSErrors: true` | yes | 14 / 14 | **no** | none |
| `none` — control, CA not trusted | **no** — `ERR_CERT_AUTHORITY_INVALID` | 0 / 1 | — | none |

### 2 — What the fourteen assertions are

Worth listing, because together they are most of what the browser tier exists for and they now have
a measured answer rather than a planned one.

| # | Assertion | Result |
|---|---|---|
| A1 | The UI origin loads over real HTTPS | pass |
| A2 | The origin is a **secure context** | pass |
| B1 | A `__Host-drydock` cookie is accepted by the browser | pass |
| B2 | Stored with `Secure`, `Path=/`, `HttpOnly` | pass |
| B3 | Replayed on a same-origin request — *asserted at the server* | pass |
| C1 | `__Host-` with `Secure; Path=/` is accepted | pass |
| C2 | `__Host-` with `Path=/sub` is **refused** | pass |
| C3 | `__Host-` without `Secure` is **refused** | pass |
| C4 | `__Host-` with `Domain=` is **refused** | pass |
| D0 | The preview origin is genuinely a different site | pass |
| D1 | A cross-site `POST` carries **no cookie** | pass — `sec-fetch-site=cross-site`, `cookie=null` |
| D2 | …and that `POST` still reached the server | pass — the positive control for D1 |
| F1 | A cross-site `GET` carries **no cookie** | pass |
| E1 | A cross-site **top-level navigation** *does* carry it (`Lax`) | pass — `sec-fetch-site=cross-site` |

Three of these deserve a note.

**D2 is not padding.** "No cookie was sent" and "no request was sent" produce identical server-side
evidence, so D1 alone would pass against a harness whose `fetch` silently threw. This is §4.1 of the
testing plan applied to the tier that most needs it.

**E1 is the measured justification for `Lax` over `Strict`.** The cookie is sent on a cross-site
top-level navigation and withheld everywhere else cross-site, which is exactly the split overall
§13.2 relies on: the preview authorize hop works, and a cross-site `POST` is still cookieless. To
measure it the harness **clicks a real link** rather than calling `page.goto` — a browser-initiated
navigation is `sec-fetch-site: none` and carries the cookie under any policy, so a `goto`-based test
passes whatever the cookie's `SameSite` value is. That is the shape of a test that proves nothing.

**C2–C4 are the browser, not Drydock.** Nothing in Drydock refuses those cookies; Chromium does.
They are in the suite because the `__Host-` prefix is load-bearing in §13.2 and a prefix enforced by
nobody is a comment.

### 3 — The differentiator: only real trust notices a wrong certificate

Three routes tie at 14/14, so the choice has to be made on something the cookie assertions do not
see. Serving the UI host a certificate that is **validly signed by the trusted CA but issued for the
wrong name** separates them immediately:

```
=== nss (wrong cert for drydock.test) ===
 FAIL  A1 UI origin loads over HTTPS  [net::ERR_CERT_COMMON_NAME_INVALID]
=== spki (wrong cert for drydock.test) ===
 PASS  A1 UI origin loads over HTTPS  [status 200]   -> 14/14
=== ignore (wrong cert for drydock.test) ===
 PASS  A1 UI origin loads over HTTPS  [status 200]   -> 14/14
```

`spki` allowlists a **public key**, which bypasses name checking along with everything else, so it
cannot tell a correct certificate from a misissued one. `ignoreHTTPSErrors` is the same blindness by
a different route. Only `nss` leaves the browser doing the job a browser does.

This matters because the browser tier is partly a test of **Caddy's configuration** (testing plan
§3.2), and a certificate served for the wrong host is a plausible Caddyfile regression. Under `spki`
or `ignore` the suite would stay green through it.

### 4 — Two mechanical details the harness needed

Both are the kind of thing that costs an afternoon and belongs written down.

**Port remapping in the resolver rules, so no test needs root.** The URLs must carry no port or the
origin is not the one under test — `__Host-` and the site comparisons are about
`https://drydock.test`, not `https://drydock.test:8443`. Chromium's `--host-resolver-rules` accepts a
port on the right-hand side, which separates the two:

```
--host-resolver-rules=MAP drydock.test 127.0.0.1:8443,MAP *.drydock-preview.test 127.0.0.1:8443
```

The page's origin stays port-less while the connection lands on an unprivileged listener. A wildcard
on the left is also something `/etc/hosts` cannot express, which the plan already noted.

**A leaf needs a `subjectAltName`.** Chromium has ignored `commonName` since M58, so a certificate
with only a CN fails with `ERR_CERT_COMMON_NAME_INVALID` — which looks exactly like a
misconfigured CA and sends you debugging the wrong thing. One wildcard leaf (`*.drydock-preview.test`)
covers every preview slug, and a wildcard matches exactly one label, so slugs must stay flat.

---

## Consequences for the design

**A. Testing plan §16.1 question 1 is answered: the browser tier is built as specified.** The nine
assertions stay, the three cookie ones included. Remove the "spike this before building it" callout
in §10.1 and the open question in §16.1, and record the mechanism: a per-run CA, `certutil -A` into
`~/.pki/nssdb`, and resolver rules with a port.

**B. Keep the `ignoreHTTPSErrors` prohibition, and fix its stated reason.** §10.1 says disabling
certificate validation "changes the thing under test". Measured, it does not: `ignoreHTTPSErrors`
passes all fourteen, secure context included. The honest reason is result 3 — it makes the tier
blind to a misissued or wrong-host certificate, which is a Caddy regression the tier is otherwise
well placed to catch. Same verdict, defensible ground, and worth correcting because a prohibition
resting on a wrong reason is one someone will relax the first time it is inconvenient.

**C. `spki` is the documented fallback, with its cost named.** If a CI environment cannot write to an
NSS store, `--ignore-certificate-errors-spki-list` gives all fourteen assertions with no system
state. What it gives up is certificate validation, so the Caddyfile conformance test (§3.2) carries
more weight there. Note the flag matches a certificate **in the presented chain**, so the CA's key is
not enough — every leaf must be listed, and the list is regenerated whenever a cert is.

**D. The devcontainer needs `libnss3-tools`.** `certutil` is what the chosen route depends on, and it
is not in the image. One package in the existing `apt-get-packages` feature. Playwright's browsers
stay a lifecycle install rather than a package list — `--with-deps` pulls in some forty transitive
system libraries and enumerating them by hand is how that list goes stale.

**E. The harness must remove the CA it trusted.** A throwaway CA left trusted in the operator's own
browser store is a standing hole — anything holding that key could impersonate any site to this
browser. `run.sh` removes it on exit including on `SIGINT`, and the CA is regenerated per run so a
leaked key is worthless by the next one. Any future harness that touches a trust store inherits this
requirement.

**F. Record what the tier cannot prove.** The assertions are about *Chromium's* implementation of
`__Host-` and `SameSite`. iOS Safari is the binding constraint for the real UI (frontend §1) and is
not testable here at all. The tier buys confidence that Drydock's own half is right — the cookie
attributes it sets, the origins it serves, the routes it refuses — not that every browser agrees.

---

## Reproducing

Needs `node`, `openssl`, and `certutil` (`libnss3-tools`). Playwright and Chromium are installed on
first run into `~/.cache/drydock-spike-04`, outside the repo.

```sh
cd docs/design/spikes/harness-04-browser-ca
./run.sh all          # the four trust routes, including the `none` control
./run.sh nss          # the recommendation on its own
./run.sh wrongcert    # result 3: which routes notice a misissued certificate
```

The CA is generated per run and removed from the NSS store on exit. Nothing binds a privileged port
and nothing touches a real domain: `drydock.test` and `drydock-preview.test` are reserved (RFC 6761)
and absent from the Public Suffix List, which is what makes them distinct eTLD+1 and the cross-site
assertions meaningful.
