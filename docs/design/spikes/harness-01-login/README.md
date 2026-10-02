# Spike 01 harness — the login handshake

Answers: *can the two-way login handshake be driven by a program that owns the PTY, and what patterns
catch the login URL and the success marker?* Report:
[`../01-login-handshake.md`](../01-login-handshake.md).

```sh
./run.sh probe            # scrape the authorize URL and the paste prompt
./run.sh badcode          # write a malformed code, capture the rejection
./run.sh login            # the real handshake — needs a browser and a human
./auth-status.sh          # what `claude auth status --json` reports per credential state
COLS=200 ./run.sh probe   # reproduces the mid-token URL wrapping
```

`run.sh` starts `claude auth login --claudeai` in a throwaway `debian:bookworm-slim` container with
the `claude` binary bind-mounted and an **empty** `CLAUDE_CONFIG_DIR`, driven through a tmux PTY. The
container is the faithful case: design §2.2 is specifically about a container, where no local callback
server is reachable and the flow falls back to a pasted code. It is also what makes this safe — the
real credential is never visible to it.

`badcode` is the useful trick. Submitting a deliberately malformed code proves the whole write path
(Drydock → PTY stdin → prompt → verdict) without needing a real authorization, and it exercises the
same verdict branch the success path uses. `login` is the only mode that needs a human.

`auth-status.sh` builds four config dirs — missing, valid, expired, and blanked (the Spike 00
tombstone) — and prints what `claude auth status --json` says about each. All tokens are **fake**;
none can authenticate, which is fine because the question is what the command reports.

Defaults to a 1000-column PTY on purpose: at 200 columns the 450-character authorize URL wraps
mid-token and a line-based regex silently captures a fragment. `run.sh` matches against the stream
with newlines removed so it works either way, and prints which case it hit.

Never run `claude auth logout` while investigating this. Per Spike 00 it blanks the shared credential
for every container at once.
