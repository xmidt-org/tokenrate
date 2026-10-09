// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Package tokenrate limits how fast each Caller may make requests, using the
// Rate Capabilities carried by the Caller's authenticated Token.
//
// tokenrate never sees the Token.  After authenticating a request, a service
// passes its principal and capability strings to Limiter.Check, or lets
// Middleware do so, and applies the Decision it gets back.  See CONTEXT.md
// for the terms used here and docs/design.md for the behavior.
package tokenrate

import (
	"container/list"
	"errors"
	"fmt"
	"hash/maphash"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/xmidt-org/tokenrate/internal/gcra"
)

const (
	// maxShards bounds how many locks the Callers are spread over.
	maxShards = 64

	// minCallersPerShard keeps small limiters in few shards, so the least
	// recently used Caller evicted is close to the least recently used
	// overall.
	minCallersPerShard = 1024
)

// ErrNoPrefixes is returned by New when no Capability Prefix was configured.
var ErrNoPrefixes = errors.New("at least one capability prefix is required")

// Limiter decides, for each request, whether its Caller is within their
// Caller Rates.  It is safe for concurrent use.
type Limiter struct {
	prefixes          []*regexp.Regexp
	rates             []Rate            // Configured Rates for every Caller
	callerRates       map[string][]Rate // Configured Rates per Caller, merged with rates by New
	overrides         map[string][]Rate
	maxCallers        int
	sweepInterval     time.Duration
	minWindow         time.Duration
	maxWindow         time.Duration
	now               func() time.Time
	required          bool
	permissive        bool
	limitUnrestricted bool

	seed   maphash.Seed
	shards []*shard

	runMu sync.Mutex
	stop  chan struct{}
	done  chan struct{}
}

// New creates a Limiter.  WithPrefixes is required.
func New(opts ...Option) (*Limiter, error) {
	l := Limiter{
		callerRates:   make(map[string][]Rate),
		overrides:     make(map[string][]Rate),
		maxCallers:    DefaultMaxCallers,
		sweepInterval: DefaultSweepInterval,
		minWindow:     DefaultMinWindow,
		maxWindow:     DefaultMaxWindow,
		now:           time.Now,
		seed:          maphash.MakeSeed(),
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

	if l.minWindow > l.maxWindow {
		errs = append(errs, fmt.Errorf("min window %v exceeds max window %v", l.minWindow, l.maxWindow))
	}

	for principal, rates := range l.callerRates {
		if _, ok := l.overrides[principal]; ok {
			errs = append(errs, fmt.Errorf("%q has both rates and an override", principal))
		}

		l.callerRates[principal] = addRates(slices.Clone(l.rates), rates...)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	n := min(max(l.maxCallers/minCallersPerShard, 1), maxShards)
	l.shards = make([]*shard, n)
	for i := range l.shards {
		size := l.maxCallers / n
		if i < l.maxCallers%n {
			size++
		}

		l.shards[i] = &shard{
			max:     size,
			callers: make(map[string]*list.Element),
		}
	}

	return &l, nil
}

// Check decides one request from principal, whose Token carries capabilities,
// and spends one call from each of the Caller's allowances.
func (l *Limiter) Check(principal string, capabilities []string) Decision {
	var d Decision
	token, present := l.selectRates(capabilities, &d.Warnings)

	now := l.now()
	s := l.shard(principal)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepOldest(now)

	if override, ok := l.overrides[principal]; ok {
		l.spend(s.get(principal, true), slices.Clone(override), now, &d)
		return d
	}

	if !present {
		if l.required {
			l.fail(&d, NoRateCapability)
			return d
		}

		if !l.limitUnrestricted {
			d.Allowed = true
			return d
		}
	}

	// A Caller with nothing to remember and no Configured Rates gets no
	// state, so a Token whose only rates are malformed creates none.
	configured := l.configured(principal)
	c := s.get(principal, len(token)+len(configured) > 0)

	var applied []Rate
	if c != nil {
		c.remember(token, now)
		c.prune(now)
		applied = c.applied(now, configured)
	}

	if len(applied) == 0 {
		if c != nil && len(c.allowances) == 0 {
			s.remove(c)
		}

		// A Token with Rate Capabilities that left nothing to apply is held
		// to zero.  One without any is Unrestricted after all.
		if present {
			l.fail(&d, RateExceeded)
		} else {
			d.Allowed = true
		}

		return d
	}

	l.spend(c, applied, now, &d)
	return d
}

// selectRates finds the capabilities that match a prefix and returns their
// valid rates, held within the Window Bounds, and whether any matched at all.
// Each malformed one adds a warning.
func (l *Limiter) selectRates(capabilities []string, warnings *[]Warning) (rates []Rate, present bool) {
	for _, c := range capabilities {
		for _, re := range l.prefixes {
			m := re.FindStringSubmatch(c)
			if m == nil {
				continue
			}

			present = true
			if r, err := ParseRate(m[len(m)-1]); err != nil {
				*warnings = append(*warnings, malformedWarning(c))
			} else {
				rates = addRates(rates, l.bound(r))
			}

			break
		}
	}

	return rates, present
}

// bound returns r rescaled to the nearer Window Bound if its window is
// outside them, otherwise r.  The calls per second are kept, so a 100/1s
// under a one minute minimum becomes 6000/1m.
func (l *Limiter) bound(r Rate) Rate {
	switch {
	case r.Window < l.minWindow:
		return r.rescale(l.minWindow)
	case r.Window > l.maxWindow:
		return r.rescale(l.maxWindow)
	default:
		return r
	}
}

// configured returns the Configured Rates for principal.
func (l *Limiter) configured(principal string) []Rate {
	if rates, ok := l.callerRates[principal]; ok {
		return rates
	}

	return l.rates
}

// spend takes one call from each of c's allowances at rates, which must not
// be empty, recording the outcome in d.  Every rate is checked before any is
// spent, so a refusal spends nothing.  Permissive modes spend regardless.
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

	if ok || l.permissive {
		for _, p := range spends {
			p.a.tat = p.next
		}
	}

	if ok {
		d.Allowed = true
		return
	}

	l.fail(d, RateExceeded)
}

func (l *Limiter) fail(d *Decision, reason Reason) {
	d.Reason = reason
	if l.permissive {
		d.Allowed = true
		d.Warnings = append(d.Warnings, wouldRejectWarning(reason, d.Limit))
		return
	}

	d.Allowed = false
}

func (l *Limiter) shard(principal string) *shard {
	if len(l.shards) == 1 {
		return l.shards[0]
	}

	return l.shards[maphash.String(l.seed, principal)%uint64(len(l.shards))]
}

// Start begins sweeping expired state every sweep interval, until Stop is
// called.  Expired state is also dropped lazily as Callers are checked, so
// Start is optional; it bounds how long idle state is kept.  Starting a
// started Limiter does nothing.
func (l *Limiter) Start() {
	l.runMu.Lock()
	defer l.runMu.Unlock()

	if l.stop != nil {
		return
	}

	l.stop = make(chan struct{})
	l.done = make(chan struct{})
	go l.run(l.stop, l.done)
}

// Stop ends sweeping, and waits for a sweep in progress to finish.  Stopping
// a stopped Limiter does nothing.
func (l *Limiter) Stop() {
	l.runMu.Lock()
	defer l.runMu.Unlock()

	if l.stop == nil {
		return
	}

	close(l.stop)
	<-l.done
	l.stop, l.done = nil, nil
}

func (l *Limiter) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	t := time.NewTicker(l.sweepInterval)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			l.sweep()
		}
	}
}

// sweep drops every Caller whose state no longer carries information.
func (l *Limiter) sweep() {
	for _, s := range l.shards {
		s.mu.Lock()
		s.sweep(l.now())
		s.mu.Unlock()
	}
}

// len returns how many Callers are tracked.
func (l *Limiter) len() int {
	n := 0
	for _, s := range l.shards {
		s.mu.Lock()
		n += s.lru.Len()
		s.mu.Unlock()
	}

	return n
}

// shard holds some of the Callers, in least recently used order.
type shard struct {
	mu      sync.Mutex
	max     int
	callers map[string]*list.Element
	lru     list.List // of *caller; the front is the most recently used
}

// get returns the state for principal and marks it most recently used.  If
// there is none, it returns nil unless create is set, in which case it adds
// empty state, evicting the least recently used Caller if the shard is full.
func (s *shard) get(principal string, create bool) *caller {
	if e, ok := s.callers[principal]; ok {
		s.lru.MoveToFront(e)
		return e.Value.(*caller)
	}

	if !create {
		return nil
	}

	c := &caller{
		principal:  principal,
		allowances: make(map[Rate]*allowance, 1),
	}

	s.callers[principal] = s.lru.PushFront(c)
	for s.lru.Len() > s.max {
		s.remove(s.lru.Back().Value.(*caller))
	}

	return c
}

func (s *shard) remove(c *caller) {
	if e, ok := s.callers[c.principal]; ok {
		s.lru.Remove(e)
		delete(s.callers, c.principal)
	}
}

// sweepOldest drops the least recently used Caller if its state has expired.
// Doing this on every check keeps idle state from piling up between sweeps.
func (s *shard) sweepOldest(now time.Time) {
	e := s.lru.Back()
	if e == nil {
		return
	}

	if c := e.Value.(*caller); c.prune(now) {
		s.remove(c)
	}
}

func (s *shard) sweep(now time.Time) {
	for e := s.lru.Front(); e != nil; {
		next := e.Next()
		if c := e.Value.(*caller); c.prune(now) {
			s.remove(c)
		}

		e = next
	}
}

// caller is the state kept for one Caller: an allowance per rate.
type caller struct {
	principal  string
	allowances map[Rate]*allowance
}

// allowance is the state of one rate for one Caller.
type allowance struct {
	// tat is the theoretical arrival time, the whole state of the GCRA.
	tat time.Time

	// seen is when a Token last presented the rate, making it a Remembered
	// Rate.  It is zero for a rate only configuration applied.
	seen time.Time
}

// remembered reports whether the rate r is a Remembered Rate: presented by a
// Token within twice its window.
func (a *allowance) remembered(r Rate, now time.Time) bool {
	if a.seen.IsZero() {
		return false
	}

	// Written so that twice a very long window can't overflow.
	age := now.Sub(a.seen)
	return age <= r.Window || age-r.Window <= r.Window
}

// allowance returns the state for r, creating a full one if there is none.
func (c *caller) allowance(r Rate) *allowance {
	a, ok := c.allowances[r]
	if !ok {
		a = &allowance{}
		c.allowances[r] = a
	}

	return a
}

// remember records each of rates as presented now.
func (c *caller) remember(rates []Rate, now time.Time) {
	for _, r := range rates {
		c.allowance(r).seen = now
	}
}

// applied returns the rates that apply to c: every Remembered Rate plus the
// configured ones, shortest window first.
func (c *caller) applied(now time.Time, configured []Rate) []Rate {
	rates := make([]Rate, 0, len(c.allowances)+len(configured))
	for r, a := range c.allowances {
		if a.remembered(r, now) {
			rates = append(rates, r)
		}
	}

	rates = addRates(rates, configured...)
	slices.SortFunc(rates, compareRates)
	return rates
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
