package model

import "time"

// Clock abstracts system time for deterministic testing and metrics.
type Clock interface {
	Now() time.Time
}

// SystemClock provides the real system time in UTC.
type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}
