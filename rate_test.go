// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"math"
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
			_, err := ParseRate(in)
			assert.ErrorIs(t, err, ErrMalformedRate)
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

func TestRateLess(t *testing.T) {
	r := func(s string) Rate {
		rate, err := ParseRate(s)
		require.NoError(t, err)
		return rate
	}

	tests := []struct {
		name string
		a, b Rate
		want bool
	}{
		{name: "slower", a: r("5/1s"), b: r("20/1s"), want: true},
		{name: "faster", a: r("20/1s"), b: r("5/1s"), want: false},
		{name: "equal", a: r("5/1s"), b: r("5/1s"), want: false},
		{name: "tie, smaller burst", a: r("60/1m"), b: r("3600/1h"), want: true},
		{name: "tie, larger burst", a: r("3600/1h"), b: r("60/1m"), want: false},
		{name: "different windows", a: r("100/24h"), b: r("1/1m"), want: true},
		{name: "zero is least", a: Rate{}, b: r("1/24h"), want: true},
		{name: "nothing is less than zero", a: r("1/24h"), b: Rate{}, want: false},
		{name: "huge values", a: r("9223372036854775807/1s"), b: r("9223372036854775807/1ns"), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.a.Less(tc.b))
		})
	}
}

// TestInvalidRates covers Rates built directly rather than parsed, where one
// field may be bad on its own.  Each is treated like the zero Rate.
func TestInvalidRates(t *testing.T) {
	valid := Rate{Count: 1, Window: time.Second}

	tests := []struct {
		name string
		rate Rate
	}{
		{name: "zero", rate: Rate{}},
		{name: "zero count", rate: Rate{Count: 0, Window: time.Second}},
		{name: "negative count", rate: Rate{Count: -1, Window: time.Second}},
		{name: "zero window", rate: Rate{Count: 10, Window: 0}},
		{name: "negative window", rate: Rate{Count: 10, Window: -time.Second}},
		{name: "both negative", rate: Rate{Count: -10, Window: -time.Second}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, tc.rate.valid())
			assert.Equal(t, "0", tc.rate.String())
			assert.True(t, tc.rate.Less(valid), "invalid is less than valid")
			assert.False(t, valid.Less(tc.rate), "valid is not less than invalid")
			assert.False(t, tc.rate.Less(tc.rate), "invalid is not less than itself")
			assert.False(t, tc.rate.Less(Rate{}), "invalid is not less than zero")

			l, err := New(WithPrefixes("p:"), WithOverride("abc", tc.rate))
			assert.ErrorContains(t, err, `override for "abc"`)
			assert.Nil(t, l)
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
