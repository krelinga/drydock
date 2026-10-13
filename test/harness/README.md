# test/harness — shared by the browser tier and the extension's client tier

Node built-ins only, imported by path from `test/browser/harness.ts` and `test/extension/stack.ts`
(vscode-extension §12.4).

- `certs.ts` — a throwaway CA and its leaves, each with a subjectAltName.
- `caddy.ts` — real Caddy on the shipped `deploy/Caddyfile`, byte for byte, its admin API on a
  `0600` Unix socket; `CaddySite` is what it needs to know of the stack it fronts, and
  `CaddyOptions` serves another certificate (the wrong-host controls) or a chosen port.
- `tap.ts` — the tap between Caddy and a Drydock socket: forwards bytes unchanged, streams, relays
  upgrades, and records what arrived (the session cookie, `Authorization`, `Origin`,
  `Last-Event-ID`), which is where "the client did not send it" is asserted.
- `proc.ts` — free ports, waiting (`GaveUp` ends a wait at once), file sweeps, socket probes.

The TLS liveness probe (`tlsAnswers`) is the one place validation is off, and only to ask whether
something speaks TLS on a port yet; nothing that is asserted on runs without validation.
