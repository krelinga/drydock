# Spike 02 — Does a `remote-control` server survive a supervisor restart?

**Question** (design doc §14 Phase 0, and the *Resume after a Drydock restart* row of §8): *if Drydock
restarts or crashes, does re-running `claude remote-control` in the same directory bring back the
sessions the previous server was serving — and what identity do they come back with?*

**Answer: yes, and faster than the design assumed — with one sharp edge.** A restart in the same
directory reconnects **the same environment and the same session**, after a graceful `SIGTERM` and
after a hard `SIGKILL` alike, in about two seconds. §8's resume row is correct.

The edge: that only holds when the dying server had **at least one live session**. Kill a server that
had none and the folder registration outlives the process — every restart is refused with
`409 … This folder is already served by a terminal claude remote-control on this device` for one to
three minutes, and what finally comes back is a *new* environment.

Along the way the spike turned up **three gates that stop a headless `remote-control` from starting at
all**, only one of which §7.2 mentions, and one of them means **a copied `.credentials.json` is not
enough to run Remote Control** — which retires an option §7.1 lists as merely lossy.

- **Verified against:** Claude Code `2.1.246`, Linux, `tmux` as the PTY.
- **Date:** 2026-10-02.
- **Harness:** [`harness-02-restart/`](harness-02-restart/) — re-runnable; see *Reproducing* below.

---

## Results

### 1 — With a live session, restart preserves identity, on both signals

Server started in a git repo, waited until it reported a live session, signalled, restarted plain
(no flags changed) in the same directory:

| Signal | Restart accepted | Environment id | Session id | Capacity |
|---|---|---|---|---|
| `SIGTERM` (Drydock stops it) | **2 s**, first attempt | unchanged | unchanged, shown `Attached` | `1/4` → `1/4` |
| `SIGKILL` (Drydock crashed) | **2 s**, first attempt | unchanged | unchanged | `1/4` → `1/4` |

```
  before: env=env_01XySFjY3UuosSX8AhpjFNM3  Capacity: 1/4
          sessions=session_011cWgUx9pZMgZdibn47d68s
  sending SIGKILL to 25007 … process gone
    t=2s attempt 1: ACCEPTED
    after:  env=env_01XySFjY3UuosSX8AhpjFNM3  Capacity: 1/4
            sessions=session_011cWgUx9pZMgZdibn47d68s
    => environment PRESERVED across sigkill
```

On a graceful stop the server says so itself, which is as close to a documented guarantee as this
gets:

```
[01:26:22] Shutting down 1 active session(s)…
[01:26:22] Environment preserved. Restart `claude remote-control` to reconnect existing sessions.
```

A hard kill prints nothing — but reconnects just the same.

### 2 — With no live session, the folder stays registered for minutes

Same test with `--no-create-session-in-dir`, so the server is listening but serving nothing, then
`SIGKILL`:

```
  before: env=env_01JFhCXVwcuqfbFniiNNmr7w  Capacity: 0/4
  sending SIGKILL … process gone
    t=0s   attempt 1:  refused 409 (folder still registered)
    …
    t=170s attempt 25: refused 409 (folder still registered)
    t=177s attempt 26: ACCEPTED
    after: env=env_01FhGBrWgVhkh6yxfLvUupHt  Capacity: 0/4
    => environment CHANGED (env_01JFhCXVwcuqfbFniiNNmr7w -> env_01FhGBrWgVhkh6yxfLvUupHt)
```

Three runs cleared at roughly 60–100 s, 170–200 s, and 170–180 s. **The window is not a fixed
value**, so nothing should be coded against a constant. The reading that fits every observation: a
restart is admitted immediately when there is a preserved environment *with a session* to reconnect
to, and otherwise has to wait out the dead server's registration.

Because §8 leaves `--create-session-in-dir` at its default (on), a healthy workspace always has at
least one session and lands in case 1. Case 2 is the narrow window between the server starting and
its first session existing — but that is exactly when a crash-loop would be happening.

### 3 — Three gates stop a headless start, and only one is in the design

Each exits immediately with **status 1** and a distinct message:

| Gate | Message | Fix without a human |
|---|---|---|
| Workspace trust | ``Error: Workspace not trusted. Please run `claude` in <dir> first…`` | `projects["<dir>"].hasTrustDialogAccepted = true` |
| Remote Control consent | `Enable Remote Control? (y/n)` — an interactive prompt, so it **hangs** rather than exits | `remoteDialogSeen = true` |
| Account / organization | ``Error: Unable to determine your organization for Remote Control eligibility. Run `claude auth login`…`` | the `oauthAccount` record must be present |

All three live in **`$CLAUDE_CONFIG_DIR/.claude.json`**. With all three seeded,
`claude remote-control` starts silently with no TTY interaction at all — verified, and that is what
[`seed-config.sh`](harness-02-restart/seed-config.sh) does.

The consent prompt is the dangerous one: it is not an error, it is a question. §8's
`devcontainer exec … claude remote-control` would sit at `Enable Remote Control? (y/n)` forever,
looking like a hung start rather than a missing config key.

The eligibility failure also prints a labelled debug block that is worth knowing about, because it is
the only machine-readable account state Claude Code offers:

```
[debug] Remote Control auth state:
  hasOAuthAccessToken=true
  oauthScopes=user:file_upload,user:inference,user:mcp_servers,user:profile,user:sessions:claude_code
  isClaudeAISubscriber=true
  oauthAccount.organizationUuid=unset        <- the one that failed
```

### 4 — The identity model is environment-first, not session-first

§8 assumes the supervisor scrapes `claude.ai/code/<id>` session URLs. Both exist, and the
**environment** is the durable one:

```
Environment ID: env_01XySFjY3UuosSX8AhpjFNM3
·✔︎· Connected · repo
    Capacity: 1/4 · New sessions will be created in an isolated worktree
Continue coding in the Claude mobile app or https://claude.ai/code?environment=env_01XySFjY3UuosSX8AhpjFNM3
```

Scraping notes that cost time to find:

- The **per-session URL is wrapped in an OSC 8 hyperlink** (`\e]8;;<url>\e\\<label>\e]8;;\e\\`), so the
  URL and its label run together in the byte stream:
  `claude.ai/code/session_011cWg…?from=cli72c05ced6a2d-joyful-dongarra]8;;`. Match the **id**, not
  the URL: `session_[A-Za-z0-9]+` and `env_[A-Za-z0-9]+` are unambiguous and survive the escapes.
  Anything that parses a URL to a space or end-of-line will capture the label too.
- ANSI cursor-movement sequences rewrite the status block in place, so the same line appears many
  times in a `pipe-pane` capture. The tail must be idempotent — upsert by id, which §8 already says.
- `Capacity: N/4` is the session count, free, on every repaint. It is what §8 wants for the "link out
  with a count" card.
- **The pre-created session counts toward capacity** (`1/4` with no client connected), so
  `--capacity 4` buys three on-demand sessions, not four.

### 5 — `--continue` is not available to a server-mode supervisor

```
Error: --session-id and --continue cannot be used with --spawn, --capacity, or --create-session-in-dir.
```

`-c` is for the single-session shape. §8's invocation uses `--spawn` and `--capacity`, so the resume
path is a plain restart — which, per result 1, is the one that works anyway.

### 6 — Exit status 1 means nothing on its own

Every failure above exits 1: untrusted workspace, ineligible account, 409, and even the flag
conflict. **The supervisor must discriminate on message text**, and the right action differs sharply
— one of these should be retried and the others must not be:

| Signature | State | Retry? |
|---|---|---|
| `409` / `already served by a terminal` | previous registration has not lapsed | **yes**, patiently |
| `Workspace not trusted` | feature or `postCreate` is broken | no — config error |
| `Unable to determine your organization` | credential present but account record missing | no — `awaiting_login` |
| `cannot be used with --spawn` | Drydock built a bad command line | no — bug |

---

## Consequences for the design

**A. §8's resume row is right and can be stated more strongly.** "Brings back every session the
previous server was serving" is what happens, and the environment id and session ids are preserved,
so **the link on the workspace card stays valid across a Drydock restart**. Re-deriving `rc_session`
rows from the tail is still correct, but it is a reconciliation, not a rebuild.

**B. A 409 is not a crash, and the restart policy must not spend its budget on one.** §8's backoff is
2 s → 60 s capped at 6 attempts in 10 minutes. Against a 170-second refusal that budget is spent
before the folder frees, and the workspace parks in `degraded` for a reason that would have cleared
itself. Give 409 its own state — *waiting for the previous server's registration to lapse* — retried
on a flat interval, not counted against the crash-restart budget, and surfaced in the UI as waiting
rather than failing. The event text should say which it is; this is precisely the §12 "a failure names
its step" case.

**C. Stop with `SIGTERM` and wait for the process to exit.** A graceful stop deregisters the folder,
prints `Environment preserved`, and lets the next start in immediately; `SIGKILL` on a session-less
server costs minutes. So the Phase 6 stop/rebuild/delete actions and the supervisor's own restart
should always `SIGTERM` first and only escalate to `SIGKILL` after a timeout — the ordinary
expectation for supervising a process, with a measured reason behind it here.

**D. §4: store the environment id, and treat it as the workspace's handle.** It is one id per server
per workspace, it survives restart, and `claude.ai/code?environment=<id>` is the link the card should
carry. `rc_session` remains a cache of discovered `session_…` ids, and `Capacity: N/4` gives the count
without enumerating anything.

**E. §7.1's "credential copied in" option does not work at all — not merely imperfectly.** The table
says copying the credential costs Drydock plaintext access and loses refreshes. The real blocker is
result 3: Remote Control needs `oauthAccount.organizationUuid`, which lives in `.claude.json`, not in
`.credentials.json`. A container with a perfectly valid copied credential makes model requests fine
and **refuses to start Remote Control**. The shared volume carries `.claude.json`, so §7.1's chosen
option is unaffected — but the row should be marked unworkable rather than second-best, and §2.3's
"credentials live in one relocatable file" is too narrow: *Remote Control eligibility* lives in a
second file in the same directory.

**F. The feature should seed the three config keys rather than run `claude` once.** §7.2 says
`postCreate` runs `claude` in `/workspace` to clear the trust dialog. That needs a TTY, answers only
one of the three gates, and leaves the consent prompt to hang a later headless start. Writing
`hasTrustDialogAccepted`, `remoteDialogSeen`, and the account record into `.claude.json` is
deterministic and testable. Two further notes:

- Trust is keyed on the **absolute path**, and every workspace mounts its clone at the same
  in-container path, so on the shared volume **one `/workspace` trust record satisfies every
  container** — a real benefit of the §7.1 sharing that was not called out.
- `--spawn worktree` puts on-demand sessions at *other* paths. Whether a worktree path needs its own
  trust record is **untested** and is the most likely way this bites: it would fail only for the
  second and later sessions, which is the hardest case to notice. Check it in Phase 5.

**G. One more thing the shared volume now shares: machine identity.** Answering the consent prompt
also writes `remoteControlMachineId` (a UUID) to `.claude.json`. Every container on the shared volume
will present the same machine id. Nothing observed breaks, but it is worth knowing before debugging
why fifteen workspaces look like one device.

**H. Re-run this spike on every Claude Code bump,** with Spike 00 and Spike 03. The reconnect
behaviour, the 409 window, the three config keys, and the OSC 8 wrapping are all undocumented
internals of `2.1.246`.

---

## What this spike did not cover

Additional sessions can only be created from the Claude app or claude.ai, so every measurement here
is against the one pre-created session. "Brings back **every** session" is therefore verified for one
session and inferred for several — the environment is what is preserved, and the server reattached to
it without being told anything, which is the mechanism that would carry the rest. Worth a two-minute
confirmation from a phone in Phase 5, along with the worktree-trust question in consequence F.

The 409 window was measured three times and varied by a factor of three. No attempt was made to find
what actually governs it (a server-side heartbeat timeout is the obvious guess), because consequence
B does not depend on the number — only on not coding against one.

---

## Reproducing

Needs `claude` on `PATH` (override with `CLAUDE_BIN=`), `tmux`, `jq`, and a git repo to serve.
`seed-config.sh` builds a `CLAUDE_CONFIG_DIR` that passes all three startup gates, copying the
account record and credential out of your real config:

```sh
cd docs/design/spikes/harness-02-restart
mkdir -p /tmp/ws && git -C /tmp/ws init -q && git -C /tmp/ws commit -qm init --allow-empty
./seed-config.sh /tmp/cfg /tmp/ws

export CLAUDE_CONFIG_DIR=/tmp/cfg
./run.sh sigterm   /tmp/ws   # result 1: graceful stop, identity preserved
./run.sh sigkill   /tmp/ws   # result 1: hard kill, identity preserved
./run.sh nosession /tmp/ws   # result 2: no live session -> 409 for minutes
```

Each round settles for `SETTLE` seconds (default 120) before starting, so a registration left by the
previous round cannot be mistaken for this one's. Runs start real Remote Control servers on the
signed-in account; they are short-lived and every round reaps what it started.

---

## Re-measured on Claude Code `2.1.289` (2026-10-04)

Re-run under the §11.1 ritual after the rebuild moved the version. The headline
holds — a plain restart reconnected the **same** environment and the **same**
session across `SIGTERM` in 2 seconds, with `Capacity: 1/4` preserved and the
`Environment preserved` notice unchanged. OSC 8 still wraps per-session URLs
(`ESC]8;;https://claude.ai/code/session_…?from=cli BEL`), and the status block
still reprints in place — 12 times in a 6.7 KB capture.

Two changes, and the first one breaks a parser.

**The `409` token is gone from the refusal message.** Result 6 recorded the
retryable signature as `` `409` / `already served by a terminal` ``. On `2.1.289`
the message is:

```
Error: This folder is already served by a terminal `claude remote-control` on this device. Stop it first.
Exiting in about 50 seconds.
```

The string `409` does not appear anywhere in it. **A classifier matching on the
status number would reclassify the one retryable refusal as unrecognised**, and
the supervisor would stop waiting for a registration that was about to lapse.
Match `already served by a terminal`. The new second line is a bonus: the wait
is now *announced*, where this spike had to measure it at 60–200 s — still not a
constant to code against, but something the UI can show.

**An untrusted workspace hangs on a TTY, and only errors when redirected.**
Result 3 recorded `Workspace not trusted` as a fast exit. That is true with
output redirected, and false on a terminal:

| stdout | behaviour |
|---|---|
| redirected (no TTY) | `exit 1`, `Error: Workspace not trusted. Please run …` |
| a real PTY | **hangs** on `Trust <dir>? [y/N]` |

Drydock's supervisor owns a PTY, so **the supervisor gets the hang.** That makes
two config gates that hang rather than fail — this one and the missing
`remoteDialogSeen` — and a hang has no error string to assert on, only an
absence. Both now have fixtures that assert a *timeout*.

The other three refusals reproduced verbatim, all still exiting `1`, so result 6's
central point — exit status is not a discriminator — is unchanged and now rests
on four recorded fixtures.
