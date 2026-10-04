# Spike 04 harness — local CA, headless Chromium, `__Host-` semantics

Answers: *can a headless Chromium under Playwright be made to trust a locally-generated CA, such
that `__Host-` cookie semantics are real — without `ignoreHTTPSErrors`?* Report:
[`../04-browser-ca.md`](../04-browser-ca.md).

```sh
./run.sh all          # four trust routes: none (control), spki, ignore, nss
./run.sh nss          # the recommendation: real trust via certutil
./run.sh wrongcert    # which routes still notice a misissued certificate
```

Requires `node`, `openssl`, and `certutil` from `libnss3-tools`. Playwright and Chromium install on
first run into `~/.cache/drydock-spike-04` (override with `PW_DIR`), outside the repo so a design
tree never grows a `node_modules`.

| File | Does |
|---|---|
| `mkcerts.sh` | A throwaway CA plus two leaves: one for the UI host, one wildcard for the preview domain |
| `server.mjs` | One HTTPS listener serving both domains by SNI, recording what arrived on every request |
| `spike.mjs` | Drives Chromium and makes the fourteen assertions |
| `run.sh` | Certs, trust, server lifecycle, and the result table |

**The CA is trusted only for the duration of a run and removed on exit, including on Ctrl-C.** A
throwaway CA left trusted in your own browser store would let anything holding that key impersonate
any site to this browser. It is also regenerated per run, so a leaked key is worthless by the next
one. Keep both properties if you edit this.

Four things that are load-bearing rather than incidental:

- **Cookie assertions are made at the server, not in the page.** A cross-site `fetch` blocked by
  CORS and one sent without credentials are indistinguishable from the caller, so `server.mjs`
  records whether a cookie actually arrived and the test reads that back. Assertion D2 — *the
  request still reached the server* — is the positive control that keeps D1 from passing against a
  `fetch` that silently threw.
- **The cross-site navigation test clicks a real link.** `page.goto` is browser-initiated,
  `sec-fetch-site: none`, and carries the cookie whatever its `SameSite` value is — so a
  `goto`-based version of E1 passes against `Strict`, `Lax`, and `None` alike.
- **URLs carry no port.** The origin must be `https://drydock.test`, not `…:8443`, or the `__Host-`
  rules and site comparisons are not the ones under test. `--host-resolver-rules` takes a port on
  its right-hand side, which is what lets the listener stay unprivileged while the origin stays
  port-less.
- **`spike.mjs` is copied next to `node_modules` before running.** ESM resolves bare imports relative
  to the script's own directory, not the cwd, so running it in place fails with
  `ERR_MODULE_NOT_FOUND`.

The `none` mode exists to prove the harness can fail. Without it, a green run might mean Chromium is
ignoring certificates for some reason nobody noticed.
