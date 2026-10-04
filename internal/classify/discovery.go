package classify

// 4. The discovery tail (design §8, Spike 02)

// Discovery is what the supervisor's `--verbose` stream reveals.
type Discovery struct {
	// EnvironmentID is the durable handle — one per workspace, survives a
	// restart, and the card's only link. Stored in workspace.environment_id.
	EnvironmentID string
	// SessionIDs are matched as `session_[A-Za-z0-9]+` rather than parsed
	// out of a URL: per-session URLs arrive wrapped in OSC 8 hyperlink
	// escapes (`ESC]8;;<url>BEL`), so the URL and its label run together in
	// the byte stream and only an id match is unambiguous.
	SessionIDs []string
	// CapacityUsed and CapacityTotal come from `Capacity: N/4`, reprinted
	// on every repaint. The pre-created session counts toward Used, so a
	// total of 4 buys three on-demand sessions and the UI must not imply
	// otherwise.
	CapacityUsed, CapacityTotal int
}

// ClassifyDiscovery reads an ANSI + OSC 8 byte stream.
//
// Two hazards: ANSI cursor movement reprints the status block in place, so the
// same line recurs constantly (12 times in the recorded fixture) and the result
// must be deduplicated by id rather than appended; and a `session_…` id the
// *model* printed in its own output is not a server announcement, which is the
// false positive the negative fixture exists to catch.
func ClassifyDiscovery(stream []byte) (Discovery, error) { panic("not implemented: Phase 5") }
