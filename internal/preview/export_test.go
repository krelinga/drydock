package preview

// SetBeforeInsert runs f between StartSession's checks and its insert, so a
// test can land a disable exactly there.
func (s *Service) SetBeforeInsert(f func()) { s.beforeInsert = f }
