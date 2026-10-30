# Spike 08 — Is `claude --bg --rc` safe and workable per workspace?

**Question.** [Spike 06](06-terminal-attach.md) found that Claude Code's own background mode,
`claude --bg --remote-control` with `claude attach <id>`, gives an operator a conversation shared
between a terminal and the Claude app with no tmux. It held that back until measured in two
containers, because the daemon keeps its state in `CLAUDE_CONFIG_DIR`, which in Drydock is the
volume every workspace shares (design §7.1), and refreshes the shared OAuth credential on its own
schedule. This spike asks the two halves of that: is it *safe* to run per workspace on a shared
volume, and is it *workable* (can Drydock supervise it, and does it behave on the volume)?

**Status: phase 1 done (a reading of the `2.1.289` binary, nothing measured); phase 2 open.**

**Answer to "safe", phase 1.** The question turned out to be bigger than `--bg`. Reading the binary
shows the shared volume already carries everything Claude Code treats as the *user's* configuration,
and much of that runs code. So code in one workspace container that can write to
`CLAUDE_CONFIG_DIR` can very likely run code in every other workspace's Claude Code — and that
Claude Code has its own workspace's broker socket in reach, so its repository's GitHub token and its
secrets. `--bg` adds a second path of the same kind: its roster, jobs and launch requests are in the
shared directory. This contradicts the security review's §1.2 *Compromised container* row as it was
written ("Wants: another repo's token, another workspace" — not reachable), with or without `--bg`.

**Decision (the owner, 2026-10-30).** Isolation between workspaces is not a goal. Drydock adopts the
threat model *repositories are trusted; workspaces are mutually trusted, one trust domain; the
agent's own actions are not trusted, but are held only at a few cheap chokepoints* (design §13,
*The threat model*). The cross-workspace reach is accepted and documented (design §7.1, §13.4,
§15.2; security review addendum), not closed. "Safe" is therefore answered by the decision, and
phase 2 measures only whether `--bg --rc` is workable.

- **Read from:** the Claude Code `2.1.289` binary (its bundled JavaScript), by reading only. **No
  binary was run for this phase**, and no result below is a measurement. Each is marked *read*.
- **Date:** 2026-10-30.
- **Harness:** none yet. Phase 2 will add `harness-08-bg-rc/`, with a README naming the command per
  result, as every other spike has.

---

## Results (phase 1)

| # | Reading | Result | How |
|---|---|---|---|
| 1 | User-level `settings.json` under `CLAUDE_CONFIG_DIR` | its `hooks` are loaded by every Claude Code process whose `CLAUDE_CONFIG_DIR` it is, and run commands before and after tool calls | *read* |
| 2 | Plugins and skills under `CLAUDE_CONFIG_DIR` | loaded as the user's own, in every process on that directory | *read* |
| 3 | The user-level `CLAUDE.md` | read into every session's context | *read* |
| 4 | `.claude.json`'s `mcpServers` | user-scope MCP servers, started by every process on that directory (a `stdio` server is a command) | *read* |
| 5 | `claude --bg`'s daemon state | the roster (`daemon/`), the jobs (`jobs/`) and its launch requests are under `CLAUDE_CONFIG_DIR`; its sockets are under `/tmp/cc-daemon-<uid>/` (Spike 06, measured), so per container | *read* |
| 6 | `CLAUDE_SECURESTORAGE_CONFIG_DIR` | moves `.credentials.json` **and** `.oauth_refresh.lock` together to another directory, leaving the rest of `CLAUDE_CONFIG_DIR` where it is; undocumented (requested in [anthropics/claude-code#79223](https://github.com/anthropics/claude-code/issues/79223)) | *read* |

### 1 — The shared volume is one trust domain

In Drydock every workspace container mounts `drydock-claude-config` at `CLAUDE_CONFIG_DIR`
(design §7.1). Results 1–4 put four code-running or context-writing inputs in that one directory, so
a write by one container is an input to every other's Claude Code. The likely path: a
prompt-injected agent in workspace A writes a hook into the user `settings.json`; the next tool call
in workspace B runs it, inside B's container, where B's broker socket is mounted
(`GET-TOKEN scope=git`, `GET-SECRETS`). The broker is right to answer — the socket is B's — and
socket-as-identity holds; what the shared volume adds is a way for A's code to *be* in B. It reaches
only workspaces that are running (a hook runs in a live Claude Code), and never the App private key,
the secrets master key or the host: those stay behind the chokepoints design §13 keeps.

Not measured: whether every one of results 1–4 is picked up by an already-running
`remote-control` server and its sessions without a restart, and which one is fastest. That would
matter for an isolation design; under the adopted model it does not, and phase 2 does not measure
it.

### 2 — `--bg` adds a shared roster

Result 5 puts the daemon's roster, jobs and launch requests on the shared volume while its sockets
and the pids it records are per container. So with `--bg` in use, one container could very likely
launch a session, or act on one, in another container's daemon. That is one more path inside the one
trust domain, not a new boundary crossed. What it *does* raise is a workability question — whether
two containers' daemons on one roster confuse each other — which is phase 2's.

### 3 — The isolation fix considered, and declined

Result 6 suggests a fix: give each container its own `CLAUDE_CONFIG_DIR` and share only the
credential, through `CLAUDE_SECURESTORAGE_CONFIG_DIR` on the volume. Because it moves the credential
and the refresh lock *together*, it would keep Spike 00's cross-container lock — the reason
mitigation (b) in design §7.1 was struck. It would not by itself carry `oauthAccount`, which lives
in `.claude.json` (Spike 02) and Remote Control needs.

**Declined**, for two reasons: the variable is undocumented, and a bump could move or drop it
silently, which is the failure every Claude Code invariant in Drydock exists to catch; and with the
adopted model, isolation is not a goal it would serve. If the model is ever tightened, the
**documented** route is managed settings baked into the image that restrict hooks and MCP servers
(design §15.2), not this variable.

### Residual risk, for the operator

A prompt-injected agent in workspace A can likely reach workspace B's repository token and secrets
through a shared hook. Do not run a sensitive repository beside one whose agent reads a lot of
hostile input — issue bodies from strangers, untrusted dependencies, scraped pages.

---

## Phase 2 — what it will measure

Operational viability only; isolation is out of scope. Each in two real workspace containers on one
shared volume, under the Feature's environment.

- **The refresh lock with the daemon.** The daemon schedules its own proactive refresh (Spike 06:
  about four minutes before the access token's `expiresAt`). Does it take Spike 00's
  `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock` and adopt a peer's newer token like every other
  refresher, with two containers' daemons and a `remote-control` server contending? A live refresh
  **needs a dedicated login by the owner** for the run, and is **never** run on a copied credential:
  refreshing a copy rotates the real refresh token away and blanks the real login (design §7.1).
- **Supervisability.** Can Drydock start, stop and observe a `--bg --rc` session the way §8
  supervises the server: a start it can classify, a `SIGTERM` stop that deregisters, a way to tell
  it exited (Spike 06: a killed worker settled as `done` and the daemon exited), and the environment
  id and session ids scraped from somewhere stable — OSC 8 targets, `sessions/<pid>.json`, or the
  daemon's own listing.
- **Environment inheritance.** Do daemon-spawned sessions inherit the container's environment as a
  direct `claude` does — in particular the variables CLAUDE.md says must stay unset
  (`ANTHROPIC_BASE_URL`, `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_GROWTHBOOK`), `DISABLE_AUTOUPDATER=1`, and
  `CLAUDE_ENV_FILE`, without which a session runs with no secrets — or the daemon's, from whichever
  shell first started it?
- **Two daemons, one roster.** What `claude agents` and `claude attach` list in each container, and
  whether a job one container launched is shown, resumed or reaped by the other.
- **Lifecycle across container stop and rebuild.** What a stopped or rebuilt container leaves in
  `daemon/` and `jobs/`, whether the next start reads it as live, and whether anything left there
  blocks or confuses a later `--bg` in the same or another workspace.

## Open questions

- Whether phase 2 should run at all before someone wants `--bg` in a workspace: Spike 06's cheap
  answer, `claude --rc` in the container's terminal, needs no daemon.
- Whether a running `remote-control` server picks up a hook or MCP server written after it started
  (result 1's *not measured*), which would only matter if the model is tightened.
