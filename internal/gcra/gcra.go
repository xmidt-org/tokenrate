// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Package gcra implements the Generic Cell Rate Algorithm: a leaky bucket
// whose whole state is a single theoretical arrival time (TAT).
package gcra

import "time"

// Spend works out taking one call from an allowance of count calls per
// window, whose state is tat.  The tolerance is the window, so a full
// allowance permits count calls at once.
//
// next is the TAT after the call.  The caller stores it to spend the call,
// or keeps tat to refuse it, which lets several allowances be checked before
// any is spent.  ok reports whether the call fits, and when it doesn't,
// retryAfter is how long until it would.
//
// count and window must be positive.
func Spend(tat, now time.Time, count int, window time.Duration) (next time.Time, ok bool, retryAfter time.Duration) {
	interval := Interval(count, window)

	if tat.Before(now) {
		tat = now
	}

	next = tat.Add(interval)
	allowAt := next.Add(-window)
	if !now.Before(allowAt) {
		return next, true, 0
	}

	return next, false, allowAt.Sub(now)
}

// Interval is the time one call costs at count calls per window.  It is never
// less than a nanosecond.
func Interval(count int, window time.Duration) time.Duration {
	return max(window/time.Duration(count), 1)
}
