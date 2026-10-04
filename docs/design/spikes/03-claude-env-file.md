# Spike 03 — Is `CLAUDE_ENV_FILE` re-read before each Bash command?

**Question** (design doc §10.3, the unverified row in *Adding or rotating a secret on a live session*):
*can a rotated secret reach a running session without restarting the supervisor, by pointing
`CLAUDE_ENV_FILE` at a script that runs `eval "$(drydock-secrets export)"`?*

**Answer: yes. The script runs once per Bash command, in the command's own shell.** Three separate
Bash calls in one session saw three different values from a counter the script bumps itself. "The
environment pulls" is therefore the primary route for §10.3, and the `CLAUDE.md` convention ("the
command pulls") drops to a fallback.

The behaviour comes with four constraints that are not obvious from the variable's name, and three of
them are the kind that would have been found in production instead. The script's **text** is read
only once per session, the text is passed to every command as **`argv`**, its **stdout and stderr are
prepended to every tool result the agent sees**, and it can **abort the command outright** — which
turns out to be the behaviour to want.

- **Verified against:** Claude Code `2.1.246`, Linux, `bash`.
- **Date:** 2026-10-02.
- **Harness:** [`harness-03-env-file/`](harness-03-env-file/) — re-runnable; see *Reproducing* below.

---

## What the mechanism actually is

Read out of the shipped binary and then confirmed empirically. Claude Code reads the file named by
`CLAUDE_ENV_FILE` as **text**, concatenates it with any env-file contributions from hooks, and caches
the result as a shell *script*:

```js
async function qon(e){ let t=We(), n=Bon();
  if (n.script !== void 0 && n.owner === t) return n.script;   // cache hit, keyed on owner
  let r=[], o=V.CLAUDE_ENV_FILE;
  if (o) try { let i=(await $on(o,"utf8")).trim();
    if (i) r.push(i), b(`Session environment loaded from CLAUDE_ENV_FILE: ${o} (${i.length} chars)`)
```

That script is then prepended to every Bash tool command. It is not parsed for `KEY=value` pairs and
it is not sourced once to capture an environment — it is shell code that runs in the same shell as
the command, every time.

## Results

### 1 — The script runs once per Bash command, 1:1

The env file bumps a counter on disk and exports the new value, so the exported value is
observably different on every execution. Three Bash calls, one session:

```
  CMD  echo "READ-A=$DRYDOCK_SPIKE_VALUE ..."      OUT  READ-A=v1
  CMD  echo "READ-B=$DRYDOCK_SPIKE_VALUE ..."      OUT  READ-B=v2
  CMD  echo "READ-C=$DRYDOCK_SPIKE_VALUE ..."      OUT  READ-C=v3

exec n=1 pid=10924 ppid=10809    <- written by the env file itself
exec n=2 pid=10944 ppid=10809
exec n=3 pid=10956 ppid=10809
  total=3  counter=3
```

Three commands, three executions, three distinct values, each in a fresh process under a common
parent. A value the broker returns at 14:05 reaches a command issued at 14:05 even though the session
started at 09:00.

### 2 — The *text* is read once per session; only execution repeats

The `mutate` mode has the env file append `export DRYDOCK_SPIKE_MUTATED=yes` to itself on its first
execution. The write lands on disk — and no later command ever sees the variable:

```
  OUT  READ-A=v1 mutated=unset
  OUT  READ-B=v2 mutated=unset
  OUT  READ-C=v3 mutated=unset
```

Claude Code's own log agrees, appearing exactly once per session against three executions:

```
[DEBUG] Session environment loaded from CLAUDE_ENV_FILE: …/envfile.sh (2624 chars)
[DEBUG] Session environment script ready (2624 chars total)
```

That is the `Bon()` cache above. **Values are live; the script is frozen.**

### 3 — The whole script text is passed as `argv`

The env file logged its own `/proc/$$/cmdline`. The command process is handed the entire prelude
followed by the command:

```
argv: … <the full 2624 characters of the env file, verbatim> …
      : && shopt -u extglob … && eval 'echo "READ-C=$DRYDOCK_SPIKE_VALUE …"' < /dev/null && pw…
```

So the env file's contents are visible in `ps`, inside the container and to anything that can read
that process's `cmdline`. What is *not* in `argv` is the output of anything the script runs: a
`eval "$(drydock-secrets export)"` line appears as those 34 characters, and the values the subshell
produces are consumed by `eval`. §10.3's "values never appear in `argv`, so they stay out of `ps`"
holds — but it holds *because the helper is invoked from the prelude*, not as a general property.

### 4 — Prelude stdout and stderr are prepended to every tool result

With the env file writing one line to each stream, every command's output carries both:

```
  OUT  PRELUDE-STDOUT-LEAK\nPRELUDE-STDERR-LEAK\nREAD-A=v1
```

The agent noticed unprompted and reported it back ("each call's output was preceded by …"), which is
the real cost: noise here is injected into the model's context on every single Bash command, and it
sits in front of the output the agent is trying to read.

### 5 — `exit` in the prelude aborts the command; a failing last command does not

Two different failure shapes, two different outcomes:

| Env file ends with | Command runs? | What the agent sees |
|---|---|---|
| `false` (non-zero last command) | **Yes** | Command output, with the prelude's stderr above it |
| `exit 7` | **No** | `Exit code 7` and the prelude's stderr, nothing else |

In the `exit 7` run, even a bare `echo hi` the agent tried as a fallback returned `Exit code 7`. The
whole shell terminates before reaching the command, and the agent correctly concluded the tool was
unavailable rather than inventing output.

---

## Consequences for the design

**A. §10.3's "the environment pulls" row is verified and becomes the answer.** `CLAUDE_ENV_FILE`
points at a Drydock-written script; the next Bash command after a rotation gets the new value with no
restart and no agent cooperation. The shared-`CLAUDE.md` convention stays as documentation of the
explicit route, not as the mechanism the design depends on.

**B. The env file must be a constant one-liner that delegates to the helper.** Both of the first two
constraints point the same way. Because the text is cached per session, anything Drydock might want
to *change* must not live in the text; because the text is `argv` on every command, the text must not
contain values and should stay short. One line —
`eval "$(drydock-secrets export)"` — satisfies all of it, and the file then never needs rewriting for
the life of the workspace.

**C. The restart table in §10.3 gains a row, and it is a narrow one.** Changing the env file's *text*
(switching mechanism, inlining a grant list, adding a second helper) needs a supervisor restart,
exactly like an MCP server does. Changing which secrets are *granted* does not, because the grant set
is resolved inside the broker on each call and the text never changes. Worth stating explicitly in
the table so a future change does not quietly assume text edits are live.

**D. `drydock-secrets export` must be silent on success — this is now a correctness requirement, not
tidiness.** Any byte it writes to stdout or stderr is prepended to every Bash result the agent reads
for the rest of the session. A progress line, a deprecation warning, or a stray `set -x` would
contaminate the agent's view of every command it runs. This belongs with the redact-by-default
invariant: the helper writes `export` statements to stdout for `eval` to consume and nothing else,
and diagnostics go to Drydock's event log over the socket, never to the terminal.

**E. Fail closed, by `exit`ing — the broker being down should break commands loudly.** The
alternative is worse than it looks: a prelude that fails soft lets `pytest` run with
`TEST_DATABASE_URL` unset, and the agent then debugs a connection error in the repo for twenty
minutes. `exit 69` (`EX_UNAVAILABLE`) with a one-line `Drydock: secrets broker unreachable` on stderr
gives every command the same legible failure, and per result 5 the agent reads it correctly and stops
rather than fabricating. The cost is that a broker outage makes the workspace unusable instead of
subtly wrong, which is the right trade for something whose whole job is holding credentials.

**F. The script runs per command, so the broker is called per command.** At one `GET-SECRETS` per
Bash call the socket is on the hot path of everything the agent does. It is a Unix socket to a local
process holding values in memory, so the cost is microseconds — but the broker must not do anything
expensive per call (no GitHub round-trip, no KDF per request), and §10.2's decryption should happen
once at grant-resolution time rather than on every export. Not a problem today; a trap if the broker
ever grows.

**G. Pinning the Claude Code version now guards this too.** The 1:1 execution, the text cache, the
`argv` composition, and the `exit`-aborts-the-command behaviour are all undocumented internals of
`2.1.246`. Add this spike to the list of things to re-run on a bump, alongside Spike 00.

---

## What this spike did not cover

The measurements were taken through headless `claude -p`, not inside `claude remote-control`. The
Bash tool is the same code path in both, and the prelude is composed from the process's environment,
which §8 sets on the server process — so the expectation is that it behaves identically. But the
cache in result 2 is keyed on an `owner` the binary derives per client, and a multi-session
`remote-control` server may therefore read the file once *per session* rather than once per process.
That difference does not affect values, which are live either way; it only changes how long a text
edit stays stale. **Confirm it in Phase 5** when a real `remote-control` server is running, by
rotating a secret and watching a session pick it up.

---

## Re-measured on Claude Code `2.1.289` (2026-10-04)

Re-run under the §11.1 ritual. **Everything reproduced exactly**: three Bash
calls produced three prelude executions with three distinct values (`v1`, `v2`,
`v3`), the script text was still read once per session (`mutated=unset` on every
call, one `Session environment loaded from CLAUDE_ENV_FILE` line), the whole
script text still arrives as `argv` on every command shell, and stdout/stderr
from the prelude are still prepended to the agent's view of each result.

No consequence in this report changes.
