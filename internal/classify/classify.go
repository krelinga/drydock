// Package classify turns foreign bytes into Drydock states.
//
// Five places in the system read output produced by something Drydock does not
// control, and every one of them must be callable without the thing that
// produced the bytes (testing §5.5). That is what converts the most brittle
// dependencies in the design — two scrapes of Claude Code's TUI, one JSON
// contract with the `devcontainer` CLI — into table-driven unit tests over a
// recorded corpus.
//
// Every function here is `func(input) (state, error)` with no clock, no
// filesystem, and no process. One classifier per file, deliberately: they are
// the most parallelizable work in the project, and a shared file would make
// five independent jobs collide.
//
// What the corpus proves and does not: it proves the parser handles the
// recorded bytes, not that the bytes are still what Claude Code emits. Only a
// live run shows that, which is why re-recording is a ritual (testing §11.1)
// and why the five spike harnesses exist.
package classify

// FixtureRoot is where recorded bytes live, one directory per version so an
// old corpus stays until that version is no longer deployable.
//
//	test/fixtures/transcripts/claude-2.1.289/login-url-80col
//	test/fixtures/transcripts/claude-2.1.289/session-url-osc8
//	test/fixtures/authstatus/blanked.json
//	test/fixtures/credentials/blanked.json
//
// Each transcript file is **raw PTY bytes, escapes intact** — never a cleaned
// up transcript — with a sidecar `.meta` naming the version, the date, the
// command, the PTY width, and whether it was recorded, hand-written, or
// synthetic. Read the `.meta` before trusting a fixture: three of them are not
// recordings, and one of those is still owed a real one.
const FixtureRoot = "test/fixtures"

// ClaudeCodeVersion is the pinned version every transcript was recorded
// against. A bump re-runs all four Claude Code harnesses and re-records the
// corpus (testing §11.1), and a consistency test asserts this constant agrees
// with CLAUDE.md, the Feature, and all four spike reports.
const ClaudeCodeVersion = "2.1.289"
