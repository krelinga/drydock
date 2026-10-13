# Ritual: the extension's keys on macOS and Windows

**Run it** when the extension's default keybinding changes (`contributes.keybindings` in
`extension/package.json`), and when the pinned VS Code moves ([`vscode-bump.md`](vscode-bump.md)).
CI checks VS Code's Linux defaults (§12.5's keybinding spec). This covers what a Linux runner cannot
see (vscode-extension §6.2, §12.11):

- the defaults on macOS and Windows;
- the operating system's own shortcuts;
- Spike 05 #8, `notification.acceptPrimaryAction`'s default key on each platform.

Run it on each OS you have. An OS not run is recorded as *not run*, never as a pass. It takes ten
minutes per OS.

The keys under test:

| | macOS | Windows |
|---|---|---|
| The hub, `drydock.go` | `cmd+alt+shift+d` (⌘⌥⇧D) | `ctrl+alt+shift+d` |
| `notification.acceptPrimaryAction` | measured below | measured below |

You need the extension installed and signed in ([`vscode-connect.md`](vscode-connect.md) 1.1–1.2).
Note the OS version and VS Code's *Help → About* in the run record at the end.

## 1. Nothing else in VS Code holds the hub's chord

**Do:**

1. Open *Preferences: Open Keyboard Shortcuts* (`cmd+k cmd+s` / `ctrl+k ctrl+s`).
2. Press the keyboard icon at the right of the search box (*Record Keys*).
3. Press the hub's chord.

**Expect:** one row, *Drydock: Go to Workspace…*, source *Extension*, with no *When* clause.

**If not:** any other row is a conflict.

- **A VS Code default:** the chord has to change. Choose a new one with the owner, change
  `extension/package.json`, `extension/README.md` and vscode-extension §6.2, and run this ritual
  again.
- **Another extension you installed:** that is a local conflict, not a default. Record it; it does
  not block.

**Record:** the rows listed.

## 2. The operating system lets the chord through

**Do:**

1. Close the Keyboard Shortcuts editor.
2. With the focus in an editor, press the chord.
3. Press Escape.
4. With the focus in the Explorer, press it again.

**Expect:** the hub opens both times.

**If not:** something outside VS Code takes the chord. Find it and record its name:

- **macOS:** *System Settings → Keyboard → Keyboard Shortcuts…*. Look through every section,
  *App Shortcuts* and *Services* especially, for ⌘⌥⇧D.
- **Windows:** keyboard utilities (PowerToys' *Keyboard Manager*), graphics-driver and game
  overlays, and any app with global hotkeys.

An OS default that takes it means the chord has to change, as in step 1. A third-party tool's
binding is local, and is recorded but does not block.

**Record:** pass, or what took it.

## 3. Spike 05 #8: `notification.acceptPrimaryAction`'s key on this OS

**Do:**

1. In Keyboard Shortcuts, type `notification.acceptPrimaryAction` in the search box. Note its key,
   or *none*.
2. Press *Record Keys* and press that key. Note every row.
3. Close the editor and run *Drydock: Sign In*. A notification appears whose primary action is
   *Paste a token*. Do not click it.
4. Press the key from step 1.
5. Press Escape. That stores nothing: the token you have stays.

**Expect:**

- Step 2: one row, unless the others are clearly scoped elsewhere by their *When* clause.
- Step 4: the paste box opens. The key pressed the notification's primary action.

**If not:**

- **No default key on this OS:** design §6.2 promises a key that does not exist here. Update §6.2
  and the README to say so. The hub's *ready* row stays the keyboard path, and the notification is
  only a convenience (§12.9 #8's *If wrong*).
- **The key does nothing:** the same consequence. Record it and update §6.2.

**Record:** the key on this OS, and whether it pressed the action.

## Recording

Paste the run record into *Laptop ritual runs* in
[Spike 05's report](../../docs/design/spikes/05-vscode-extension-host.md), and put #8's per-OS
result in that row of *Answers so far*. A chord that has to change is a `fix:` PR to the extension,
with this ritual run again on it.

```text
Ritual: vscode-keymap        Date:         Extension: vX.Y.Z
OS (and version):            VS Code:      (version, commit)
1 VS Code conflicts ......... none / <rows>
2 OS lets it through ......... pass / taken by <what>
3 #8 acceptPrimaryAction ..... key <key> / none; presses the action: yes/no
Notes:
```
