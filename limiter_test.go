// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const prefix = "x1:webpa:rate:"

// clock is a manually advanced clock, safe for concurrent use.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Unix(1_700_000_000, 0)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newLimiter builds a Limiter for tests.  The minimum window is lowered to
// a second so tests can use short windows; TestDefaultWindowBounds covers
// the real default.
func newLimiter(t *testing.T, c *clock, opts ...Option) *Limiter {
	t.Helper()
	opts = append([]Option{WithPrefixes(prefix), WithClock(c.Now), WithMinWindow(time.Second)}, opts...)
	l, err := New(opts...)
	require.NoError(t, err)
	return l
}

func rateCaps(rates ...string) []string {
	caps := make([]string, 0, len(rates)+1)
	caps = append(caps, "x1:webpa:api:.*:all")
	for _, r := range rates {
		caps = append(caps, prefix+r)
	}

	return caps
}

func mustRate(t *testing.T, s string) Rate {
	t.Helper()
	r, err := ParseRate(s)
	require.NoError(t, err)
	return r
}

// allowedCount makes n checks and returns how many were allowed.
func allowedCount(l *Limiter, principal string, caps []string, n int) int {
	allowed := 0
	for range n {
		if l.Check(principal, caps).Allowed {
			allowed++
		}
	}

	return allowed
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "prefix", opts: []Option{WithPrefixes(prefix)}},
		{name: "no prefix", wantErr: true},
		{name: "empty prefixes", opts: []Option{WithPrefixes()}, wantErr: true},
		{name: "bad regex", opts: []Option{WithPrefixes("(")}, wantErr: true},
		{name: "bad override", opts: []Option{WithPrefixes(prefix), WithOverride("p", Rate{})}, wantErr: true},
		{name: "bad max callers", opts: []Option{WithPrefixes(prefix), WithMaxCallers(0)}, wantErr: true},
		{name: "bad sweep interval", opts: []Option{WithPrefixes(prefix), WithSweepInterval(0)}, wantErr: true},
		{name: "bad min window", opts: []Option{WithPrefixes(prefix), WithMinWindow(0)}, wantErr: true},
		{name: "bad max window", opts: []Option{WithPrefixes(prefix), WithMaxWindow(-time.Second)}, wantErr: true},
		{name: "min window above max window", opts: []Option{WithPrefixes(prefix), WithMinWindow(2 * time.Hour), WithMaxWindow(time.Hour)}, wantErr: true},
		{name: "min window equal to max window", opts: []Option{WithPrefixes(prefix), WithMinWindow(time.Hour), WithMaxWindow(time.Hour)}},
		{name: "nil clock", opts: []Option{WithPrefixes(prefix), WithClock(nil)}, wantErr: true},
		{name: "nil option", opts: []Option{nil, WithPrefixes(prefix)}},
		{
			name: "everything",
			opts: []Option{
				WithPrefixes(prefix, "x1:xmidt:rate:"),
				WithOverride("p", Rate{Count: 1, Window: time.Second}),
				WithMaxCallers(10),
				WithSweepInterval(time.Second),
				WithMinWindow(time.Second),
				WithMaxWindow(time.Hour),
				WithPermissiveRequired(),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(tc.opts...)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Nil(t, l)
				return
			}

			assert.NoError(t, err)
			assert.NotNil(t, l)
		})
	}

	_, err := New()
	assert.ErrorIs(t, err, ErrNoPrefixes)
}

func TestBurstAndRefill(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	caps := rateCaps("5/1s")

	for i := range 5 {
		d := l.Check("abc", caps)
		require.True(t, d.Allowed, "call %d", i+1)
		assert.Equal(t, None, d.Reason)
		assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
		assert.Empty(t, d.Warnings)
	}

	d := l.Check("abc", caps)
	assert.False(t, d.Allowed)
	assert.Equal(t, RateExceeded, d.Reason)
	assert.Equal(t, 200*time.Millisecond, d.RetryAfter)
	assert.Empty(t, d.Warnings)

	c.Advance(200 * time.Millisecond)
	assert.True(t, l.Check("abc", caps).Allowed)
	assert.False(t, l.Check("abc", caps).Allowed)
}

func TestLargestRateInToken(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	d := l.Check("abc", rateCaps("5/1s", "20/1s", "10/1s"))
	assert.Equal(t, mustRate(t, "20/1s"), d.Limit)
	assert.Equal(t, 19, allowedCount(l, "abc", rateCaps("5/1s", "20/1s"), 30))
}

func TestCallerRateSharedAcrossTokens(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	tokenA := rateCaps("5/1s")
	tokenB := rateCaps("20/1s")

	assert.Equal(t, mustRate(t, "5/1s"), l.Check("abc", tokenA).Limit)
	assert.Equal(t, mustRate(t, "20/1s"), l.Check("abc", tokenB).Limit)

	// Token A is now held to 20 as well, and both draw from one allowance.
	// The first call cost 200ms at 5/1s and the next two 50ms each, leaving
	// 700ms, or 14 calls, of the second's tolerance.
	d := l.Check("abc", tokenA)
	assert.Equal(t, mustRate(t, "20/1s"), d.Limit)

	allowed := 0
	for i := range 40 {
		token := tokenA
		if i%2 == 1 {
			token = tokenB
		}

		if l.Check("abc", token).Allowed {
			allowed++
		}
	}

	assert.Equal(t, 14, allowed)
}

func TestRememberedRateForgotten(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	l.Check("abc", rateCaps("20/1s"))

	// Presenting 5/1s within twice 20/1s's window keeps the Caller at 20.
	c.Advance(1500 * time.Millisecond)
	assert.Equal(t, mustRate(t, "20/1s"), l.Check("abc", rateCaps("5/1s")).Limit)

	c.Advance(600 * time.Millisecond)
	d := l.Check("abc", rateCaps("5/1s"))
	assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
}

func TestRememberedRateRefreshed(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	for range 5 {
		l.Check("abc", rateCaps("20/1s"))
		c.Advance(1500 * time.Millisecond)
	}

	assert.Equal(t, mustRate(t, "20/1s"), l.Check("abc", rateCaps("5/1s")).Limit)
}

func TestMalformed(t *testing.T) {
	malformed := `malformed; kind=rate; cap="x1:webpa:rate:10/0s"`

	t.Run("only malformed", func(t *testing.T) {
		l := newLimiter(t, newClock())
		for range 3 {
			d := l.Check("abc", rateCaps("10/0s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, RateExceeded, d.Reason)
			assert.True(t, d.Limit.IsZero())
			assert.Zero(t, d.RetryAfter)
			require.Len(t, d.Warnings, 1)
			assert.Equal(t, malformed, d.Warnings[0].String())
		}

		assert.Zero(t, l.len(), "no state is kept for a zero limit")
	})

	t.Run("only malformed, required", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithRequired())
		d := l.Check("abc", rateCaps("10/0s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason, "a malformed rate still counts as present")
	})

	t.Run("alongside a valid rate", func(t *testing.T) {
		l := newLimiter(t, newClock())
		d := l.Check("abc", rateCaps("10/0s", "5/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
		require.Len(t, d.Warnings, 1)
		assert.Equal(t, malformed, d.Warnings[0].String())
	})

	t.Run("after a remembered rate", func(t *testing.T) {
		l := newLimiter(t, newClock())
		l.Check("abc", rateCaps("5/1s"))
		d := l.Check("abc", rateCaps("10/0s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
		assert.Len(t, d.Warnings, 1)
	})

	t.Run("after a remembered rate is forgotten", func(t *testing.T) {
		c := newClock()
		l := newLimiter(t, c, WithMaxCallers(10)) // one shard, so def guards the LRU tail

		l.Check("def", rateCaps("5/1h"))
		l.Check("abc", rateCaps("5/1s"))
		assert.Equal(t, 2, l.len())

		// abc's only rate ages out and its allowance refills, but it is not
		// the least recently used, so the check itself finds it.
		c.Advance(2*time.Second + time.Nanosecond)
		d := l.Check("abc", rateCaps("10/0s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason)
		assert.True(t, d.Limit.IsZero())
		assert.Zero(t, d.RetryAfter)
		assert.Len(t, d.Warnings, 1)
		assert.Equal(t, 1, l.len(), "abc carries no information and is dropped")
	})
}

func TestWindowBounds(t *testing.T) {
	bounds := []Option{WithMinWindow(time.Second), WithMaxWindow(time.Hour)}

	t.Run("shorter than the minimum", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check("abc", rateCaps("100/1ms"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "100000/1s"), d.Limit, "the same calls per second, per minimum window")
		assert.Empty(t, d.Warnings)
	})

	t.Run("longer than the maximum", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check("abc", rateCaps("100/2h"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "50/1h"), d.Limit, "the same calls per second, per maximum window")
		assert.Empty(t, d.Warnings)
	})

	t.Run("at the bounds", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check("abc", rateCaps("5/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "5/1s"), d.Limit)

		d = l.Check("def", rateCaps("5/1h"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "5/1h"), d.Limit)
	})

	t.Run("rescaled rates compete as rescaled", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check("abc", rateCaps("1/1ms", "500/1s", "7200/2h"))
		assert.True(t, d.Allowed)
		assert.Equal(t, mustRate(t, "1000/1s"), d.Limit)
	})

	t.Run("rescaled rates are remembered as one", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		l.Check("abc", rateCaps("5/1s"))
		l.Check("abc", rateCaps("5/1ms"))
		l.Check("abc", rateCaps("500/100ms"))
		assert.Equal(t, mustRate(t, "5000/1s"), l.Check("abc", rateCaps("1/1s")).Limit)
		assert.Len(t, l.shard("abc").get("abc", false).rates, 3, "5/1s, 5000/1s and 1/1s")
	})

	t.Run("burst is bounded by the maximum window", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		assert.Equal(t, 5, allowedCount(l, "abc", rateCaps("10/2h"), 10), "10/2h is held as 5/1h")
	})

	t.Run("override is exempt", func(t *testing.T) {
		for _, rate := range []string{"10/1ms", "10/2h"} {
			l := newLimiter(t, newClock(), append(bounds, WithOverride("abc", mustRate(t, rate)))...)
			d := l.Check("abc", rateCaps("100/1ms"))
			assert.True(t, d.Allowed)
			assert.Equal(t, mustRate(t, rate), d.Limit)
		}
	})
}

func TestDefaultWindowBounds(t *testing.T) {
	l, err := New(WithPrefixes(prefix))
	require.NoError(t, err)

	tests := []struct {
		rate string
		want string
	}{
		{rate: "100/1s", want: "6000/1m"},
		{rate: "100/59s", want: "102/1m"},
		{rate: "100/1m", want: "100/1m"},
		{rate: "100/1h", want: "100/1h"},
		{rate: "100/24h", want: "100/24h"},
		{rate: "100/25h", want: "96/24h"},
		{rate: "1/720h", want: "1/24h"},
	}

	for i, tc := range tests {
		t.Run(tc.rate, func(t *testing.T) {
			d := l.Check(strconv.Itoa(i), rateCaps(tc.rate))
			assert.True(t, d.Allowed)
			assert.Equal(t, mustRate(t, tc.want), d.Limit)
			assert.Empty(t, d.Warnings)
		})
	}
}

func TestNoRateCapability(t *testing.T) {
	tests := []struct {
		name        string
		opts        []Option
		wantAllowed bool
		wantReason  Reason
		wantWarning string
	}{
		{name: "correct if present (default)", wantAllowed: true},
		{name: "correct if present", opts: []Option{WithCorrectIfPresent()}, wantAllowed: true},
		{name: "required", opts: []Option{WithRequired()}, wantReason: NoRateCapability},
		{name: "permissive correct if present", opts: []Option{WithPermissiveCorrectIfPresent()}, wantAllowed: true},
		{
			name:        "permissive required",
			opts:        []Option{WithPermissiveRequired()},
			wantAllowed: true,
			wantReason:  NoRateCapability,
			wantWarning: "would-reject; kind=rate; reason=no-rate-capability",
		},
		{name: "last mode wins", opts: []Option{WithRequired(), WithCorrectIfPresent()}, wantAllowed: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLimiter(t, newClock(), tc.opts...)
			for _, caps := range [][]string{nil, {"x1:webpa:api:.*:all", "x1:other:rate:5/1s"}} {
				d := l.Check("abc", caps)
				assert.Equal(t, tc.wantAllowed, d.Allowed)
				assert.Equal(t, tc.wantReason, d.Reason)
				assert.True(t, d.Limit.IsZero())
				if tc.wantWarning == "" {
					assert.Empty(t, d.Warnings)
				} else {
					require.Len(t, d.Warnings, 1)
					assert.Equal(t, tc.wantWarning, d.Warnings[0].String())
				}
			}

			assert.Zero(t, l.len(), "an Unrestricted Token doesn't touch the allowance")
		})
	}
}

func TestOverride(t *testing.T) {
	tests := []struct {
		name     string
		override string
		caps     []string
		opts     []Option
		want     int
	}{
		{name: "looser than the token", override: "10/1s", caps: rateCaps("2/1s"), want: 10},
		{name: "stricter than the token", override: "2/1s", caps: rateCaps("10/1s"), want: 2},
		{name: "no rate capability", override: "3/1s", caps: rateCaps(), want: 3},
		{name: "no rate capability, required", override: "3/1s", caps: rateCaps(), opts: []Option{WithRequired()}, want: 3},
		{name: "malformed", override: "4/1s", caps: rateCaps("10/0s"), want: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithOverride("abc", mustRate(t, tc.override))}, tc.opts...)
			l := newLimiter(t, newClock(), opts...)

			d := l.Check("abc", tc.caps)
			assert.True(t, d.Allowed)
			assert.Equal(t, mustRate(t, tc.override), d.Limit)
			assert.Equal(t, tc.want-1, allowedCount(l, "abc", tc.caps, 20))
		})
	}
}

func TestOverrideDoesNotRemember(t *testing.T) {
	l := newLimiter(t, newClock(), WithOverride("abc", Rate{Count: 1, Window: time.Second}))
	l.Check("abc", rateCaps("100/1s"))

	s := l.shard("abc")
	c := s.get("abc", false)
	require.NotNil(t, c)
	assert.Empty(t, c.rates)
}

func TestPermissive(t *testing.T) {
	t.Run("rate exceeded", func(t *testing.T) {
		c := newClock()
		l := newLimiter(t, c, WithPermissiveCorrectIfPresent())
		caps := rateCaps("2/1s")

		for range 2 {
			d := l.Check("abc", caps)
			assert.True(t, d.Allowed)
			assert.Empty(t, d.Warnings)
		}

		d := l.Check("abc", caps)
		assert.True(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason)
		assert.Equal(t, 500*time.Millisecond, d.RetryAfter)
		require.Len(t, d.Warnings, 1)
		assert.Equal(t, `would-reject; kind=rate; reason=rate-exceeded; limit="2/1s"`, d.Warnings[0].String())

		// The over-limit call was spent: after one interval there is still
		// no allowance.
		c.Advance(500 * time.Millisecond)
		assert.Equal(t, RateExceeded, l.Check("abc", caps).Reason)
	})

	t.Run("only malformed", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithPermissiveRequired())
		d := l.Check("abc", rateCaps("abc/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason)
		require.Len(t, d.Warnings, 2)
		assert.Equal(t, `malformed; kind=rate; cap="x1:webpa:rate:abc/1s"`, d.Warnings[0].String())
		assert.Equal(t, `would-reject; kind=rate; reason=rate-exceeded; limit=0`, d.Warnings[1].String())
	})

	t.Run("override", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithPermissiveRequired(), WithOverride("abc", Rate{Count: 1, Window: time.Second}))
		assert.Empty(t, l.Check("abc", nil).Warnings)

		d := l.Check("abc", nil)
		assert.True(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason)
		assert.Len(t, d.Warnings, 1)
	})
}

func TestIndependentCallers(t *testing.T) {
	l := newLimiter(t, newClock())
	caps := rateCaps("3/1s")

	assert.Equal(t, 3, allowedCount(l, "abc", caps, 10))
	assert.Equal(t, 3, allowedCount(l, "def", caps, 10))
}

func TestPrefixes(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		caps     []string
		want     string
	}{
		{name: "plain", prefixes: []string{"x1:webpa:rate:"}, caps: []string{"x1:webpa:rate:5/1s"}, want: "5/1s"},
		{
			name:     "subexpressions",
			prefixes: []string{`x1:(webpa|xmidt):(rate|limit):`},
			caps:     []string{"x1:xmidt:limit:7/1m"},
			want:     "7/1m",
		},
		{
			name:     "alternation stays anchored",
			prefixes: []string{`a:|b:`},
			caps:     []string{"zzb:9/1s", "b:4/1s"},
			want:     "4/1s",
		},
		{
			name:     "several prefixes",
			prefixes: []string{"x1:webpa:rate:", "x1:xmidt:rate:"},
			caps:     []string{"x1:webpa:rate:5/1s", "x1:xmidt:rate:8/1s"},
			want:     "8/1s",
		},
		{name: "unanchored match is ignored", prefixes: []string{"rate:"}, caps: []string{"x1:rate:5/1s"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(WithPrefixes(tc.prefixes...), WithClock(newClock().Now), WithMinWindow(time.Second))
			require.NoError(t, err)

			d := l.Check("abc", tc.caps)
			assert.True(t, d.Allowed)
			assert.Empty(t, d.Warnings)
			if tc.want == "" {
				assert.True(t, d.Limit.IsZero())
			} else {
				assert.Equal(t, mustRate(t, tc.want), d.Limit)
			}
		})
	}
}

func TestMaxCallersEvictsLeastRecentlyUsed(t *testing.T) {
	l := newLimiter(t, newClock(), WithMaxCallers(2))
	caps := rateCaps("1/1h")

	assert.True(t, l.Check("a", caps).Allowed)
	assert.True(t, l.Check("b", caps).Allowed)
	assert.False(t, l.Check("a", caps).Allowed, "touches a, so b is least recently used")

	assert.True(t, l.Check("c", caps).Allowed)
	assert.Equal(t, 2, l.len())

	assert.False(t, l.Check("a", caps).Allowed, "a was kept")
	assert.True(t, l.Check("b", caps).Allowed, "b was evicted, so its allowance is full again")
}

func TestMaxCallersSharded(t *testing.T) {
	l := newLimiter(t, newClock(), WithMaxCallers(10_000))
	require.Greater(t, len(l.shards), 1)

	total := 0
	for _, s := range l.shards {
		total += s.max
	}

	assert.Equal(t, 10_000, total)

	for i := range 20_000 {
		l.Check(strconv.Itoa(i), rateCaps("1/1h"))
	}

	assert.LessOrEqual(t, l.len(), 10_000)
}

func TestExpiredStateDropped(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, WithMaxCallers(10)) // one shard, so access sweeps it

	l.Check("abc", rateCaps("5/1s"))
	l.Check("def", rateCaps("5/1m"))
	assert.Equal(t, 2, l.len())

	c.Advance(2*time.Second + time.Nanosecond)
	l.sweep()
	assert.Equal(t, 1, l.len(), "abc forgot its rate and its allowance is full")

	c.Advance(2 * time.Minute)
	l.Check("ghi", rateCaps("5/1s"))
	assert.Equal(t, 1, l.len(), "def is dropped lazily on access")
}

func TestStartStop(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, WithSweepInterval(time.Millisecond))

	l.Stop() // stopping a stopped limiter does nothing
	l.Start()
	l.Start()
	defer l.Stop()

	l.Check("abc", rateCaps("5/1s"))
	c.Advance(time.Hour)
	assert.Eventually(t, func() bool { return l.len() == 0 }, time.Second, time.Millisecond)

	l.Stop()
	l.Stop()
}

func TestConcurrentNeverExceedsBurst(t *testing.T) {
	l := newLimiter(t, newClock())
	caps := rateCaps("50/1s")

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 20 {
				if l.Check("abc", caps).Allowed {
					allowed.Add(1)
				}
			}
		})
	}

	wg.Wait()
	assert.Equal(t, int64(50), allowed.Load())
}
