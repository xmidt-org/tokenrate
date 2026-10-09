// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const prefix = "prefix:rate:"

var ctx = context.Background()

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
	caps = append(caps, "prefix:api:.*:all")
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

// rates parses several rates, in the order given; none is nil.
func rates(t *testing.T, ss ...string) []Rate {
	t.Helper()
	if len(ss) == 0 {
		return nil
	}

	out := make([]Rate, 0, len(ss))
	for _, s := range ss {
		out = append(out, mustRate(t, s))
	}

	return out
}

// allowedCount makes n checks and returns how many were allowed.
func allowedCount(l *Limiter, principal string, caps []string, n int) int {
	allowed := 0
	for range n {
		if l.Check(ctx, principal, caps).Allowed {
			allowed++
		}
	}

	return allowed
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr string
	}{
		{name: "prefix", opts: []Option{WithPrefixes(prefix)}},
		{name: "no prefix", wantErr: ErrNoPrefixes.Error()},
		{name: "empty prefixes", opts: []Option{WithPrefixes()}, wantErr: ErrNoPrefixes.Error()},
		{name: "bad regex", opts: []Option{WithPrefixes("(")}, wantErr: `capability prefix "("`},
		{name: "nil resolver", opts: []Option{WithPrefixes(prefix), WithResolver(nil)}, wantErr: "resolver"},
		{name: "bad max callers", opts: []Option{WithPrefixes(prefix), WithMaxCallers(0)}, wantErr: "max callers"},
		{name: "bad sweep interval", opts: []Option{WithPrefixes(prefix), WithSweepInterval(0)}, wantErr: "sweep interval"},
		{name: "bad min window", opts: []Option{WithPrefixes(prefix), WithMinWindow(0)}, wantErr: "min window must"},
		{name: "bad max window", opts: []Option{WithPrefixes(prefix), WithMaxWindow(-time.Second)}, wantErr: "max window must"},
		{name: "min window above max window", opts: []Option{WithPrefixes(prefix), WithMinWindow(2 * time.Hour), WithMaxWindow(time.Hour)}, wantErr: "exceeds max window"},
		{name: "min window equal to max window", opts: []Option{WithPrefixes(prefix), WithMinWindow(time.Hour), WithMaxWindow(time.Hour)}},
		{name: "nil clock", opts: []Option{WithPrefixes(prefix), WithClock(nil)}, wantErr: "clock"},
		{name: "nil option", opts: []Option{nil, WithPrefixes(prefix)}},
		{
			name: "everything",
			opts: []Option{
				WithPrefixes(prefix, "other:rate:"),
				WithResolver(func(_ context.Context, _ string, r []Rate) ([]Rate, error) { return r, nil }),
				WithMaxCallers(10),
				WithSweepInterval(time.Second),
				WithMinWindow(time.Second),
				WithMaxWindow(time.Hour),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(tc.opts...)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
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
		d := l.Check(ctx, "abc", caps)
		require.True(t, d.Allowed, "call %d", i+1)
		assert.Equal(t, None, d.Reason)
		assert.Equal(t, rates(t, "5/1s"), d.Limits)
		assert.True(t, d.Limit.IsZero(), "nothing refused")
		assert.Empty(t, d.Warnings)
	}

	d := l.Check(ctx, "abc", caps)
	assert.False(t, d.Allowed)
	assert.Equal(t, RateExceeded, d.Reason)
	assert.Equal(t, rates(t, "5/1s"), d.Limits)
	assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
	assert.Equal(t, 200*time.Millisecond, d.RetryAfter)
	assert.Empty(t, d.Warnings)

	c.Advance(200 * time.Millisecond)
	assert.True(t, l.Check(ctx, "abc", caps).Allowed)
	assert.False(t, l.Check(ctx, "abc", caps).Allowed)
}

func TestEveryRateInTokenApplies(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	d := l.Check(ctx, "abc", rateCaps("20/1s", "5/1s", "10/1s", "5/1s"))
	assert.True(t, d.Allowed)
	assert.Equal(t, rates(t, "5/1s", "10/1s", "20/1s"), d.Limits, "sorted, without the duplicate")

	// The smallest count bounds the burst, and is the rate that refuses.
	assert.Equal(t, 4, allowedCount(l, "abc", rateCaps("5/1s", "20/1s"), 30))
	d = l.Check(ctx, "abc", rateCaps("5/1s", "20/1s"))
	assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
	assert.Equal(t, 200*time.Millisecond, d.RetryAfter)
}

func TestTieredRates(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	caps := rateCaps("3/1s", "5/1m")

	assert.Equal(t, 3, allowedCount(l, "abc", caps, 10), "3/1s bounds the burst")

	// A second later 3/1s has refilled, but 5/1m has only 2 left.
	c.Advance(time.Second)
	assert.Equal(t, 2, allowedCount(l, "abc", caps, 10))

	d := l.Check(ctx, "abc", caps)
	assert.False(t, d.Allowed)
	assert.Equal(t, mustRate(t, "5/1m"), d.Limit)
	assert.Equal(t, 11*time.Second, d.RetryAfter)

	// A refusal spends nothing: 3/1s still has its allowance when 5/1m
	// recovers.
	c.Advance(11 * time.Second)
	assert.True(t, l.Check(ctx, "abc", caps).Allowed)
}

func TestCallerHeldToEveryTokensRates(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	tokenA := rateCaps("5/1s")
	tokenB := rateCaps("20/1s")

	assert.Equal(t, rates(t, "5/1s"), l.Check(ctx, "abc", tokenA).Limits)
	assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", tokenB).Limits, "A's rate applies to B")
	assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", tokenA).Limits, "and B's to A")

	// Both Tokens draw from the same allowances; 5/1s has 2 calls left.
	allowed := 0
	for i := range 40 {
		token := tokenA
		if i%2 == 1 {
			token = tokenB
		}

		if l.Check(ctx, "abc", token).Allowed {
			allowed++
		}
	}

	assert.Equal(t, 2, allowed)
}

func TestRateCutLandsImmediately(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)
	oldToken := rateCaps("20/1s")
	newToken := rateCaps("2/1s")

	l.Check(ctx, "abc", oldToken)
	l.Check(ctx, "abc", newToken)

	// The old Token is now held to the new rate, which has one call left.
	d := l.Check(ctx, "abc", oldToken)
	assert.Equal(t, rates(t, "2/1s", "20/1s"), d.Limits)
	assert.True(t, d.Allowed)

	d = l.Check(ctx, "abc", oldToken)
	assert.False(t, d.Allowed)
	assert.Equal(t, mustRate(t, "2/1s"), d.Limit)
}

func TestRememberedRateForgotten(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	l.Check(ctx, "abc", rateCaps("20/1s"))

	// Within twice its window, 20/1s still applies.
	c.Advance(1500 * time.Millisecond)
	assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)

	c.Advance(600 * time.Millisecond)
	assert.Equal(t, rates(t, "5/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)
}

func TestRememberedRateRefreshed(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	for range 5 {
		l.Check(ctx, "abc", rateCaps("20/1s"))
		c.Advance(1500 * time.Millisecond)
	}

	assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)
}

func TestForgottenRateKeepsItsSpentAllowance(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	// 2/1s is presented once, then spent by another Token's requests just
	// before it is forgotten.
	l.Check(ctx, "abc", rateCaps("2/1s"))
	c.Advance(1900 * time.Millisecond)
	l.Check(ctx, "abc", rateCaps("5/1s"))

	c.Advance(200 * time.Millisecond)
	d := l.Check(ctx, "abc", rateCaps("5/1s"))
	assert.Equal(t, rates(t, "5/1s"), d.Limits, "2/1s is forgotten")
	assert.Len(t, l.shard("abc").get("abc").allowances, 2, "but its allowance is not full, so it is kept")

	// Presenting it again finds that allowance rather than a fresh burst.
	d = l.Check(ctx, "abc", rateCaps("2/1s"))
	assert.True(t, d.Allowed)
	assert.False(t, l.Check(ctx, "abc", rateCaps("2/1s")).Allowed)
}

func TestMalformed(t *testing.T) {
	malformed := `malformed; kind=rate; cap="prefix:rate:10/0s"`

	t.Run("only malformed", func(t *testing.T) {
		l := newLimiter(t, newClock())
		for range 3 {
			d := l.Check(ctx, "abc", rateCaps("10/0s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, RateExceeded, d.Reason)
			assert.Equal(t, []Rate{{Malformed: "10/0s"}}, d.Limits)
			assert.True(t, d.Limit.IsZero())
			assert.Zero(t, d.RetryAfter)
			require.Len(t, d.Warnings, 1)
			assert.Equal(t, malformed, d.Warnings[0].String())
		}

		assert.Zero(t, l.len(), "no state is kept for a zero limit")
	})

	t.Run("alongside a valid rate", func(t *testing.T) {
		l := newLimiter(t, newClock())
		d := l.Check(ctx, "abc", rateCaps("10/0s", "5/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "5/1s"), d.Limits)
		require.Len(t, d.Warnings, 1)
		assert.Equal(t, malformed, d.Warnings[0].String())
	})

	t.Run("after a remembered rate", func(t *testing.T) {
		l := newLimiter(t, newClock())
		l.Check(ctx, "abc", rateCaps("5/1s"))
		d := l.Check(ctx, "abc", rateCaps("10/0s"))
		assert.False(t, d.Allowed, "a broken Token is held to zero, whatever the Caller's other Tokens say")
		assert.Equal(t, RateExceeded, d.Reason)
		assert.Equal(t, []Rate{{Malformed: "10/0s"}}, d.Limits)
		assert.Len(t, d.Warnings, 1)
		assert.Equal(t, 1, l.len(), "and does not disturb what is remembered")
	})

	t.Run("several malformed", func(t *testing.T) {
		l := newLimiter(t, newClock())
		d := l.Check(ctx, "abc", rateCaps("10/0s", "abc/1s", "10/0s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, []Rate{{Malformed: "10/0s"}, {Malformed: "abc/1s"}}, d.Limits, "each text once, in order")
		assert.Len(t, d.Warnings, 3, "but every capability is warned about")
	})

	t.Run("with a resolver that drops the zero", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(func(ctx context.Context, p string, provided []Rate) ([]Rate, error) {
			return DefaultResolver(ctx, p, slices.DeleteFunc(provided, Rate.IsZero))
		}))
		d := l.Check(ctx, "abc", rateCaps("10/0s"))
		assert.True(t, d.Allowed, "the resolver decided the Token is Unrestricted")
		assert.Empty(t, d.Limits)
		assert.Len(t, d.Warnings, 1, "it is still warned about")
	})
}

func TestWindowBounds(t *testing.T) {
	bounds := []Option{WithMinWindow(time.Second), WithMaxWindow(time.Hour)}

	t.Run("shorter than the minimum", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check(ctx, "abc", rateCaps("100/1ms"))
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "100000/1s"), d.Limits, "the same calls per second, per minimum window")
		assert.Empty(t, d.Warnings)
	})

	t.Run("longer than the maximum", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check(ctx, "abc", rateCaps("100/2h"))
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "50/1h"), d.Limits, "the same calls per second, per maximum window")
		assert.Empty(t, d.Warnings)
	})

	t.Run("at the bounds", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		assert.Equal(t, rates(t, "5/1s", "5/1h"), l.Check(ctx, "abc", rateCaps("5/1h", "5/1s")).Limits)
	})

	t.Run("rescaled rates apply as rescaled", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		d := l.Check(ctx, "abc", rateCaps("1/1ms", "500/1s", "7200/2h"))
		assert.Equal(t, rates(t, "500/1s", "1000/1s", "3600/1h"), d.Limits)
	})

	t.Run("rescaled rates are remembered as one", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		l.Check(ctx, "abc", rateCaps("5/1s"))
		l.Check(ctx, "abc", rateCaps("5/1ms"))
		l.Check(ctx, "abc", rateCaps("500/100ms"))
		assert.Equal(t, rates(t, "1/1s", "5/1s", "5000/1s"), l.Check(ctx, "abc", rateCaps("1/1s")).Limits)
		assert.Len(t, l.shard("abc").get("abc").allowances, 3)
	})

	t.Run("burst is bounded by the maximum window", func(t *testing.T) {
		l := newLimiter(t, newClock(), bounds...)
		assert.Equal(t, 5, allowedCount(l, "abc", rateCaps("10/2h"), 10), "10/2h is held as 5/1h")
	})

	t.Run("resolved rates are exempt", func(t *testing.T) {
		opts := append(bounds, WithResolver(ceiling(mustRate(t, "10/1ms"), mustRate(t, "10/2h"))))
		l := newLimiter(t, newClock(), opts...)
		d := l.Check(ctx, "abc", rateCaps("100/1ms"))
		assert.Equal(t, rates(t, "10/1ms", "100000/1s", "10/2h"), d.Limits, "the Token's rate is rescaled, the resolver's are not")
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
			d := l.Check(ctx, strconv.Itoa(i), rateCaps(tc.rate))
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, tc.want), d.Limits)
			assert.Empty(t, d.Warnings)
		})
	}
}

func TestUnrestricted(t *testing.T) {
	l := newLimiter(t, newClock())
	for _, caps := range [][]string{nil, {"prefix:api:.*:all", "other:rate:5/1s"}} {
		d := l.Check(ctx, "abc", caps)
		assert.True(t, d.Allowed)
		assert.Equal(t, None, d.Reason)
		assert.Empty(t, d.Limits)
		assert.True(t, d.Limit.IsZero())
		assert.Empty(t, d.Warnings)
	}

	assert.Zero(t, l.len(), "an Unrestricted Token doesn't touch the allowance")
}

// ceiling is a Resolver that adds rates to every Token's, on top of what
// DefaultResolver makes of them.
func ceiling(rates ...Rate) Resolver {
	return func(ctx context.Context, principal string, provided []Rate) ([]Rate, error) {
		resolved, err := DefaultResolver(ctx, principal, provided)
		return append(resolved, rates...), err
	}
}

func TestDefaultResolver(t *testing.T) {
	five := Rate{Count: 5, Window: time.Second}
	ten := Rate{Count: 10, Window: time.Second}

	tests := []struct {
		name     string
		provided []Rate
		want     []Rate
	}{
		{name: "nothing", provided: nil, want: nil},
		{name: "valid", provided: []Rate{five, ten}, want: []Rate{five, ten}},
		{name: "malformed beside valid", provided: []Rate{five, {Malformed: "x"}, ten}, want: []Rate{five, ten}},
		{name: "only malformed", provided: []Rate{{Malformed: "x"}, {Malformed: "y"}}, want: []Rate{{Malformed: "x"}, {Malformed: "y"}}},
		{name: "zero", provided: []Rate{{}}, want: []Rate{{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.provided)
			got, err := DefaultResolver(ctx, "abc", tc.provided)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, before, tc.provided, "provided is not modified")
		})
	}
}

func TestResolver(t *testing.T) {
	t.Run("sees the token's rates", func(t *testing.T) {
		type ctxKey struct{}
		var gotPrincipal string
		var gotRates []Rate
		var gotValue any
		l := newLimiter(t, newClock(), WithResolver(func(ctx context.Context, principal string, provided []Rate) ([]Rate, error) {
			gotPrincipal, gotRates, gotValue = principal, provided, ctx.Value(ctxKey{})
			return DefaultResolver(ctx, principal, provided)
		}))

		d := l.Check(context.WithValue(ctx, ctxKey{}, "v"), "abc", rateCaps("10/0s", "100/1ms", "5/1s", "abc/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, "abc", gotPrincipal)
		assert.Equal(t, []Rate{{Malformed: "10/0s"}, mustRate(t, "100000/1s"), mustRate(t, "5/1s"), {Malformed: "abc/1s"}}, gotRates, "within the bounds, in Token order, malformed as zero with its text")
		assert.Equal(t, "v", gotValue)
		assert.Equal(t, rates(t, "5/1s", "100000/1s"), d.Limits)
		assert.Len(t, d.Warnings, 2)
	})

	t.Run("ceiling", func(t *testing.T) {
		c := newClock()
		l := newLimiter(t, c, WithResolver(ceiling(mustRate(t, "3/1s"), mustRate(t, "100/1m"))))

		d := l.Check(ctx, "abc", rateCaps("10/1s", "3/1s"))
		assert.Equal(t, rates(t, "3/1s", "10/1s", "100/1m"), d.Limits, "one allowance for a rate however it arrived")
		assert.Equal(t, 2, allowedCount(l, "abc", rateCaps("10/1s"), 10), "the ceiling binds")

		// Resolved rates are remembered like any other.
		assert.Equal(t, rates(t, "3/1s", "10/1s", "100/1m"), l.Check(ctx, "abc", nil).Limits)
		c.Advance(2*time.Second + time.Nanosecond)
		assert.Equal(t, rates(t, "3/1s", "100/1m"), l.Check(ctx, "abc", nil).Limits, "10/1s is forgotten, the ceiling re-added")
	})

	t.Run("override and required", func(t *testing.T) {
		override := rates(t, "5/1m", "2/1s")
		required := errors.New("a rate is required")
		l := newLimiter(t, newClock(), WithResolver(func(_ context.Context, principal string, provided []Rate) ([]Rate, error) {
			switch {
			case principal == "abc":
				return override, nil
			case len(provided) == 0:
				return nil, required
			default:
				return provided, nil
			}
		}))

		d := l.Check(ctx, "abc", rateCaps("100/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "2/1s", "5/1m"), d.Limits, "the Token's rate is replaced")

		d = l.Check(ctx, "abc", nil)
		assert.True(t, d.Allowed, "the resolver vouches for a Token with no rate")
		assert.Equal(t, rates(t, "2/1s", "5/1m"), d.Limits)
		assert.Equal(t, 0, allowedCount(l, "abc", rateCaps("100/1s"), 10), "the override's burst of 2 is spent")

		d = l.Check(ctx, "def", nil)
		assert.False(t, d.Allowed)
		assert.Equal(t, Denied, d.Reason, "others still need a rate")
		assert.ErrorIs(t, d.Err, required)
	})

	t.Run("unrestricted stays unrestricted", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
			if len(provided) == 0 {
				return nil, nil
			}

			return append(provided, Rate{Count: 3, Window: time.Second}), nil
		}))

		d := l.Check(ctx, "abc", nil)
		assert.True(t, d.Allowed)
		assert.Empty(t, d.Limits)
		assert.Zero(t, l.len())

		assert.Equal(t, rates(t, "3/1s", "10/1s"), l.Check(ctx, "abc", rateCaps("10/1s")).Limits)
	})

	t.Run("holding an unrestricted token", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(ceiling(mustRate(t, "3/1s"))))
		d := l.Check(ctx, "abc", nil)
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "3/1s"), d.Limits)
		assert.Equal(t, 2, allowedCount(l, "abc", nil, 10))
	})

	t.Run("nothing resolved for a token with rates", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(func(context.Context, string, []Rate) ([]Rate, error) { return nil, nil }))
		d := l.Check(ctx, "abc", rateCaps("10/1s"))
		assert.True(t, d.Allowed, "the resolver made the Token Unrestricted")
		assert.Empty(t, d.Limits)
		assert.Zero(t, l.len())
	})

	t.Run("zero refuses without denying", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
			return append(provided, Rate{}), nil
		}))
		d := l.Check(ctx, "abc", rateCaps("10/1s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, RateExceeded, d.Reason)
		assert.NoError(t, d.Err)
		assert.Equal(t, []Rate{{}, mustRate(t, "10/1s")}, d.Limits, "zero sorts first")
		assert.True(t, d.Limit.IsZero())
		assert.Zero(t, d.RetryAfter, "a zero limit never recovers")
		assert.Zero(t, l.len(), "nothing is remembered")
	})

	t.Run("denied", func(t *testing.T) {
		blocked := errors.New("blocked")
		l := newLimiter(t, newClock(), WithResolver(func(context.Context, string, []Rate) ([]Rate, error) { return nil, blocked }))
		d := l.Check(ctx, "abc", rateCaps("10/1s", "10/0s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, Denied, d.Reason)
		assert.ErrorIs(t, d.Err, blocked)
		assert.Empty(t, d.Limits)
		assert.Len(t, d.Warnings, 1, "the malformed rate is still warned about")
		assert.Zero(t, l.len(), "nothing is spent or remembered")
	})

	t.Run("invalid rate", func(t *testing.T) {
		l := newLimiter(t, newClock(), WithResolver(ceiling(Rate{Count: 1})))
		d := l.Check(ctx, "abc", rateCaps("10/1s"))
		assert.False(t, d.Allowed)
		assert.Equal(t, Denied, d.Reason)
		assert.ErrorIs(t, d.Err, ErrInvalidRate)
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
		want     []string
	}{
		{name: "plain", prefixes: []string{"prefix:rate:"}, caps: []string{"prefix:rate:5/1s"}, want: []string{"5/1s"}},
		{
			name:     "subexpressions",
			prefixes: []string{`(prefix|other):(rate|limit):`},
			caps:     []string{"other:limit:7/1m"},
			want:     []string{"7/1m"},
		},
		{
			name:     "alternation stays anchored",
			prefixes: []string{`a:|b:`},
			caps:     []string{"zzb:9/1s", "b:4/1s"},
			want:     []string{"4/1s"},
		},
		{
			name:     "several prefixes",
			prefixes: []string{"prefix:rate:", "other:rate:"},
			caps:     []string{"prefix:rate:5/1s", "other:rate:8/1s"},
			want:     []string{"5/1s", "8/1s"},
		},
		{name: "unanchored match is ignored", prefixes: []string{"rate:"}, caps: []string{"other:rate:5/1s"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(WithPrefixes(tc.prefixes...), WithClock(newClock().Now), WithMinWindow(time.Second))
			require.NoError(t, err)

			d := l.Check(ctx, "abc", tc.caps)
			assert.True(t, d.Allowed)
			assert.Empty(t, d.Warnings)
			assert.Equal(t, rates(t, tc.want...), d.Limits)
		})
	}
}

func TestMaxCallersEvictsLeastRecentlyUsed(t *testing.T) {
	l := newLimiter(t, newClock(), WithMaxCallers(2))
	caps := rateCaps("1/1s")

	assert.True(t, l.Check(ctx, "a", caps).Allowed)
	assert.True(t, l.Check(ctx, "b", caps).Allowed)
	assert.False(t, l.Check(ctx, "a", caps).Allowed, "touches a, so b is least recently used")

	assert.True(t, l.Check(ctx, "c", caps).Allowed)
	assert.Equal(t, 2, l.len())

	assert.False(t, l.Check(ctx, "a", caps).Allowed, "a was kept")
	assert.True(t, l.Check(ctx, "b", caps).Allowed, "b was evicted, so its allowance is full again")
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
		l.Check(ctx, strconv.Itoa(i), rateCaps("1/1h"))
	}

	assert.LessOrEqual(t, l.len(), 10_000)
}

func TestExpiredStateDropped(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, WithMaxCallers(10)) // one shard, so access sweeps it

	l.Check(ctx, "abc", rateCaps("5/1s"))
	l.Check(ctx, "def", rateCaps("5/1m"))
	assert.Equal(t, 2, l.len())

	c.Advance(2*time.Second + time.Nanosecond)
	l.sweep()
	assert.Equal(t, 1, l.len(), "abc forgot its rate and its allowance is full")

	c.Advance(2 * time.Minute)
	l.Check(ctx, "ghi", rateCaps("5/1s"))
	assert.Equal(t, 1, l.len(), "def is dropped lazily on access")
}

func TestStartStop(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, WithSweepInterval(time.Millisecond))

	l.Stop() // stopping a stopped limiter does nothing
	l.Start()
	l.Start()
	defer l.Stop()

	l.Check(ctx, "abc", rateCaps("5/1s"))
	c.Advance(time.Hour)
	assert.Eventually(t, func() bool { return l.len() == 0 }, time.Second, time.Millisecond)

	l.Stop()
	l.Stop()
}

func TestConcurrentNeverExceedsBurst(t *testing.T) {
	l := newLimiter(t, newClock())
	caps := rateCaps("50/1s", "200/1m")

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 20 {
				if l.Check(ctx, "abc", caps).Allowed {
					allowed.Add(1)
				}
			}
		})
	}

	wg.Wait()
	assert.Equal(t, int64(50), allowed.Load())
}
