package classify

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

// ClassifyRefusal reads one line of `remote-control` stderr. The exit code is
// deliberately not a parameter: it is always 1 and accepting it would invite
// an implementation that uses it.
func ClassifyRefusal(stderrLine []byte) (Refusal, error) { panic("not implemented: Phase 5") }
