package booking

// SetAfterLockExpired makes ExpireHolds call fn right after it has locked the
// expired holds, so tests can commit a change at exactly that point.
func SetAfterLockExpired(s *Service, fn func()) { s.afterLockExpired = fn }
