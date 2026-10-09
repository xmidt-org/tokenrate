// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package gcra

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSpendBurstThenRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	var tat time.Time

	for i := range 5 {
		var ok bool
		tat, ok, _ = Spend(tat, now, 5, time.Second)
		assert.True(t, ok, "call %d", i+1)
	}

	next, ok, retryAfter := Spend(tat, now, 5, time.Second)
	assert.False(t, ok)
	assert.Equal(t, 200*time.Millisecond, retryAfter)
	assert.Equal(t, tat.Add(200*time.Millisecond), next, "next is what spending anyway would store")

	// Refusing means keeping tat, so 200ms later one call fits.
	now = now.Add(200 * time.Millisecond)
	tat, ok, _ = Spend(tat, now, 5, time.Second)
	assert.True(t, ok)

	_, ok, _ = Spend(tat, now, 5, time.Second)
	assert.False(t, ok)
}

func TestSpendPastTAT(t *testing.T) {
	now := time.Unix(1000, 0)
	tat := now.Add(-time.Hour)

	next, ok, _ := Spend(tat, now, 10, time.Second)
	assert.True(t, ok)
	assert.Equal(t, now.Add(100*time.Millisecond), next, "an old TAT counts as now")
}

func TestInterval(t *testing.T) {
	assert.Equal(t, 200*time.Millisecond, Interval(5, time.Second))
	assert.Equal(t, time.Duration(1), Interval(1_000_000_000_000, time.Second))
}
