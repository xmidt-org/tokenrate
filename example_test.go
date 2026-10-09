// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/xmidt-org/tokenrate"
)

// A service's authentication middleware normally leaves the Token in the
// request's context.  This stand-in reads it from headers instead.
func principalAndCapabilities(r *http.Request) (string, []string) {
	return r.Header.Get("Principal"), strings.Split(r.Header.Get("Capabilities"), ",")
}

// A fixed clock keeps the example's output stable.
var now = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

func ExampleMiddleware() {
	// Every Token must carry a rate; the Resolver denies the ones that don't.
	requireRate := func(_ context.Context, _ string, provided []tokenrate.Rate) ([]tokenrate.Rate, error) {
		if len(provided) == 0 {
			return nil, errors.New("no rate capability")
		}

		return provided, nil
	}

	limiter, err := tokenrate.New(
		tokenrate.WithPrefixes("prefix:rate:"),
		tokenrate.WithResolver(requireRate),
		tokenrate.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		panic(err)
	}

	limiter.Start() // optional: periodically sweep idle state
	defer limiter.Stop()

	handler := tokenrate.Middleware{
		Limiter: limiter,
		Extract: principalAndCapabilities,
	}.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// The Token allows 2 calls per minute, so the third is refused.  Its
	// second capability is malformed, which every response warns about.
	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Principal", "abc")
		r.Header.Set("Capabilities", "prefix:rate:2/1m,prefix:rate:5/0s")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		fmt.Println(w.Code, w.Header().Get("Retry-After"), w.Header().Get(tokenrate.DefaultWarningHeader))
	}

	// A Token with no rate is denied by the Resolver.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Principal", "def")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	fmt.Println(w.Code)

	// Output:
	// 204  malformed; kind=rate; cap="prefix:rate:5/0s"
	// 204  malformed; kind=rate; cap="prefix:rate:5/0s"
	// 429 30 malformed; kind=rate; cap="prefix:rate:5/0s"
	// 403
}

func ExampleLimiter_Check() {
	limiter, err := tokenrate.New(tokenrate.WithPrefixes("prefix:rate:"))
	if err != nil {
		panic(err)
	}

	// Without HTTP, apply the Decision yourself.  Every rate in the Token
	// applies: this Caller may burst 1000, and make at most 100000 a day.
	d := limiter.Check(context.Background(), "abc", []string{"prefix:rate:1000/1m", "prefix:rate:100000/24h"})
	fmt.Println(d.Allowed, d.Limits, d.Reason)

	// A Token with no rate is Unrestricted, unless a Resolver says otherwise.
	d = limiter.Check(context.Background(), "def", nil)
	fmt.Println(d.Allowed, d.Limits, d.Reason)

	// Output:
	// true [1000/1m 100000/24h] none
	// true [] none
}

func ExampleWithResolver() {
	ceiling := tokenrate.Rate{Count: 500, Window: time.Minute}
	overrides := map[string][]tokenrate.Rate{
		"partner": {{Count: 10000, Window: time.Minute}},
	}

	// The Resolver has the final say over the rates a Token counts as
	// carrying, so the deployment's special cases live in one place.
	resolve := func(_ context.Context, principal string, provided []tokenrate.Rate) ([]tokenrate.Rate, error) {
		if override, ok := overrides[principal]; ok {
			return override, nil // replaces whatever the Token says
		}

		if len(provided) == 0 {
			return nil, nil // a Token with no rate stays Unrestricted
		}

		return append(provided, ceiling), nil // nobody else exceeds the ceiling
	}

	limiter, err := tokenrate.New(
		tokenrate.WithPrefixes("prefix:rate:"),
		tokenrate.WithResolver(resolve),
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	fmt.Println(limiter.Check(ctx, "abc", []string{"prefix:rate:1000/1m"}).Limits)
	fmt.Println(limiter.Check(ctx, "partner", []string{"prefix:rate:1000/1m"}).Limits)
	fmt.Println(limiter.Check(ctx, "def", nil).Limits)

	// Output:
	// [500/1m 1000/1m]
	// [10000/1m]
	// []
}
