package preview

import "context"

// SetBeforeInsert runs f between StartSession's checks and its insert, so a
// test can land a disable exactly there.
func (s *Service) SetBeforeInsert(f func()) { s.beforeInsert = f }

// Round runs one discovery round now, on the test's goroutine, answering the
// rescans of asks: the worker's own function, without the worker.
func (s *Scanner) Round(ctx context.Context, asks ...string) { s.run(ctx, asks) }
