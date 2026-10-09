// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

// Package tokenrate limits how fast each Caller may make requests, using the
// Rate Capabilities carried by the Caller's authenticated Token.
//
// tokenrate never sees the Token.  After authenticating a request, a service
// passes its principal and capability strings to Limiter.Check and decides
// how to apply the Decision it gets back.  See CONTEXT.md for the terms used
// here and docs/design.md for the behavior.
package tokenrate

import (
	"container/list"
	"errors"
	"fmt"
	"hash/maphash"
	"regexp"
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
// Caller Rate.  It is safe for concurrent use.
type Limiter struct {
	prefixes      []*regexp.Regexp
	overrides     map[string]Rate
	maxCallers    int
	sweepInterval time.Duration
	minWindow     time.Duration
	maxWindow     time.Duration
	now           func() time.Time
	required      bool
	permissive    bool

	seed   maphash.Seed
	shards []*shard

	runMu sync.Mutex
	stop  chan struct{}
	done  chan struct{}
}

// New creates a Limiter.  WithPrefixes is required.
func New(opts ...Option) (*Limiter, error) {
	l := Limiter{
		overrides:     make(map[string]Rate),
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
// and spends one call from the Caller's allowance.
func (l *Limiter) Check(principal string, capabilities []string) Decision {
	var d Decision
	best, present := l.selectRates(capabilities, &d.Warnings)

	if override, ok := l.overrides[principal]; ok {
		l.checkOverride(principal, override, &d)
		return d
	}

	if !present {
		if l.required {
			l.fail(&d, NoRateCapability)
		} else {
			d.Allowed = true
		}

		return d
	}

	l.checkRemembered(principal, best, &d)
	return d
}

// selectRates finds the capabilities that match a prefix and returns the
// largest valid rate among them, held within the Window Bounds, and whether
// any matched at all.  Each malformed one adds a warning.
func (l *Limiter) selectRates(capabilities []string, warnings *[]Warning) (best Rate, present bool) {
	for _, c := range capabilities {
		for _, re := range l.prefixes {
			m := re.FindStringSubmatch(c)
			if m == nil {
				continue
			}

			present = true
			if r, err := ParseRate(m[len(m)-1]); err != nil {
				*warnings = append(*warnings, malformedWarning(c))
			} else if r = l.bound(r); best.Less(r) {
				best = r
			}

			break
		}
	}

	return best, present
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

func (l *Limiter) checkOverride(principal string, limit Rate, d *Decision) {
	now := l.now()
	s := l.shard(principal)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepOldest(now)
	c := s.get(principal, true)
	d.Limit = limit
	l.spend(c, now, d)
}

func (l *Limiter) checkRemembered(principal string, best Rate, d *Decision) {
	now := l.now()
	s := l.shard(principal)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepOldest(now)

	// A token whose only rates were malformed has nothing to remember, so
	// it creates no state.
	c := s.get(principal, best.valid())
	if c == nil {
		l.fail(d, RateExceeded)
		return
	}

	if best.valid() {
		c.rates[best] = now
	}

	c.forget(now)
	d.Limit = c.limit()
	l.spend(c, now, d)

	if c.expired(now) {
		s.remove(c)
	}
}

// spend takes one call at d.Limit from c's allowance, recording a failure in
// d if there is none.  Permissive modes spend the call even then.
func (l *Limiter) spend(c *caller, now time.Time, d *Decision) {
	if !d.Limit.valid() {
		l.fail(d, RateExceeded)
		return
	}

	next, ok, retryAfter := gcra.Spend(c.tat, now, d.Limit.Count, d.Limit.Window, l.permissive)
	c.tat = next
	if ok {
		d.Allowed = true
		return
	}

	d.RetryAfter = retryAfter
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
// state with a full allowance, evicting the least recently used Caller if
// the shard is full.
func (s *shard) get(principal string, create bool) *caller {
	if e, ok := s.callers[principal]; ok {
		s.lru.MoveToFront(e)
		return e.Value.(*caller)
	}

	if !create {
		return nil
	}

	c := &caller{
		principal: principal,
		rates:     make(map[Rate]time.Time, 1),
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

	c := e.Value.(*caller)
	c.forget(now)
	if c.expired(now) {
		s.remove(c)
	}
}

func (s *shard) sweep(now time.Time) {
	for e := s.lru.Front(); e != nil; {
		next := e.Next()
		c := e.Value.(*caller)
		c.forget(now)
		if c.expired(now) {
			s.remove(c)
		}

		e = next
	}
}

// caller is the state kept for one Caller.
type caller struct {
	principal string

	// tat is the theoretical arrival time of the Caller's allowance, shared
	// by every rate.
	tat time.Time

	// rates are the Remembered Rates, each with when it was last presented.
	rates map[Rate]time.Time
}

// forget drops the Remembered Rates not presented for more than twice their
// window.
func (c *caller) forget(now time.Time) {
	for r, last := range c.rates {
		// Written so that twice a very long window can't overflow.
		if age := now.Sub(last); age > r.Window && age-r.Window > r.Window {
			delete(c.rates, r)
		}
	}
}

// limit returns the largest Remembered Rate, or the zero Rate if none.
func (c *caller) limit() Rate {
	var best Rate
	for r := range c.rates {
		if best.Less(r) {
			best = r
		}
	}

	return best
}

// expired reports whether c carries no information: nothing is remembered
// and its allowance is full.
func (c *caller) expired(now time.Time) bool {
	return len(c.rates) == 0 && !c.tat.After(now)
}
