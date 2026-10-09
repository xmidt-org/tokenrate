// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import "time"

// Reason says why a check failed.
type Reason int

const (
	// None means the check passed.
	None Reason = iota

	// NoRateCapability means the check is Required but the Token carried no
	// Rate Capability and its Caller has no Override.
	NoRateCapability

	// RateExceeded means the Caller has no allowance left at the limit.
	RateExceeded
)

// String returns the reason as written in a Capability Warning, e.g.
// rate-exceeded.
func (r Reason) String() string {
	switch r {
	case None:
		return "none"
	case NoRateCapability:
		return "no-rate-capability"
	case RateExceeded:
		return "rate-exceeded"
	default:
		return "unknown"
	}
}

// Decision is the result of one Check.
type Decision struct {
	// Allowed is whether the request may proceed.  In a Permissive mode it
	// is always true.
	Allowed bool

	// Reason is why the check failed, or None.  In a Permissive mode it is
	// set even though the request is Allowed.
	Reason Reason

	// Limits are the rates applied, shortest window first.  The request had
	// to fit every one.  It is empty when the Token is Unrestricted, and
	// when the Token's only Rate Capabilities were malformed and nothing
	// else applied, which is held to a rate of zero.
	Limits []Rate

	// Limit is the rate that refused the request: when Reason is
	// RateExceeded, the one of Limits with the longest wait, or the zero
	// Rate if Limits is empty.  It is the zero Rate otherwise.
	Limit Rate

	// RetryAfter is how long until the Caller has allowance for another
	// call at Limit.  It is set only when Reason is RateExceeded and Limit
	// is not zero; a zero limit never recovers by waiting.
	RetryAfter time.Duration

	// Warnings are Capability Warnings for the Caller.  They are returned
	// whether or not the request is Allowed.
	Warnings []Warning
}
