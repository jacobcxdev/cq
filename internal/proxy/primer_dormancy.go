package proxy

import "time"

// A dormant rolling window exposes an unused reset that moves with the clock.
// A fixed future reset is already active, even when usage rounds to 100%.
func movingPrimerReset(previousReset, observedAt, reset, now time.Time) bool {
	shift, elapsed := reset.Sub(previousReset), now.Sub(observedAt)
	return shift > 0 && elapsed > 0 && absDuration(shift-elapsed) <= 2*time.Second
}
