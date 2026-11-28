package quota

import "time"

const WeeklyQuotaSpendPriorityWindow = 24 * time.Hour

// ResetCreditSpendDeadline applies when the credit can expire during the
// current allowance. After a natural refill, reconsider the new allowance.
func ResetCreditSpendDeadline(now, naturalRefill, creditExpiry time.Time) time.Time {
	if creditExpiry.After(now) && naturalRefill.After(creditExpiry) {
		return creditExpiry
	}
	return time.Time{}
}

// SpendDeadlineBefore places active deadlines before an absent deadline.
func SpendDeadlineBefore(left, right time.Time) bool {
	return !left.IsZero() && (right.IsZero() || left.Before(right))
}

// CreditSpendTieBefore gives equal credit deadlines a stable order shared by
// forecasting and routing, preserving the system account when possible.
func CreditSpendTieBefore(left, right string, leftSystem, rightSystem bool) bool {
	if leftSystem != rightSystem {
		return !leftSystem
	}
	return left < right
}
