package identity_test

import (
	"fmt"
	"strconv"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
