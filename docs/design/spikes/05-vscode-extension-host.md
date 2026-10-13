# Spike 05 — What the VS Code extension assumes about VS Code

**Question** ([vscode-extension design](../vscode-extension/vscode-extension-design.md) §12.9):
nine beliefs about undocumented or unmeasured VS Code behaviour that the extension's design leans
on. Each is measured against the pinned VS Code, or marked for the laptop ritual where measuring
it needs a real Remote-SSH attach, a real container window or another OS. Nothing here is guessed:
an item not measured says so.

**Answers so far:**

| # | Belief | Result |
|---|---|---|
| 1 | `NODE_EXTRA_CA_CERTS`, set on the launched VS Code, reaches the extension host's `fetch` | **Wrong.** The variable is in the extension host's environment, and its `fetch` still refuses the tier's certificate. The design's fallback holds: the CA in the system store, with `http.systemCertificates` on (VS Code's default), is trusted. CI uses it. |
| 2 | A command a container window's remote host invokes runs in that window's own local instance | Not measured: needs a real attached-container window. **Laptop ritual.** |
| 3 | `remoteName` is `attached-container` / `dev-container` in those windows | Not measured: as #2. **Laptop ritual.** |
| 4 | No workbench command accepts a modal, under either `window.dialogStyle` | Not measured here: the custom style is the nightly keyboard suite's (§12.6, not built yet); the native style needs a desktop. **Laptop ritual** for native. |
| 5 | Whether a remote host can run `workbench.action.terminal.newLocal` then `sendSequence` | Not measured: needs a real remote extension host. **Laptop ritual.** |
| 6 | `application`-scoped settings are ignored in remote and workspace settings; remote settings cannot move the local host's `http.*` | **Workspace half: confirmed.** A workspace `.vscode/settings.json` setting `drydock.url` does not change the effective value, and VS Code reports no workspace value for it, while `editor.tabSize` from the same file applies (the control). **Remote half: not measured** — needs a real remote. **Laptop ritual.** |
| 7 | VS Code's default keybindings can be read from an extension | **Right.** `openTextDocument(vscode://defaultsettings/keybindings.json)` returns the document (1054 bindings on 1.105.1), extensions' contributions included; the `openDefaultKeybindingsFile` fallback is not needed. |
| 8 | `notification.acceptPrimaryAction`'s default key per platform | Not measured: a per-platform keymap question. **The keymap ritual** (§12.11). |
| 9 | VS Code's test resolver can be built and loaded for a real remote host in CI | Not attempted. The tier keeps a reported `remoteName` and workspace settings, as designed. |

- **Verified against:** VS Code 1.105.1 (linux-x64, the pinned archive, checked by SHA-256), Node
  24, Ubuntu noble, headless under `xvfb-run`, `--password-store=basic`.
- **Date:** 2026-10-13.
- **Harness:** the extension-host tier itself — `extension/test/host/` (see *Reproducing*). Its
  specs are the measurements, and it writes each one to a results file.

---

## Results

### 1 — Trusting a test CA in the extension host

The extension-host tier launched VS Code with `NODE_EXTRA_CA_CERTS` naming the tier's CA, in the
environment of the whole VS Code process and again in `extensionTestsEnv`. Inside the extension
host, `process.env.NODE_EXTRA_CA_CERTS` named the CA, and `http.systemCertificates` was `true`. The
extension's sign-in — `fetch` of `GET /api/meta` through the tier's Caddy — never reached the server
(the tap saw nothing), and a direct `fetch` of the wrong-host Caddy failed with
`UNABLE_TO_VERIFY_LEAF_SIGNATURE`: the chain was not trusted at all, so the name was never checked.

With the CA installed in the system store (`test/extension/system-ca.sh`: `/usr/local/share/ca-certificates`,
`update-ca-certificates`) the same run signs in over HTTPS, and the wrong-host Caddy — a valid chain
for `other.test` — fails with `ERR_TLS_CERT_ALTNAME_INVALID`. That is the control: validation is on,
and it is the system store doing the work.

**Consequence:** the extension-host tier needs root once per run to trust its CA. CI does it on a
runner that lives for one job. `test/extension/run.sh host` never edits a trust store: it refuses
unless `DRYDOCK_EXTENSION_CERTS` names a CA the system store already verifies, and prints the two
commands that make (and later remove) one. The client tier, which runs the extension's client in
plain Node, keeps `NODE_EXTRA_CA_CERTS`.

What this does **not** say: why VS Code's patched `fetch` ignores the variable (plausibly its proxy
agent passes the certificates it loads as `ca`, replacing Node's defaults). It is a measurement of
1.105.1 and is re-measured on every bump.

### 6 — `application` scope against workspace settings

User settings: `drydock.url` = the stack's origin. Workspace `.vscode/settings.json`:
`drydock.url` = `https://other.test` and `editor.tabSize` = 7. In the extension host,
`getConfiguration('drydock').get('url')` is the stack's origin and `inspect('url').workspaceValue`
is `undefined`; `editor.tabSize` is 7. The remote-settings half (a container's
`customizations.vscode.settings`) needs a real container window.

### 7 — Reading the default keybindings

`vscode.workspace.openTextDocument(Uri.parse('vscode://defaultsettings/keybindings.json'))` resolves
from an extension, with VS Code's defaults and every installed extension's contributions (ours,
`drydock.go` on `ctrl+shift+alt+d`, among them — the tier's control that the chord is found when it
is there). No default binding uses the hub's chord on Linux; `ctrl+shift+p` is
`workbench.action.showCommands` (the second control).

---

## Reproducing

```sh
cd extension && npm ci && cd ..
dir=$(sudo test/extension/system-ca.sh install)   # name-constrained; its key deleted once the leaves are signed
DRYDOCK_EXTENSION_CERTS=$dir DRYDOCK_HOST_RESULTS=/tmp/results.json test/extension/run.sh host
sudo test/extension/system-ca.sh remove "$dir"    # untrusts it and deletes the directory
```

`/tmp/results.json` holds each spec's result and the measurements above (`spike`). For #1's
negative — the CA named by `NODE_EXTRA_CA_CERTS` alone, not in the system store — run
`DRYDOCK_SPIKE_05_NEGATIVE=1 DRYDOCK_HOST_RESULTS=/tmp/negative.json test/extension/run.sh host`:
the run fails, the sign-in spec with nothing reaching the tap, and `1-wrong-host-control` is absent
because the wrong-host fetch failed with `UNABLE_TO_VERIFY_LEAF_SIGNATURE` instead.

**The system-store CA** (`system-ca.sh`) is a root trusted machine-wide for the run, so it is held
to what the run needs: critical `nameConstraints` permitting only `drydock.test`,
`drydock-preview.test` (and its subdomains) and `other.test`, and excluding every IP address; its
private key deleted as soon as the three leaves are signed; its directory made by `mktemp` as root,
`0700`, and trusted before it is handed to the invoking user. `run.sh host` refuses a trusted CA
without the constraint, or with its key still on disk.

The items marked *laptop ritual* belong to `test/rituals/vscode-bump.md` and
`test/rituals/vscode-connect.md` (§12.11), which E1's last PRs write; until then they stay open in
the design's §14.
