# test/rituals — procedures run by hand, on an event

A ritual is what testing §11 calls a test that is triggered by an event, never by a commit, and that
needs something CI does not have: a real laptop, a real Remote-SSH connection, another OS. Each is a
checklist with the exact commands, so someone who has forgotten the details can still run it. Each
step gives the action, the expected result, what a failure means, and where the result is recorded.

| Ritual | Run it when | Covers |
|---|---|---|
| [`vscode-connect.md`](vscode-connect.md) | Before each release that changes `extension/`; in the release smoke test (testing §11.4); in the bump ritual | The extension end to end on the owner's laptop over Remote-SSH, and Spike 05 #2, #3, #4 (native dialogs), #5 and #6's remote half in a throwaway container window |
| [`vscode-keymap.md`](vscode-keymap.md) | A default keybinding changes; the pinned VS Code moves | The hub's chord and `notification.acceptPrimaryAction` on macOS and Windows (Spike 05 #8) |
| [`vscode-bump.md`](vscode-bump.md) | The pinned VS Code moves; Remote - SSH or Dev Containers updates past the version `internal/vscode` was read from | The new pin, Spike 05 re-measured, the attach authority re-read, and the other two rituals |

`vscode-probe/` is the throwaway extension `vscode-connect.md` part 2 installs into a throwaway
container. It plays a hostile workspace extension, and it is never installed on the laptop.

Results are written down where the design looks for them. Spike 05's items go in its report,
[`docs/design/spikes/05-vscode-extension-host.md`](../../docs/design/spikes/05-vscode-extension-host.md):
the row's *Result* in *Answers so far*, plus a line in *Laptop ritual runs*. A failed connect step is
an issue titled `ritual: vscode-connect <step>`, and the release it was run for waits for it.

The other rituals in testing §11 (Claude Code bump, the GitHub App contract, the reboot drill, the
Chromium bump) are described there and have no file here yet.
