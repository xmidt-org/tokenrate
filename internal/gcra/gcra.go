// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Package gcra implements the Generic Cell Rate Algorithm: a leaky bucket
// whose whole state is a single theoretical arrival time (TAT).
package gcra

import "time"

// Spend tries to take one call from an allowance of count calls per window,
// whose state is tat.  The tolerance is the window, so a full allowance
// permits count calls at once.
//
// When the call fits, ok is true and next is the new TAT.  When it doesn't,
// ok is false, retryAfter is how long until it would fit, and next is tat
// unchanged unless force is set, in which case the call is spent anyway.
//
// count and window must be positive.
func Spend(tat, now time.Time, count int, window time.Duration, force bool) (next time.Time, ok bool, retryAfter time.Duration) {
	interval := Interval(count, window)

	if tat.Before(now) {
		tat = now
	}

	next = tat.Add(interval)
	allowAt := next.Add(-window)
	if !now.Before(allowAt) {
		return next, true, 0
	}

	retryAfter = allowAt.Sub(now)
	if force {
		return next, false, retryAfter
	}

	return tat, false, retryAfter
}

// Interval is the time one call costs at count calls per window.  It is never
// less than a nanosecond.
func Interval(count int, window time.Duration) time.Duration {
	return max(window/time.Duration(count), 1)
}
