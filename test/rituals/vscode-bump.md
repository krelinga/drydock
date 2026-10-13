# Ritual: moving the VS Code pin, or the Remote extensions

**Run it** in either of these cases (vscode-extension §12.11):

- **VS Code:** you move the VS Code that the extension-host tier pins
  (`extension/test/host/vscode.json`, and `engines.vscode` with it).
- **The Remote extensions:** Remote - SSH or Dev Containers on the laptop updates past the version
  `internal/vscode` was read from, Dev Containers **0.470.0**. That is the day you notice it: VS Code
  updates them on its own.

Everything in it lands in one PR: the pins, the re-measured spike, and any code a changed result
requires. It is a `fix:` if code changed, `test:` if only the pin and the measurements did.

## A. Move the VS Code pin (skip for a Remote-extensions-only bump)

1. **Find the build.**
   - **Do:**

     ```sh
     ver=1.NNN.N
     curl -fsSL "https://update.code.visualstudio.com/api/versions/$ver/linux-x64/stable" | jq '{url, sha256hash, productVersion}'
     ```

   - **Expect:** a `url` on `vscode.download.prss.microsoft.com` and a 64-hex `sha256hash`.
   - **If not:** not a published stable build. Pick another.
2. **Check it before trusting it.**
   - **Do:**

     ```sh
     curl -fsSLo /tmp/code.tar.gz "<url>"
     echo "<sha256hash>  /tmp/code.tar.gz" | sha256sum -c
     ```

   - **Expect:** `OK`.
   - **If not:** stop. The update API and the download disagree, and neither is pinned until they
     agree.
3. **Pin it.**
   - **Do:**
     - Write `version`, `url` and `sha256` into `extension/test/host/vscode.json`.
     - Set `engines.vscode` in `extension/package.json` to `^1.NNN.0`, the new minor.
     - Set `@types/vscode` to `1.NNN.0`. `test/manifest.spec.ts` holds `engines.vscode` to the
       types' minor.
     - Run `npm install` in `extension/` so the lock file follows.
   - **Expect:** `npm run check` in `extension/` passes.
   - **If not:** a type error is a VS Code API change. Fix the adapter in the same PR.
4. **Re-measure what CI can.** This is Spike 05 #1, #6's workspace half, and #7.
   - **Do:** run the report's *Reproducing* block (`test/extension/system-ca.sh install`,
     `test/extension/run.sh host` with `DRYDOCK_HOST_RESULTS`, then `system-ca.sh remove`), and
     the negative (`DRYDOCK_SPIKE_05_NEGATIVE=1`).
   - **Expect:**
     - Every spec passes.
     - The results file's `spike` entries match the report's *Answers so far*.
     - The negative fails exactly as the report says.
   - **If not:** a changed answer is a design question.
     - Update the report's row and its *Results* section.
     - Apply the row's *If wrong* from vscode-extension §12.9.
     - Bring it to the owner in the PR body.
     - #1 turning *right* (the extension host honouring `NODE_EXTRA_CA_CERTS`) changes nothing in
       CI by itself: record it.
5. **Update the report's *Verified against* line** to the new version.

## B. Re-read the attach authority in Dev Containers (always)

`internal/vscode` builds `attached-container+<hex>@ssh-remote+<host>` from how Dev Containers 0.470.0
writes it for *Attach to Running Container…* from a Remote-SSH window: the `NV` and `kk` functions,
which are minified names and change between versions. Re-read the same thing in the version now
installed:

1. **Open the bundle.**
   - **Do:** on the laptop, open the installed extension's directory,
     `~/.vscode/extensions/ms-vscode-remote.remote-containers-<version>/`
     (`%USERPROFILE%\.vscode\extensions\…` on Windows), and note `<version>`. Its `package.json`'s
     `main` names the bundle.
2. **The container authority.**
   - **Do:** search the bundle for `attached-container`. Follow the code that builds the authority
     from a container.
   - **Expect:**
     - It hex-encodes the JSON `{"containerName":"/<name>"}`, with the leading `/` kept.
     - Then it appends `@` and the window's own authority.
     - Its resolver splits `name+config@parent` and reads `containerName` from the hex JSON.
3. **The SSH authority.**
   - **Do:** search Remote - SSH's bundle (`ms-vscode-remote.remote-ssh-<version>/`, the file its
     `package.json`'s `main` names) for `ssh-remote+`.
   - **Expect:** a bare lowercase host is written as is. A host with a user, a port or capitals is
     hex of `{"hostName","user","port"}`.
4. **If either changed:**
   - Update `internal/vscode` and its golden strings in `vscode_test.go`
     (`TestAttachURLIsTheExtensionsOwn`) to the new form.
   - Update core's `vscodeURL` (`web/src/core/`), which checks the shape on both clients.
   - Update the version named in the package comment, in the test's comment, and in
     vscode-extension §12.11.
5. **If neither changed:** move the version named in those three places anyway, so the next reader
   knows which version was read.

## C. Run the laptop rituals

1. **[`vscode-connect.md`](vscode-connect.md), all of it.** Its part 2 re-measures Spike 05 #2, #3,
   #4 (native) and #5, and #6's remote half, on the new versions. Its 1.4 is the proof that part B's
   reading is right: the server's link opens the right container.
2. **[`vscode-keymap.md`](vscode-keymap.md)** on each OS you have: a VS Code bump can take a key.

## D. Record

- In [Spike 05's report](../../docs/design/spikes/05-vscode-extension-host.md):
  - the *Verified against* line (VS Code);
  - the versions of Remote - SSH and Dev Containers in *Laptop ritual runs*;
  - every row whose result moved.
- Then vscode-extension §14 for a result that changed.
- Open the PR with the pins and the report together.
- Its body names the old and new versions, and every Spike 05 answer that moved, or *no answer
  moved*.
