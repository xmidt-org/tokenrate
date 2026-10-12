// SPDX-FileCopyrighttext: 2026 Comcast Cable Communications Management, LLC
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
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const prefix = "prefix:rate:"

var ctx = context.Background()

// bounded is DefaultResolver with other Window Bounds.
func bounded(minWindow, maxWindow time.Duration) Resolver {
	return func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
		return defaultResolve(provided, minWindow, maxWindow), nil
	}
}

// testResolver is DefaultResolver with the minimum window lowered to a
// second, so tests can use short windows; TestDefaultWindowBounds covers the
// real bounds.
var testResolver = bounded(time.Second, 24*time.Hour)

// newLimiter builds a Limiter for tests, using testResolver unless opts
// replace it.  Tests call it inside a synctest bubble, so time stands still
// until they sleep.
func newLimiter(t *testing.T, opts ...Option) *Limiter {
	t.Helper()
	opts = append([]Option{WithPrefixes(prefix), WithResolver(testResolver)}, opts...)
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

// mustRate parses s and drops its text, matching how a Decision lists the
// Caller's allowances.
func mustRate(t *testing.T, s string) Rate {
	t.Helper()
	r, err := ParseRate(s)
	require.NoError(t, err)
	return r.key()
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

// callerOf returns the state kept for principal, without marking it used.
func callerOf(l *Limiter, principal string) *caller {
	v, _ := l.callers.Load(principal)
	return v.(*caller)
}

// dueAt returns when the Limiter is next due a sweep, or the zero time if
// no Caller is tracked.
func dueAt(l *Limiter) time.Time {
	if n := l.due.Load(); n != noneDue {
		return time.Unix(0, n)
	}

	return time.Time{}
}

// sweptAt returns when the Limiter was last swept, or the zero time.
func sweptAt(l *Limiter) time.Time {
	if n := l.swept.Load(); n != 0 {
		return time.Unix(0, n)
	}

	return time.Time{}
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
		{name: "empty prefix", opts: []Option{WithPrefixes(prefix, "")}, wantErr: ErrEmptyPrefix.Error()},
		{name: "nil resolver", opts: []Option{WithPrefixes(prefix), WithResolver(nil)}, wantErr: "resolver"},
		{name: "nil option", opts: []Option{nil, WithPrefixes(prefix)}},
		{
			name: "everything",
			opts: []Option{
				WithPrefixes(prefix, "other:rate:"),
				WithResolver(func(_ context.Context, _ string, r []Rate) ([]Rate, error) { return r, nil }),
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
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
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

		time.Sleep(200 * time.Millisecond)
		assert.True(t, l.Check(ctx, "abc", caps).Allowed)
		assert.False(t, l.Check(ctx, "abc", caps).Allowed)
	})
}

func TestEveryRateInTokenApplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)

		d := l.Check(ctx, "abc", rateCaps("20/1s", "5/1s", "10/1s", "5/1s"))
		assert.True(t, d.Allowed)
		assert.Equal(t, rates(t, "5/1s", "10/1s", "20/1s"), d.Limits, "sorted, without the duplicate")

		// The smallest count bounds the burst, and is the rate that refuses.
		assert.Equal(t, 4, allowedCount(l, "abc", rateCaps("5/1s", "20/1s"), 30))
		d = l.Check(ctx, "abc", rateCaps("5/1s", "20/1s"))
		assert.Equal(t, mustRate(t, "5/1s"), d.Limit)
		assert.Equal(t, 200*time.Millisecond, d.RetryAfter)
	})
}

func TestTieredRates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		caps := rateCaps("3/1s", "5/1m")

		assert.Equal(t, 3, allowedCount(l, "abc", caps, 10), "3/1s bounds the burst")

		// A second later 3/1s has refilled, but 5/1m has only 2 left.
		time.Sleep(time.Second)
		assert.Equal(t, 2, allowedCount(l, "abc", caps, 10))

		d := l.Check(ctx, "abc", caps)
		assert.False(t, d.Allowed)
		assert.Equal(t, mustRate(t, "5/1m"), d.Limit)
		assert.Equal(t, 11*time.Second, d.RetryAfter)

		// A refusal spends nothing: 3/1s still has its allowance when 5/1m
		// recovers.
		time.Sleep(11 * time.Second)
		assert.True(t, l.Check(ctx, "abc", caps).Allowed)
	})
}

func TestCallerHeldToEveryTokensRates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
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
	})
}

func TestRateCutLandsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
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
	})
}

func TestRememberedRateForgotten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)

		l.Check(ctx, "abc", rateCaps("20/1s"))

		// Within twice its window, 20/1s still applies.
		time.Sleep(1500 * time.Millisecond)
		assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)

		time.Sleep(600 * time.Millisecond)
		assert.Equal(t, rates(t, "5/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)
	})
}

func TestRememberedRateRefreshed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)

		for range 5 {
			l.Check(ctx, "abc", rateCaps("20/1s"))
			time.Sleep(1500 * time.Millisecond)
		}

		assert.Equal(t, rates(t, "5/1s", "20/1s"), l.Check(ctx, "abc", rateCaps("5/1s")).Limits)
	})
}

func TestForgottenRateKeepsItsSpentAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)

		// 2/1s is presented once, then spent by another Token's requests just
		// before it is forgotten.
		l.Check(ctx, "abc", rateCaps("2/1s"))
		time.Sleep(1900 * time.Millisecond)
		l.Check(ctx, "abc", rateCaps("5/1s"))

		time.Sleep(200 * time.Millisecond)
		d := l.Check(ctx, "abc", rateCaps("5/1s"))
		assert.Equal(t, rates(t, "5/1s"), d.Limits, "2/1s is forgotten")
		assert.Len(t, callerOf(l, "abc").allowances, 2, "but its allowance is not full, so it is kept")

		// Presenting it again finds that allowance rather than a fresh burst.
		d = l.Check(ctx, "abc", rateCaps("2/1s"))
		assert.True(t, d.Allowed)
		assert.False(t, l.Check(ctx, "abc", rateCaps("2/1s")).Allowed)
	})
}

func TestMalformed(t *testing.T) {
	malformed := `malformed; kind=rate; cap="prefix:rate:10/0s"`

	t.Run("only malformed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t)
			for range 3 {
				d := l.Check(ctx, "abc", rateCaps("10/0s"))
				assert.False(t, d.Allowed)
				assert.Equal(t, RateExceeded, d.Reason)
				assert.Equal(t, []Rate{{text: "10/0s"}}, d.Limits)
				assert.True(t, d.Limit.IsZero())
				assert.Zero(t, d.RetryAfter)
				require.Len(t, d.Warnings, 1)
				assert.Equal(t, malformed, d.Warnings[0].String())
			}

			assert.Zero(t, l.len(), "no state is kept for a zero limit")
		})
	})

	t.Run("alongside a valid rate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t)
			d := l.Check(ctx, "abc", rateCaps("10/0s", "5/1s"))
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, "5/1s"), d.Limits)
			require.Len(t, d.Warnings, 1)
			assert.Equal(t, malformed, d.Warnings[0].String())
		})
	})

	t.Run("after a remembered rate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t)
			l.Check(ctx, "abc", rateCaps("5/1s"))
			d := l.Check(ctx, "abc", rateCaps("10/0s"))
			assert.False(t, d.Allowed, "a broken Token is held to zero, whatever the Caller's other Tokens say")
			assert.Equal(t, RateExceeded, d.Reason)
			assert.Equal(t, []Rate{{text: "10/0s"}}, d.Limits)
			assert.Len(t, d.Warnings, 1)
			assert.Equal(t, 1, l.len(), "and does not disturb what is remembered")
		})
	})

	t.Run("several malformed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t)
			d := l.Check(ctx, "abc", rateCaps("10/0s", "abc/1s", "10/0s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, []Rate{{text: "10/0s"}, {text: "abc/1s"}}, d.Limits, "each text once, in order")
			assert.Len(t, d.Warnings, 3, "but every capability is warned about")
		})
	})

	t.Run("with a resolver that drops the zero", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(func(ctx context.Context, p string, provided []Rate) ([]Rate, error) {
				return testResolver(ctx, p, slices.DeleteFunc(provided, Rate.IsZero))
			}))
			d := l.Check(ctx, "abc", rateCaps("10/0s"))
			assert.True(t, d.Allowed, "the resolver decided the Token is Unrestricted")
			assert.Empty(t, d.Limits)
			assert.Len(t, d.Warnings, 1, "it is still warned about")
		})
	})
}

func TestWindowBounds(t *testing.T) {
	bounds := []Option{WithResolver(bounded(time.Second, time.Hour))}

	t.Run("shorter than the minimum", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			d := l.Check(ctx, "abc", rateCaps("100/1ms"))
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, "100000/1s"), d.Limits, "the same calls per second, per minimum window")
			assert.Empty(t, d.Warnings)
		})
	})

	t.Run("longer than the maximum", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			d := l.Check(ctx, "abc", rateCaps("100/2h"))
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, "50/1h"), d.Limits, "the same calls per second, per maximum window")
			assert.Empty(t, d.Warnings)
		})
	})

	t.Run("at the bounds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			assert.Equal(t, rates(t, "5/1s", "5/1h"), l.Check(ctx, "abc", rateCaps("5/1h", "5/1s")).Limits)
		})
	})

	t.Run("rescaled rates apply as rescaled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			d := l.Check(ctx, "abc", rateCaps("1/1ms", "500/1s", "7200/2h"))
			assert.Equal(t, rates(t, "500/1s", "1000/1s", "3600/1h"), d.Limits)
		})
	})

	t.Run("rescaled rates are remembered as one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			l.Check(ctx, "abc", rateCaps("5/1s"))
			l.Check(ctx, "abc", rateCaps("5/1ms"))
			l.Check(ctx, "abc", rateCaps("500/100ms"))
			assert.Equal(t, rates(t, "1/1s", "5/1s", "5000/1s"), l.Check(ctx, "abc", rateCaps("1/1s")).Limits)
			assert.Len(t, callerOf(l, "abc").allowances, 3)
		})
	})

	t.Run("burst is bounded by the maximum window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, bounds...)
			assert.Equal(t, 5, allowedCount(l, "abc", rateCaps("10/2h"), 10), "10/2h is held as 5/1h")
		})
	})

	t.Run("rates the resolver adds are not bounded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			resolve := bounded(time.Second, time.Hour)
			l := newLimiter(t, WithResolver(func(ctx context.Context, p string, provided []Rate) ([]Rate, error) {
				resolved, err := resolve(ctx, p, provided)
				return append(resolved, mustRate(t, "10/1ms"), mustRate(t, "10/2h")), err
			}))
			d := l.Check(ctx, "abc", rateCaps("100/1ms"))
			assert.Equal(t, rates(t, "10/1ms", "100000/1s", "10/2h"), d.Limits, "the Token's rate is rescaled, the resolver's are not")
		})
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
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		for _, caps := range [][]string{nil, {"prefix:api:.*:all", "other:rate:5/1s"}} {
			d := l.Check(ctx, "abc", caps)
			assert.True(t, d.Allowed)
			assert.Equal(t, None, d.Reason)
			assert.Empty(t, d.Limits)
			assert.True(t, d.Limit.IsZero())
			assert.Empty(t, d.Warnings)
		}

		assert.Zero(t, l.len(), "an Unrestricted Token doesn't touch the allowance")
	})
}

// ceiling is a Resolver that adds rates to every Token's, on top of what
// testResolver makes of them.
func ceiling(rates ...Rate) Resolver {
	return func(ctx context.Context, principal string, provided []Rate) ([]Rate, error) {
		resolved, err := testResolver(ctx, principal, provided)
		return append(resolved, rates...), err
	}
}

func TestDefaultResolver(t *testing.T) {
	five := Rate{Count: 5, Window: time.Minute}
	ten := Rate{Count: 10, Window: time.Hour}

	tests := []struct {
		name     string
		provided []Rate
		want     []Rate
	}{
		{name: "nothing", provided: nil, want: nil},
		{name: "valid", provided: []Rate{five, ten}, want: []Rate{five, ten}},
		{name: "malformed beside valid", provided: []Rate{five, {text: "x"}, ten}, want: []Rate{five, ten}},
		{name: "only malformed", provided: []Rate{{text: "x"}, {text: "y"}}, want: []Rate{{text: "x"}, {text: "y"}}},
		{name: "zero", provided: []Rate{{}}, want: []Rate{{}}},
		{name: "outside the bounds", provided: []Rate{{Count: 100, Window: time.Second, text: "100/1s"}, five, {Count: 100, Window: 48 * time.Hour, text: "100/48h"}}, want: []Rate{{Count: 6000, Window: time.Minute}, five, {Count: 50, Window: 24 * time.Hour}}},
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
		synctest.Test(t, func(t *testing.T) {
			type ctxKey struct{}
			var gotPrincipal string
			var gotRates []Rate
			var gotValue any
			l := newLimiter(t, WithResolver(func(ctx context.Context, principal string, provided []Rate) ([]Rate, error) {
				gotPrincipal, gotRates, gotValue = principal, provided, ctx.Value(ctxKey{})
				return testResolver(ctx, principal, provided)
			}))

			d := l.Check(context.WithValue(ctx, ctxKey{}, "v"), "abc", rateCaps("10/0s", "100/1ms", "5/1s", "abc/1s"))
			assert.True(t, d.Allowed)
			assert.Equal(t, "abc", gotPrincipal)
			fast, five := mustRate(t, "100/1ms"), mustRate(t, "5/1s")
			fast.text, five.text = "100/1ms", "5/1s"
			assert.Equal(t, []Rate{{text: "10/0s"}, fast, five, {text: "abc/1s"}}, gotRates, "in Token order as parsed, with the text as written and malformed as zero")
			assert.Equal(t, "v", gotValue)
			assert.Equal(t, rates(t, "5/1s", "100000/1s"), d.Limits)
			assert.Len(t, d.Warnings, 2)
		})
	})

	t.Run("ceiling", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(ceiling(mustRate(t, "3/1s"), mustRate(t, "100/1m"))))

			d := l.Check(ctx, "abc", rateCaps("10/1s", "3/1s"))
			assert.Equal(t, rates(t, "3/1s", "10/1s", "100/1m"), d.Limits, "one allowance for a rate however it arrived")
			assert.Equal(t, 2, allowedCount(l, "abc", rateCaps("10/1s"), 10), "the ceiling binds")

			// Resolved rates are remembered like any other.
			assert.Equal(t, rates(t, "3/1s", "10/1s", "100/1m"), l.Check(ctx, "abc", nil).Limits)
			time.Sleep(2*time.Second + time.Nanosecond)
			assert.Equal(t, rates(t, "3/1s", "100/1m"), l.Check(ctx, "abc", nil).Limits, "10/1s is forgotten, the ceiling re-added")
		})
	})

	t.Run("override and required", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			override := rates(t, "5/1m", "2/1s")
			required := errors.New("a rate is required")
			l := newLimiter(t, WithResolver(func(_ context.Context, principal string, provided []Rate) ([]Rate, error) {
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
	})

	t.Run("unrestricted stays unrestricted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
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
	})

	t.Run("holding an unrestricted token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(ceiling(mustRate(t, "3/1s"))))
			d := l.Check(ctx, "abc", nil)
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, "3/1s"), d.Limits)
			assert.Equal(t, 2, allowedCount(l, "abc", nil, 10))
		})
	})

	t.Run("nothing resolved for a token with rates", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(func(context.Context, string, []Rate) ([]Rate, error) { return nil, nil }))
			d := l.Check(ctx, "abc", rateCaps("10/1s"))
			assert.True(t, d.Allowed, "the resolver made the Token Unrestricted")
			assert.Empty(t, d.Limits)
			assert.Zero(t, l.len())
		})
	})

	t.Run("a legacy format made sense of", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
				for i, r := range provided {
					if r.String() == "five per second" {
						provided[i].Count, provided[i].Window = 5, time.Second // still reads back as written
					}
				}

				return provided, nil
			}))

			d := l.Check(ctx, "abc", rateCaps("five per second", "5/1s"))
			assert.True(t, d.Allowed)
			assert.Equal(t, rates(t, "5/1s"), d.Limits, "both spellings are one rate")
			assert.Len(t, d.Warnings, 1, "it is still warned about")
			assert.Equal(t, 4, allowedCount(l, "abc", rateCaps("5/1s"), 10), "and share one allowance")
		})
	})

	t.Run("spellings share an allowance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t)
			assert.Equal(t, rates(t, "4/1h30m"), l.Check(ctx, "abc", rateCaps("4/90m")).Limits)
			assert.Equal(t, rates(t, "4/1h30m"), l.Check(ctx, "abc", rateCaps("4/1h30m")).Limits)
			assert.Equal(t, 2, allowedCount(l, "abc", rateCaps("4/5400s"), 10))
		})
	})

	t.Run("zero refuses without denying", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(func(_ context.Context, _ string, provided []Rate) ([]Rate, error) {
				return append(provided, Rate{}), nil
			}))
			d := l.Check(ctx, "abc", rateCaps("10/1s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, RateExceeded, d.Reason)
			assert.NoError(t, d.Err)
			assert.Equal(t, []Rate{{}, {Count: 10, Window: time.Second, text: "10/1s"}}, d.Limits, "zero sorts first; the Token's rate as written")
			assert.True(t, d.Limit.IsZero())
			assert.Zero(t, d.RetryAfter, "a zero limit never recovers")
			assert.Zero(t, l.len(), "nothing is remembered")
		})
	})

	t.Run("denied", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			blocked := errors.New("blocked")
			l := newLimiter(t, WithResolver(func(context.Context, string, []Rate) ([]Rate, error) { return nil, blocked }))
			d := l.Check(ctx, "abc", rateCaps("10/1s", "10/0s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, Denied, d.Reason)
			assert.ErrorIs(t, d.Err, blocked)
			assert.Empty(t, d.Limits)
			assert.Len(t, d.Warnings, 1, "the malformed rate is still warned about")
			assert.Zero(t, l.len(), "nothing is spent or remembered")
		})
	})

	t.Run("invalid rate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newLimiter(t, WithResolver(ceiling(Rate{Count: 1})))
			d := l.Check(ctx, "abc", rateCaps("10/1s"))
			assert.False(t, d.Allowed)
			assert.Equal(t, Denied, d.Reason)
			assert.ErrorIs(t, d.Err, ErrInvalidRate)
		})
	})
}

func TestIndependentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		caps := rateCaps("3/1s")

		assert.Equal(t, 3, allowedCount(l, "abc", caps, 10))
		assert.Equal(t, 3, allowedCount(l, "def", caps, 10))
	})
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
			name:     "not a pattern",
			prefixes: []string{"a.b:", "(c|d):"},
			caps:     []string{"aXb:9/1s", "c:9/1s", "a.b:4/1s", "(c|d):7/1m"},
			want:     []string{"4/1s", "7/1m"},
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
			l, err := New(WithPrefixes(tc.prefixes...), WithResolver(testResolver))
			require.NoError(t, err)

			d := l.Check(ctx, "abc", tc.caps)
			assert.True(t, d.Allowed)
			assert.Empty(t, d.Warnings)
			assert.Equal(t, rates(t, tc.want...), d.Limits)
		})
	}
}

func TestExpiredStateDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)

		l.Check(ctx, "abc", rateCaps("5/1s"))
		l.Check(ctx, "def", rateCaps("5/1m"))
		assert.Equal(t, 2, l.len())

		time.Sleep(2*time.Second + time.Nanosecond)
		l.Check(ctx, "ghi", rateCaps("5/1m"))
		synctest.Wait()
		assert.Equal(t, 2, l.len(), "abc expired, so the next check had it swept")

		// Sweeps are at least minSweepGap apart.
		swept := sweptAt(l)
		l.Check(ctx, "jkl", rateCaps("5/1s"))
		time.Sleep(3 * time.Second)
		l.Check(ctx, "def", rateCaps("5/1m"))
		synctest.Wait()
		assert.Equal(t, swept, sweptAt(l), "jkl expired, but the Limiter was swept too recently")
		assert.Equal(t, 3, l.len())

		time.Sleep(minSweepGap)
		l.Check(ctx, "def", rateCaps("5/1m"))
		synctest.Wait()
		assert.Equal(t, 2, l.len(), "jkl is swept once the gap has passed")
	})
}

func TestSweepOnlyWhenDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		assert.True(t, dueAt(l).IsZero(), "nothing tracked, nothing due")

		l.Check(ctx, "abc", rateCaps("5/1h"))
		due := dueAt(l)
		assert.Equal(t, time.Now().Add(2*time.Hour), due, "abc is forgotten after twice its window")

		for range 10 {
			time.Sleep(10 * time.Minute)
			l.Check(ctx, "abc", rateCaps("5/1h"))
		}

		synctest.Wait()
		assert.True(t, sweptAt(l).IsZero(), "using a Caller never sweeps")
		assert.Equal(t, due, dueAt(l), "nor moves the next sweep")

		l.Check(ctx, "def", rateCaps("5/1m"))
		assert.Equal(t, time.Now().Add(2*time.Minute), dueAt(l), "a new Caller brings it forward")

		// Once def is due, the next check has the Limiter swept: def has
		// expired, and abc, just presented again, is due in twice its window.
		time.Sleep(2*time.Minute + time.Nanosecond)
		l.Check(ctx, "abc", rateCaps("5/1h"))
		synctest.Wait()
		assert.Equal(t, 1, l.len())
		assert.Equal(t, time.Now(), sweptAt(l))
		assert.Equal(t, time.Now().Add(2*time.Hour), dueAt(l))
	})
}

func TestSweepMany(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 10_000
		l := newLimiter(t)
		for i := range n {
			window := "1s" // expires
			if i%3 == 0 {
				window = "1h" // stays
			}

			l.Check(ctx, strconv.Itoa(i), rateCaps("5/"+window))
		}

		time.Sleep(2*time.Second + time.Nanosecond)
		l.Check(ctx, "0", rateCaps("5/1h"))
		synctest.Wait()
		assert.Equal(t, (n+2)/3, l.len(), "only the 1h Callers are left")
	})
}

func TestOneTidyAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		l.Check(ctx, "abc", rateCaps("5/1s"))
		time.Sleep(2*time.Second + time.Nanosecond)

		l.tidying.Store(true) // as if a tidy were running
		l.Check(ctx, "def", rateCaps("5/1m"))
		synctest.Wait()
		assert.Equal(t, 2, l.len(), "no second tidy is started")

		l.tidying.Store(false)
		l.Check(ctx, "def", rateCaps("5/1m"))
		synctest.Wait()
		assert.Equal(t, 1, l.len(), "the next check starts one")
		assert.False(t, l.tidying.Load(), "and it exits when done")
	})
}

func TestConcurrentChecksWhileSweeping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				for i := range 500 {
					l.Check(ctx, strconv.Itoa(g*1000+i%300), rateCaps("5/1s"))
					if i%50 == 0 {
						time.Sleep(time.Minute) // let state expire and sweeps come due
					}
				}
			})
		}

		wg.Wait()
		synctest.Wait()
		assert.LessOrEqual(t, l.len(), 8*300)
	})
}

func TestForgottenWhileLookedUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
		caps := rateCaps("1/1h")
		assert.True(t, l.Check(ctx, "abc", caps).Allowed)

		// As if the sweep forgot abc between a check's lookup and its lock:
		// the check holds the old state, which is no longer in callers.
		old := callerOf(l, "abc")
		old.mu.Lock()
		l.forget(old)
		old.mu.Unlock()

		assert.True(t, l.Check(ctx, "abc", caps).Allowed, "the check gets fresh state, not the forgotten one")
		assert.NotSame(t, old, callerOf(l, "abc"))
	})
}

func TestCallerExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := &caller{allowances: map[Rate]*allowance{
		{Count: 5, Window: time.Minute}: {seen: now, tat: now.Add(time.Hour)},
		{Count: 5, Window: time.Hour}:   {seen: now},
	}}
	assert.Equal(t, now.Add(2*time.Hour), c.expiry(), "the latest of each rate being forgotten and its allowance refilling")

	c.allowances[Rate{Count: 1, Window: time.Minute}] = &allowance{seen: now, tat: now.Add(3 * time.Hour)}
	assert.Equal(t, now.Add(3*time.Hour), c.expiry())

	assert.True(t, (&caller{}).expiry().IsZero(), "no allowances")
}

func TestConcurrentNeverExceedsBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t)
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
	})
}
