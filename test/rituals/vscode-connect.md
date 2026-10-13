# Ritual: the VS Code extension, connected for real

**Run it** before each release that changes `extension/`, as part of the release smoke test
(testing §11.4), and from [`vscode-bump.md`](vscode-bump.md). It covers what CI cannot
(vscode-extension §12.11):

- **Part 1:** the E1 flow end to end on the owner's laptop, over Remote-SSH to the real Drydock
  server.
- **Part 2:** Spike 05's measurements that need a real container window: #2, #3, #4 (native
  dialogs), #5, and #6's remote half.
- **Part 3:** cleanup.

It takes about an hour, most of it builds.

Each step gives four things:

- **Do:** the exact action.
- **Expect:** what a pass looks like.
- **If not:** what a failure means.
- **Record:** where the result goes.

"The run record" is the block at the end of this file. Copy it into a scratch file before you start,
fill it in as you go, and paste it into the report at the end.

Keys are written for Linux and Windows. On macOS read `cmd` for `ctrl` in the hub's chord
(`cmd+alt+shift+d`, ⌘⌥⇧D).

## Safety: what this ritual will and will not touch

- **The probe** (`vscode-probe/`) plays a hostile extension. It is installed **only** into the
  container of the throwaway `krelinga/drydock-testbed-a` workspace that part 1 clones, and that
  workspace is deleted in part 3.
  - It is never installed on the laptop, and never into the docker-in-docker repository's container.
  - Each of its commands checks that it is in a container, and does nothing otherwise.
  - It reads no credential, sends nothing over the network, and types only `echo <marker>; hostname`.
- **The token** is pasted in exactly one place, *Drydock: Sign In*'s password box. Never paste it into
  the run record, a terminal, an issue or a chat.
  - Nothing here reads VS Code's secret storage, the browser's cookies, or `~/.ssh`.
- **Every setting a step changes is undone by a later step**, and part 3 lists them all again.

## Before you start

| Need | Check |
|---|---|
| The release under test is running on the server | On the server, `drydock version` prints `vX.Y.Z`. |
| VS Code on the laptop | Note *Help → About* (version, commit, OS) in the run record. |
| Remote - SSH and Dev Containers, installed locally | *Extensions*: `ms-vscode-remote.remote-ssh`, `ms-vscode-remote.remote-containers`. Note both versions; `internal/vscode` was read from Dev Containers 0.470.0, and a newer one means running [`vscode-bump.md`](vscode-bump.md) as well. |
| Remote-SSH reaches the server at Drydock's `--vscode-ssh-host` | On the server: `grep DRYDOCK_VSCODE_SSH_HOST /etc/drydock/drydock.env`. On the laptop, *Remote-SSH: Connect to Host…* with that exact value opens a window, and in its terminal `docker ps` works. ([first-deployment.md](../../docs/deploy/first-deployment.md) sets this up.) |
| `code` on the laptop's `PATH` | `code --version` in a laptop terminal. On macOS: *Shell Command: Install 'code' command in PATH*. |
| Python 3 on the laptop (to pack the probe) | `python3 --version`. |
| Two repositories the App can see | `krelinga/drydock-testbed-a` (a fixture: plain dev container, nothing to approve), and a docker-in-docker repository of yours (its configuration asks for `privileged`, so it stops for approval). |
| Neither repository has a workspace yet | Delete any left over in the web UI first. |

## Part 1: end to end

### 1.1 Install the release's `.vsix`

**Do**, in a laptop terminal, with the release under test (for a draft not yet published, swap the
two `curl`s for `gh release download vX.Y.Z -R krelinga/drydock -p drydock-vscode.vsix -p SHA256SUMS`):

```sh
v=vX.Y.Z
mkdir -p ~/drydock-ritual && cd ~/drydock-ritual
curl -fsSLO "https://github.com/krelinga/drydock/releases/download/$v/drydock-vscode.vsix"
curl -fsSLO "https://github.com/krelinga/drydock/releases/download/$v/SHA256SUMS"
grep '  drydock-vscode.vsix$' SHA256SUMS | shasum -a 256 -c     # Linux: sha256sum -c
code --install-extension drydock-vscode.vsix
code --list-extensions --show-versions | grep '^krelinga.drydock@'
```

**Expect:** `drydock-vscode.vsix: OK`, then `krelinga.drydock@X.Y.Z` with the release's version. Restart
VS Code. The activity bar has *Drydock*.

**If not:**

- A checksum mismatch means the asset is not what the release built. Stop, and file it against the
  release.
- A wrong version means the release's version stamps drifted (`test/docs`'s
  `extension_version_test.go` should have caught that).

**Record:** VS Code, extension and Remote extension versions.

### 1.2 Sign in with a minted token

**Do:**

1. In VS Code's **user** settings, set `drydock.url` to the UI origin, for example
   `https://drydock.example.com`.
2. In the browser, open Drydock's **Settings → API tokens → Create token**:
   - Label: `VS Code on <laptop>`.
   - Tick **Approve host access**. The warning beside it is the web card's sentence.
   - Keep the default lifetime.
   - Press **Create token**, then **Copy**. Leave the page open.
3. In VS Code, run *Drydock: Sign In* (`ctrl+shift+p`) and choose *Paste a token*.
4. **Negative first:** paste `drydock_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA`, a well-formed
   token that does not exist.
5. Then run *Sign In* again and paste the real token.
6. Press **Done** on the web page.

**Expect:**

- The fake token: *Drydock refused this token: it was revoked or has expired, …*, and the views stay
  signed out.
- The real token: *Signed in to Drydock vX.Y.Z as "VS Code on <laptop>".*
- The *Workspaces* and *Repositories* views fill.
- *Drydock: Show Log* has `signed in to https://… (Drydock vX.Y.Z, api 1) as "…", scopes read,
  lifecycle, approve`.
- Searching that output (`ctrl+f`) for `drydock_` finds nothing.

**If not:**

- The fake token accepted means the server's token check or the extension's `GET /api/meta` handshake
  is broken. This is a release blocker.
- A refusal of the real token, with the server on the same release, is a bug in the token path.
- `drydock_` in the log means the redaction rule broke (§12.8). That is a blocker; also revoke the
  token in Settings.

**Record:** pass/fail. Do not record the token.

### 1.3 Clone the plain repository with the keyboard only

**Do:**

1. `ctrl+alt+shift+d` (the hub).
2. Type `testbed-a`. *krelinga/drydock-testbed-a* is listed under repositories.
3. Press Enter, then keep your hands off the mouse.

**Expect:**

- A progress notification follows the steps. It does not take focus: keep typing in an editor to
  check.
- It ends *krelinga/drydock-testbed-a is running. Connect opens it over SSH to `<host>`.*, with
  **Connect**. `<host>` is exactly the server's `--vscode-ssh-host`.
- The hub (`ctrl+alt+shift+d`) now lists the workspace first, as *ready*.

**If not:**

- A step failure shows the step's own message. Compare it with the web UI's card, which shows the
  same workspace.
- A host other than the configured one is a bug in the link. Do not connect.
- Focus stolen mid-typing breaks frontend's *nothing takes focus* rule (§6.2).

**Record:** pass/fail, and the time from Enter to *running*.

### 1.4 Connect

**Do:** `ctrl+alt+shift+d`, then Enter on the running *krelinga/drydock-testbed-a*. (Pressing the
notification's key is checked in [`vscode-keymap.md`](vscode-keymap.md), not here.)

**Expect:**

- A **new** window opens, with no *Allow opening this URI?* prompt.
- Its remote indicator (bottom left) names a container on the SSH host.
- The Explorer's folder is the clone: in its terminal, `pwd` prints `/workspaces/repo` (or the path
  the repository's `workspaceFolder` sets), and `git remote -v` names `krelinga/drydock-testbed-a`.
- Back in the window you connected **from**, *Drydock: Show Log* has
  `connecting to workspace <id> over SSH to <host>`, with the same host as 1.3.

**If not:**

- No window, or *Could not find container*, means the link's authority no longer matches what Dev
  Containers resolves. Run [`vscode-bump.md`](vscode-bump.md)'s authority step.
- A window on the SSH host but outside the container means the same.
- The wrong folder means `internal/vscode`'s folder rule is out of date.

**Record:** pass/fail.

### 1.5 The container window refuses mutations (rule 1; Spike 05 #3)

**Do:** in the new container window:

1. Run *Drydock: Refresh Repositories*.
2. Run *Drydock: Show Log*.
3. Open the hub.

**Expect:**

- The refresh shows *Do this from a local or SSH window, or in Drydock: this window may run code from
  the workspace it is attached to.*, with *Open in Drydock*.
- This window's log has `drydock.refreshRepositories: refused in a attached-container window (§8
  rule 1)`. The window kind in that line is Spike 05 #3's answer.
- The hub still lists and connects. Reading is allowed there.

**If not:**

- A refresh that runs means rule 1 is not holding. This is a release blocker.
- A kind other than `attached-container` means #3 is wrong. That is harmless by construction (an
  allowlist), but record it and tell the owner.

**Record:** Spike 05 #3 (attached-container half): the logged kind.

### 1.6 The docker-in-docker repository: review, type, approve, connect

**Do:**

1. In a **local** window: `ctrl+alt+shift+d`, type the repository's name, then Enter.
2. Wait for the follow to end *… needs host-access approval: …*. Press **Review**.
3. Read the review document. It lists what the configuration asks for (`privileged` among it), in
   the same sentences as the web card.
4. **Negative first:** press *Approve Host Access…* in the editor title, and type the name with one
   character wrong.
5. Then *Approve Host Access…* again, and type the exact full name (`owner/name`).

**Expect:**

- The wrong name: *That is not this repository's full name, so nothing was approved.* The workspace
  stays waiting (the web card agrees).
- The right name: the follow resumes (*Approved; continuing…*) and ends *running* with **Connect**.
- Show Log has `approved host access for workspace <id> (<hash>)`.
- Connect as in 1.4. In the new window's terminal, `docker run --rm hello-world` prints *Hello from
  Docker!*: the approval did what it said.

**If not:**

- An approval from the wrong name is a release blocker.
- `The configuration changed after it was shown` means the repository's configuration really changed
  between review and approval. Review again; if it repeats with no change, that is a bug.
- `hello-world` failing in an approved docker-in-docker container is a provisioning bug, not the
  extension's. Compare with the web UI.

**Record:** pass/fail.

### 1.7 Push through the broker

**Do:** in the docker-in-docker repository's container window's terminal:

```sh
b=drydock/ritual-$(date +%Y%m%d%H%M%S)
git switch -c "$b"
git commit --allow-empty -m "ritual: vscode-connect push check"
git push -u origin "$b"
git push origin --delete "$b"
git switch -
git branch -D "$b"
```

**Expect:**

- The push succeeds with no credential prompt (the broker's token, scoped to this repository), and
  so does the delete.

**If not:**

- A prompt or `403` is a broker or Feature fault. Run the same push from a workspace opened with the
  web UI's *Open in VS Code* to tell the two apart.

**Record:** pass/fail.

### 1.8 *Reopen in Container* finds Drydock's container (Spike 05 #3, `dev-container`)

**Do:**

1. *Remote-SSH: Connect to Host…* to the server, with the same `--vscode-ssh-host` value.
2. *File → Open Folder…* the testbed workspace's clone, `/srv/drydock/ws/<id>/repo` (the `<id>` is
   the ULID in the web UI's URL, `/ws/<id>`).
3. Before going on, in that window's terminal:
   `docker ps --filter label=devcontainer.local_folder=/srv/drydock/ws/<id>/repo --format '{{.ID}} {{.Names}}'`.
4. Run *Dev Containers: Reopen in Container*.
5. In the new window, run *Drydock: Refresh Repositories*, then *Drydock: Show Log*.

**Expect:**

- Step 3 lists exactly one container, Drydock's.
- The reopen attaches to it without building: its log shows no `docker build` or `docker run`, and
  the same `docker ps` still lists one.
- The refresh is refused, and the log line names `dev-container`.

**If not:**

- If the SSH user cannot read `/srv/drydock/ws/<id>/repo`, the step is *not run*. That is the
  server's file modes, not the extension. Record it so.
- A build, or a second container, means Drydock's `devcontainer.local_folder`/`config_file` labels no
  longer match what Dev Containers looks for (overall §6, *The container is labelled as a dev
  container*). Record it, close that window, and remove the container **VS Code** made:
  `docker rm -f <the new ID>`, never the one listed in step 3.
- Another window kind is #3, as in 1.5.

**Record:** Spike 05 #3 (`dev-container` half): the logged kind, or *not run*.

### 1.9 The token's *last used* moves

**Do:** reload Drydock's Settings page in the browser.

**Expect:** the *VS Code on <laptop>* row's *last used* is within the last few minutes.

**If not:** the server's `last_used_at` bookkeeping is best-effort (§4.2). A stale value with
everything else working is a low-priority bug, not a blocker.

**Record:** pass/fail.

## Part 2: what a container can do to the window (Spike 05)

Everything in this part happens in the **testbed-a** container window from 1.4, or a new one made
the same way. Never use the docker-in-docker repository's window.

### 2.1 Install the probe into the throwaway container

**Do:**

1. On the laptop, from a checkout of this repository at the commit under test:
   `python3 test/rituals/vscode-probe/pack.py ~/drydock-ritual/drydock-ritual-probe.vsix`
   (it prints the file's SHA-256: record it).
2. Drag `drydock-ritual-probe.vsix` from the laptop's file manager onto the Explorer of the
   **testbed-a container window**. VS Code uploads it into the clone.
3. In **that window's** terminal, which runs in the container:

   ```sh
   code --install-extension ./drydock-ritual-probe.vsix && rm ./drydock-ritual-probe.vsix
   ```

4. Check where it landed, in a **laptop** terminal (not the container's):
   `code --list-extensions | grep drydock-ritual`

**Expect:**

- The container's `code` (the remote CLI) says it was installed.
- The Extensions view shows *Drydock ritual probe* under the container's *Installed* section, not
  under *Local*.
- The laptop terminal prints **nothing**.

**If not:** if the probe is listed locally, or the laptop's `grep` prints it, run
`code --uninstall-extension drydock-ritual.probe` in the laptop terminal **now** and stop part 2.
Record *not run: installed locally*.

### 2.2 Where is it?

**Do:** run *Drydock Ritual Probe: Where Am I?* in the container window.

**Expect:**

- *remoteName attached-container, host `<12 hex digits>`; in a container: go on.*
- Its output channel, *Drydock Ritual Probe*, has the same line.

**If not:** *NOT in a container*. Uninstall the probe (2.7) and stop.

**Record:** the `where:` line.

### 2.3 Spike 05 #2: a container's call runs in this window's own instance

**Do:**

1. **Positive control first:** in a **local** window, run *Drydock: Refresh Repositories*.
2. Then in the testbed-a container window, run *Drydock Ritual Probe: #2: Call
   drydock.refreshRepositories*.

**Expect:**

- The control: no refusal sentence, and the local window's log has no `refused` line.
- The probe's call: the refusal sentence appears **in the container window**, and that window's
  Drydock log gains `drydock.refreshRepositories: refused in a attached-container window (§8 rule 1)`.
- Dismiss the sentence. The probe's output then says `ran, returned undefined` (rule 3: nothing
  comes back). The command answers only once the sentence is dismissed, and the probe waits a minute
  for it.
- No other window shows anything.

**If not:**

- The refresh runs, or the refusal or a picker appears in **another** window. Then rule 1 is judging
  the wrong window, and §12.9 #2's *If wrong* applies: mutations leave the extension until VS Code
  offers a caller check. **Stop, record, and tell the owner. This blocks the release.**
- A return value other than `undefined` breaks rule 3. That is also a blocker.

**Record:** Spike 05 #2, with the probe's `#2` lines.

### 2.4 Spike 05 #4: can a command accept a native modal?

**Do:**

1. In the laptop's **user** settings, check `window.dialogStyle` is `native`, the default. If you
   change it, VS Code asks to restart. Reconnect the testbed-a window afterwards.
2. Run *Drydock Ritual Probe: #4: Try to Accept a Modal*. A modal appears.
3. **Press nothing** until the probe's output says to close it, about ten seconds. Then press Cancel
   (or Escape).

**Expect:** the output reads `#4 RESULT: the dialog is still open after every attempt`, with each
command's outcome above it. Then `closed by the operator: dismissed`.

**If not:**

- `#4 RESULT: the dialog closed with no key pressed`. Then a command accepted a native modal, and
  §8's rule 4 is no depth at all. Rule 1 still carries everything, as the design already assumes.
  Record it and tell the owner. This is not a release blocker.
- If the dialog never appeared as a native OS dialog, `window.dialogStyle` was not `native`. Fix
  that and run again.

**Record:** Spike 05 #4 (native): the result line, which acceptors existed (`refused: command …
not found` means it does not exist), and the *for the record* list.

(Optional: set `window.dialogStyle` to `custom`, restart, and run it again. Record that as *custom,
measured on the laptop*: the nightly keyboard suite owns custom. Set it back to `native` afterwards.)

### 2.5 Spike 05 #5: can a container run a command on the laptop?

**Do:** run *Drydock Ritual Probe: #5: Try a Local Terminal*. Then look at the terminal panel's
list of terminals.

**Expect (the control):** a container terminal prints `DRYDOCK-RITUAL-5-CONTROL` and then the
container's hostname, the same one the probe logs at the end.

**Record (the measurement, either way):** look for a second terminal labelled local:

- If one exists and printed `DRYDOCK-RITUAL-5-LOCAL` followed by **the laptop's** hostname, then
  container code can run code on the laptop.
- Otherwise it cannot, as measured. That includes the probe's log showing `newLocal` or
  `sendSequence` refused.

**If the control fails:** the probe's commands do not reach the workbench at all, and the
measurement is void. Record *inconclusive*.

**What "can" means:** it is the residual risk of every attach (vscode-extension §8.2), not a bug to
fix in the extension. Record it in Spike 05 #5, and tell the owner: it weighs on §14's open
question 1.

Close both terminals afterwards.

### 2.6 Spike 05 #6, remote half: remote settings cannot move `drydock.url` or the local `http.*`

**Do:**

1. **The signal's positive control, in a local window:**
   1. Add `"http.proxy": "http://127.0.0.1:9"` to the laptop's **user** settings JSON. Nothing
      listens on port 9.
   2. *Developer: Reload Window*.
   3. Run *Drydock: Refresh Repositories*.
2. **Remove that line at once**, reload, and check the views work again.
3. In the testbed-a container window, run *Preferences: Open Remote Settings (JSON)*. It edits the
   container's `~/.vscode-server/data/Machine/settings.json`, the file Dev Containers fills from
   `customizations.vscode.settings`. Add:

   ```json
   "drydock.url": "https://other.test",
   "http.proxy": "http://127.0.0.1:9",
   "http.proxyStrictSSL": false,
   "http.systemCertificates": false,
   "editor.detectIndentation": false,
   "editor.tabSize": 7
   ```

4. Save, then *Developer: Reload Window*.
5. Open any file from the clone.
6. Open the Drydock views, the hub, and *Drydock: Show Log*.

**Expect:**

- Step 1: the refresh fails, and the log names a connection failure. This proves a moved `http.proxy`
  would be seen.
- Step 5 (the settings file applies): the status bar says *Spaces: 7*.
- Step 6:
  - The views and the hub list the real Drydock's workspaces.
  - Nothing mentions `other.test`.
  - No request in the log fails on the proxy or a certificate.
- *Preferences: Open Settings (UI)*, *Remote* tab, search `drydock.url`: VS Code marks it as not
  applying there.

**If not:**

- Step 1 does not fail: the local host's `fetch` ignores `http.proxy` altogether. The proxy half of
  this measurement is void; record *inconclusive (proxy)*.
- The views go to `other.test`, or ask to sign in to it: `application` scope does not hold against
  remote settings (§6.4). **A release blocker; tell the owner.**
- The views fail on the proxy, or on a certificate, while user settings are clean: remote settings
  move the local host's `http.*`. Per §12.9 #6, the client must build its own TLS agent. Record it
  and tell the owner.
- No *Spaces: 7*: the remote settings file did not apply at all, so the measurement is void. Check
  you edited the *Remote* settings, not the user ones.

**Then:** delete those six lines from the remote settings, save, and reload.

**Record:** Spike 05 #6 (remote half), with step 1's outcome.

### 2.7 Remove the probe

**Do:**

1. In the testbed-a container window's terminal:
   `code --uninstall-extension drydock-ritual.probe`
2. *Developer: Reload Window*.
3. Close the window.
4. In a laptop terminal:

   ```sh
   code --list-extensions | grep drydock-ritual                      # prints nothing
   ls ~/.vscode/extensions | grep drydock-ritual                     # prints nothing
   rm -f ~/drydock-ritual/drydock-ritual-probe.vsix
   ```

**Expect:** the probe is gone from the container's Extensions list, and both laptop checks print
nothing. Part 3 deletes the container itself.

## Part 3: cleanup

1. **The testbed workspace:** in the web UI, open it and press **Delete**, typing
   `krelinga/drydock-testbed-a`. That removes the container the probe was in.
2. **The docker-in-docker workspace:** keep it or delete it, as you like. The approval stays on its
   repository either way.
3. **Settings changed in this ritual. Check each is back:**
   - user `http.proxy` (2.6): absent;
   - the remote settings' six lines (2.6): absent;
   - `window.dialogStyle` (2.4): `native`.
4. **The token:** keep it if this laptop keeps using the extension. Otherwise run *Drydock: Sign Out*,
   which revokes it, and check the Settings row says *Revoked*.
5. `rm -rf ~/drydock-ritual`.
6. **The record:**
   - Paste the run record into the *Laptop ritual runs* section of
     [Spike 05's report](../../docs/design/spikes/05-vscode-extension-host.md).
   - Update the *Result* cell of every Spike 05 row this run measured.
   - A changed result goes to vscode-extension §14 as well, as a decision for the owner.
   - A failed part-1 step is an issue titled `ritual: vscode-connect <step>`.
   - Open a `docs:` PR with the report change.

## The run record

```text
Ritual: vscode-connect            Date:            Release: vX.Y.Z (server and .vsix)
Laptop OS:                        VS Code:         (version, commit)
Remote - SSH:                     Dev Containers:
1.1 install ............ pass/fail   1.2 sign in ............. pass/fail
1.3 clone .............. pass/fail   (Enter → running: __ min)
1.4 connect ............ pass/fail   1.5 rule 1 ............. pass/fail
1.6 approve + dind ..... pass/fail   1.7 push ............... pass/fail
1.8 reopen ............. pass/fail/not run   1.9 last used .......... pass/fail
Probe .vsix SHA-256:
#2  (2.3):
#3  attached-container (1.5):          dev-container (1.8):
#4  native (2.4):                      custom (optional):
#5  (2.5):
#6  remote half (2.6):                 proxy control (2.6 step 1):
Notes:
```
