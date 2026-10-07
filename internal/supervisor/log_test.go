package supervisor

import (
	"strings"
	"testing"
	"time"
)

// The ring's masks are its own and only grow. A value its source stops
// returning — rotated out, its grant revoked, or the snapshot briefly
// undeliverable — is still in a running server's environment, so it stays
// masked; a value that contains another is masked whole; and a value newly
// returned is masked from the next write on. Control: text that is no value
// is kept verbatim, so the masking is not passing on an empty or blanked log.
func TestRingMasksEveryValueItWasEverGiven(t *testing.T) {
	const old, longer, fresh = "OLD-VALUE-1234", "OLD-VALUE-1234-AND-MORE", "NEW-VALUE-5678"
	now := time.Now()
	current := []string{old, longer}
	r := NewRing(0, func() []string { return current })

	r.Write(now, []byte("one "+longer+" ok\n"))
	current = nil // rotated out, revoked, or undeliverable for a moment
	r.Write(now, []byte("two "+old+" ok\n"))
	r.Write(now, []byte("three "+fresh+" before\n"))
	current = []string{fresh}
	r.Write(now, []byte("four "+fresh+" after\n"))
	r.Write(now, []byte("five "+old+" tail"))
	r.Flush(now)
	r.Mark(now, "mark "+fresh)

	lines, _ := r.Tail(0)
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	want := []string{
		"one [redacted] ok",
		"two [redacted] ok",
		"three " + fresh + " before", // control: not yet a value
		"four [redacted] after",
		"five [redacted] tail",
		"— mark [redacted]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
