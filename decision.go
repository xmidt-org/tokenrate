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

	// Limit is the rate applied.  It is the zero Rate when the Token is
	// Unrestricted, or when its only Rate Capabilities were malformed.
	Limit Rate

	// RetryAfter is how long until the Caller has allowance for another
	// call.  It is set only when Reason is RateExceeded and the limit is not
	// zero; a zero limit never recovers by waiting.
	RetryAfter time.Duration

	// Warnings are Capability Warnings for the Caller.  They are returned
	// whether or not the request is Allowed.
	Warnings []Warning
}
