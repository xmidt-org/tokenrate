// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate_test

import (
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
	limiter, err := tokenrate.New(
		tokenrate.WithPrefixes("prefix:rate:"),
		tokenrate.WithRequired(),
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

	// A Token with no rate is refused, because the Limiter is Required.
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

	// Without HTTP, apply the Decision yourself.
	d := limiter.Check("abc", []string{"prefix:rate:100/1m"})
	fmt.Println(d.Allowed, d.Limit, d.Reason)

	// A Token with no rate is Unrestricted under the default mode.
	d = limiter.Check("def", nil)
	fmt.Println(d.Allowed, d.Limit, d.Reason)

	// Output:
	// true 100/1m none
	// true 0 none
}
