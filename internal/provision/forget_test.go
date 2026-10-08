package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestTheBuildLogNeverForgetsAValue is round 2's blocker: a value granted when
// the build printed it stays masked after the secret is rotated, its grant
// revoked, or the secret deleted — each of which takes it out of the set the
// broker resolves now — because the log is masked when it is kept and only
// that copy is held. The serve-time pass still masks a value granted after
// the failure. The control is a line with no value in it, served as written.
func TestTheBuildLogNeverForgetsAValue(t *testing.T) {
	const old = "OLD-secret-value-123456"
	const later = "LATER-granted-value-777"
	ctx := context.Background()
	for name, now := range map[string][]string{
		"rotated": {"NEW-secret-value-999"},
		"revoked": {},
		"deleted": nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.cli.up = upFailing("ARG X=pfx-" + old + "\nprinted " + old + "\nplain build line\nthen " + later)
			e.wire(t)
			current := []string{old}
			e.p.Redact = func(context.Context, string) ([]string, error) { return current, nil }
			v := e.create(t, alpha, "")
			current = append(append([]string{}, now...), later) // the change, and a grant since
			log, ok, err := e.p.BuildLog(ctx, v.ID)
			joined := strings.Join(log.Lines, "\n")
			if err != nil || !ok {
				t.Fatalf("held %v %v", ok, err)
			}
			if strings.Contains(joined, "OLD-secret") || strings.Contains(joined, "LATER-granted") {
				t.Errorf("a value shows after it was %s: %q", name, log.Lines)
			}
			for _, want := range []string{"ARG X=pfx-[redacted]", "printed [redacted]", "then [redacted]", "plain build line"} {
				if !strings.Contains(joined, want) {
					t.Errorf("want %q in %q", want, log.Lines)
				}
			}
		})
	}
}

// TestNoValueStraddlesTheLineCut: a line longer than maxBuildLine with a
// value, and a token, crossing the cut is masked whole before it is cut, so
// neither shows a prefix — a value known when the log was kept, and one
// granted only after, which the serve-time pass alone can mask. The control
// is a long line with nothing in it, cut to exactly maxBuildLine.
func TestNoValueStraddlesTheLineCut(t *testing.T) {
	const value = "OLD-secret-value-123456"
	pad := strings.Repeat("x", maxBuildLine-10)
	token := "ghp_" + strings.Repeat("A", 36)
	e := newEnv(t)
	const later = "LATER-granted-value-777"
	e.cli.up = upFailing(pad + value + "\n" + pad + token + "\n" + pad + later + "\n" + strings.Repeat("y", maxBuildLine+50))
	e.wire(t)
	current := []string{value}
	e.p.Redact = func(context.Context, string) ([]string, error) { return current, nil }
	v := e.create(t, alpha, "")
	current = []string{value, later}
	log, ok, err := e.p.BuildLog(context.Background(), v.ID)
	if err != nil || !ok {
		t.Fatalf("held %v %v", ok, err)
	}
	n := len(log.Lines)
	if n != 4 {
		t.Fatalf("lines %d: %q", n, log.Lines)
	}
	for _, l := range log.Lines {
		if len(l) > maxBuildLine {
			t.Errorf("a line of %d bytes was served", len(l))
		}
	}
	joined := strings.Join(log.Lines, "\n")
	for _, leak := range []string{"OLD-sec", "ghp_A", "LATER-gr"} {
		if strings.Contains(joined, leak) {
			t.Errorf("%q leaked across the cut", leak)
		}
	}
	if got := log.Lines[3]; got != strings.Repeat("y", maxBuildLine) {
		t.Errorf("control: the plain long line is %d bytes", len(got))
	}
}

// TestALogThatCouldNotBeMaskedIsNeverServed: values unreadable when the log is
// kept hold no lines — nothing raw stays in memory — and it is withheld even
// once the values can be read again; the journal gets a count, not the lines.
// The control is the same failure with values readable, held and served.
func TestALogThatCouldNotBeMaskedIsNeverServed(t *testing.T) {
	const value = "OLD-secret-value-123456"
	ctx := context.Background()
	e := newEnv(t)
	var journal []string
	e.p.Logf = func(f string, a ...any) { journal = append(journal, strings.TrimSpace(sprintf(f, a...))) }
	e.cli.up = upFailing("printed " + value)
	e.wire(t)
	var valuesErr = errors.New("a secret does not open")
	e.p.Redact = func(context.Context, string) ([]string, error) { return []string{value}, valuesErr }
	v := e.create(t, alpha, "")
	valuesErr = nil
	log, ok, err := e.p.BuildLog(ctx, v.ID)
	if !ok || !errors.Is(err, ErrLogWithheld) || len(log.Lines) != 0 {
		t.Errorf("unmasked at keep: %v %v %q", ok, err, log.Lines)
	}
	e.p.mu.Lock()
	held := e.p.buildLogs[v.ID]
	e.p.mu.Unlock()
	if len(held.Lines) != 0 {
		t.Errorf("raw lines held: %q", held.Lines)
	}
	all := strings.Join(journal, "\n")
	if strings.Contains(all, value) || !strings.Contains(all, "lines withheld") {
		t.Errorf("journal: %q", journal)
	}

	// Control: values readable — held, served, and the journal masked.
	journal = nil
	e2 := newEnv(t)
	e2.p.Logf = func(f string, a ...any) { journal = append(journal, sprintf(f, a...)) }
	e2.cli.up = upFailing("printed " + value)
	e2.wire(t)
	e2.p.Redact = func(context.Context, string) ([]string, error) { return []string{value}, nil }
	v2 := e2.create(t, alpha, "")
	if log, ok, err := e2.p.BuildLog(ctx, v2.ID); err != nil || !ok || !strings.Contains(strings.Join(log.Lines, "\n"), "printed [redacted]") {
		t.Errorf("control: %v %v %q", ok, err, log.Lines)
	}
	all = strings.Join(journal, "\n")
	if strings.Contains(all, value) || !strings.Contains(all, "printed [redacted]") {
		t.Errorf("control journal: %q", journal)
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
