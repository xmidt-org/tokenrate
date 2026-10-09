// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headerExtractor reads the principal and capabilities from request headers,
// standing in for whatever a service's authentication leaves behind.
func headerExtractor(r *http.Request) (string, []string) {
	var caps []string
	if h := r.Header.Get("Capabilities"); h != "" {
		caps = strings.Split(h, ",")
	}

	return r.Header.Get("Principal"), caps
}

func newRequest(principal string, caps ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Principal", principal)
	if len(caps) > 0 {
		r.Header.Set("Capabilities", strings.Join(rateCaps(caps...), ","))
	}

	return r
}

func TestMiddleware(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c)

	var observed []Decision
	served := 0
	h := Middleware{
		Limiter: l,
		Extract: headerExtractor,
		Observe: func(_ *http.Request, d Decision) { observed = append(observed, d) },
	}.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusNoContent)
	}))

	// Two calls at 2/1s pass; the third is refused with a one second wait.
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest("abc", "2/1s"))
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Header().Values(DefaultWarningHeader))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("abc", "2/1s"))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"), "500ms rounds up to a second")
	assert.Equal(t, 2, served)

	// Warnings are written whether or not the request goes on.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("def", "10/0s", "5/1s"))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{`malformed; kind=rate; cap="prefix:rate:10/0s"`}, rec.Header().Values(DefaultWarningHeader))

	// A zero limit never recovers, so there is no Retry-After.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("ghi", "10/0s"))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
	assert.Len(t, rec.Header().Values(DefaultWarningHeader), 1)

	require.Len(t, observed, 5)
	assert.Equal(t, RateExceeded, observed[2].Reason)
	assert.Equal(t, time.Second/2, observed[2].RetryAfter)
}

func TestMiddlewareRequired(t *testing.T) {
	l := newLimiter(t, newClock(), WithRequired())
	h := Middleware{Limiter: l, Extract: headerExtractor, WarningHeader: "Warning"}.
		Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("abc"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
	assert.Empty(t, rec.Header())

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("abc", "5/1s"))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMiddlewarePermissive(t *testing.T) {
	l := newLimiter(t, newClock(), WithPermissiveRequired())
	h := Middleware{Limiter: l, Extract: headerExtractor, WarningHeader: "Warning"}.
		Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("abc"))
	assert.Equal(t, http.StatusNoContent, rec.Code, "permissive modes let the request through")
	assert.Equal(t, []string{"would-reject; kind=rate; reason=no-rate-capability"}, rec.Header().Values("Warning"))
	assert.Empty(t, rec.Header().Values(DefaultWarningHeader), "the configured header is used instead")
}

func TestMiddlewareMisconfigured(t *testing.T) {
	l := newLimiter(t, newClock())
	next := http.NotFoundHandler()

	assert.PanicsWithValue(t, "tokenrate: Middleware.Limiter is nil", func() {
		Middleware{Extract: headerExtractor}.Wrap(next)
	})

	assert.PanicsWithValue(t, "tokenrate: Middleware.Extract is nil", func() {
		Middleware{Limiter: l}.Wrap(next)
	})
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: time.Nanosecond, want: "1"},
		{d: 999 * time.Millisecond, want: "1"},
		{d: time.Second, want: "1"},
		{d: time.Second + time.Nanosecond, want: "2"},
		{d: 90 * time.Second, want: "90"},
	}

	for _, tc := range tests {
		t.Run(tc.d.String(), func(t *testing.T) {
			assert.Equal(t, tc.want, retryAfterSeconds(tc.d))
		})
	}
}
