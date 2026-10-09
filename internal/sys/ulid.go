package sys

import (
	"fmt"
	"io"
	"time"
)

// crockford is the ULID alphabet: Crockford's base32, no I, L, O or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID mints a ULID: time-ordered, so ids sort in creation order, with 80
// random bits from the injected source, so a test can force a collision and
// production cannot predict one. It lives here, below every package that mints
// row ids, so a package workspace imports (internal/preview) can mint one too.
func NewULID(now time.Time, random io.Reader) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := io.ReadFull(random, b[6:]); err != nil {
		return "", fmt.Errorf("id: %w", err)
	}
	// 128 bits into 26 characters of 5 bits each; the first carries only 3.
	var out [26]byte
	for i := 0; i < 26; i++ {
		bit := 128 - 5*(26-i) // index of this character's first bit, from the top
		var v byte
		for j := 0; j < 5; j++ {
			k := bit + j
			if k < 0 {
				continue
			}
			v = v<<1 | (b[k/8]>>(7-k%8))&1
		}
		out[i] = crockford[v]
	}
	return string(out[:]), nil
}
