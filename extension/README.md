# extension/ — Drydock for VS Code

The VS Code extension (`krelinga.drydock`): sign in with an API token, then drive Drydock from the
command palette. The design is `docs/design/vscode-extension/vscode-extension-design.md`; it is
being built in phase E1's series of PRs, and this directory holds what has landed so far.

```sh
npm ci && npm run check   # types (twice: whole, and the pure layer alone), specs, the bundle and its budget
```

## Layers

- **`src/adapter/`** is the only code that imports `vscode`: it builds the ports from VS Code's API
  and registers every command in `src/commands.ts` through the dispatcher. Thin, with no branch
  worth a unit test.
- **`src/node/`** is Node's `fetch` as a `Transport` (no `vscode`), so the client tier can run the
  real client against real Caddy without an editor. `redirect: 'manual'` (its spec runs a real
  server whose Location receives nothing); a URL that parsing would change (a dot segment, `%2e%2e`
  decoded and resolved, a user, a fragment) is refused before any connection; TLS validation is
  Node's, never turned off (a test CA arrives through `NODE_EXTRA_CA_CERTS` only).
- **Everything else is pure**, compiled again by `src/tsconfig.pure.json` with `lib: ES2023`,
  `types: []` and `noResolve`: no `vscode`, no Node globals, no DOM. Every effect is a port in
  `src/ports.ts`. It shares the web UI's `web/src/core` by path (the reducer, the settling
  predicates, the marks, `ApiError` and the error sentences): the bundle includes it, and there is
  no package.
- `src/boundary.spec.ts` is the named rule beside the compiler's: `vscode` only in the adapter,
  only relative imports into `src/`, the two JSON files and `web/src/core` from the pure layer, and
  nowhere a TLS bypass, a child process, a webview or a triple-slash directive.

## Rules the code holds (design §4, §8)

- **The token is bound to its origin.** `src/credential.ts` stores `{origin, token}` in
  SecretStorage, and `src/client.ts` sends the token only to that origin, only on a path
  `contract.json` names (a parameter is one plain segment: no `%`, no dot segment), with `Origin`
  on every mutation, never following a redirect. A 401 is the one 401 path (the entry is cleared);
  a `403 forbidden_scope` and a 503 are not. A network failure keeps its cause's name and code
  (`ECONNREFUSED`, `CERT_…`) for Show Log, never its message.
- **No token is left live by accident** (`src/session.ts`): Sign Out revokes the token at the
  origin it was stored for, and a sign-in that replaces a token revokes the old one there, best
  effort, and forgets it. A `drydock.url` that no longer names the stored token's origin revokes
  nothing by itself (the field is written on every keystroke): `StaleTokenWatch` waits for the
  value to hold still, and if it is a different valid origin, the focused local or SSH window asks
  once, *Sign out of <old>* or *Keep*. Meanwhile the token is sent nowhere.
- **Mutations only from local and SSH windows** (`src/windowRule.ts`, `src/dispatch.ts`): every
  command runs through the dispatcher, which refuses a `mutating` command in any other window,
  ignores arguments and returns `undefined`. `mutating` is required in the registry.
- **`drydock.url` is `application`-scoped with `ignoreSync`** (`test/manifest.spec.ts`), and must be
  `https://` and a lowercase host (`src/settings.ts`).
- **The output channel never sees a credential** (`src/log.ts` masks token shapes on every line).

## The contract

`contract.json` lists every route the extension calls and every event kind it reads.
`internal/api`'s `TestExtensionContract` holds it to the server's route table and event registry
(scope `read`), so a server change that would break the extension fails `go test` by name, saying to
bump `APIVersion` or keep the route. The client refuses any path the contract does not match. Add a
route here before the extension calls it.

`package.json`'s `drydock.apiRange` is the `api` range of `GET /api/meta` this extension speaks
(`src/handshake.ts`), and its `version` is Drydock's own (the manifest spec checks it against
`.release-please-manifest.json`).
