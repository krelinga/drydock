package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/reconcile"
)

// TestReconcileWarningSaysWhatHappened: the boot warning says "nothing was
// changed" only for a failure that changed nothing, and otherwise says how
// many workspaces could not be reconciled. A stuck resumed delete used to
// make every boot announce that nothing changed while the same run adopted
// and stopped the other rows.
func TestReconcileWarningSaysWhatHappened(t *testing.T) {
	nothing := fmt.Errorf("cannot list containers: %w: %w", reconcile.ErrNothingChanged, errors.New("daemon down"))
	if got := reconcileWarning(nothing); !strings.Contains(got, "nothing was changed") {
		t.Errorf("a failed listing says %q; want it to say nothing was changed", got)
	}
	for _, c := range []struct {
		p    *reconcile.Partial
		want string
	}{
		{&reconcile.Partial{Applied: 3, Errs: []error{errors.New("x")}}, "one could not be"},
		{&reconcile.Partial{Applied: 1, Errs: []error{errors.New("x"), errors.New("y")}}, "2 could not be"},
	} {
		got := reconcileWarning(c.p)
		if strings.Contains(got, "nothing was changed") || !strings.Contains(got, c.want) {
			t.Errorf("%d failures: %q; want %q and never \"nothing was changed\"", len(c.p.Errs), got, c.want)
		}
	}
	if got := reconcileWarning(errors.New("other")); strings.Contains(got, "nothing was changed") {
		t.Errorf("an unclassified error claims nothing changed: %q", got)
	}
}
