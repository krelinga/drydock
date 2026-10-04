package classify

import (
	"bytes"
	"regexp"
)

// 3. Supervisor startup refusals (design §8, Spike 02)

// Refusal is why a `remote-control` server would not start.
//
// All four exit `1`. **Exit status is not a discriminator**, and the natural
// wrong implementation — branching on it — fails by crash-looping against a
// config error or by giving up on a wait. One of these must be retried and
// three must not.
type Refusal uint8

const (
	// RefusalNone: the stream is not a refusal.
	RefusalNone Refusal = iota
	// RefusalWaitRegistration: `already served by a terminal`. The previous
	// server's folder registration has not lapsed. **Retry, patiently.**
	//
	// Do not match on "409": that token was in the 2.1.246 message and is
	// GONE from 2.1.289, which says only "This folder is already served by
	// a terminal ... Stop it first." plus "Exiting in about 50 seconds."
	// Matching the status number would silently reclassify the one
	// retryable refusal as a non-retryable one. The wait is announced now
	// but is still not a constant to code against. It is a wait, not a
	// crash, and it must not spend the restart budget.
	RefusalWaitRegistration
	// RefusalWorkspaceNotTrusted: the Feature or postCreate is broken. Do
	// not retry; the fix is a rebuild. Note this signature only appears
	// when output is redirected — on a PTY the same condition HANGS on
	// "Trust <dir>? [y/N]", and the supervisor owns a PTY.
	RefusalWorkspaceNotTrusted
	// RefusalNoOrganization: credential present, account record missing.
	// Do not retry; this is awaiting_login (§7.3).
	RefusalNoOrganization
	// RefusalBadCommandLine: Drydock built an invalid invocation. Do not
	// retry; this is a bug report.
	RefusalBadCommandLine
)

// Retryable reports whether the supervisor should try again. Exactly one
// refusal is.
func (r Refusal) Retryable() bool { return r == RefusalWaitRegistration }

// refusalSignatures is checked in order, and the order is deliberate: the
// three non-retryable signatures come first, so that output which somehow
// carried two of them is never resolved to the one verdict that waits
// forever. Every entry is message text, never a status code.
var refusalSignatures = []struct {
	phrase  []byte
	verdict Refusal
}{
	{[]byte("cannot be used with --spawn"), RefusalBadCommandLine},
	{[]byte("Workspace not trusted"), RefusalWorkspaceNotTrusted},
	{[]byte("Unable to determine your organization"), RefusalNoOrganization},
	{[]byte("already served by a terminal"), RefusalWaitRegistration},
}

// ansiEscape matches CSI sequences (`ESC [ … final`) and OSC sequences
// (`ESC ] … BEL` or `ESC ] … ESC \`), the two kinds the recorded corpus
// carries. An escape landing inside a phrase — a colour change on one word —
// would otherwise split the signature.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// whitespaceRun collapses every run of whitespace, line breaks included, to
// one space, so a phrase broken across a soft wrap still matches.
var whitespaceRun = regexp.MustCompile(`\s+`)

// ClassifyRefusal reads `remote-control` stderr — the whole refusal, which on
// 2.1.289 is two lines for the registration wait — and names the refusal by
// its message text. Escapes are stripped and whitespace collapsed first.
//
// The exit code is deliberately not a parameter: it is always 1 and accepting
// it would invite an implementation that uses it.
//
// Text that matches no signature is RefusalNone, not an error. That includes
// the PTY trust prompt (`Trust <dir>? [y/N]`): an untrusted workspace on a PTY
// hangs rather than refusing, and noticing a hang is the supervisor's timeout,
// not something a classifier of bytes can do.
func ClassifyRefusal(stderr []byte) (Refusal, error) {
	text := ansiEscape.ReplaceAll(stderr, nil)
	text = whitespaceRun.ReplaceAll(text, []byte(" "))
	for _, sig := range refusalSignatures {
		if bytes.Contains(text, sig.phrase) {
			return sig.verdict, nil
		}
	}
	return RefusalNone, nil
}
