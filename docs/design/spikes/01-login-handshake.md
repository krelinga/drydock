# Spike 01 — The scripted login handshake against a PTY

**Question** (design doc §14 Phase 0, and §7.2): *can the two-way login handshake be driven by a
program that owns the PTY — and is there a regex that reliably catches the login URL and the success
marker?*

**Answer: yes, and the flow is exactly what §2.2 predicted.** In a container the browser cannot reach
a local callback, so Claude Code prints an authorize URL and waits at a paste prompt; writing a code
into the PTY reaches that prompt and produces a verdict. The regexes are below.

Three things the design has slightly wrong, each of which would have cost an afternoon later:

- **The authorize URL wraps mid-token at terminal width.** At 200 columns a line-based regex captures
  a fragment of a 450-character URL. Drydock owns the PTY, so the fix is to allocate it wide.
- **The pasted code has a known shape — `<code>#<state>`** — so Drydock can reject a mis-paste
  *before* writing anything to the PTY, instead of round-tripping through the terminal and the server.
- **The paste prompt does not echo.** The PTY buffer never contains the code, so §7.2's "scrape,
  match, redact, then store" is guarding something that is not there. The invariant should stay, for a
  different reason.

The spike also answers a question it was not asked. **`claude auth status --json` exists**, is
machine-readable by default, and detects the Spike 00 tombstone — which takes the §7.3 expiry watch
out of the terminal-scraping business entirely.

- **Verified against:** Claude Code `2.1.246`, `debian:bookworm-slim` container, `tmux` as the PTY.
- **Date:** 2026-10-02.
- **Harness:** [`harness-01-login/`](harness-01-login/) — re-runnable; see *Reproducing* below.

---

## Results

### 1 — In a container, the flow is the code-paste flow

Run in a throwaway container with an empty `CLAUDE_CONFIG_DIR` (the faithful case — §2.2 is
specifically about a container, and a host with a reachable browser takes a different path):

```
Opening browser to sign in…
If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=…
Paste code here if prompted >
```

The `redirect_uri` is `https://platform.claude.com/oauth/code/callback` — a remote callback, not
`localhost`. Nothing is listening inside the container, which is precisely why the code has to come
back by hand. §2.2 stands as written.

Worth noting in passing: the URL's `scope` list includes `user:sessions:claude_code`, the scope §2.1
says a `setup-token` credential cannot provide. The constraint that shapes the whole design is
visible right there in the login URL.

### 2 — Scraping the URL: wrapping is the trap

The URL is 450 characters. The PTY wraps it at the terminal width, **mid-token**, so consecutive rows
split a parameter in half:

```
…&scope=org%3Acreate_api_          <- row ends here, inside the value
key+user%3Aprofile+user%3Ainference…
```

At `-x 1000` the harness reports `URL appears intact on a single line (longest line: 947 chars)`.
Two defences, and Drydock should use both:

```
allocate the PTY at >= 1000 columns          # the URL fits on one row
match against the stream with newlines removed   # survives a narrow PTY anyway
```

The pattern that works, applied to the de-wrapped stream:

```
https://claude\.com/cai/oauth/authorize\?[A-Za-z0-9&=_%.~+-]+
```

### 3 — Writes reach the prompt, and a bad code is recoverable

Prompt text, exactly: `Paste code here if prompted >`

Writing `not-a-real-code-12345` followed by Enter produces, within a second or two:

```
Invalid code. Please make sure the full code was copied.
```

The process **stays alive at the prompt and accepts another code** — verified by sending a second bad
one and getting a second rejection. So a mis-paste does not require tearing down the handshake and
re-deriving a new URL; the same PKCE challenge and `state` remain valid. Drydock's UI can simply ask
again.

One consequence for the scrape: after a failure the prompt is **not** re-printed. Nothing should wait
for the prompt string to reappear as a readiness signal — the process is continuously ready for
input.

### 4 — The code's shape is known, so validate it before touching the PTY

The rejection comes from this, read out of the binary:

```js
let [k, ee] = P.split("#");
if (!k || !ee) { L({ state: "error",
  message: "Invalid code. Please make sure the full code was copied",
  toRetry: { state: "waiting_for_login…
```

The pasted value is `<code>#<state>`, and a missing half is rejected locally before any network call.
`toRetry` is the mechanism behind result 3.

So Drydock can apply `^[^#\s]+#[^#\s]+$` in its own handler and return a precise error immediately on
a truncated paste — the single most likely user error — without writing to the PTY, waiting, and
scraping a verdict.

### 5 — The prompt does not echo: the code never enters the PTY buffer

The entire captured PTY log for a full bad-code round is **1078 bytes**, and the submitted code
appears nowhere in it — not raw, not after stripping ANSI escapes, not after removing newlines (which
would catch a character-at-a-time echo interleaved with cursor moves). The log goes straight from the
prompt to the verdict:

```
Paste code here if prompted > Invalid code. Please make sure the full code was copied.
```

### 6 — The success marker has several forms

`grep` over the binary finds `Login successful`, `Login successful.`, and `Login successful. Press …`
among others. **Match it as a prefix, never as an exact line.**

### 7 — Cancel leaves nothing behind

Killing the container at the paste prompt — what §7.2's cancel endpoint would do — leaves no
credential and nothing to reap:

```
container after kill: '' (gone)
config dir left behind: backups .claude.json
credential written: no
```

### 8 — `claude auth status --json` is machine-readable, and sees the tombstone

Not part of the question, but it lands on §7.3. The command defaults to JSON (`--text` for humans).
Against four synthetic credential states:

| Credential state | `auth status --json` |
|---|---|
| no credential file | `{"loggedIn":false,"authMethod":"none","subscriptionType":null}` |
| valid, far from expiry | `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}` |
| **expired** (`expiresAt` in the past) | `{"loggedIn":true,…}` — **expiry is not considered** |
| **blanked** (Spike 00 tombstone) | `{"loggedIn":false,"authMethod":"none",…}` |

And when logged in it also reports `email`, `orgId`, `orgName`. That `orgId` is the same field whose
absence makes Remote Control refuse to start (Spike 02, result 3).

Two limits to respect: it says nothing about expiry, and **a blanked credential is indistinguishable
from a missing one** in this output.

---

## Consequences for the design

**A. Allocate the login PTY wide, and de-wrap before matching.** 1000 columns, plus stripping newlines
from the captured stream before applying the pattern. Either alone would do; both together make the
scrape independent of a terminal-size decision nobody will remember. This belongs in §7.2 next to the
scraping note, because a fragment of a URL is the failure that *looks* like a working scrape right up
until a user clicks the link.

**B. Shape-check the code in Drydock, before the PTY.** `^[^#\s]+#[^#\s]+$` on the submitted value,
with its own error message. It makes the most common user error (a partial copy) a local, instant,
precise failure instead of a terminal round-trip, and it removes one case from the set of things the
scrape has to interpret.

**C. §7.2's redaction note is aimed at the wrong place — keep the rule, change the reason.** The PTY
buffer does not contain the code (result 5), so "the login PTY buffer contains the one-time code" is
not true of `2.1.246`. But Drydock *receives* the code over HTTP and holds it in memory to write it,
so it can still leak through a request log, an error string, an event row, or a crash dump. The
invariant should read: **the code must never be logged by Drydock, and the PTY buffer is still not
stored verbatim** — the second half because non-echoing is undocumented behaviour of a pinned version
and is exactly the kind of thing a patch release changes silently.

**D. Match `Login successful` as a prefix** (result 6), and treat `Invalid code` as the other terminal
verdict. Those two plus a timeout are the complete state machine for step 5.

**E. §7.3 needs no terminal scraping at all — and gains a pre-flight.** Replace "runs a
`claude /status`-equivalent and records `expires_at`" with two mechanical reads:

- `claude auth status --json` in the auth container for the verdict: `loggedIn:false` is the
  **signed-out / tombstoned** condition that §7.3 wants its own message for, and it is a clean boolean
  rather than a scrape. Because the same call returns `orgId`, it doubles as the Remote Control
  eligibility check from Spike 02 — so the supervisor can refuse to start with a legible reason
  instead of launching a process that exits 1.
- `.credentials.json → claudeAiOauth.expiresAt` read directly off the shared volume for the
  three-day countdown, which `auth status` does not report. Spike 00 established the file is written
  by atomic rename, so an unsynchronised read is safe.

To tell "blanked" from "never logged in" — both report `loggedIn:false` — check whether the file
exists with empty token strings (blanked, *everyone* just lost access) or is absent (never logged in).
The two deserve different words in the UI, which is §7.3's whole point.

This also narrows §15.3's claim about terminal scraping being one of the two weakest dependencies. The
expiry watch comes off that list. What remains is the login handshake's two scrapes (URL and success
marker) and the session-discovery tail — both of which now have measured, pinned patterns rather than
assumed ones.

**F. The five-minute deadline, cancel endpoint, and hard kill in §7.2 are all right as written**, and
results 3 and 7 add that a wrong code does not need to burn the handshake and a cancel leaves no
state. Worth saying in the UI: a bad code can simply be re-entered.

**G. Re-run this spike on every Claude Code bump,** with Spikes 00 and 02. The URL shape, the prompt
text, the `#` code format, the non-echoing prompt, the success strings, and the `auth status` schema
are all undocumented internals of `2.1.246`.

---

## What this spike did not cover

**The success path was not exercised end to end.** Completing a login needs a human to authorize in a
browser and paste a real one-time code, so `Login successful`, the credential write, and the mode-6
prefix rule come from the binary and from the harness's code path rather than from an observed
success. Everything up to and including the verdict branch is observed, using an invalid code to
drive it.

`./run.sh login` exists to close that gap: it prints the URL, waits for a pasted code, writes it, and
reports which marker matched. It takes about two minutes at a keyboard and should be run once before
Phase 5 starts, against a throwaway config dir so the real credential is untouched. Until then, treat
the success-marker match as the one assertion in this report that is reasoned rather than measured.

Also untested: whether the authorize URL has a server-side deadline of its own (the harness holds it
open for minutes without complaint, but that is not the same as establishing a bound).

---

## Reproducing

Needs `claude` on `PATH` (override with `CLAUDE_BIN=`), `tmux`, `jq`, and Docker. Every mode runs in a
throwaway container with an empty `CLAUDE_CONFIG_DIR`, so the real credential is never read or
written.

```sh
cd docs/design/spikes/harness-01-login
./run.sh probe      # results 1, 2: scrape the URL and the prompt; no human needed
./run.sh badcode    # results 3, 5: write a bad code, capture the rejection; no human needed
./run.sh login      # the real handshake — needs you to authorize in a browser
./auth-status.sh    # result 8: what auth status reports for four credential states
COLS=200 ./run.sh probe   # reproduces the mid-token URL wrapping
```

`auth-status.sh` builds four config dirs with **fake** tokens; none can authenticate, which is fine
because the question is what the command reports. Never run `claude auth logout` against the shared
volume while investigating this — per Spike 00 it blanks the credential for every container at once.

---

## Re-measured on Claude Code `2.1.289` (2026-10-04)

The 4 October devcontainer rebuild moved Claude Code from `2.1.246` to `2.1.289`,
because the Feature carried no version pin. Re-running this harness under the
§11.1 ritual confirmed most of the above and **falsified result 2**.

**Result 2 is retracted. The authorize URL does not wrap.** Forced PTY widths of
80, 200 and 1000 columns all produce a byte stream whose longest line is 987
characters, and a **per-line** match yields the complete 465-character URL at
every width:

| pane width | longest raw line | URL matched per-line | rows holding it when *rendered* |
|---|---|---|---|
| 80 | 987 | **465 (complete)** | 4 |
| 200 | 987 | **465 (complete)** | 3 |
| 1000 | 987 | **465 (complete)** | 1 |

Claude Code writes the URL unbroken; the terminal soft-wraps it for display. The
original evidence for "wraps mid-token" was a `tmux capture-pane` observation —
which renders the wrapped pane by construction — attributed to the stream.
`capture-pane` was the wrong instrument, and the harness's own output always
said `URL appears intact on a single line`, which should have caught it.

What survives: de-wrapping before matching is cheap insurance against a future
version that *does* wrap, so **consequence A stands as a recommendation and
falls as a hazard**. The `login-url-80col` fixture is kept with its assertion
inverted — it now guards against a regression *into* wrapping rather than
demonstrating one.

Also changed, and both are real: the URL grew from 450 to **465 characters**
because the scope list gained `user:plugins`, so any length-range assertion
needs widening. Everything else reproduced exactly — the prompt string, the
`Invalid code` retry-in-place, the non-echoing prompt (the submitted code
appears 0 times in the transcript), the clean cancel, and all four rows of the
`auth status --json` matrix including `expired` still reporting `loggedIn:true`.
