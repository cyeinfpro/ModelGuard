package guard

import "time"

// API and host metadata can serialize the same revision in different timezones.
// Missing revisions never prove ownership, even when both sides are empty.
func sameRevision(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	x, e1 := time.Parse(time.RFC3339Nano, a)
	y, e2 := time.Parse(time.RFC3339Nano, b)
	return e1 == nil && e2 == nil && x.Equal(y)
}
