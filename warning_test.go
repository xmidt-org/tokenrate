// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"testing"

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
	assert.Equal(t, "rate-exceeded", RateExceeded.String())
	assert.Equal(t, "denied", Denied.String())
	assert.Equal(t, "unknown", Reason(99).String())
}
