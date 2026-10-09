// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"net/http"
	"strconv"
	"time"
)

// DefaultWarningHeader is the header Middleware writes Capability Warnings
// to unless Middleware.WarningHeader says otherwise.
const DefaultWarningHeader = "X-Webpa-Capability-Warning"

// Extractor returns the principal and capabilities of an authenticated
// request, however the service stores them, e.g. from the Token in the
// request's context.  Returning an empty principal and no capabilities for an
// unauthenticated request lets the Limiter's mode decide what to do with it.
type Extractor func(*http.Request) (principal string, capabilities []string)

// Middleware applies a Limiter to HTTP requests.  Place it after the
// middleware that authenticates the request, since it needs the Token's
// principal and capabilities.
//
// For each request it checks the Caller and adds one WarningHeader per
// Capability Warning, whether or not the request goes on.  An allowed request
// is passed to the next handler.  A rejected one gets 429 Too Many Requests,
// with Retry-After when waiting would help; 403 Forbidden when a Rate
// Capability is required and missing; or 503 Service Unavailable when the
// Resolver failed, which Observe can log from Decision.Err.
type Middleware struct {
	// Limiter decides each request.  Required.
	Limiter *Limiter

	// Extract finds the principal and capabilities in a request.  Required.
	Extract Extractor

	// WarningHeader is the header Capability Warnings are written to.  Empty
	// means DefaultWarningHeader.
	WarningHeader string

	// Observe, if set, is called with every Decision, for metrics or
	// logging.  It runs before the response is written.
	Observe func(*http.Request, Decision)
}

// Wrap returns a handler that checks each request with m before passing it
// to next.  It panics if Limiter or Extract is nil, since the Middleware
// cannot work without them.
func (m Middleware) Wrap(next http.Handler) http.Handler {
	if m.Limiter == nil {
		panic("tokenrate: Middleware.Limiter is nil")
	}

	if m.Extract == nil {
		panic("tokenrate: Middleware.Extract is nil")
	}

	if m.WarningHeader == "" {
		m.WarningHeader = DefaultWarningHeader
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, capabilities := m.Extract(r)
		d := m.Limiter.Check(r.Context(), principal, capabilities)
		if m.Observe != nil {
			m.Observe(r, d)
		}

		for _, warning := range d.Warnings {
			w.Header().Add(m.WarningHeader, warning.String())
		}

		switch {
		case d.Allowed:
			next.ServeHTTP(w, r)
		case d.Reason == RateExceeded:
			if d.RetryAfter > 0 {
				w.Header().Set("Retry-After", retryAfterSeconds(d.RetryAfter))
			}

			w.WriteHeader(http.StatusTooManyRequests)
		case d.Reason == ResolverFailed:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

// retryAfterSeconds formats d as the whole seconds Retry-After wants, rounded
// up so a client that waits exactly that long is not refused again.
func retryAfterSeconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
}
