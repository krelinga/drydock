package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// TestAVerboseBuildStillNamesItsCause: an `up` whose stderr runs past a MiB
// before the Feature's probe line still fails with the broker sentence, and
// its held log ends with that line — stderr's tail is kept, not its head. A
// timed-out or unreadable up holds its log too, and says so.
func TestAVerboseBuildStillNamesItsCause(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.cli.up = "head -c 3000000 /dev/zero | tr '\\0' 'x' | fold -w 100 >&2\n" +
		"echo 'drydock: this container was started without a working broker socket; GitHub access will fail. Restart it from Drydock.' >&2\n" +
		"cat <<'EOF'\n" + fixtureBytes("up-error-postcreate.json") + "\nEOF\nexit 1"
	e.wire(t)
	v := e.create(t, alpha, "")
	if got := v.Steps[workspace.StepUp].Detail; !strings.Contains(got, NoBrokerSentence) {
		t.Errorf("detail %q, want the broker sentence", got)
	}
	log, ok, err := e.p.BuildLog(ctx, v.ID)
	if err != nil || !ok || len(log.Lines) != buildLogLines ||
		!strings.Contains(log.Lines[len(log.Lines)-1], "without a working broker socket") {
		t.Errorf("held %v %v, last %q", ok, err, log.Lines[len(log.Lines)-1])
	}

	// An unreadable result is a failed build too: its output is held.
	e2 := newEnv(t)
	e2.cli.up = "echo 'Step 9/9 : the last thing it did' >&2; echo 'not json'"
	e2.wire(t)
	v2 := e2.create(t, alpha, "")
	if got := v2.Steps[workspace.StepUp].Detail; !strings.Contains(got, "shows the output it printed") {
		t.Errorf("unreadable up: %q", got)
	}
	if log, ok, _ := e2.p.BuildLog(ctx, v2.ID); !ok || log.Lines[len(log.Lines)-1] != "Step 9/9 : the last thing it did" {
		t.Errorf("unreadable up held %v %q", ok, log.Lines)
	}
}
