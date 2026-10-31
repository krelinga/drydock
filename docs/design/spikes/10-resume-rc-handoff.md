# Spike 10 — Can a laptop terminal take over a conversation started from the phone?

**Question** (the owner's use case): create a workspace from the phone, explore in a session the
Claude app opened on the workspace's `claude remote-control` server, then carry on building from the
laptop in VS Code's terminal, in the same container, with the whole conversation, and still see it
in the app. [Spike 06](06-terminal-attach.md) found that plain `claude --resume` of a server
session gives the terminal a private copy the app never sees, and that `claude --rc` gives a new
conversation shared with the app. This spike tests the two together, using documented flags only:
`claude --resume <id> --rc`, with `--fork-session`, `--continue --rc`, `--resume` then
`/remote-control`, and the picker.

**Answer: yes on the terminal side (measured), as a copy, not a move; the app side is owed
(H1–H5).** `claude --resume <id> --rc` in the conversation's folder is accepted while the server is
still serving that session. Nothing refuses it, the server is untouched, and the terminal has the
whole history: it recalled a marker word sent before the hand-off. Remote Control comes up on a
**new** app session, not the server's, and the bridge uploads the conversation into it (`Flushing N
history events`). The phone's original session stays live in the environment, keeps its place in
the server's capacity, and goes stale from then on. Without `--fork-session` the two processes share
one local session id and one transcript file. With `--fork-session` the terminal gets its own id and
file, with the history copied, and the server's file is left alone.

**The variant to use is `claude --resume --fork-session --rc`** (Claude Code's own picker; or
`claude --resume <id> --fork-session --rc`), **run in the folder the conversation ran in**, then
archive the phone's session. The folder matters: a fork drops a worktree conversation's binding to
its worktree, so run from the clone it continues **in the clone**, and nothing refuses or warns
(result 8, measured). For the workspace's first session that folder is the workspace folder. For a
session that ran in a worktree it is that worktree, which the operator finds from the picker
itself: Ctrl+W in the workspace folder shows each conversation's branch. That needs nothing from
Drydock.

- **Verified against:** Claude Code `2.1.289` (`claude --version`), Linux, `tmux` 3.4 (a private `-L
  dd-spike-10` server). Every process ran on the host under `env -i` with only a Drydock container's
  variables (`CLAUDE_CONFIG_DIR` on a scratch config seeded like the shared volume,
  `DISABLE_AUTOUPDATER=1`, a scratch `HOME`), as in Spike 06. **Not in a workspace container:** no
  Feature environment, no `CLAUDE_ENV_FILE` prelude, one pid namespace.
- **Date:** 2026-10-31.
- **Harness:** [`harness-10-resume-rc/`](harness-10-resume-rc/), re-runnable. Each result names its
  mode. `handoff` ran twice on fresh scratches with the same results, and a third time by accident on
  a used one, which is why the harness now refuses a used scratch. After review, `cli`, `ids`,
  `picker`, `takeover` (new) and `worktree` were re-run with the changes below. `worktree` ran six
  times: the fork rows (g) and (h) on every run; the Ctrl+W picker row (i) on the last three
  (fresh scratches; the first two runs' Enter took the wrong row, and the third's Up left the list);
  and the worktree-path picker row (j), added for the fifth run, on the last two. In the sixth run
  each fork row was also checked to descend from the worktree conversation (its transcript carries
  that conversation's first user record, and it recalled this run's marker), and all four did. The
  second `takeover` run kept (f) alive 60 s before stopping it and timestamped the signal.
- **Measured vs read.** Every result is *measured* unless marked *read* (read from the binary's
  bundled JavaScript, nothing run) or *inferred* (a cause that fits what was measured, not tested on
  its own).
- **The stand-in.** Only the app can send a turn into a server session. Unattended, the history was
  one turn typed into a terminal that `--continue` had opened on the server's pre-created session
  (Spike 06's method). The terminal therefore wrote that turn, not the server's session process
  (`sdk-cli`). In the real case the server's process writes the app's turns into the same file
  (Spike 06 H1), so the local history a resume reads is the same kind of thing. But everything
  measured about the history upload below is measured **on the stand-in's transcript**. Whether the
  server's process leaves anything that changes it is H1's to check (result 2's last row).

---

## Results

| # | Measurement | Result | Mode |
|---|---|---|---|
| 1 | Flags | `-r, --resume [value]` (an id, or the picker with an optional search term), `-c, --continue`, `--fork-session` (*"When resuming, create a new session ID instead of reusing the original"*), `--remote-control [name]` (`--rc` its hidden alias, Spike 06), `-w, --worktree [name]` | `cli` |
| 1 | What the bridge does with a resumed conversation (*read*) | mints a session and uploads the history unless something suppresses it. Two families of suppression are in the strings. The bridge's, for an account mismatch or a foreign binding (`… minting fresh (history not uploaded)`, `Persistence backfill suppressed (cross-account veto or foreign binding)`). And the persistence sync's `Refusing backfill:` for a history-suppression taint or entry in the transcript, a *foreign binding or suppression carrier*, a session id that changed since the scan was anchored, and a taint sweep that ran out of its line budget | `cli` |
| 1 | What `--fork-session` drops (*read*) | every resume path that forks (5 in the bundle) rewrites the session with `stripWorktreeSession` and `stripRelocatedCwd`: the fork loses the worktree binding. The worktree refusal says so: *"to continue without the worktree, re-run with --fork-session"* | `cli` |
| 2 | `--resume <server's local id> --rc`, server live and serving it | accepted, no refusal; `/remote-control is active · … https://claude.ai/code/session_…` (plain text, **no OSC 8**), a **new** app session; debug log `Created session cse_…`, `Flushing 2 history events` (on the stand-in's transcript) | `handoff` (a) |
| 2 | History in the terminal | recalled the marker (read from the transcript, an answer written after the question) | `handoff` (a) |
| 2 | Effect on the server | none: `Capacity: 1/4` before and after, its session process alive, its debug log gained 1 line (a heartbeat) across the terminal's turn | `handoff` (a) |
| 2 | Who holds the local id | **two processes**: the server's (`sdk-cli`, the server's app session) and the terminal's (`cli`, the new one); the transcript stayed one chain, 0 invalid lines | `handoff` (a) |
| 2 | Does the server's session leave a pointer a resume would follow? (stand-in) | no `bridge-session` record in its transcript and no mention of its app session id; `bridge-pointer.json` in the project directory names the server's session (`source: "standalone"`) | `handoff`, `picker` |
| 3 | `--resume <id> --fork-session --rc` | a new local id **and** a new app session; `Flushing 4 history events` (every turn so far); marker recalled; the server's transcript unchanged (4 → 4 records) | `handoff` (b) |
| 3 | `--resume --fork-session --rc` with no id, `--rc` bare and last | the picker lists the conversation by its first prompt or a generated title (`QUOKKA… · 1 second ago · master · 221.3KB`; in `worktree`, *Session with memorable word*); Enter → a new local id, a new app session, `Flushing 2 history events`, marker recalled, server unchanged (`Capacity: 1/4`). A bare `--rc` names the session after the folder: registry `name` `ws-65` for a clone at `…/ws` | `picker` |
| 4 | `--continue --rc` | opened the folder's **most recent** conversation, which was (b)'s fork, not the server's session, and **reattached (b)'s app session** (`Reattaching to persisted bridge session …`) | `handoff` (c) |
| 4 | `--resume <id>` with no flag, then `/remote-control` | the plain resume **already reattached (a)'s app session**: its registry row named it before any command was typed. `/remote-control` then opened a panel (*This session is available … Disconnect this session · Show QR code · Continue*) | `handoff` (d) |
| 4 | Each reattach | an inbound `control_request subtype=end_session` replayed from the stream and answered `result=error`; the session stayed active and answered | `handoff` (c), (d), (e) |
| 5 | Server `SIGTERM`ed, then `--resume <id> --rc` | reattached (a)'s app session (`at seq 18`, `Unarchive … status=200`); marker recalled | `handoff` (e) |
| 5 | Server restarted while (e) holds the conversation | same environment, same server session, `Capacity: 1/4`; two processes on the local id again; (e) unaffected | `handoff` (e2) |
| 6 | A second `--resume <id> --rc` while (e) holds it | no refusal: `Explicit enable is taking over bridge session cse_… from local pid <e>`. **(e) is displaced while (f) is still alive.** Second run (with (f) kept alive 60 s and the signal timestamped): take-over 05:31:38.13; 16 s later (e)'s next request met `superseded_by_worker` and it closed with `4090` (*another connection took over this session … standing down*), 05:31:54.41; at the end of the 60 s its registry row's `bridgeSessionId` was `null`; (f) was SIGTERMed at 05:32:42.92, **48.5 s after the drop**, and logged `Torn down (archive=200)` at 05:32:43.26. (e)'s process stayed alive with Remote Control off. The first run (14 s from take-over to drop, then 10 s) had the drop 0.7 s before (f)'s teardown, too close to order by its output alone | `takeover` |
| 7 | `--resume session_…` or `--resume cse_…` (the ids Drydock knows) | not an id to it: the picker opens searching for it, `No sessions match "session_…"` | `ids` |
| 7 | Where the local id is | not in the server's tail or debug log. The session process's argv is `--session-id cse_…` and `--sdk-url https://api.anthropic.com/v1/code/sessions/cse_…`. The local id is a **version-5 UUID, `uuid5(3ab19d7e-9f35-45c2-926e-75e271cc60b3, <the --sdk-url>)`**: computed over each of the binary's 21 distinct UUID literals as the namespace and each name the process was handed, that pair, and only it, gives the local id (measured). The binary calls it *"a uuidv5 of the session id in its `cse_` form … the same for the life of the cloud session … until a /clear in the session"* (*read*). The live-session registry `$CLAUDE_CONFIG_DIR/sessions/<pid>.json` also maps `bridgeSessionId` → `sessionId` | `ids` |
| 8 | A `claude -w` worktree conversation, `--resume <id>` from the clone's path | accepted, and it **runs in the worktree** (registry `cwd` = `.claude/worktrees/<name>`); marker recalled; with `--rc`, a new app session as in (a). That the transcript's `worktree-state` record is what moves it is *inferred* (the binary's resume restores it; the record was not removed to test it) | `worktree` (a), (b) |
| 8 | Same from the worktree's path | same | `worktree` (c) |
| 8 | **`--resume <id> --fork-session --rc` from the clone's path** | accepted, history kept (marker recalled), a new app session, and it **runs in the clone** (registry `cwd` = the clone; the fork's transcript under the clone's project directory, 0 `worktree-state` records in it). The worktree binding is gone, as result 1 read. Six runs out of six | `worktree` (g) |
| 8 | Same from the worktree's path | runs **in the worktree**, history kept, a new app session. Six out of six | `worktree` (h) |
| 8 | The picker from the clone's path | does not list the worktree conversation; its footer offers *Ctrl+W to show all worktrees* | `worktree` (d) |
| 8 | The picker from the clone's path with `--fork-session --rc`, Ctrl+W, Enter on the worktree conversation | lists it (`… · worktree-ddspike10wt · 245.9KB`); the fork **runs in the clone**, history kept, as (g). Ctrl+W does not move the cursor: on a scratch with clone conversations, Enter took the one it was on | `worktree` (i) |
| 8 | The picker from the worktree's path with `--fork-session --rc`, Enter | lists the worktree conversation without Ctrl+W (`❯ Session with memorable word · 1 minute ago · worktree-ddspike10wt`); the fork **runs in the worktree**, history kept, a new app session. Two runs (the row was added for the fifth) | `worktree` (j) |
| 8 | A clone conversation resumed from the worktree | runs where it was launched (the worktree) and appends to the clone's transcript file | `worktree` (e) |
| 8 | Either resumed from `$HOME` (no trust record) | the trust menu (`❯ No, exit` / `Yes, I trust this folder`), which waits | `worktree` (f) |

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

That upload was measured on the stand-in's transcript. The binary has several reasons to refuse it
(result 1), and one is a **foreign binding**. If the server's session process records a binding of
its own in the transcript, which the stand-in cannot write, the resume could mint its session with
the history not uploaded: the terminal would have the history and the app session would not. H1
checks the transcript for it, and H2 and H4 check what the app shows. The harness now prints the
persistence sync's lines with the bridge's, so a refusal shows in the run's output.

Nothing told the server. It kept serving its session, unchanged, and the registry then listed two
live processes on the one local id. This is Spike 06's "second writer", with Remote Control
added: the terminal's turns go to the new app session and into the shared file. The server's
session process loads its history from the API (Spike 06 result 3), so it never sees them. A turn
the phone sends into the *original* session after the hand-off is the second writer on that file,
and Spike 06 result 4 shows what that does on an interactive conversation: two branches in one file.
For a server session it is still unmeasured (H3).

A bare `--rc` last works (result 3, `picker`) and names the session after the folder. `--rc` takes
an optional `[name]`, so a plain argument after it would be taken as the name: a command Drydock
ships should put `--rc` last.

### 3 — `--fork-session`: the same, without the shared file

`--fork-session` gives the terminal a new local id and a new transcript with the history copied in
(the forked file's records keep their `uuid`s). The bridge uploads all of it, and the server's
file is not touched afterwards. That removes the two-writer problem at its source. The phone's
session and the laptop's are then two conversations that share a past: the phone can keep using
its own, and the laptop's is a separate app session.

With no id, `claude --resume --fork-session --rc` opens the picker scoped to the current folder. It
lists the server's sessions by their first prompt or a generated title, and Enter on one does the same as (b). That is
the whole hand-off with nothing from Drydock: the operator chooses the conversation by what was said
in it, which is how they would recognise it anyway. The fork also drops a worktree binding (result
8), so the folder it is run in is the folder it continues in.

### What "a copy" costs

The hand-off copies the conversation; it does not move the phone's session. So:

- **The phone's session stays in the environment** with the history up to the hand-off and nothing
  after. It still holds one of the server's four places (`Capacity: 1/4` before and after every
  variant), until it is archived or ended.
- **Repeated hand-offs fill the capacity.** Each conversation handed off leaves its phone session
  behind. Four of them and the server has no room for a new one.
- **The card's *Open in Claude* link no longer leads to the live conversation.** It opens the
  environment, which lists the stale session. The continued one is a separate app session, which by
  Spike 06 H1/H2 the app lists under *Remote Control* (H2 to confirm).
- **A phone user typing in the old session diverges silently.** The turn goes to the server's
  session process; the terminal never sees it. Without `--fork-session` it is also a second writer
  on the terminal's transcript (H3).

So the hand-off ends with archiving the phone's session in the app. Whether archiving it frees the
server's place is owed (H4).

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
app session over (`Explicit enable is taking over … from local pid`). The first terminal is
displaced while the second is still running: its next request, 14 and 16 s after the take-over in
the two `takeover` runs, met `superseded_by_worker`, and it stood down with `4090` and stayed open
with Remote Control off. In the second run the second terminal was then left alive a further 48 s
before it was stopped, so the order is not a matter of timing resolution. When the second exits it
archives the app session. The first draft of this report read the order the other way (the first
dropped after the second exited). Without `--rc`, Spike 06 result 4 got `Remote Control not started here … run /remote-control
to move it`. With `--rc` the operator asked for the move, so it happens. One terminal per
conversation is the rule.

### 7 — From Drydock's id to the one `--resume` takes

Drydock's `rc_session` rows hold `session_…` ids, taken from the server's OSC 8 links (design §8).
`--resume` does not take them: it opens the picker searching for the string. The local id it needs
appears nowhere in the server's output. It can be computed, though. The session process is started
with `--sdk-url https://api.anthropic.com/v1/code/sessions/cse_<x>`, where `cse_<x>` is the
`session_<x>` id in its other spelling, and the local id is the version-5 UUID of that URL in the
namespace `3ab19d7e-9f35-45c2-926e-75e271cc60b3`, a literal in the binary (measured in `ids`). The
registry `$CLAUDE_CONFIG_DIR/sessions/<pid>.json` (`sessionId`, `bridgeSessionId`, `cwd`) pairs them
too, but only for live processes, and per pid on a volume every container shares (Spike 08: pids
collide across containers).

Both are undocumented internals of `2.1.289`: the namespace, the URL's form and the registry can
each change on a bump. And the binary says the id lasts *"until a /clear in the session"* (*read*).
The computed id is a function of the `--sdk-url` alone, which a `/clear` does not change, so after a
`/clear` in the app it still names the conversation **before** the clear, whose transcript is still
on the volume: `--resume` of it is accepted and silently continues the pre-clear conversation
(*inferred*; `/clear` was not run). A stored id does the same.

### 8 — Worktrees: the fork continues where it is run

A conversation born in a worktree (`claude -w`, which makes `.claude/worktrees/<name>` on branch
`worktree-<name>` and records a `worktree-state` in its transcript) can be resumed by id **from the
clone's path**, and the plain resume moves into the worktree. **`--fork-session` does not**: the
binary's fork paths strip the worktree binding (result 1, *read*), and measured, the fork from the
clone ran in the clone, with the history but outside the worktree the conversation's work is in.
That held by id and from the picker after Ctrl+W. The same fork run from the worktree's own path,
by id or from the picker (which lists the worktree's conversations there without Ctrl+W), runs in
the worktree. So the plain-`--resume` result above covers only the unforked case. For the
recommended command the rule is: **run it in the folder the conversation ran in**.

The picker from the clone's path does not list a worktree conversation until *Ctrl+W*. A
conversation born in the clone resumed from a worktree stays in the worktree it was launched in,
still writing the clone's file. From an untrusted folder (`$HOME` here) the trust menu waits, as in
Spike 06 result 8. So the command runs in the workspace folder or one of its worktrees, which the
Feature trusts (whether a `--spawn worktree` path needs its own trust record is Spike 02's open
question F, unchanged).

Whether the **server's** `--spawn worktree` sessions are bound the same way (same layout, a
`worktree-state` record) was not measured; only the app can make one (H5). If they are, every
session the app creates after the server's pre-created one lives in a worktree, and its hand-off
must be run from there.

---

## Verdict

**Use `claude --resume --fork-session --rc` and pick (or `claude --resume <id> --fork-session
--rc`), run in the container's terminal in the folder the conversation ran in: the workspace folder
for the server's first session, its worktree for a session that ran in one. Then archive the phone's
session.** On the terminal side it does everything the owner's flow asks for: it is accepted while
the phone's session is live, it brings the whole local history, it leaves the server and its
capacity alone, and it shares the continued conversation to the app as a new Remote Control session
that carries the history. `--fork-session` keeps the phone's file and the laptop's apart, so nothing
writes one transcript twice. Its cost is that it drops a worktree binding, which is why the folder
matters: run from the clone, a worktree conversation continues in the clone, away from its work.
Without `--fork-session` a resume from the clone follows the conversation into its worktree, but the
phone's session and the laptop's then share a file and a local id, and the phone can fork it.

What it is not: a move of the phone's session. The original stays in the environment, with the
history up to the hand-off and nothing after, holding a place in the server's capacity, and the
card's link still leads to it. Hence the archive. The continued conversation is a new session, which
by Spike 06's H1 and H2 the app lists under **"Remote Control"**, not in the environment. That is an
extrapolation until H2. Whether the app *shows* the uploaded history (the binary can refuse the
upload on a foreign binding, and only the stand-in was measured), and whether turns flow both ways,
are H1, H2 and H4.

`--continue --rc` is out (result 4). A plain `--resume` of a conversation that once held Remote
Control re-shares it without asking, and a second `--rc` on a served conversation takes it over and
stands the first terminal down. Both belong in the docs sentence.

## What Drydock would build

1. **Now, nothing but a sentence** (docs and the extension's README). *To carry a conversation from
   the phone to this terminal, run `claude --resume --fork-session --rc` in the folder the
   conversation ran in, and pick it. If you are not sure which folder that is, run it in the
   workspace folder first, press Ctrl+W to list every worktree's conversations, read the branch on
   the one you want (`worktree-<name>`), press Esc, `cd` into that worktree and run the command
   again. A wrong folder is not refused: picked from the workspace folder, a conversation that ran
   in a worktree silently continues in the workspace folder, away from its work. Then archive the
   phone's session in the app: it stays in the environment, holds one of the server's four places,
   and anything typed there afterwards does not reach this terminal. The continued conversation
   appears in the app under Remote Control.* Where the server keeps the worktrees of the sessions
   the app starts (under `.claude/worktrees/`, like `claude -w`'s, or elsewhere) is H5's to say, so
   the sentence names no path until then. Gate it on H2, H4 and H5. Measured on the host, so one
   run in a real workspace container should go with it (Spike 06 owes the same for `claude --rc`).
   The alternative to finding the folder is a plain `claude --resume --rc` from the workspace
   folder, which follows a worktree conversation into its worktree (result 8, measured) but shares
   the phone session's transcript and local id: the two-writer cost `--fork-session` exists to
   avoid.
2. **An extension command, "Continue a phone session here"**, for the attached-container window.
   It sends nothing to Drydock and mutates nothing, so it is outside the extension's rule-1 gate
   (extension design §8). It first offers a quick-pick of the workspace folder and each of its git
   worktrees, each labelled by its branch (`git worktree list` in the container), then opens an
   integrated terminal in the one chosen and runs `claude --resume --fork-session --rc` there; the
   picker chooses the conversation. The branch is what the picker shows for a conversation, so the
   operator can match the two; nothing can check the match, since the folder must be chosen before
   the picker runs. Never `$HOME`. Its message also says to archive the phone's session. This needs
   no API, schema or Feature change.
3. **By id, only if the picker proves not enough.** A card or tree item per session would run
   `cd <its folder> && claude --resume <local id> --fork-session --rc` for one chosen session. That
   needs (i) the session ids in the workspace view (today it carries only the environment link and
   counts: `internal/workspace/view.go`'s `SessionData`), (ii) the local id, and (iii) the folder,
   which for a worktree session only the transcript's records or the registry's `cwd` give. The
   local id can be computed from the `session_…` id (result 7) with no registry, but that is wrong
   after a `/clear` in the app, and not detectably so: the computed id still names the pre-clear
   conversation, whose transcript is still there, so the resume is accepted and continues the wrong
   conversation, and no "missing transcript" fallback ever fires. A by-id command must instead take
   the id from the live registry row whose `bridgeSessionId` is the session (it lists only live
   processes and is rewritten by them, so it should follow a `/clear`; unmeasured), or check that
   no newer transcript belongs to that session, and otherwise fall back to the picker. A Feature helper (`drydock-continue <session_…>`, with a
   version bump) could do that in the container at click time and exec `claude`. Either way it is
   more undocumented internals for the §11.1 bump ritual. The card itself gets no link: a URL cannot run a terminal command. At
   most it gets a copyable command.

## Owed (needs a human at the Claude app)

H1–H5 in the [harness README](harness-10-resume-rc/README.md#the-human-steps-h1h5), all from
one run: `./setup.sh /tmp/s10h && S=/tmp/s10h WAIT_APP=300 ./run.sh handoff`.

- **H1** — the real start: a marker turn sent from the app into the server's session. Also checks
  whether the server's process leaves anything in the transcript a resume would follow or treat as
  a foreign binding (result 2's last row and section 2, which measured only the stand-in).
- **H2** — after `--resume <id> --rc`: where the app lists the new session, whether it shows the
  history (and whether the debug log says `Refusing backfill` or `suppressed`), and whether an app
  turn reaches the terminal and a terminal turn reaches the app.
- **H3** — a turn into the phone's original session after the hand-off: does the shared file fork,
  and does the terminal see it (expected: no)?
- **H4** — H2 for `--fork-session`, the recommended variant; then archive the phone's original
  session and record whether the server's capacity drops.
- **H5** — the same for a session the server spawned in a worktree: where the worktree is, and
  that a fork resumed from that worktree runs there (and, from the clone's path, in the clone).

## Open questions

- What the app shows for a hand-off session after the terminal exits (`Torn down (archive=200)`;
  a later resume `Unarchive`s it, result 5). This is Spike 06's open question, unchanged.
- What the replayed `end_session` control request answered `error` on each reattach is (result 4).
  It was harmless here.
- What a `/clear` in the app does to the local id and the transcript a resume finds (result 7's
  *read*).

## Reproducing

```sh
cd docs/design/spikes/harness-10-resume-rc
./setup.sh /tmp/s10a && export S=/tmp/s10a
./run.sh cli; ./run.sh handoff; ./run.sh ids
./setup.sh /tmp/s10b && S=/tmp/s10b ./run.sh picker
./setup.sh /tmp/s10c && S=/tmp/s10c ./run.sh takeover
./setup.sh /tmp/s10d && S=/tmp/s10d ./run.sh worktree   # fresh, for the picker rows (i), (j)
for s in /tmp/s10a /tmp/s10b /tmp/s10c /tmp/s10d; do S=$s ./reap.sh || break; done
rm -rf /tmp/s10a /tmp/s10b /tmp/s10c /tmp/s10d
```

`setup.sh` refuses unless the copied access token's `expiresAt` is at least an hour away, and every
mode unless it is an hour plus the mode's own minutes away (Spike 08's fail-closed guard, which
counts `WAIT_APP`). Every mode ends by sweeping any process still carrying the scratch's
`CLAUDE_CONFIG_DIR`; `reap.sh` does the same after a killed run and fails if anything survives. The
scratch holds a live refresh token, so delete it afterwards, after `reap.sh`. Re-run on a Claude Code
bump if the docs sentence or the extension command ships: the history upload, the fork's worktree
stripping, the bridge pointer, the registry and the id's derivation are all undocumented internals
of `2.1.289`.
