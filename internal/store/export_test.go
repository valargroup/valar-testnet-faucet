package store

import "time"

// SetClock replaces the store's clock for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }
