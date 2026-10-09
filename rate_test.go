// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in   string
		want Rate
	}{
		{in: "50/1s", want: Rate{Count: 50, Window: time.Second}},
		{in: "600/1m", want: Rate{Count: 600, Window: time.Minute}},
		{in: "100/24h", want: Rate{Count: 100, Window: 24 * time.Hour}},
		{in: "4/1h30m", want: Rate{Count: 4, Window: 90 * time.Minute}},
		{in: "1/500ms", want: Rate{Count: 1, Window: 500 * time.Millisecond}},
		{in: "7/720h", want: Rate{Count: 7, Window: 30 * 24 * time.Hour}},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRate(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseRateMalformed(t *testing.T) {
	tests := []string{
		"",
		"10",
		"/1s",
		"10/",
		"0/1s",
		"10/0s",
		"-1/1s",
		"+1/1s",
		" 1/1s",
		"1.5/1s",
		"abc/1s",
		"10/1x",
		"10/-1s",
		"10/1d",
		"10/1d12h",
		"10/1.5d",
		"10/0d",
		"10/d",
		"10/1s/2",
		"99999999999999999999/1s",
		"1/999999999d",
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			r, err := ParseRate(in)
			assert.ErrorIs(t, err, ErrMalformedRate)
			assert.Equal(t, Rate{Malformed: in}, r, "the text is kept")
			assert.True(t, r.IsZero())
			if in == "" {
				assert.Equal(t, "0", r.String(), "nothing to show")
			} else {
				assert.Equal(t, in, r.String())
			}
		})
	}
}

func TestRateString(t *testing.T) {
	tests := []struct {
		rate Rate
		want string
	}{
		{rate: Rate{}, want: "0"},
		{rate: Rate{Count: 50, Window: time.Second}, want: "50/1s"},
		{rate: Rate{Count: 600, Window: time.Minute}, want: "600/1m"},
		{rate: Rate{Count: 2, Window: time.Hour}, want: "2/1h"},
		{rate: Rate{Count: 4, Window: 90 * time.Minute}, want: "4/1h30m"},
		{rate: Rate{Count: 100, Window: 24 * time.Hour}, want: "100/24h"},
		{rate: Rate{Count: 3, Window: 1500 * time.Millisecond}, want: "3/1.5s"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.rate.String())
			if !tc.rate.IsZero() {
				parsed, err := ParseRate(tc.want)
				require.NoError(t, err)
				assert.Equal(t, tc.rate, parsed)
			}
		})
	}
}

func TestCompareRates(t *testing.T) {
	r := func(s string) Rate {
		rate, err := ParseRate(s)
		require.NoError(t, err)
		return rate
	}

	in := []Rate{r("100/24h"), {Malformed: "b"}, r("20/1s"), r("5/1m"), {}, r("5/1s"), {Malformed: "a"}, r("5/1s")}
	want := []Rate{{}, {Malformed: "a"}, {Malformed: "b"}, r("5/1s"), r("20/1s"), r("5/1m"), r("100/24h")}

	got := addRates(nil, in...)
	slices.SortFunc(got, compareRates)
	assert.Equal(t, want, got, "zero first, by text, then shortest window, then smallest count, no duplicates")
	assert.Equal(t, want, addRates(got, r("5/1m")), "adding a held rate changes nothing")
}

// TestInvalidRates covers Rates built directly rather than parsed, where one
// field may be bad on its own.  Each is treated like the zero Rate.
func TestInvalidRates(t *testing.T) {
	valid := Rate{Count: 1, Window: time.Second}

	tests := []struct {
		name string
		rate Rate
		str  string
	}{
		{name: "zero", rate: Rate{}, str: "0"},
		{name: "malformed", rate: Rate{Malformed: "ten/1s"}, str: "ten/1s"},
		{name: "zero count", rate: Rate{Count: 0, Window: time.Second}, str: "0"},
		{name: "negative count", rate: Rate{Count: -1, Window: time.Second}, str: "0"},
		{name: "zero window", rate: Rate{Count: 10, Window: 0}, str: "0"},
		{name: "negative window", rate: Rate{Count: 10, Window: -time.Second}, str: "0"},
		{name: "both negative", rate: Rate{Count: -10, Window: -time.Second}, str: "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, tc.rate.valid())
			assert.Equal(t, tc.str, tc.rate.String())

			// A Resolver may return a zero Rate, which refuses, but no
			// other invalid one.
			l, err := New(WithPrefixes("p:"), WithResolver(func(context.Context, string, []Rate) ([]Rate, error) {
				return []Rate{valid, tc.rate}, nil
			}))
			require.NoError(t, err)
			d := l.Check(context.Background(), "abc", []string{"p:1/1m"})
			if tc.rate.IsZero() {
				assert.Equal(t, RateExceeded, d.Reason)
				assert.NoError(t, d.Err)
			} else {
				assert.Equal(t, Denied, d.Reason)
				assert.ErrorIs(t, d.Err, ErrInvalidRate)
			}
		})
	}
}

func TestRateRescale(t *testing.T) {
	tests := []struct {
		name   string
		rate   Rate
		window time.Duration
		want   Rate
	}{
		{name: "exact, up", rate: Rate{Count: 100, Window: time.Second}, window: time.Minute, want: Rate{Count: 6000, Window: time.Minute}},
		{name: "exact, down", rate: Rate{Count: 100, Window: 2 * time.Hour}, window: time.Hour, want: Rate{Count: 50, Window: time.Hour}},
		{name: "same window", rate: Rate{Count: 7, Window: time.Minute}, window: time.Minute, want: Rate{Count: 7, Window: time.Minute}},
		{name: "rounds down", rate: Rate{Count: 1, Window: 7 * time.Second}, window: time.Minute, want: Rate{Count: 9, Window: time.Minute}},
		{name: "rounds up", rate: Rate{Count: 100, Window: 59 * time.Second}, window: time.Minute, want: Rate{Count: 102, Window: time.Minute}},
		{name: "half rounds up", rate: Rate{Count: 3, Window: 48 * time.Hour}, window: 24 * time.Hour, want: Rate{Count: 2, Window: 24 * time.Hour}},
		{name: "never below one", rate: Rate{Count: 1, Window: 72 * time.Hour}, window: 24 * time.Hour, want: Rate{Count: 1, Window: 24 * time.Hour}},
		{name: "product overflows", rate: Rate{Count: math.MaxInt, Window: time.Second}, window: time.Minute, want: Rate{Count: math.MaxInt, Window: time.Minute}},
		{name: "quotient overflows", rate: Rate{Count: math.MaxInt, Window: 2 * time.Second}, window: 3 * time.Second, want: Rate{Count: math.MaxInt, Window: 3 * time.Second}},
		{name: "huge count fits", rate: Rate{Count: math.MaxInt, Window: time.Minute}, window: time.Second, want: Rate{Count: math.MaxInt / 60, Window: time.Second}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.rate.rescale(tc.window))
		})
	}
}
