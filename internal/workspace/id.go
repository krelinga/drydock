package workspace

import (
	"fmt"
	"io"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// NewID mints a ULID (§4: workspace.id, also the container's id-label value).
// Time-ordered, so ids sort in creation order and a directory listing of
// /srv/drydock/ws reads oldest first; 80 random bits from the injected source,
// so a test can force a collision and production cannot predict one.
func NewID(now time.Time, random io.Reader) (string, error) {
	id, err := sys.NewULID(now, random)
	if err != nil {
		return "", fmt.Errorf("workspace %w", err)
	}
	return id, nil
}
