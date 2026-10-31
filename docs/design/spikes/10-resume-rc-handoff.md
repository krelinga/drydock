# Spike 10 — Can a laptop terminal take over a conversation started from the phone?

**Question** (the owner's use case): create a workspace from the phone, explore in a session the
Claude app opened on the workspace's `claude remote-control` server, then carry on building from the
laptop in VS Code's terminal, in the same container, with the whole conversation, and still see it
in the app. [Spike 06](06-terminal-attach.md) found that plain `claude --resume` of a server
session gives the terminal a private copy the app never sees, and that `claude --rc` gives a new
conversation shared with the app. This spike tests the two together, using documented flags only:
`claude --resume <id> --rc`, with `--fork-session`, `--continue --rc`, `--resume` then
`/remote-control`, and the picker.

**Answer: yes on the terminal side (measured); the app side is owed (H1–H5).** `claude --resume
<id> --rc` in the clone's folder is accepted while the server is still serving that session. Nothing
refuses it, the server is untouched, and the terminal has the whole history: it recalled a marker
word sent before the hand-off. Remote Control comes up on a **new** app session, not the server's,
and the bridge uploads the conversation into it (`Flushing N history events`). The phone's original
session stays live in the environment and goes stale from then on. So this is a hand-off by copy:
the laptop gets the conversation and a new app session that carries it. It is not a move of the
phone's session. Without `--fork-session` the two processes share one local session id and one
transcript file. With `--fork-session` the terminal gets its own id and file, with the history
copied, and the server's file is left alone. **The variant to use is `claude --resume <id>
--fork-session --rc`, or with no id, `claude --resume --fork-session --rc`**, which opens Claude
Code's own picker. That needs nothing from Drydock.

- **Verified against:** Claude Code `2.1.289` (`claude --version`), Linux, `tmux` 3.4 (a private `-L
  dd-spike-10` server). Every process ran on the host under `env -i` with only a Drydock container's
  variables (`CLAUDE_CONFIG_DIR` on a scratch config seeded like the shared volume,
  `DISABLE_AUTOUPDATER=1`, a scratch `HOME`), as in Spike 06. **Not in a workspace container:** no
  Feature environment, no `CLAUDE_ENV_FILE` prelude, one pid namespace.
- **Date:** 2026-10-31.
- **Harness:** [`harness-10-resume-rc/`](harness-10-resume-rc/), re-runnable. Each result names its
  mode. `handoff` ran twice on fresh scratches with the same results, and a third time by accident on
  a used one, which is why the harness now refuses a used scratch.
- **Measured vs read.** Every result is *measured* unless marked *read* (read from the binary's
  bundled JavaScript, nothing run).
- **The stand-in.** Only the app can send a turn into a server session. Unattended, the history was
  one turn typed into a terminal that `--continue` had opened on the server's pre-created session
  (Spike 06's method). The terminal therefore wrote that turn, not the server's session process. In
  the real case the server's process writes the app's turns into the same file (Spike 06 H1), so the
  local history a resume reads is the same kind of thing. Whether the server's process also leaves
  anything a resume would act on is H1's to check (result 2's last row).

---

## Results

| # | Measurement | Result | Mode |
|---|---|---|---|
| 1 | Flags | `-r, --resume [value]` (an id, or the picker with an optional search term), `-c, --continue`, `--fork-session` (*"When resuming, create a new session ID instead of reusing the original"*), `--remote-control [name]` (`--rc` its hidden alias, Spike 06), `-w, --worktree [name]` | `cli` |
| 1 | What the bridge does with a resumed conversation (*read*) | mints a session and uploads the history unless suppressed: the only suppressions in the strings are for an account mismatch (`… minting fresh (history not uploaded)`, `Persistence backfill suppressed (cross-account veto or foreign binding)`) | `cli` |
| 2 | `--resume <server's local id> --rc`, server live and serving it | accepted, no refusal; `/remote-control is active · … https://claude.ai/code/session_…` (plain text, **no OSC 8**), a **new** app session; debug log `Created session cse_…`, `Flushing 2 history events` | `handoff` (a) |
| 2 | History in the terminal | recalled the marker (read from the transcript, an answer written after the question) | `handoff` (a) |
| 2 | Effect on the server | none: `Capacity: 1/4` before and after, its session process alive, its debug log gained 1 line (a heartbeat) across the terminal's turn | `handoff` (a) |
| 2 | Who holds the local id | **two processes**: the server's (`sdk-cli`, the server's app session) and the terminal's (`cli`, the new one); the transcript stayed one chain, 0 invalid lines | `handoff` (a) |
| 2 | Does the server's session leave a pointer a resume would follow? (stand-in) | no `bridge-session` record in its transcript and no mention of its app session id; `bridge-pointer.json` in the project directory names the server's session (`source: "standalone"`) | `handoff` |
| 3 | `--resume <id> --fork-session --rc` | a new local id **and** a new app session; `Flushing 4 history events` (every turn so far); marker recalled; the server's transcript unchanged (4 → 4 records) | `handoff` (b) |
| 3 | `--resume --fork-session --rc` with no id | the picker lists the conversation by its first prompt (`QUOKKA… · 1 second ago · master · 221.3KB`); Enter → a new local id, a new app session, `Flushing 2 history events`, marker recalled, server unchanged | `picker` |
| 4 | `--continue --rc` | opened the folder's **most recent** conversation, which was (b)'s fork, not the server's session, and **reattached (b)'s app session** (`Reattaching to persisted bridge session …`) | `handoff` (c) |
| 4 | `--resume <id>` with no flag, then `/remote-control` | the plain resume **already reattached (a)'s app session**: its registry row named it before any command was typed. `/remote-control` then opened a panel (*This session is available … Disconnect this session · Show QR code · Continue*) | `handoff` (d) |
| 4 | Each reattach | an inbound `control_request subtype=end_session` replayed from the stream and answered `result=error`; the session stayed active and answered | `handoff` (c), (d), (e) |
| 5 | Server `SIGTERM`ed, then `--resume <id> --rc` | reattached (a)'s app session (`at seq 18`, `Unarchive … status=200`); marker recalled | `handoff` (e) |
| 5 | Server restarted while (e) holds the conversation | same environment, same server session, `Capacity: 1/4`; two processes on the local id again; (e) unaffected | `handoff` (e2) |
| 6 | A second `--resume <id> --rc` while (e) holds it | no refusal: `Explicit enable is taking over bridge session cse_… from local pid <e>`. When it exited (`Torn down (archive=200)`), (e) closed **~1 s later** with `4090` (`session_not_active`) | `handoff` (f) |
| 7 | `--resume session_…` or `--resume cse_…` (the ids Drydock knows) | not an id to it: the picker opens searching for it, `No sessions match "session_…"` | `ids` |
| 7 | Where the local id is | not in the server's tail or debug log. The session process's argv is `--session-id cse_…` (the app id, not the local one). The local id is a **version-5 UUID** (measured); the binary calls it *"a uuidv5 of the session id in its `cse_` form"* (*read*), but none of the binary's 23 UUID literals is the namespace (measured). The live-session registry `$CLAUDE_CONFIG_DIR/sessions/<pid>.json` maps `bridgeSessionId` → `sessionId` | `ids` |
| 8 | A `claude -w` worktree conversation, `--resume <id>` from the clone's path | accepted, and it **runs in the worktree** (registry `cwd` = `.claude/worktrees/<name>`, from the transcript's `worktree-state` record); marker recalled; with `--rc`, a new app session as in (a) | `worktree` (a), (b) |
| 8 | Same from the worktree's path | same | `worktree` (c) |
| 8 | The picker from the clone's path | does not list the worktree conversation; its footer offers *Ctrl+W to show all worktrees* | `worktree` (d) |
| 8 | A clone conversation resumed from the worktree | runs where it was launched (the worktree) and appends to the clone's transcript file | `worktree` (e) |
| 8 | Either resumed from `$HOME` (no trust record) | the trust menu (`❯ No, exit`), which waits | `worktree` (f) |

### 2 — `--resume <id> --rc`: a copy with its own app session

The combination is accepted as is. The terminal resumes the local conversation, the server's own,
which it reads from the transcript on the shared volume. Turning Remote Control on then mints a
fresh app session and pushes the history into it. The bridge's own lines, in order:

```
[remote-bridge] Created session cse_01W4JFWQXZKE…
[remote-bridge] v2 transport connected
[remote-bridge] Flushing 2 history events
[bridge:repl] handleStateChange state=connected
```

Nothing told the server. It kept serving its session, unchanged, and the registry then listed two
live processes on the one local id. This is Spike 06's "second writer", with Remote Control
added: the terminal's turns go to the new app session and into the shared file. The server's
session process loads its history from the API (Spike 06 result 3), so it never sees them. A turn
the phone sends into the *original* session after the hand-off is the second writer on that file,
and Spike 06 result 4 shows what that does on an interactive conversation: two branches in one file.
For a server session it is still unmeasured (H3).

The harness always gave `--rc` a name (`--rc ddspike10a`). `--help` documents `[name]` as an
optional value, so a bare `--rc` followed by a plain argument may take that argument as the name.
This is *read*, not tried. A command Drydock ships should put `--rc` last.

### 3 — `--fork-session`: the same, without the shared file

`--fork-session` gives the terminal a new local id and a new transcript with the history copied in
(the forked file's records keep their `uuid`s). The bridge uploads all of it, and the server's
file is not touched afterwards. That removes the two-writer problem at its source. The phone's
session and the laptop's are then two conversations that share a past: the phone can keep using
its own, and the laptop's is a separate app session.

With no id, `claude --resume --fork-session --rc` opens the picker scoped to the current folder. It
lists the server's sessions by their first prompt, and Enter on one does the same as (b). That is
the whole hand-off with nothing from Drydock: the operator chooses the conversation by what was said
in it, which is how they would recognise it anyway.

### 4 — A conversation remembers its app session, and `--continue` is not a handle

Once a terminal has held Remote Control on a conversation, the transcript carries `bridge-session`
records naming that app session, and **every later resume of it reattaches that session, `--rc` or
not**. The plain `--resume` in (d) was back on (a)'s app session before anything was typed. Spike
06 result 4 saw the same thing from `--continue`. So a hand-off is sticky: an operator who resumes
the conversation later, for any reason, puts it back in the app.

`--continue --rc` means "the most recent conversation in this folder". After (b), that was the fork,
not the server's session. Spike 06 also saw it open the server's session on one run and find nothing
on another. It cannot be what a button runs.

### 5–6 — Restarts and a second terminal

A server restart while the hand-off holds the conversation brings the server back on its own
environment and session (Spike 02), next to the terminal, and neither notices the other. A second
`--resume <id> --rc` for a conversation another terminal is serving is **not refused**: it takes the
app session over (`Explicit enable is taking over … from local pid`). When it exits it archives the
app session, and the first terminal then drops with `4090 session_not_active`. Without `--rc`, Spike
06 result 4 got `Remote Control not started here … run /remote-control to move it`. With `--rc` the
operator asked for the move, so it happens. Two laptop terminals on one hand-off fight; one per
conversation is the rule.

### 7 — From Drydock's id to the one `--resume` takes

Drydock's `rc_session` rows hold `session_…` ids, taken from the server's OSC 8 links (design §8).
`--resume` does not take them: it opens the picker searching for the string. The local id it needs
appears nowhere in the server's output. The session process is started with `--session-id cse_…`,
and the local id is derived from that: it is a v5 UUID, and the binary describes it as a uuidv5 of
the `cse_` id (*read*). The namespace was not found, so Drydock cannot compute it. The one place
that pairs the two is Claude Code's live-session registry, `$CLAUDE_CONFIG_DIR/sessions/<pid>.json`
(`sessionId`, `bridgeSessionId`, `cwd`). It is an undocumented internal file, it is per pid on a
volume every container shares (Spike 08: pids collide across containers), and it lists only live
processes. Matching on `bridgeSessionId` is exact, so a collision can lose a row but cannot point at
the wrong conversation.

### 8 — Worktrees: run it from the clone

A conversation born in a worktree (`claude -w`, which makes `.claude/worktrees/<name>` on branch
`worktree-<name>` and records a `worktree-state` in its transcript) can be resumed by id **from the
clone's path**, and the resumed process moves into the worktree. The picker from the clone's path
does not list it until *Ctrl+W*. A conversation born in the clone resumed from a worktree stays in
the worktree it was launched in, still writing the clone's file. From an untrusted folder (`$HOME`
here) the trust menu waits, as in Spike 06 result 8. So the command runs in the workspace folder,
which the Feature trusts.

Whether the **server's** `--spawn worktree` sessions are bound the same way (same layout, a
`worktree-state` record, so that a resume from the clone follows them into their worktree) was not
measured. Only the app can make one (H5). Spike 02's question F (does a worktree session need its
own trust record?) is unchanged.

---

## Verdict

**Use `claude --resume <id> --fork-session --rc`, or `claude --resume --fork-session --rc` and pick,
run in the workspace folder in the container's terminal.** On the terminal side it does everything
the owner's flow asks for: it is accepted while the phone's session is live, it brings the whole
local history, it leaves the server and its capacity alone, and it shares the continued conversation
to the app as a new Remote Control session that carries the history. `--fork-session` keeps the
phone's file and the laptop's apart, so nothing writes one transcript twice. Without it the hand-off
also works, but the phone's session and the laptop's share a file and a local id, and the phone can
fork it.

What it is not: a move of the phone's session. The original stays in the environment, with the
history up to the hand-off and nothing after. The continued conversation is a new session, which by
Spike 06's H1 and H2 the app lists under **"Remote Control"**, not in the environment. That is an
extrapolation until H2. Whether the app *shows* the uploaded history, and whether turns flow both
ways, are H2 and H4. The bridge says it uploaded the history; the app has not been looked at.

`--continue --rc` is out (result 4). A plain `--resume` of a conversation that once held Remote
Control re-shares it without asking, and a second `--rc` on a served conversation takes it over and
leaves the first terminal dead when it exits. Both belong in the docs sentence.

## What Drydock would build

1. **Now, nothing but a sentence** (docs and the extension's README). *To carry a conversation from
   the phone to this terminal, run `claude --resume --fork-session --rc` in the workspace folder and
   pick it (Ctrl+W lists worktree sessions). The phone's session stays as it was; the continued
   conversation appears in the app under Remote Control.* Gate it on H2 and H4. Measured on the host,
   so one run in a real workspace container should go with it (Spike 06 owes the same for `claude
   --rc`).
2. **An extension command, "Continue a phone session here"**, for the attached-container window.
   It sends nothing to Drydock and mutates nothing, so it is outside the extension's rule-1 gate
   (extension design §8). It opens an integrated terminal in the container at the workspace folder
   (the window's own folder, which the Feature trusts; never `$HOME`) and runs
   `claude --resume --fork-session --rc`. The picker does the choosing. This needs no API, schema or
   Feature change.
3. **By id, only if the picker proves not enough.** A card or tree item per session would run
   `claude --resume <local id> --fork-session --rc` for one chosen session. That needs (i) the
   session ids in the workspace view (today it carries only the environment link and counts:
   `internal/workspace/view.go`'s `SessionData`), and (ii) the `session_…` → local id mapping, which
   only Claude Code's registry has (result 7). The supervisor could read `sessions/*.json` inside
   the container when it records an `rc_session` row and store the local id beside it. Or a Feature
   helper (`drydock-continue <session_…>`, with a version bump) could do the lookup at click time and
   exec `claude`. Either way it is one more undocumented internal for the §11.1 bump ritual. The
   card itself gets no link: a URL cannot run a terminal command. At most it gets a copyable command.

## Owed (needs a human at the Claude app)

H1–H5 in the [harness README](harness-10-resume-rc/README.md#the-human-steps-h1h5), all from
one run: `./setup.sh /tmp/s10h && S=/tmp/s10h WAIT_APP=300 ./run.sh handoff`.

- **H1** — the real start: a marker turn sent from the app into the server's session. Also checks
  whether the server's process leaves anything in the transcript a resume would follow (result 2's
  last row, which measured only the stand-in).
- **H2** — after `--resume <id> --rc`: where the app lists the new session, whether it shows the
  history, and whether an app turn reaches the terminal and a terminal turn reaches the app.
- **H3** — a turn into the phone's original session after the hand-off: does the shared file fork,
  and does the terminal see it (expected: no)?
- **H4** — H2 for `--fork-session`, the recommended variant.
- **H5** — the same for a session the server spawned in a worktree: where the worktree is, and
  whether a resume from the clone's path follows it there.

## Open questions

- What the app shows for a hand-off session after the terminal exits (`Torn down (archive=200)`;
  a later resume `Unarchive`s it, result 5). This is Spike 06's open question, unchanged.
- What the replayed `end_session` control request answered `error` on each reattach is (result 4).
  It was harmless here.
- How the `uuidv5` namespace is chosen (result 7). It would let Drydock compute the local id
  without the registry.

## Reproducing

```sh
cd docs/design/spikes/harness-10-resume-rc
./setup.sh /tmp/s10a && export S=/tmp/s10a
./run.sh cli; ./run.sh handoff; ./run.sh ids; ./run.sh worktree
./setup.sh /tmp/s10b && S=/tmp/s10b ./run.sh picker
rm -rf /tmp/s10a /tmp/s10b
```

`setup.sh` and every mode refuse unless the copied access token's `expiresAt` is at least an hour
away (Spike 06's fail-closed guard). The scratch holds a live refresh token, so delete it afterwards.
Re-run on a Claude Code bump if the docs sentence or the extension command ships: the history upload,
the bridge pointer and the registry are all undocumented internals of `2.1.289`.
