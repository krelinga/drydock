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
- **Every file carries a sidecar `.meta`** naming the version, the date, the
  command, the PTY width, and whether it was *recorded*, *hand-written*, or
  *synthetic*. The testing plan says "a header"; a header inside a file of raw
  PTY bytes would corrupt the thing under test, so provenance lives beside the
  bytes instead. Width is part of the
  fixture: the authorize URL wraps mid-token at ordinary widths, so
  `login-url-200col` and `login-url-1000col` must both yield the same complete
  URL.
- **Negative fixtures count.** `session-url-osc8-urlmatch` exists to keep the
  id-matching rule from being "simplified" into URL parsing later, and
  `session-id-in-model-output` must yield *no* row — an agent discussing its own
  session id is not a server announcement.

## Recording it

`./record.sh [login|discovery|refusals|hangs|identity|credentials|all]` regenerates
everything from the real binary. That script *is* testing-plan §11.1 step 3, and
the ritual order matters: **re-run the four spike harnesses first.** They drive
the binary and fail loudly on a behavioural change; the recorder only records
whatever comes out, so re-recording first would bake a regression into the
fixtures and leave the parser tests green.

Two mechanics the recorder depends on, both of which silently produce a useless
fixture if you get them wrong:

- **`tmux new-session -x` is advisory.** tmux clamps a window to the smallest
  attached client, so on a server with any client attached the requested width
  is ignored and a width-dependent fixture gets recorded at the wrong width.
  `set-option window-size manual` plus `resize-window` is what actually forces
  it, and the recorder warns if the pane came back a different size.
- **`new-session -c <dir>` is not optional.** Without it the pane inherits
  whatever directory tmux was invoked from, and `remote-control` serves *that*
  folder — which records a fixture of the wrong repository, or a trust prompt
  for the one you happened to be standing in.

## What is still owed

- **`login-success-{plain,period,press}` are HAND-WRITTEN**, and their `.meta`
  says so. Completing a real login needs a human in a browser, so these strings
  come from `grep` over the binary. A hand-written fixture asserting a
  hand-written expectation proves only that the regex matches itself. Replace
  them with `docs/design/spikes/harness-01-login/run.sh login`.
- **`login-timeout`** needs the five-minute deadline to elapse; not recorded.
- **`devcontainer/`** needs a real `devcontainer up`; not recorded.
- **`session-ids-delayed`** and **`session-url-osc8-urlmatch`** from testing §7
  are not separate recordings yet. The negative URL-match assertion can be made
  against `session-url-osc8` as it stands, since that file already contains the
  OSC 8 form.

One fixture will be **hand-written rather than recorded** when it arrives, and
the header must say so: the three `login-success-*` files. Spike 01 drove the
handshake through an *invalid* code because completing a real one needs a human
in a browser, so the `Login successful` strings come from `grep` over the binary.
A hand-written fixture asserting a hand-written expectation proves only that the
regex matches itself. Run `docs/design/spikes/harness-01-login/run.sh login` to
replace them with the real thing.
