// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Package tokenrate limits how fast each Caller may make requests, using the
// Rate Capabilities carried by the Caller's authenticated Token.
//
// tokenrate never sees the Token.  After authenticating a request, a service
// passes its principal and capability strings to Limiter.Check, or lets
// Middleware do so, and applies the Decision it gets back.  A Resolver lets
// the service adjust the rates first, for ceilings, overrides and the like.
// See CONTEXT.md for the terms used here and docs/design.md for the behavior.
package tokenrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xmidt-org/tokenrate/internal/gcra"
)

const (
	// minSweepGap is the least time between sweeps, so Callers expiring one
	// after another don't have the Limiter swept on every check.
	minSweepGap = time.Minute
)

// Errors from New and Check.
var (
	// ErrNoPrefixes is returned by New when no Capability Prefix was
	// configured.
	ErrNoPrefixes = errors.New("at least one capability prefix is required")

	// ErrEmptyPrefix is returned by New when a Capability Prefix is empty.
	ErrEmptyPrefix = errors.New("a capability prefix cannot be empty")

	// ErrInvalidRate is the Decision.Err when a Resolver returns a rate that
	// is neither valid nor the zero Rate, which denies the request.
	ErrInvalidRate = errors.New("resolver returned an invalid rate")
)

// Limiter decides, for each request, whether its Caller is within their
// Caller Rates.  It is safe for concurrent use.
//
// It keeps state for each Caller whose Token carries a rate, until twice
// the longest window after their last request: at most 48 hours under
// DefaultResolver.  There is no cap on how many Callers it tracks, since
// forgetting a Caller early would give them a fresh allowance.
//
// It needs no starting or stopping.  A check that finds expired state starts
// a goroutine to sweep it, unless one is running, which exits when done.
type Limiter struct {
	prefixes []string
	resolve  Resolver

	// callers holds each tracked Caller's state, by principal.  Looking up
	// a Caller takes no lock; checking one locks only that Caller.
	callers sync.Map // of string to *caller

	// start is when the Limiter was made.  due and swept are nanoseconds
	// after it, so they follow the monotonic clock, as every other time
	// here does, and a wall clock step cannot move them.
	start time.Time

	// due is the earliest a Caller may expire, after start, or noneDue if
	// there are none.  No Caller expires before it, though one
	// may last longer.
	due atomic.Int64

	// swept is when the Limiter was last swept, after start, or neverSwept.
	swept atomic.Int64

	// sweeping is whether a sweep goroutine is running.
	sweeping atomic.Bool
}

// noneDue is Limiter.due when no Caller is tracked: no time reaches it.
const noneDue = math.MaxInt64

// neverSwept is Limiter.swept before the first sweep.
const neverSwept = math.MinInt64

// New creates a Limiter.  WithPrefixes is required.
func New(opts ...Option) (*Limiter, error) {
	l := Limiter{
		resolve: DefaultResolver,
	}

	var errs []error
	for _, o := range opts {
		if o == nil {
			continue
		}

		if err := o.apply(&l); err != nil {
			errs = append(errs, err)
		}
	}

	if len(l.prefixes) == 0 {
		errs = append(errs, ErrNoPrefixes)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	l.start = time.Now()
	l.due.Store(noneDue)
	l.swept.Store(neverSwept)
	return &l, nil
}

// Check decides one request from principal, whose Token carries capabilities,
// and spends one call from each of the Caller's allowances.  ctx is passed to
// the Resolver.
func (l *Limiter) Check(ctx context.Context, principal string, capabilities []string) Decision {
	var d Decision
	provided := l.selectRates(capabilities, &d.Warnings)

	token, err := l.resolveRates(ctx, principal, provided)
	if err != nil {
		d.Err = err
		d.Reason = Denied
		return d
	}

	// A Token that counts as carrying no rate is Unrestricted.
	if len(token) == 0 {
		d.Allowed = true
		return d
	}

	// One that counts as carrying a zero Rate allows nothing.  That is not
	// worth remembering, so it creates no state.
	if slices.ContainsFunc(token, Rate.IsZero) {
		slices.SortFunc(token, compareRates)
		d.Limits = token
		d.Reason = RateExceeded
		return d
	}

	now := time.Now()
	if l.isDue(now) {
		l.startSweep()
	}

	for {
		c, added := l.get(principal)
		if l.check(c, added, token, now, &d) {
			return d
		}
	}
}

// check judges the request against c, the state found for its Caller, and
// reports whether it did.  It reports false, having touched nothing, if c
// was forgotten since it was looked up: c is then no longer in callers, so
// the Caller must be looked up again.  added says whether the lookup created
// c.
func (l *Limiter) check(c *caller, added bool, token []Rate, now time.Time, d *Decision) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.forgotten {
		return false
	}

	c.remember(token, now)
	c.prune(now)
	l.spend(c, c.applied(now), now, d)

	// Using a Caller only makes their state last longer, so only a new one
	// can bring the next sweep forward.
	if added {
		l.expect(c.expiry())
	}

	return true
}

// resolveRates asks the Resolver what rates the Token counts as carrying,
// and checks that each is valid or zero.  Duplicates are dropped.
func (l *Limiter) resolveRates(ctx context.Context, principal string, provided []Rate) ([]Rate, error) {
	resolved, err := l.resolve(ctx, principal, provided)
	if err != nil {
		return nil, err
	}

	var rates []Rate
	for _, r := range resolved {
		if !r.valid() && !r.IsZero() {
			return nil, fmt.Errorf("%w: count %d, window %s", ErrInvalidRate, r.Count, r.Window)
		}

		rates = addRates(rates, r)
	}

	return rates, nil
}

// selectRates finds the capabilities that start with a prefix and returns
// their rates, in Token order.  Each malformed one adds a warning and
// contributes a zero Rate.  Each carries its text.
func (l *Limiter) selectRates(capabilities []string, warnings *[]Warning) (rates []Rate) {
	for _, c := range capabilities {
		for _, p := range l.prefixes {
			text, ok := strings.CutPrefix(c, p)
			if !ok {
				continue
			}

			r, err := ParseRate(text)
			if err != nil {
				*warnings = append(*warnings, malformedWarning(c))
			}

			rates = addRates(rates, r)

			break
		}
	}

	return rates
}

// spend takes one call from each of c's allowances at rates, which must not
// be empty, recording the outcome in d.  Every rate is checked before any is
// spent, so a refusal spends nothing.
func (l *Limiter) spend(c *caller, rates []Rate, now time.Time, d *Decision) {
	d.Limits = rates

	type pending struct {
		a    *allowance
		next time.Time
	}

	spends := make([]pending, len(rates))
	ok := true
	for i, r := range rates {
		a := c.allowance(r)
		next, fits, retryAfter := gcra.Spend(a.tat, now, r.Count, r.Window)
		spends[i] = pending{a: a, next: next}
		if !fits {
			ok = false
			if retryAfter > d.RetryAfter {
				d.RetryAfter = retryAfter
				d.Limit = r
			}
		}
	}

	if !ok {
		d.Reason = RateExceeded
		return
	}

	for _, p := range spends {
		p.a.tat = p.next
	}

	d.Allowed = true
}

// get returns the state for principal, adding empty state if there is none
// and reporting that it was added.
func (l *Limiter) get(principal string) (c *caller, added bool) {
	if v, ok := l.callers.Load(principal); ok {
		return v.(*caller), false
	}

	v, loaded := l.callers.LoadOrStore(principal, &caller{
		principal:  principal,
		allowances: make(map[Rate]*allowance, 1),
	})

	return v.(*caller), !loaded
}

// forget drops c, whose lock must be held, and marks it forgotten, so a check
// that looked it up meanwhile looks again.
func (l *Limiter) forget(c *caller) {
	if !c.forgotten {
		c.forgotten = true
		l.callers.CompareAndDelete(c.principal, c)
	}
}

// since returns t as nanoseconds after start, measured on the monotonic
// clock, for due and swept.
func (l *Limiter) since(t time.Time) int64 {
	return int64(t.Sub(l.start))
}

// isDue reports whether a Caller may have expired, at least minSweepGap
// after the last sweep.  On most checks it is not, and costs two atomic
// loads.
func (l *Limiter) isDue(now time.Time) bool {
	n := l.since(now)
	if n < l.due.Load() {
		return false
	}

	swept := l.swept.Load()
	return swept == neverSwept || n-swept >= int64(minSweepGap)
}

// expect notes that a Caller expires at t.
func (l *Limiter) expect(t time.Time) {
	n := l.since(t)
	for {
		due := l.due.Load()
		if n >= due || l.due.CompareAndSwap(due, n) {
			return
		}
	}
}

// startSweep starts a goroutine to sweep the Limiter, unless one is already
// running, so no check waits for a sweep.
func (l *Limiter) startSweep() {
	if l.sweeping.CompareAndSwap(false, true) {
		go l.sweep()
	}
}

// sweep drops every Caller whose state no longer carries information, notes
// when the rest expire, and exits.  It locks one Caller at a time, so checks
// carry on while it works.
func (l *Limiter) sweep() {
	defer l.sweeping.Store(false)

	now := time.Now()

	// Callers added from here on note their own expiry, and the sweep notes
	// the rest.
	l.swept.Store(l.since(now))
	l.due.Store(noneDue)
	l.callers.Range(func(_, v any) bool {
		c := v.(*caller)
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.prune(now) {
			l.forget(c)
		} else if !c.forgotten {
			l.expect(c.expiry())
		}

		return true
	})
}

// len returns how many Callers are tracked.
func (l *Limiter) len() int {
	n := 0
	l.callers.Range(func(_, _ any) bool {
		n++
		return true
	})

	return n
}

// caller is the state kept for one Caller: an allowance per rate, keyed by
// Rate.key.
type caller struct {
	principal string

	mu         sync.Mutex
	allowances map[Rate]*allowance

	// forgotten is whether the Caller has been dropped from the Limiter.
	forgotten bool
}

// allowance is the state of one rate for one Caller.
type allowance struct {
	// tat is the theoretical arrival time, the whole state of the GCRA.
	tat time.Time

	// seen is when a Token last presented the rate, making it a Remembered
	// Rate.
	seen time.Time
}

// remembered reports whether the rate r is a Remembered Rate: presented by a
// Token within twice its window.
func (a *allowance) remembered(r Rate, now time.Time) bool {
	// Written so that twice a very long window can't overflow.
	age := now.Sub(a.seen)
	return age <= r.Window || age-r.Window <= r.Window
}

// allowance returns the state for r, creating a full one if there is none.
// Rates are keyed by count and window, so every spelling of a rate draws from
// one allowance.
func (c *caller) allowance(r Rate) *allowance {
	k := r.key()
	a, ok := c.allowances[k]
	if !ok {
		a = &allowance{}
		c.allowances[k] = a
	}

	return a
}

// remember records each of rates as presented now.
func (c *caller) remember(rates []Rate, now time.Time) {
	for _, r := range rates {
		c.allowance(r).seen = now
	}
}

// applied returns the rates that apply to c: every Remembered Rate, shortest
// window first.
func (c *caller) applied(now time.Time) []Rate {
	rates := make([]Rate, 0, len(c.allowances))
	for r, a := range c.allowances {
		if a.remembered(r, now) {
			rates = append(rates, r)
		}
	}

	slices.SortFunc(rates, compareRates)
	return rates
}

// expiry returns when c's state will carry no information if c makes no
// more requests: when every rate is forgotten and its allowance is full.
func (c *caller) expiry() time.Time {
	var t time.Time
	for r, a := range c.allowances {
		// Written so that twice a very long window can't overflow.
		if forgotten := a.seen.Add(r.Window).Add(r.Window); forgotten.After(t) {
			t = forgotten
		}

		if a.tat.After(t) {
			t = a.tat
		}
	}

	return t
}

// prune drops the allowances that carry no information: their rate is no
// longer remembered and they are full.  It reports whether none are left.
func (c *caller) prune(now time.Time) bool {
	for r, a := range c.allowances {
		if !a.remembered(r, now) && !a.tat.After(now) {
			delete(c.allowances, r)
		}
	}

	return len(c.allowances) == 0
}
