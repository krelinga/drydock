# extension/ — Drydock for VS Code

The VS Code extension (`krelinga.drydock`): sign in with an API token, then drive Drydock from the
command palette. The design is `docs/design/vscode-extension/vscode-extension-design.md`; it is
being built in phase E1's series of PRs, and this directory holds what has landed so far.

```sh
npm ci && npm run check   # types (twice: whole, and the pure layer alone), specs, the bundle and its budget
../test/extension/run.sh  # the client tier: this client against the real Drydock behind real Caddy
```

The client tier's specs are `test/client/` (config `vitest.client.config.mts`); its stack and
what it covers are `test/extension/README.md`. The extension-host tier is `test/host/` (config
`vitest.host.config.mts`, run by `../test/extension/run.sh host`): the pinned VS Code, checked by
SHA-256, running `test/host/inner/suite.ts` against the same stack. In `ExtensionMode.Test` only,
`activate` returns the seam (a Prompter, Opener, Window and Extensions a spec can replace).

## Installing

Each Drydock release carries the matching extension as `drydock-vscode.vsix`:

```sh
curl -fsSLO https://github.com/krelinga/drydock/releases/latest/download/drydock-vscode.vsix
code --install-extension drydock-vscode.vsix
```

or *Extensions: Install from VSIX…*. It needs Remote - SSH and Dev Containers to connect, and
offers to install them then. VS Code does not update a sideloaded extension, so the extension does:
when the Drydock it signs in to is another release, it offers that release's `.vsix`, downloaded
from GitHub and checked against the release's `SHA256SUMS` before VS Code installs it.
`npm run package` builds the `.vsix` locally.

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

## E1 so far

`drydock.signIn`, `signOut`, `openInBrowser`, `showLog`; and (4a) `drydock.go` (the hub, bound to
`ctrl+alt+shift+d` / `cmd+alt+shift+d` with no `when`), `clone`, `connect` and
`refreshRepositories`, over `src/live.ts` (the window's reducer, marks and stream, held while a
command, a follow or a mark needs it and closed five minutes after nothing does) and
`src/flows.ts`. A follow is a progress notification; its end is a notification whose primary
action is Connect (`drydock.connectWhenReady` connects without asking). Connect reads the view now,
rewrites only the scheme of the server's own link, checks Remote - SSH and Dev Containers, and
hands VS Code `vscode.openFolder` in a new window. The status bar (`src/status.ts`) is the running
count, the stream's phase, and the one thing to act on: a token near expiry or a version mismatch.
`test/fakeDrydock.ts` is a Drydock in memory for these flows' specs.

(5) Host-access approvals (`src/review.ts`): *Review Host Access…* opens a read-only
`drydock-approval:/<id>` document with exactly the web card's content (core's `lib/approval.ts`);
*Approve Host Access…* (from that document or its title bar) asks for the repository's full name,
typed exactly, and sends the hash of the snapshot the document showed — never one read at send
time or from an argument; a new request cancels the box and re-renders the document; a stale hash
reopens it. *Cancel Host-Access Request* is a modal. No keybinding, hub item or notification
approves.

(4b) The activity bar's *Drydock* container holds two tree views (`src/tree.ts`): *Workspaces*,
each with its steps as children, Enter connecting to a running one and opening any other in
Drydock; and *Repositories* without a workspace, cloned from the context menu. A shown view holds
the stream. Only `drydock.connect` and `drydock.openInBrowser` take a workspace id (`takesId`),
from a tree item or its node; a mutating command never does.

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
