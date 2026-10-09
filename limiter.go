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
	"container/list"
	"context"
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

// Errors from New and Check.
var (
	// ErrNoPrefixes is returned by New when no Capability Prefix was
	// configured.
	ErrNoPrefixes = errors.New("at least one capability prefix is required")

	// ErrInvalidRate is the Decision.Err when a Resolver returns a rate that
	// is neither valid nor the zero Rate, which denies the request.
	ErrInvalidRate = errors.New("resolver returned an invalid rate")
)

// Limiter decides, for each request, whether its Caller is within their
// Caller Rates.  It is safe for concurrent use.
type Limiter struct {
	prefixes      []*regexp.Regexp
	resolve       Resolver
	maxCallers    int
	sweepInterval time.Duration
	minWindow     time.Duration
	maxWindow     time.Duration
	now           func() time.Time

	seed   maphash.Seed
	shards []*shard

	runMu sync.Mutex
	stop  chan struct{}
	done  chan struct{}
}

// New creates a Limiter.  WithPrefixes is required.
func New(opts ...Option) (*Limiter, error) {
	l := Limiter{
		resolve:       DefaultResolver,
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

	now := l.now()
	s := l.shard(principal)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepOldest(now)

	c := s.get(principal)
	c.remember(token, now)
	c.prune(now)
	l.spend(c, c.applied(now), now, &d)
	return d
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
			return nil, fmt.Errorf("%w: %+v", ErrInvalidRate, r)
		}

		rates = addRates(rates, r)
	}

	return rates, nil
}

// selectRates finds the capabilities that match a prefix and returns their
// rates, held within the Window Bounds, in Token order.  Each malformed one
// adds a warning and contributes a zero Rate carrying its text.
func (l *Limiter) selectRates(capabilities []string, warnings *[]Warning) (rates []Rate) {
	for _, c := range capabilities {
		for _, re := range l.prefixes {
			m := re.FindStringSubmatch(c)
			if m == nil {
				continue
			}

			r, err := ParseRate(m[len(m)-1])
			if err != nil {
				*warnings = append(*warnings, malformedWarning(c))
			} else {
				r = l.bound(r)
			}

			rates = addRates(rates, r)

			break
		}
	}

	return rates
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
// there is none, it adds empty state, evicting the least recently used Caller
// if the shard is full.
func (s *shard) get(principal string) *caller {
	if e, ok := s.callers[principal]; ok {
		s.lru.MoveToFront(e)
		return e.Value.(*caller)
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
