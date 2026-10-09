// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// DefaultMaxCallers is the number of Callers a Limiter tracks unless
	// WithMaxCallers says otherwise.
	DefaultMaxCallers = 100_000

	// DefaultSweepInterval is how often a started Limiter sweeps expired
	// state unless WithSweepInterval says otherwise.
	DefaultSweepInterval = time.Minute

	// DefaultMinWindow is the shortest window a Rate Capability may have
	// unless WithMinWindow says otherwise.
	DefaultMinWindow = time.Minute

	// DefaultMaxWindow is the longest window a Rate Capability may have
	// unless WithMaxWindow says otherwise.
	DefaultMaxWindow = 24 * time.Hour
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
// Token's principal and its valid Rate Capabilities, held within the Window
// Bounds.  Whatever it returns is used instead, as if the Token had carried
// those rates: they are remembered for the Caller, they count as present for
// WithRequired, and they are trusted, so the Window Bounds do not apply.
//
// That one rule covers the special cases a deployment needs:
//
//   - A ceiling for every Caller: return append(provided, ceiling).
//   - An Override for one Caller: ignore provided and return the Override.
//   - Vouching for a Caller whose Token has no rate under WithRequired:
//     return rates when provided is empty.
//   - Keeping such Tokens Unrestricted while adding a ceiling to the rest:
//     return nil when provided is empty.
//
// Returning an error, or a rate whose count or window is not positive, fails
// the check with ResolverFailed.
type Resolver func(ctx context.Context, principal string, provided []Rate) ([]Rate, error)

// WithResolver sets the Resolver.  Without one, a Token's Rate Capabilities
// are used as they are.
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

// WithMinWindow sets the shortest window a Rate Capability may have.  A
// rate with a shorter window is rescaled to the minimum at the same calls
// per second, so a 100/1s under a one minute minimum is held as 6000/1m.
// This spreads a tiny window's refills over a sensible period, and keeps
// its Remembered Rate long enough to matter.  Overrides are trusted and are
// not rescaled.  The default is DefaultMinWindow.
func WithMinWindow(d time.Duration) Option {
	return optionFunc(func(l *Limiter) error {
		if d <= 0 {
			return errors.New("min window must be positive")
		}

		l.minWindow = d
		return nil
	})
}

// WithMaxWindow sets the longest window a Rate Capability may have.  A rate
// with a longer window is rescaled to the maximum at the same calls per
// second, rounded to the nearest call but at least one, so a 100/48h under
// a 24 hour maximum is held as 50/24h.  This bounds the Burst a long window
// would otherwise allow at once.  Overrides are trusted and are not rescaled.
// The default is DefaultMaxWindow.
func WithMaxWindow(d time.Duration) Option {
	return optionFunc(func(l *Limiter) error {
		if d <= 0 {
			return errors.New("max window must be positive")
		}

		l.maxWindow = d
		return nil
	})
}

// WithClock sets the source of the current time, for tests.
func WithClock(now func() time.Time) Option {
	return optionFunc(func(l *Limiter) error {
		if now == nil {
			return errors.New("clock cannot be nil")
		}

		l.now = now
		return nil
	})
}

// The mode options each set the whole mode; the last one applied wins.

// WithRequired makes the Limiter Enforcing and Required: a Token must carry a
// Rate Capability, or its Caller must have an Override.
func WithRequired() Option {
	return withMode(true, false)
}

// WithCorrectIfPresent makes the Limiter Enforcing and Correct If Present: a
// Token without Rate Capabilities is Unrestricted.  This is the default.
func WithCorrectIfPresent() Option {
	return withMode(false, false)
}

// WithPermissiveRequired is WithRequired, except failing checks are allowed
// and reported as would-reject Capability Warnings.
func WithPermissiveRequired() Option {
	return withMode(true, true)
}

// WithPermissiveCorrectIfPresent is WithCorrectIfPresent, except failing
// checks are allowed and reported as would-reject Capability Warnings.
func WithPermissiveCorrectIfPresent() Option {
	return withMode(false, true)
}

func withMode(required, permissive bool) Option {
	return optionFunc(func(l *Limiter) error {
		l.required = required
		l.permissive = permissive
		return nil
	})
}
