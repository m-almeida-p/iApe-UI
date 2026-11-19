package yapeagent

import "time"

// SetClock replaces the clock that decides when a cached token is old.
func (s *TokenSource) SetClock(now func() time.Time) { s.now = now }
