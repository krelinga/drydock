# Spike 06 — Can a terminal get onto the session the Claude app sees?

**Question** (the owner's): *after I attach to the dev container in VS Code, is there an easy way to
also attach to the running Claude session?* Drydock runs one `claude remote-control` server per
workspace (design §8); the server spawns the sessions the Claude app opens, and their transcripts
land in the shared `CLAUDE_CONFIG_DIR`. Four paths were measured: resuming a server-owned session
from a terminal (path 1), an interactive session shared to the app beside the server (path 2),
anything else the CLI offers (path 3), and replacing the server with an interactive Remote Control
session in tmux that the operator `tmux attach`es to (path 4, the owner's idea).

**Answer: not onto a session the server owns. There is no attach, and a resume is a second
writer, not a viewer.** A terminal can *open* a server session's conversation (`--resume <id>`,
`--continue`), but it gets a second writer on the same transcript file, with its own context. The
server's session process logged nothing but heartbeats while the terminal took turns, and it loads
its history from Anthropic's API, not the local file — so the terminal's turns should never reach the
app, and once the app sends turns too the file should fork (both predicted; checking needs a human,
H1). `/remote-control` in that terminal does not take the server's session over: it creates a
second app session. Nothing in `--help` or the binary attaches to a server's session.

**What does work is an interactive session with Remote Control on** — `claude --remote-control`
(`--rc` is its hidden alias), or `/remote-control` inside a running `claude`. The terminal and the
app then drive *one* conversation. It runs beside the server in the same folder with no
`already served by a terminal` refusal, restarts onto the same session id after `SIGTERM` *and*
`SIGKILL` with no lockout, and survives tmux clients coming and going. Claude Code also has its own
background mode for it, `claude --bg --remote-control` with `claude attach <id>`, which needs no
tmux. The price, if it replaced the server: one conversation per process — no `--capacity`, no
`--spawn worktree`, no environment, so nothing for the app to open a *new* session in — and a
different set of startup gates, two of which hang and one of which fails silently.

**Verdict.** Keep the server. The cheap answer to the owner's question is that the operator runs
`claude --rc` in the container's terminal for a conversation they want in both places. That was
measured on the host under `env -i` with a config seeded like the shared volume, not in a real
workspace container, so in a Drydock container it *should* work with nothing added. `claude --bg
--rc` with `claude attach` is held back until it is measured in two containers: its daemon keeps
state in `CLAUDE_CONFIG_DIR`, which in Drydock is the volume every workspace shares, and it refreshes
the shared OAuth credential on its own schedule. Replacing the server (path 4) trades away exactly
what §8 chose the server for.

- **Verified against:** Claude Code `2.1.289`, Linux, `tmux` 3.4 (a private `-L` server), every
  process on the host under `env -i` with only a Drydock container's variables (`CLAUDE_CONFIG_DIR`
  on a scratch config seeded like the shared volume, `DISABLE_AUTOUPDATER=1`, a scratch `HOME`). Not
  in a workspace container: no Feature environment, no `CLAUDE_ENV_FILE` prelude, no `/workspace`,
  one pid namespace.
- **Date:** 2026-10-30.
- **Harness:** [`harness-06-attach/`](harness-06-attach/) — re-runnable; each result names its mode.
- **Not measured** (needs a human at the Claude app): what the app shows. See *Owed* below.

---

## Results

| # | Measurement | Result | Mode |
|---|---|---|---|
| 1 | Flags for resuming / attaching | `--rc [name]` is registered as `Alias for --remote-control` with `.hideHelp()` (the binary's option table; the spike ran `--remote-control`, never `--rc`); `--bg` + `attach`/`logs`/`stop` exist; `attach` takes only `--bg` ids | `cli` |
| 2 | `--resume <id>` of the server's live session, no turns yet | `No conversation found with session ID: <id>` | `server-resume` (fresh scratch) |
| 2 | `--continue` in the same folder, server live | **opens the server's session** as a second process on the same local id | `server-resume` |
| 2 | `--resume <id>` once the conversation has turns, server live | opens it; terminal turns append to the server session's file; the server session's debug log gains only heartbeats | `server-resume` |
| 2 | `/remote-control` in that terminal | a **new** app session (`session_…TZUk…`), not the server's; server stays `Capacity: 1/4`, its process alive, no `4090` in 105 s | `server-resume` |
| 2 | `--teleport <server session>` | `This terminal now has its own copy of the session …`; `Fetched 0 events` | `server-resume` |
| 2 | Same after `SIGTERM` of the server; then a server restart | resume works; restart reattaches the same environment and session; two processes then hold one local id | `server-resume` |
| 3 | How the server's session process loads its history | `--resume=https://api.anthropic.com/v1/code/sessions/cse_…`: from the API, not the local `.jsonl` | `cli` |
| 4 | Two terminals on one interactive Remote Control conversation | second gets `Remote Control not started here · … already has Remote Control for this conversation`; one file, **two branches**, no invalid lines (turns ~15 s apart; no concurrent append tried); the first terminal blind to the second's turn | `interactive-resume` |
| 4 | `/remote-control` in the second terminal | takes the same app session; the first is told `standing down (code 4090)` ~19 s later (65 s in an earlier run) | `interactive-resume` |
| 4 | `--continue` with both gone | opens the latest branch and **silently re-enables Remote Control** on the same app session | `interactive-resume` |
| 5 | `claude --remote-control` beside the server, same folder | accepted; its own session; no environment id; server unaffected | `alongside` |
| 5 | Plain `claude`, then `/remote-control` | same as above | `alongside` |
| 5 | Same in a `git worktree` of the clone with **no trust record** | starts, no trust prompt | `alongside` |
| 6 | What an interactive Remote Control session prints | the session URL once, **plain text**, no OSC 8; no `Environment ID`, no `Capacity` | `tmux-rc` |
| 6 | A turn typed in the terminal | forwarded to the app session (`[remote-bridge] Sent result success`) | `tmux-rc` |
| 6 | A second tmux client at 80×24, then killed | window 80×23 while attached (the harness then restores 200×50 itself); no bridge state change | `tmux-rc` |
| 7 | `SIGTERM`, then `--remote-control --continue` | same session id, same local id (`Reattaching to persisted bridge session … restored_owner_match`) | `tmux-rc` |
| 7 | `SIGKILL`, restart at once | same session id, accepted first time, **no lockout**; conversation carried (`OK1`) | `tmux-rc` |
| 7 | A second `--remote-control`, then the server, beside it | both accepted (`Capacity: 1/4`, the server's usual environment) | `tmux-rc` |
| 8 | Gates for `claude --remote-control` on a PTY | trust **hangs** (default `No, exit`); `hasCompletedOnboarding` **hangs** (theme picker); `remoteDialogSeen` not needed; `oauthAccount` missing → session runs **without** Remote Control, one stderr line (printed from the debug log) | `tmux-rc-gates` |
| 9 | `--cloud <id>`, `attach <id>`, `--session-id <id>`, `agents` against the server | `not enabled for your account`; `No job matching`; `already in use`; listed with `id: null` | `cli` |
| 10 | `claude --bg --remote-control` | returns at once with an id; Remote Control active; `attach` from any terminal; two attaches at once; outlives its viewers | `bg` |
| 10 | Worker `SIGKILL`ed | not restarted: `Session <id> has exited.`; `stop` + `attach` revives it with the same session and conversation | `bg` |

### 1 — What 2.1.289 offers

The owner's `--rc` exists. The binary's option table registers
`("--rc [name]","Alias for --remote-control")…hideHelp()` next to
`("--remote-control [name]","Start an interactive session with Remote Control enabled …")`, and
`cli` prints both. Every live measurement here used the long name; `--rc` itself was not run. Everything else that sounds like "attach" was tried in result 9 and is a dead
end except `--bg`, result 10.

### 2 — Resuming the server's own session makes a second writer

The server pre-creates one session in the folder (§8). Its local id (`dded31c3-…`, from
`$CLAUDE_CONFIG_DIR/sessions/<pid>.json`, the live-session registry) is stable across server
restarts, and its transcript is `projects/<path>/<id>.jsonl` like any other.

- **Before the session has a turn**, `claude --resume <id>` says `No conversation found`, but
  `claude --continue` **opens it** — it is the most recent conversation in the folder — and the
  registry then shows two processes on one local id, the server's (`entrypoint: sdk-cli`, holding
  the app session) and the terminal's (`bridgeSessionId: null`).
- **A turn typed there** (`TERM1`) is answered and appended to the server session's file. The
  server's session process did not react. Its debug log (`sr-srv.log-cse_….debug`, checked to
  exist) gained 2 lines in the 16 s after the turn and 6 in the 105 s after `/remote-control`, all
  `CCRClient: Heartbeat sent` — so it was being written, and wrote nothing else. Re-reading the
  earlier run's log gave the same: heartbeats every 20 s from the terminal's turn to the server's
  shutdown, and nothing else.
- **`/remote-control` in that terminal** creates a second app session over the same local
  conversation. The server's session is never displaced: no `4090` within 105 s, `Capacity: 1/4`,
  process alive. So one transcript file now backs two app sessions.
- **After a `SIGTERM` of the server**, `--resume` opens it (and, since the terminal had last held
  Remote Control on it, re-registers that second app session). Restarting the server while the
  terminal holds it reattaches the server's own environment and session, as Spike 02 found; the
  registry then shows both processes on the one id.
- **The picker** (`claude --resume` with no id) lists the server's session among the folder's
  conversations — `(session) · 789 bytes` before a turn — with nothing marking it as served.
- **`--teleport <session>`** is for cloud sessions: it fetched `0 events` for every Remote Control
  session tried and started an empty local copy, saying so: *"This terminal now has its own copy of
  the session: new work here stays local and will not appear in the Claude app."*

The server-side half of this — a session with turns *sent from the app*, then resumed — is owed (H1
below). The file only forks when both sides write, and only app turns make the server side write, so
**a fork of a server session's file is predicted, not measured**. Results 3 and 4 are the reasons for
the prediction.

### 3 — Why: the server's sessions load from the API

The server's session process is `claude.exe --print --sdk-url https://api.anthropic.com/v1/code/sessions/cse_… --session-id … --input-format stream-json … --resume=https://api.anthropic.com/v1/code/sessions/cse_…`.
Its history is the API's, not the `.jsonl` on the shared volume, and its input is the stream from the
app. A terminal writing to the same file is invisible to it, and what it writes the app never sees.

### 4 — Two writers on one transcript: a fork inside one file

Measured on an interactive Remote Control conversation (`ia`), so it needed no app:

Abridged (prompts shortened to their reply word):

```
r2 (claude --resume <ia's id>):
  ● Remote Control not started here · another Claude Code on this machine (started 17s ago)
    already has Remote Control for this conversation, so this terminal can't see your sessions on
    other machines and they can't reach it · run /remote-control to move it to this terminal
tree.mjs on the one .jsonl, after OK1 in ia, OK2 in r2, then "what did you reply last time?" in ia:
  records=45 invalid_lines=0
  conversation leaves=2 (FORKED inside one file)
    leaf a4e2926e: u:…OK1 -> a:OK1 -> u:…OK2 -> a:OK2
    leaf 724071bd: u:…OK1 -> a:OK1 -> u:What did you reply last time? -> a:OK1
```

No invalid lines — whole JSON lines, linked by `parentUuid` — but two branches. The turns were
sequential, about 15 s apart, so this says nothing about two processes appending at the same
moment. `ia` was blind to `r2`'s turn (asked what it last replied, it said `OK1`); `r2` was not asked
about `ia`'s later turn, so its blindness is assumed by symmetry, not measured. `/remote-control` in `r2` moved the app session to `r2`, and
`ia` printed `Remote Control disconnected — another connection took over this session … — this device
is standing down (code 4090)` on its next poll (19 s; 65 s in the exploratory run). Afterwards,
**a plain `claude --continue` reopened the later branch and re-enabled Remote Control on the same app
session without being asked** — so a terminal can take an app session over without the operator
meaning to.

### 5 — Path 2: an interactive session beside the server

`claude --remote-control <name>` in the server's folder, a plain `claude` then `/remote-control`, and
the same in a git worktree of the clone all started and printed
`/remote-control is active · Continue here, on your phone, or at https://claude.ai/code/session_…`.
None was refused `already served by a terminal`: that refusal is about the *server's* folder
registration (an environment), and an interactive session registers no environment — its debug log
has `Created session cse_…` and no `POST /v1/environments/bridge`. The server stayed `Capacity: 1/4`.
The sessions are new conversations, not the server's.

The worktree had **no trust record** of its own and asked nothing. Why was not measured: no mode
reads how Claude Code resolves trust, so "it inherits the clone's record" is a guess, not a result.
It is also weak evidence for Spike 02's open question F (*does a `--spawn worktree` session need its
own trust record?*): the server's worktree sessions are `--print --sdk-url` processes (result 3),
which never show the interactive trust menu, so whatever gate applies to them is the server's. F stays
open.

### 6 — Path 4: what an interactive Remote Control session gives a supervisor

| | `claude remote-control` (today) | `claude --remote-control` in tmux |
|---|---|---|
| Identity printed | `Environment ID: env_…`, each session as an OSC 8 link, `Capacity: N/4` on every repaint | the session URL **once**, as plain text in a status line; no environment, no capacity |
| Sessions | many; app can open new ones (`--capacity`, `--spawn worktree`) | **one**, this conversation; the app has no environment to open another in |
| Card link | `claude.ai/code?environment=<id>` | `claude.ai/code/session_<id>` |
| Machine-readable state | the tail | `$CLAUDE_CONFIG_DIR/sessions/<pid>.json`: `sessionId`, `bridgeSessionId`, `status` |
| Terminal turns reach the app | n/a | yes (debug: `Sent result success`) |
| tmux attach / detach / resize | n/a | no bridge state change logged |

The plain-text URL is a problem for §8's rule: ids are taken **only from OSC 8 targets** because a
bare `session_…` match accepts an id the model printed. Here there is no OSC 8 at all. The registry
file is the better source — it is per process, names the app session, and is written by Claude Code,
not by the model — but it is an undocumented internal file, one more thing for the bump ritual.

### 7 — Path 4: restart keeps the session, and there is no 409

`SIGTERM` prints `Resume this session with: claude --resume <uuid>`. Restarting with
`--remote-control --continue` logged `Reattaching to persisted bridge session cse_… (… restored_owner_match)`
and came back on the **same** app session and local id. After `SIGKILL` the same restart was
accepted at once, same session, and the conversation was intact (`OK1`). Spike 02's lockout belongs
to the server's folder registration, which an interactive session does not make.

`--continue` means "the most recent conversation in this directory", which result 2 shows can be
the server's session or one an operator just opened; a supervisor would resume by id
(`--resume <uuid>`, from the registry) instead. A clean exit logs `Torn down (archive=200)` — the
app session is archived — yet a later resume reattached it; what the app shows meanwhile is owed.

### 8 — Path 4's startup gates are not the server's

Removed one at a time from a seeded config, `claude --remote-control` on a PTY, 25 s each:

| Removed | Server (Spike 02, `2.1.289`) | `claude --remote-control` |
|---|---|---|
| trust record | hangs on `Trust <dir>? [y/N]` | **hangs** on the trust menu, `❯ No, exit` preselected |
| `remoteDialogSeen` | hangs on `Enable Remote Control? (y/n)` | **not needed** (the flag is the consent), and not written |
| `oauthAccount` | exits `Unable to determine your organization …` | **runs without Remote Control**: one stderr line, `Unable to determine your organization for Remote Control eligibility. Run \`claude auth login\` …` / `--rc flag ignored.`, then an ordinary session. It refetched the account record into `.claude.json`, so a restart would have it |
| `hasCompletedOnboarding` | not consulted | **hangs** on the first-run theme picker |

The oauthAccount line is printed before the TUI draws and was not in the `pipe-pane` capture (which
attaches a moment after start); the debug file has it as `[WARN] "[stderr] …Unable to determine your
organization for Remote Control eligibility. …\n…--rc flag ignored."`, and `tmux-rc-gates` prints
that line. A supervisor
watching the screen sees a healthy session that the app cannot reach. The Feature writes trust and
`remoteDialogSeen` (§7.2); it writes no onboarding keys, so path 4 needs one more seeded key, and its
"exit means refused" classifier becomes "no URL within N seconds".

### 9 — Dead ends

- `claude --cloud <session>` — `Error: Attaching to an existing cloud session is not enabled for your account.`
- `claude attach <cse_… | local id>` — `No job matching '…'. Run 'claude agents' to list running sessions.`
- `claude --session-id <server's local id>` — `Error: Session ID … is already in use.`
- `claude agents --json` lists the server's session as `{"kind":"interactive","id":null}`: seen, not attachable.
- `claude remote-control --session-id` / `-c` reattach a *server* to a session but refuse
  `--spawn`/`--capacity` (Spike 02 result 5): not a terminal path.
- `--teleport`: result 2.

### 10 — `claude --bg --remote-control`: Claude Code's own tmux

```
$ claude --bg --remote-control ddspike06bg
Starting background service…
backgrounded · 3fbf188c (idle — send a prompt to start)
  claude attach 3fbf188c    open in this terminal
```

The session runs in Claude Code's daemon (state in `$CLAUDE_CONFIG_DIR/daemon/` and
`$CLAUDE_CONFIG_DIR/jobs/`, sockets under `/tmp/cc-daemon-<uid>/`) with Remote Control active. `claude attach <id>` opens it from any terminal
(`Ctrl+Z` detaches); two attaches at once both rendered; killing every attach client left it
running. It is not a supervisor: a `SIGKILL`ed worker settled as `done`, the daemon exited five
seconds later, and `attach` said `Session 3fbf188c has exited.`; `claude stop` then `claude attach`
relaunched it with the same app session and conversation. The daemon also schedules its own OAuth
refresh (`auth: scheduling proactive refresh in 11661s`, about four minutes before the access token's
`expiresAt`), on whatever credential its `CLAUDE_CONFIG_DIR` holds.

In Drydock that config directory is the shared credential volume, so the daemon's state would be
shared by every workspace container while its sockets and the pids it records are per container.
Unmeasured: two containers' daemons on one volume, whether `claude agents`/`attach` in one container
lists the other's jobs, and the daemon's refresh against Spike 00's lock on the shared volume.

---

## Verdict per path

**Path 1 — resume a server-owned session from a terminal: no.** It works mechanically and is
dangerous in practice. `--resume`/`--continue` open the conversation as a second writer. The server's
session process did not react to the terminal's turns, and it loads from the API, so the app should
never see them; with app turns as well, the file should fork as result 4's did (both owed, H1). `/remote-control` from there makes a second app session rather than joining
the first. After the server stops, a resume is a fine way to *continue a conversation locally*, but
not to share it. Nothing stops it in a Drydock container, which is the hazard: an operator's
`claude --continue` in `/workspace` should land in the server's pre-created session, as it did here.

**Path 2 — an interactive session shared to the app beside the server: yes, as a separate
session.** `claude --remote-control` (or `/remote-control`) gave a session the app also drives,
beside the server, in the clone and in a worktree, with no refusal and no effect on the server. It is
a new conversation, not the server's. Measured on the host under `env -i`, so in a workspace
container it *should* need nothing from Drydock: the shared volume already carries the trust record
and `oauthAccount`, and an operator's first interactive `claude` on a fresh volume meets the theme
picker, which a human answers once. One run in a real workspace would settle it.

**Path 3 — anything else: `--bg` is the only find.** `--cloud`, `--teleport`, `attach`, `agents` and
`--session-id` are dead ends against a server. `claude --bg --remote-control` plus `claude attach` is
path 2 without tmux, outliving the terminal that started it — but not to be recommended in Drydock
until it is measured in two containers on one volume (result 10).

**Path 4 — replace the server with `claude --rc` in tmux: it works, and it is a worse server.**
Headless start, restart onto the same session after either signal, no 409, terminal and app on one
conversation, tmux clients harmless. Against §8: one conversation per workspace instead of
`--capacity 4` with on-demand worktree sessions; no environment, so no `?environment=` link and no
new sessions from the phone; a plain-text URL that breaks the OSC-8-only rule; two hangs and a
silent degrade for gates; and the operator who attaches shares one input line with the phone.

## What Drydock would need for each

- **Path 2: nothing, plus a sentence**, once one run in a real workspace confirms it. Say on the card
  or in the docs: *to share a session between this terminal and the app, run `claude --rc`;
  `claude --resume` of a phone session makes a private copy.* Optionally have
  `drydock-claude-config` seed `hasCompletedOnboarding` so the first interactive `claude` skips the
  picker. It would go through the Feature's existing locked merge of `.claude.json` (Claude Code's
  own `.claude.json.lock`, temp + `rename`, `oauthAccount` kept), and it is a Feature change, so a
  version bump. Not `theme`: that would choose the operator's theme for them.
- **Path 3 (`--bg`): a measurement first** — two workspace containers on one volume, each with a
  background session: what `claude agents` lists in each, whether `attach` crosses containers, and
  how the daemon's refresh takes the lock.
- **Path 4, if it were chosen** — supervisor, classifiers and corpus all change:
  - `internal/supervisor`: launch `claude --remote-control <name>` (or `--resume <uuid> --rc`) in a
    tmux session Drydock owns (tmux added to the Feature or image), or under `--bg`; resume by id from
    the registry, never `--continue`; stop with `SIGTERM` still; `tmux attach` instructions on the
    card. One conversation per workspace, or one process per conversation with Drydock doing the
    worktrees.
  - `internal/classify`: new signatures — the trust menu and the theme picker as **timeouts**,
    `--rc flag ignored` from stderr or the debug log, no `already served by a terminal` (cannot
    happen), and session identity from `sessions/<pid>.json` or the plain-text URL instead of OSC 8.
  - §4 / the card: `rc_session` becomes the workspace's one `session_…`; the environment link goes.
  - `internal/claudetest` / `test/fixtures`: a recorded interactive-session corpus (the status line,
    the four gate screens, the `4090` takeover line), pinned like the server's.
  - Feature: seed `hasCompletedOnboarding`; `remoteDialogSeen` becomes irrelevant to this process.

## Owed (needs a human at the Claude app)

- **H1 — a server session with turns from the app, then resumed.** Run
  `WAIT_APP=300 ./run.sh server-resume` on a fresh scratch (`./setup.sh /tmp/s06b`). It prints an
  environment link and a session name; open it in the Claude app or claude.ai, pick that session, and
  send `Reply with exactly APP1 and nothing else.` The harness then runs result 2's steps. Report the
  output, and look in the app after step (a) types `TERM1` in the terminal: **does `TERM1` appear in
  the app's session?** (Predicted: no.) After step (b): **how many sessions does the app list, and
  under what?**
- **H2 — the app's half of path 4.** Run `WAIT_APP=300 ./run.sh tmux-rc-app`, open the printed
  `claude.ai/code/session_…` link, send `Reply with exactly APP1 and nothing else.`, and report the
  output (does the terminal show the app's turn) plus whether the session appears under an
  environment in the app, and whether the app offers to start another session there.
- **H3 — optional:** in H1, also start a *second* session from the app in that environment (it gets
  a `--spawn worktree` worktree) and send one turn; then `claude --resume` from that worktree's path
  in a terminal. That is path 1 in a worktree session, unmeasured here because only the app can
  create one.

## Open questions

- What an archived (`Torn down (archive=200)`) interactive session looks like in the app, and
  whether the app resumes it or only the terminal can.
- Whether the `4090` takeover delay (19 s and 65 s here) is a poll interval worth knowing.
- Whether the daemon's own OAuth refresh behaves on the shared volume like every other refresher
  (Spike 00's `mkdir` lock), and how two containers' daemons share `daemon/` and `jobs/`; untested.
- Path 2 in a real workspace container (Feature environment, `CLAUDE_ENV_FILE` prelude, `/workspace`).
- How a worktree with no trust record of its own passes the trust check; and Spike 02's question F,
  which this spike does not answer.

## Reproducing

```sh
cd docs/design/spikes/harness-06-attach
./setup.sh /tmp/s06 && export S=/tmp/s06
./run.sh cli; ./run.sh server-resume; ./run.sh interactive-resume; ./run.sh alongside
./run.sh tmux-rc; ./run.sh tmux-rc-gates; ./run.sh bg
```

`setup.sh` and every mode refuse to run unless the copied access token's `expiresAt` is an hour
away, and the scratch directory holds a live refresh token: delete it afterwards (README, traps).
The modes share one scratch config and are not independent: `server-resume` on a fresh scratch takes
result 2's no-turns branch, and on a scratch where an earlier run typed `TERM1` it takes the
with-turns branch. Both are recorded above. See the [harness README](harness-06-attach/README.md) for
the traps. Re-run on a Claude Code bump with Spikes 00–03 if path 2's advice ships in the docs or path
4 is ever built: every result here is an undocumented internal of `2.1.289`.
