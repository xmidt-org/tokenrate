// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"time"
)

// Reason says why a check failed.
type Reason int

const (
	// None means the check passed.
	None Reason = iota

	// RateExceeded means the Caller has no allowance left at Limit.
	RateExceeded

	// Denied means the Resolver returned an error, or an invalid rate.  The
	// error is in Err.
	Denied
)

// String returns the reason in kebab case, e.g. rate-exceeded.
func (r Reason) String() string {
	switch r {
	case None:
		return "none"
	case RateExceeded:
		return "rate-exceeded"
	case Denied:
		return "denied"
	default:
		return "unknown"
	}
}

// Decision is the result of one Check.
type Decision struct {
	// Allowed is whether the request may proceed.
	Allowed bool

	// Reason is why the check failed, or None.
	Reason Reason

	// Limits are the rates applied, shortest window first.  The request had
	// to fit every one.  It is empty when the Token is Unrestricted or the
	// Resolver denied the request.  It holds zero Rates when the Token
	// counts as carrying them, e.g. its only Rate Capabilities were
	// malformed, each carrying its text.
	Limits []Rate

	// Limit is the rate that refused the request: when Reason is
	// RateExceeded, the one of Limits with the longest wait, or the zero
	// Rate when that is what refused.  It is the zero Rate otherwise.
	Limit Rate

	// RetryAfter is how long until the Caller has allowance for another
	// call at Limit.  It is set only when Reason is RateExceeded and Limit
	// is not zero; a zero limit never recovers by waiting.
	RetryAfter time.Duration

	// Warnings are Capability Warnings for the Caller.  They are returned
	// whether or not the request is Allowed.
	Warnings []Warning

	// Err is the Resolver's error when Reason is Denied, or ErrInvalidRate
	// if it returned an invalid rate.  It is for the service to log, not
	// for the Caller.
	Err error
}
