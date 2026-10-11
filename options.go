// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

const (
	// DefaultMaxCallers is the number of Callers a Limiter tracks unless
	// WithMaxCallers says otherwise.
	DefaultMaxCallers = 100_000

	// DefaultSweepInterval is how often a started Limiter sweeps expired
	// state unless WithSweepInterval says otherwise.
	DefaultSweepInterval = time.Minute
)

// Option configures a Limiter.
type Option interface {
	apply(*Limiter) error
}

type optionFunc func(*Limiter) error

func (f optionFunc) apply(l *Limiter) error { return f(l) }

// WithPrefixes adds Capability Prefixes, which select the Rate Capabilities
// among a Token's capabilities.  At least one is required.
//
// A prefix may be a regular expression, and may contain subexpressions.  It
// is anchored at the start of the capability, and whatever follows it is the
// rate.  A capability that matches no prefix is ignored.
func WithPrefixes(prefixes ...string) Option {
	return optionFunc(func(l *Limiter) error {
		for _, p := range prefixes {
			// Group the prefix so a top-level alternation can't escape the
			// anchor.  The rate is always the last subexpression.
			re, err := regexp.Compile("^(?:" + p + ")(.*)$")
			if err != nil {
				return fmt.Errorf("capability prefix %q: %w", p, err)
			}

			l.prefixes = append(l.prefixes, re)
		}

		return nil
	})
}

// A Resolver has the final say over the rates a Token counts as carrying.  It
// is called on every check, outside any lock, with the request's context, the
// Token's principal and the rates of its Rate Capabilities, in Token order and
// as parsed.  Each reads back as the client wrote it from String.  A Malformed
// Capability arrives as a zero Rate, which allows nothing.
//
// Whatever the Resolver returns is used instead, as if the Token had carried
// those rates: they are remembered for the Caller, and they are trusted, so
// they are used as returned.  Returning no rates leaves the Token
// Unrestricted.  Returning a zero Rate refuses the request as RateExceeded,
// with nothing remembered.  Returning an error denies the request, whatever
// the reason: the error is returned in the Decision for the service to log,
// and is not shown to the Caller.
//
// That covers the policy a deployment needs, without options for each case:
//
//   - A ceiling for every Caller: return append(provided, ceiling).
//   - An Override for one Caller: ignore provided and return the Override.
//   - Requiring a rate: return an error when provided is empty.
//   - Vouching for a Caller whose Token has no rate: return rates for it.
//   - Blocking a Caller: return an error.
//
// A Resolver that wants DefaultResolver's handling of malformed rates and
// Window Bounds calls it first and adjusts its result.  Returning a rate that is neither valid
// nor zero denies the request with ErrInvalidRate.
type Resolver func(ctx context.Context, principal string, provided []Rate) ([]Rate, error)

// DefaultResolver is the Resolver unless WithResolver says otherwise.  It
// ignores a Malformed Capability when the Token has valid rates, so a typo
// in one rate doesn't void the others, and holds a Token whose rates are all
// malformed to those zero Rates, so a typo never means no limit.
//
// It also holds each valid rate within the Window Bounds, one minute to 24
// hours.  A rate outside them is rescaled to the nearer bound at the same
// calls per second, rounded to the nearest call but at least one, so 100/1s
// is held as 6000/1m and 100/48h as 50/24h.  This keeps a tiny window from
// churning state and a long one from allowing a huge Burst at once.  A
// rescaled rate is no longer what the client wrote, so it reads back
// formatted.
//
// It never returns an error.
func DefaultResolver(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
	return defaultResolve(provided, time.Minute, 24*time.Hour), nil
}

// defaultResolve is DefaultResolver with its Window Bounds as arguments, so
// tests can use short windows.
func defaultResolve(provided []Rate, minWindow, maxWindow time.Duration) []Rate {
	valid := slices.DeleteFunc(slices.Clone(provided), Rate.IsZero)
	if len(valid) == 0 {
		return provided
	}

	for i, r := range valid {
		switch {
		case r.Window < minWindow:
			valid[i] = r.rescale(minWindow)
		case r.Window > maxWindow:
			valid[i] = r.rescale(maxWindow)
		}
	}

	return valid
}

// WithResolver sets the Resolver, replacing DefaultResolver.
func WithResolver(r Resolver) Option {
	return optionFunc(func(l *Limiter) error {
		if r == nil {
			return errors.New("resolver cannot be nil")
		}

		l.resolve = r
		return nil
	})
}

// WithMaxCallers bounds how many Callers the Limiter tracks.  When it is
// full, the least recently used Caller is forgotten, which only ever makes
// that Caller's next request more lenient.  The default is DefaultMaxCallers.
func WithMaxCallers(n int) Option {
	return optionFunc(func(l *Limiter) error {
		if n < 1 {
			return errors.New("max callers must be positive")
		}

		l.maxCallers = n
		return nil
	})
}

// WithSweepInterval sets how often a started Limiter sweeps expired state.
// The default is DefaultSweepInterval.
func WithSweepInterval(d time.Duration) Option {
	return optionFunc(func(l *Limiter) error {
		if d <= 0 {
			return errors.New("sweep interval must be positive")
		}

		l.sweepInterval = d
		return nil
	})
}
