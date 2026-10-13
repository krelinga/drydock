# Spike 05's harness

Spike 05 ([report](../05-vscode-extension-host.md)) is measured by the extension's own
extension-host tier: the pinned VS Code, headless, running `extension/test/host/inner/suite.ts`
against the client tier's stack. Each measurement is a spec, and the tier writes them to the file
`DRYDOCK_HOST_RESULTS` names (`spike` in it). Rather than a second copy of that machinery, this
directory points at it:

| # | Spec (`extension/test/host/inner/suite.ts`) | Result key |
|---|---|---|
| 1 | *sign in … trusting the tier's CA*; *a certificate for another name is refused …* | `1-env-in-extension-host`, `1-trust`, `1-wrong-host-control` |
| 6 | *workspace settings cannot move drydock.url …* | `6-application-scope-workspace` |
| 7 | *VS Code's default keybindings can be read …* | `7-default-keybindings` |

Run it as the report's *Reproducing* says. The items the report marks for the laptop ritual are
measured by hand, by [`test/rituals/`](../../../../test/rituals/README.md)'s `vscode-connect.md` and
`vscode-keymap.md`.
