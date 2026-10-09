// Package claudetest is `fakeclaude` (testing §6.4): a stand-in for the
// `claude` binary that replays the recorded corpus onto a PTY, reads its
// stdin, and reports what it received — plus the test-side helpers that build
// it, script it, start it on a real PTY of a chosen width, and read its
// report.
//
// The binary is ./fakeclaude; this file is the contract between it and a
// test. A fake encodes a belief about the real thing, and the belief rots
// (testing §6.1), so the belief is pinned twice: Version below is the Claude
// Code version the corpus was recorded against, and contract_test.go asserts
// that what fakeclaude writes is byte-for-byte the corpus — so re-recording the
// corpus after a bump fails that test until this package is updated.
//
// What it replays comes from the corpus files at run time, never from a copy
// in this package. A cleaned or re-typed transcript tests a parser against a
// world that does not exist (fixtures README), and two copies of the bytes are
// two things to keep in step.
//
// # Rules and details
//
// fakeclaude replays the corpus onto a real PTY of a chosen width, scripted by
// a JSON file beside the binary (not an env var, which subproc replaces), and
// logs what it received — each code as a SHA-256 with its framing judged,
// anything typed at a gate or a serving server, a missing PTY. Every file it
// replays is pinned by SHA-256 in Pinned, so a re-record stops it until it is
// re-derived. Install builds and scripts it; Start runs it on a PTY;
// Events/NoViolations read its log. internal/pty is the PTY itself, usable by
// the supervisor too.
package claudetest

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the Claude Code version fakeclaude impersonates, and the only one:
// a script asking for any other makes it refuse to run rather than pretend.
// It must equal classify.ClaudeCodeVersion, which the contract test asserts —
// so a corpus bump that forgets this file fails loudly.
const Version = "2.1.289"

// VersionLine is exactly what `claude --version` prints on 2.1.289.
const VersionLine = Version + " (Claude Code)\n"

// ScriptEnv names a script file explicitly. Without it, fakeclaude reads
// `<its own path>.json`, which is what Install writes: Drydock's subprocess
// seam *replaces* the child's environment (subproc.Cmd.Env), so a script that
// rode in an environment variable would vanish exactly when a test exercised
// the real argv.
const ScriptEnv = "FAKECLAUDE_SCRIPT"

// Fixture names fakeclaude replays, relative to the corpus's
// transcripts/claude-<Version>/ directory.
const (
	FixtureLoginPrompt80  = "login-url-80col"   // recorded at 80 columns
	FixtureLoginPrompt    = "login-code-prompt" // recorded at 1000 columns
	FixtureLoginInvalid   = "login-invalid-code"
	FixtureLoginSuccess   = "login-success-after-prompt" // synthetic: prompt + verdict
	FixtureServe          = "env-status-block"           // the whole run, through SIGTERM
	FixtureServeRunning   = "session-url-osc8"           // its pre-shutdown prefix
	FixtureDelayedSession = "session-ids-delayed"        // synthetic
	FixtureModelOutputID  = "session-id-in-model-output" // synthetic
	FixtureHangTrust      = "hang-untrusted-tty"
	FixtureHangDialog     = "hang-remote-dialog-tty" // synthetic
	FixtureCrash          = "crash-after-start"      // synthetic
	FixtureRefuseWait     = "refusal-409"
	FixtureRefuseTrust    = "refusal-not-trusted"
	FixtureRefuseOrg      = "refusal-no-organization"
	FixtureRefuseCmdline  = "refusal-bad-commandline"
)

// Pinned is the SHA-256 of every corpus file fakeclaude replays, keyed by its
// path under test/fixtures. This is the belief written in the fake's source:
// fakeclaude refuses to replay a file whose bytes are not these (exit 3, and a
// violation in its log), so re-recording the corpus — even under the same
// version directory — fails every test that uses the fake until someone has
// looked at the new bytes and re-derived the modes built from them.
var Pinned = map[string]string{
	"transcripts/claude-2.1.289/login-url-80col":            "f61fcba801d2294e2773ba30dc299bb1a60361aa5fa198a6282af85c1999b44d",
	"transcripts/claude-2.1.289/login-code-prompt":          "ecaf28927a4a1f9ae0df23c9105e66a944c3430080bea01f5a30b186f1afe3e5",
	"transcripts/claude-2.1.289/login-invalid-code":         "30aa4f197c186c27de3a1648e74cfd4a600fb6ff2d99bbe46885f54a4200d7d6",
	"transcripts/claude-2.1.289/login-success-after-prompt": "e771f81520629d55226b84dcf7c3b691e1f5240a9a97a9a643b6bb3772e6dc06",
	"transcripts/claude-2.1.289/env-status-block":           "daa8296a92abc0708f4a77c8c04a45bf3c4287531507871999678601ec5a76e5",
	"transcripts/claude-2.1.289/session-url-osc8":           "9f545ef00857c2cc6986e6076db3532bce4d16ccf61648eefe9943811809eb3b",
	"transcripts/claude-2.1.289/session-ids-delayed":        "3633e8786adbbe4975e3b1908c16935dfec3d47376b5d71cd417c1e3abd5c25e",
	"transcripts/claude-2.1.289/session-id-in-model-output": "c1f218ca2c5f115cea086bb01ca7ca4d00ea537bd1454f7e0485bf41c6f547ae",
	"transcripts/claude-2.1.289/hang-untrusted-tty":         "f371dc45e51441618b760899e6c79c22919bf3eb7aa1e6b9c4a2e3ab3f010221",
	"transcripts/claude-2.1.289/hang-remote-dialog-tty":     "0fc514759738aba98ab8242b4be936825cfaea1fcb81052c30f84769924a2e8c",
	"transcripts/claude-2.1.289/crash-after-start":          "6a67a5354d460bd999e65fbebad6bf8e463d2ace062ac81cc2b03ec9a9c56719",
	"transcripts/claude-2.1.289/refusal-409":                "b1f7537ea09fb63ac2c9fb8b988e22348a8c516056b708f803c3150e1d657adf",
	"transcripts/claude-2.1.289/refusal-not-trusted":        "3d2d118900a91d59edcdf63b17c3f9993a4705409d355e856e02fad455496b4a",
	"transcripts/claude-2.1.289/refusal-no-organization":    "3771d762057c4dbbfb82db041616c6a9e7041c980b117ea3123b6087025429b7",
	"transcripts/claude-2.1.289/refusal-bad-commandline":    "63021740f909cce14bb8bba3186d110aa2368ebb0230f84b2bb61aae797999dd",
	"authstatus/absent.json":                                "6a94f1f27264898c35e84630e13fe9d603b6efc641e618f0ca8e54e6d579805a",
	"authstatus/blanked.json":                               "6a94f1f27264898c35e84630e13fe9d603b6efc641e618f0ca8e54e6d579805a",
	"authstatus/expired.json":                               "90ccc588f8e647d7c9c6cc892d69ef0f9f6a5a0dd21683bbd1cf181cdbe096c0",
	"authstatus/valid.json":                                 "90ccc588f8e647d7c9c6cc892d69ef0f9f6a5a0dd21683bbd1cf181cdbe096c0",
}

// Script is the JSON file that tells one fakeclaude what to be. A test writes
// one through Install; nothing else is read.
type Script struct {
	// Version, when set, is the version the test expects to be talking to.
	// fakeclaude refuses (exit 2) when it is anything but Version.
	Version string `json:"version,omitempty"`
	// Corpus is the absolute path of test/fixtures.
	Corpus string `json:"corpus"`
	// StateDir holds the event log and the step counter. It never holds a
	// login code: a submission is logged as its SHA-256, so the canary sweep
	// (testing §4.2) can run over a tree that contains the fake's state.
	StateDir string `json:"state_dir"`
	// Pace slows replay down. The zero value writes each recorded segment in
	// one write.
	Pace Pace `json:"pace,omitzero"`

	Login         *Login      `json:"login,omitempty"`
	AuthStatus    *AuthStatus `json:"auth_status,omitempty"`
	RemoteControl []Step      `json:"remote_control,omitempty"`
}

// Pace writes recorded bytes Chunk at a time with Delay between writes, so a
// reader sees the stream arrive in pieces — a URL cut mid-token, an OSC 8
// sequence split from its terminator — as a real PTY read can.
type Pace struct {
	Chunk int      `json:"chunk,omitempty"`
	Delay Duration `json:"delay,omitzero"`
}

// LoginMode is how `claude auth login` behaves once it is at the prompt.
type LoginMode string

const (
	// LoginAnswer: a submission whose SHA-256 is AcceptSHA256 succeeds and
	// the process exits 0; any other gets `Invalid code` and the process
	// STAYS at the prompt (Spike 01) for another try. An empty AcceptSHA256
	// rejects everything — the invalid-code mode.
	LoginAnswer LoginMode = "answer"
	// LoginTimeout: the prompt, then silence whatever is submitted. The
	// verdict is the caller's clock, never a byte in the stream
	// (classify.LoginTimedOut).
	LoginTimeout LoginMode = "timeout"
)

// Login scripts `claude auth login`.
type Login struct {
	Mode LoginMode `json:"mode"`
	// AcceptSHA256 is the hex SHA-256 of the one code that succeeds. A hash,
	// so the script file is not a copy of the canary.
	AcceptSHA256 string `json:"accept_sha256,omitempty"`
	// VerdictDelay is how long the verdict takes after a submission.
	VerdictDelay Duration `json:"verdict_delay,omitzero"`
	// AllowRepeat stops an identical second submission from being a
	// violation, for a test that deliberately re-pastes the same wrong code.
	AllowRepeat bool `json:"allow_repeat,omitempty"`
}

// AuthStatus scripts `claude auth status --json`.
type AuthStatus struct {
	// Shape is one of the four recorded documents — absent, valid, expired,
	// blanked — or "from-config", which reads $CLAUDE_CONFIG_DIR/.credentials.json
	// and answers as 2.1.289 was measured to: no file → absent, blanked
	// tokens → blanked, tokens present → valid *whatever the expiry*, since
	// auth status ignores it (testing §7, Identity).
	Shape string `json:"shape"`
}

// RCMode is one `claude remote-control` behaviour.
type RCMode string

const (
	// RCServe replays env-status-block: the environment header, Connecting →
	// Connected, a session announced in an OSC 8 hyperlink (BEL-terminated
	// on 2.1.289) and the status block reprinted in place by cursor
	// movement. It then serves until SIGTERM, when it writes the recorded
	// shutdown and exits 0. SessionDelay holds back the session.
	RCServe RCMode = "serve"
	// RCServeDelayedSession replays session-ids-delayed: a second session
	// announced SessionDelay after the first.
	RCServeDelayedSession RCMode = "serve-delayed-session"
	// RCServeModelOutputID serves, then after SessionDelay writes a
	// `session_…` id as model prose — the false positive discovery must
	// reject.
	RCServeModelOutputID RCMode = "serve-model-output-id"
	// The four startup refusals: the recorded message, on stderr, exit 1
	// after ExitAfter. All four exit 1 — exit status is not a discriminator.
	RCRefuseWaitRegistration RCMode = "refuse-wait-registration"
	RCRefuseNotTrusted       RCMode = "refuse-not-trusted"
	RCRefuseNoOrganization   RCMode = "refuse-no-organization"
	RCRefuseBadCommandLine   RCMode = "refuse-bad-commandline"
	// RCHangTrust: on a PTY, `Trust <dir>? [y/N]` and then nothing, ever.
	// Redirected, the same case exits 1 with `Workspace not trusted`, as
	// measured.
	RCHangTrust RCMode = "hang-trust"
	// RCHangRemoteDialog: `Enable Remote Control? (y/n)` and then nothing.
	// Synthetic: no recording exists.
	RCHangRemoteDialog RCMode = "hang-remote-dialog"
	// RCCrash: the start of a serve, then exit 1 after ExitAfter with no
	// refusal message — an ordinary crash, for the restart budget.
	RCCrash RCMode = "crash"
)

// Step is one stretch of a remote-control script. Each invocation of
// `claude remote-control` runs the current step; a step ends after Times
// invocations or once For has passed since its first, and the last step
// repeats forever. So `[{crash, times:2}, {serve}]` is a crash loop that
// recovers, and `[{refuse-wait-registration, for:"3s"}, {serve}]` is a
// registration that lapses after a span the test chose — the supervisor must
// not have the span coded into it (testing §8.4).
type Step struct {
	Mode  RCMode   `json:"mode"`
	Times int      `json:"times,omitempty"`
	For   Duration `json:"for,omitzero"`
	// SessionDelay: for the serve modes, the pause before the session
	// announcement (or the model's prose).
	SessionDelay Duration `json:"session_delay,omitzero"`
	// ExitAfter: for refusals and crash, the pause before exiting. The real
	// wait refusal says "Exiting in about 50 seconds."
	ExitAfter Duration `json:"exit_after,omitzero"`
}

// Duration is a time.Duration that reads and writes as "1.5s".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Event is one line of the report fakeclaude appends to StateDir/events.jsonl.
type Event struct {
	Invocation int    `json:"invocation"`
	Kind       string `json:"kind"` // start, submission, input, violation, exit
	// start
	Argv   []string `json:"argv,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	TTY    bool     `json:"tty,omitempty"`
	Width  int      `json:"width,omitempty"`
	Height int      `json:"height,omitempty"`
	// submission: never the bytes, only their hash and length
	SHA256  string `json:"sha256,omitempty"`
	Len     int    `json:"len,omitempty"`
	Verdict string `json:"verdict,omitempty"` // accepted, rejected, unanswered
	// violation
	What string `json:"what,omitempty"`
	// exit
	Code int `json:"code,omitempty"`
}

// Event kinds.
const (
	EventStart      = "start"
	EventSubmission = "submission"
	EventViolation  = "violation"
	EventExit       = "exit"
)
