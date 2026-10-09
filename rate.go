// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrMalformedRate is returned by ParseRate when a rate cannot be parsed.
var ErrMalformedRate = errors.New("malformed rate")

// Rate is a number of calls allowed per window.  Its Burst is always Count.
//
// A zero Rate, with no Count and no Window, allows nothing.  It is how a
// Malformed Capability reaches a Resolver, and how a Resolver refuses a Token
// without denying the Caller.
type Rate struct {
	Count  int
	Window time.Duration

	// text is the rate as the client wrote it, set by ParseRate whether or
	// not it could be parsed, and returned by String, so a Resolver can log
	// it or make sense of a format ParseRate doesn't.  It is empty for a
	// Rate built in code, and plays no part in which allowance a Rate draws
	// from.
	text string
}

// ParseRate parses a rate written <count>/<window>, e.g. 50/1m or 100/24h.
//
// The count is a positive base-10 integer.  The window is a positive Go
// duration (see time.ParseDuration).  ParseRate checks only the grammar; a
// Limiter rescales rates whose window is outside its Window Bounds.
//
// The Rate returned reads back as s from String.  On failure it is zero.
func ParseRate(s string) (Rate, error) {
	countStr, windowStr, found := strings.Cut(s, "/")
	if !found {
		return Rate{text: s}, fmt.Errorf("%w %q: missing '/'", ErrMalformedRate, s)
	}

	count, err := parsePositive(countStr)
	if err != nil {
		return Rate{text: s}, fmt.Errorf("%w %q: count: %w", ErrMalformedRate, s, err)
	}

	window, err := parseWindow(windowStr)
	if err != nil {
		return Rate{text: s}, fmt.Errorf("%w %q: window: %w", ErrMalformedRate, s, err)
	}

	return Rate{Count: count, Window: window, text: s}, nil
}

// parsePositive parses a positive integer made only of base-10 digits, so
// signs, spaces and other bases are refused.
func parsePositive(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}

	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q is not a positive integer", s)
		}
	}

	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}

	if n <= 0 {
		return 0, errors.New("must be positive")
	}

	return n, nil
}

func parseWindow(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}

	if d <= 0 {
		return 0, errors.New("must be positive")
	}

	return d, nil
}

// IsZero reports whether r is a zero Rate, one with no Count and no Window,
// which allows nothing.  The text it was parsed from does not matter.
func (r Rate) IsZero() bool {
	return r.Count == 0 && r.Window == 0
}

// valid reports whether r allows any calls.
func (r Rate) valid() bool {
	return r.Count > 0 && r.Window > 0
}

// key returns r without its text: the identity of the allowance it draws
// from, so that 100/90m and 100/1h30m are one rate.
func (r Rate) key() Rate {
	return Rate{Count: r.Count, Window: r.Window}
}

// String returns the rate as the client wrote it, when r was parsed.  A Rate
// built in code is formatted the way ParseRate reads it, e.g. 50/1m, 600/1h
// or 100/24h, or written as nothing at all if it is invalid.
func (r Rate) String() string {
	switch {
	case r.text != "":
		return r.text
	case r.valid():
		return strconv.Itoa(r.Count) + "/" + formatWindow(r.Window)
	default:
		return ""
	}
}

func formatWindow(d time.Duration) string {
	// time.Duration writes 1m as 1m0s and 1h as 1h0m0s; drop the zero units.
	s := d.String()
	if t, ok := strings.CutSuffix(s, "m0s"); ok {
		s = t + "m"
	}

	if t, ok := strings.CutSuffix(s, "h0m"); ok {
		s = t + "h"
	}

	return s
}

// compareRates orders rates by window, then by count, then by text, so a
// Decision lists the shortest window first and malformed rates in a steady
// order.
func compareRates(a, b Rate) int {
	return cmp.Or(cmp.Compare(a.Window, b.Window), cmp.Compare(a.Count, b.Count), cmp.Compare(a.text, b.text))
}

// addRates appends to rates each of more that it doesn't already hold.
func addRates(rates []Rate, more ...Rate) []Rate {
	for _, r := range more {
		if !slices.Contains(rates, r) {
			rates = append(rates, r)
		}
	}

	return rates
}

// rescale returns r expressed per window, allowing the same calls per second.
// The count is rounded to the nearest whole call, but never below one, so a
// rate that allowed something still does.  r and window must be valid.  The
// result is no longer what the client wrote, so it has no text.
func (r Rate) rescale(window time.Duration) Rate {
	// count * window / r.Window, in 128 bits so a large count can't overflow.
	hi, lo := bits.Mul64(uint64(r.Count), uint64(window)) //nolint:gosec // positive
	if hi >= uint64(r.Window) {                           //nolint:gosec // positive
		return Rate{Count: math.MaxInt, Window: window}
	}

	q, rem := bits.Div64(hi, lo, uint64(r.Window)) //nolint:gosec // positive
	if q >= math.MaxInt {
		return Rate{Count: math.MaxInt, Window: window}
	}

	// Round half up: rem/r.Window >= 1/2, written so 2*rem can't overflow.
	if rem >= uint64(r.Window)-rem { //nolint:gosec // positive
		q++
	}

	return Rate{Count: int(max(q, 1)), Window: window}
}
