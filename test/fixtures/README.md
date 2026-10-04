# Fixture corpus

Recorded bytes from the things Drydock does not control, so the five
classifiers in `internal/classify` are table-driven unit tests rather than
integration tests. See the testing plan §7 for what this corpus proves and §11.1
for the re-record ritual.

```
transcripts/claude-<version>/   raw PTY bytes, escapes intact, one file per scenario
authstatus/                     `claude auth status --json` documents, one per state
credentials/                    .credentials.json shapes, including the tombstone
devcontainer/                   `devcontainer up --json` results
repos/                          the five fixture repos (testing §6.5)
```

## Rules that make the corpus worth having

- **Raw bytes, never a cleaned-up transcript.** ANSI escapes and OSC 8
  hyperlink wrappers are the thing under test. A tidied transcript tests a
  parser against a world that does not exist.
- **One directory per Claude Code version.** The old corpus stays until that
  version is no longer deployable, and the parser handles both. A changed URL
  pattern, prompt string, refusal signature, or `auth status` schema is a
  **parser change**, not a fixture update.
- **Every file carries a header** naming the version, the date, the command, and
  — for a PTY transcript — the width it was recorded at. Width is part of the
  fixture: the authorize URL wraps mid-token at ordinary widths, so
  `login-url-200col` and `login-url-1000col` must both yield the same complete
  URL.
- **Negative fixtures count.** `session-url-osc8-urlmatch` exists to keep the
  id-matching rule from being "simplified" into URL parsing later, and
  `session-id-in-model-output` must yield *no* row — an agent discussing its own
  session id is not a server announcement.

## What is not here yet

The corpus is **empty**. Phase 0 built the harnesses that produce it; recording
it is what the testing plan still owes Phase 0 (§13). Until then the classifiers
are signatures, and `internal/classify` panics rather than returning a plausible
zero value — a classifier that silently returns `IdentityOK` for bytes it cannot
parse is the failure this corpus exists to prevent.

One fixture will be **hand-written rather than recorded** when it arrives, and
the header must say so: the three `login-success-*` files. Spike 01 drove the
handshake through an *invalid* code because completing a real one needs a human
in a browser, so the `Login successful` strings come from `grep` over the binary.
A hand-written fixture asserting a hand-written expectation proves only that the
regex matches itself. Run `docs/design/spikes/harness-01-login/run.sh login` to
replace them with the real thing.
