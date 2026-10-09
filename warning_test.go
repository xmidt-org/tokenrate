// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWarningString(t *testing.T) {
	tests := []struct {
		name string
		w    Warning
		want string
	}{
		{
			name: "malformed",
			w:    malformedWarning("prefix:rate:10/0s"),
			want: `malformed; kind=rate; cap="prefix:rate:10/0s"`,
		},
		{
			name: "no rate capability",
			w:    wouldRejectWarning(NoRateCapability, Rate{}),
			want: `would-reject; kind=rate; reason=no-rate-capability`,
		},
		{
			name: "rate exceeded",
			w:    wouldRejectWarning(RateExceeded, Rate{Count: 50, Window: time.Second}),
			want: `would-reject; kind=rate; reason=rate-exceeded; limit="50/1s"`,
		},
		{
			name: "rate exceeded at zero",
			w:    wouldRejectWarning(RateExceeded, Rate{}),
			want: `would-reject; kind=rate; reason=rate-exceeded; limit=0`,
		},
		{
			name: "escaping",
			w:    Warning{Reason: "r", Params: [][2]string{{"a", `x"y\z`}, {"b", ""}, {"c", "tok!#$%&'*+-.^_`|~9"}}},
			want: `r; a="x\"y\\z"; b=""; c=tok!#$%&'*+-.^_` + "`" + `|~9`,
		},
		{
			name: "no params",
			w:    Warning{Reason: "r"},
			want: "r",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.w.String())
		})
	}
}

func TestReasonString(t *testing.T) {
	assert.Equal(t, "none", None.String())
	assert.Equal(t, "no-rate-capability", NoRateCapability.String())
	assert.Equal(t, "rate-exceeded", RateExceeded.String())
	assert.Equal(t, "resolver-failed", ResolverFailed.String())
	assert.Equal(t, "unknown", Reason(99).String())
}
