# Spike 00 — Concurrent credential refresh on a shared volume

**Question** (design doc §15.1, the one open question that could force a redesign): *with N containers
sharing one `CLAUDE_CONFIG_DIR`, can two credential refreshes interleave badly enough to corrupt
`.credentials.json` or strand the fleet without a valid token?*

**Answer: no. The shared volume is safe.** Mitigation (a) in §7.1 holds; mitigations (b) and (c) are
not needed, and (b) would actively make things worse (see *Consequences*, E).

Claude Code already solves this problem internally, and it solves it with a lockfile that lives
**inside `CLAUDE_CONFIG_DIR`** — which is exactly the directory Drydock shares. Cross-container
mutual exclusion therefore comes for free, as a side effect of the sharing that created the worry.

- **Verified against:** Claude Code `2.1.246`, devcontainer CLI `0.88.0`, Docker `29.7.2`, DinD.
- **Date:** 2026-08-26.
- **Harness:** [`harness/`](harness/) — re-runnable; see *Reproducing* below.

---

## What the mechanism actually is

Read out of the shipped binary (`claude.exe`, a bun single-file executable — the JS is embedded and
greppable) and then confirmed empirically. The refresh routine is a textbook double-checked lock:

1. **Optimistic pre-check.** Re-read `.credentials.json`, compare `accessToken` against the one this
   process started from. If it differs, a peer already refreshed — **adopt the peer's token and
   return without refreshing.** Telemetry event: `tengu_oauth_token_refresh_race_resolved`.
2. **Acquire `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock`.** A `proper-lockfile`-style lock: the lock
   *is a directory*, so mutual exclusion rests on `mkdir(2)` atomicity. `stale: 60000`,
   heartbeat `update: 5000`. A second lock is also taken on the realpath-resolved path.
3. **Re-read again under the lock** and repeat the same peer-refresh check.
4. **`if (d.isCompromised()) return "lock_timeout"`** — if the lock went stale and was stolen while
   we waited, abort *instead of* refreshing. The HTTP request also carries the lock's `AbortSignal`,
   so a lock lost mid-flight cancels the request.
5. On `ELOCKED`: 5 retries, 1–2 s jittered backoff, then give up with `lock_timeout` — no refresh,
   no write.
6. Writes go through temp file → `fsync` → `rename()` → directory `fsync`, mode `0600`.

There is no path in which two processes are inside the refresh critical section at once, and no path
in which a process that lost the race writes a credential over a newer one.

## Results

### 1 — Cross-container mutual exclusion holds

Four containers on one named volume, started simultaneously, all with an expired credential so all
four want to refresh. The OAuth host was blackholed (`--add-host platform.claude.com:203.0.113.10`)
so the lock holder stalls on TCP connect and the critical section is wide enough to observe from
outside. Watching the volume's backing directory:

```
  1310 ms  LOCK_ACQUIRED  ino=13632322 type=dir
 31309 ms  LOCK_RELEASED                        <- ~30 s: blackholed connect timeout
 31931 ms  LOCK_ACQUIRED  ino=13632246 type=dir <- different inode: a different container
 36181 ms  LOCK_RELEASED
 40002 ms  (watch ended)
```

The lock **is created inside the shared volume**, it **is a directory**, and **it is never held by
two containers at once**. Four contenders serialized cleanly and left nothing behind.

### 2 — Writes are atomic; torn reads are impossible

Three containers racing to refresh against the real endpoint while a reader hammered the file:

```
  1411 ms  CRED_CHANGED 13632282:246 -> 13632359:136
TOTALS reads=16185 torn_or_empty=0
```

**The inode changes on every write** — that is `rename()`, not truncate-in-place. A reader either
sees the whole old file or the whole new one. 16,185 reads spanning a live write produced zero torn
or empty results. The credential store's `corrupt` state exists, but concurrent writers cannot
produce it.

### 3 — A container killed mid-refresh wedges the fleet for at most 60 s

Planted an abandoned lock directory and backdated its mtime, simulating a container `docker kill`ed
while holding it:

| Abandoned lock age | Elapsed | Outcome |
|---|---|---|
| **10 s** (< 60 s stale threshold) | 26.2 s | Never acquired. Retried, gave up, fell back to the existing token. **Credential left completely intact.** |
| **90 s** (> 60 s stale threshold) | 1.1 s | Stole the stale lock immediately, proceeded, cleaned up after itself. |

So the worst case is: refreshes are blocked for up to 60 seconds, then the fleet self-heals.
**Drydock does not need to clean up lockfiles**, and must not try to — a reaper racing the
heartbeat is strictly worse than waiting 60 seconds.

### 4 — The failure that *does* have fleet-wide blast radius: the tombstone

This was not what §15.1 was worried about, and it is the finding worth designing around.

On a definitive `invalid_grant` from the token endpoint, Claude Code **blanks the shared credential
in place** rather than deleting it:

```json
{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":[...],"subscriptionType":"max"}}
```

Every container on the volume loses auth at the same instant. Two things keep this from being a
concurrency bug, both verified:

- **It is compare-and-swap guarded.** The write only lands if the refresh token still on disk is
  still the one that just failed:
  ```js
  await ne().mutate((o) => {
    let i = o.claudeAiOauth;
    if (!i || i.refreshToken !== e) return o;   // peer already rotated it -> no-op
    return {...o, claudeAiOauth: {...i, refreshToken: "", accessToken: "" ...
  ```
  A losing racer **cannot** blank a winner's fresh credential.
- **Network failure does not trigger it.** In the blackholed run (test 1) the refresh failed and the
  credential was left byte-for-byte intact. Only an explicit server rejection tombstones.

So the tombstone is an honest statement about the account — the login really is dead — not a race
artifact. But its blast radius is the whole fleet, which is a Drydock concern.

---

## Consequences for the design

**A. `accessToken: ""` is a distinct state and Drydock must recognise it.** It means *revoked or
definitively rejected*, not *expired*. It is also what one container's `/logout` does to everyone.
The §7.3 expiry watch should treat a blanked credential as its own condition with its own message
("signed out — sign in again"), separate from "expires in 3 days".

**B. Nothing to reap.** Lockfiles self-heal in 60 s. No cleanup code, no startup sweep.

**C. The credential volume must be a local Docker volume — never NFS or CIFS.** The entire guarantee
rests on `mkdir(2)` atomicity, which is exactly what network filesystems do not provide. This is now
a hard constraint, not a preference.

**D. Boot is the one place contention could bite.** Reconciliation restarts many workspaces at once;
if their credential is near expiry they all reach for the lock together. The budget is 5 retries at
1–2 s ≈ 5–10 s. Losers get `lock_timeout`, keep using the existing access token, and retry later —
degraded, not broken. Worth watching if the workspace count grows; not worth pre-solving.

**E. §7.1 mitigation (b) must be struck, not kept as a fallback.** "Mount only `.credentials.json`
read-only, give each container its own writable config dir" would put each container's lockfile in a
*different* directory — destroying the cross-container exclusion that currently comes for free — and
would make the credential unwritable, so a rotated token could never be persisted. It is worse than
the thing it was proposed to protect against.

**F. Pinning the Claude Code version now guards refresh safety, not just terminal scraping.** All of
the above is behaviour of `2.1.246` — internal, undocumented, and free to change. The existing
"pin the version" invariant just got a second, heavier reason. Re-run this spike on every bump.

---

## Reproducing

Requires docker-in-docker and a `claude` binary on `PATH` (override with `CLAUDE_BIN=`). The seeded
credential is **fake** — a syntactically valid but bogus token. No real credential is ever used, and
the refresh is rejected by the server, which is the point: the lock behaviour under test happens
before the HTTP call.

```sh
cd docs/design/spikes/harness
./run-n.sh 4 contention --blackhole   # test 1: cross-container mutual exclusion
./run-atomic.sh                       # test 2: atomicity / torn reads
./stale.sh 10                         # test 3a: abandoned lock, below stale threshold
./stale.sh 90                         # test 3b: abandoned lock, above stale threshold
```

`watch.sh` and `atomic.sh` are the observers; they read the volume's backing directory directly,
which works because the DinD daemon lives in this container (see CLAUDE.md).
