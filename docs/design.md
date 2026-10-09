<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# tokenrate design

> **Status:** implemented. Terms are defined in
> [CONTEXT.md](../CONTEXT.md); the key decisions are in
> [ADR 0001](adr/0001-caller-rate-is-largest-remembered.md) and
> [ADR 0002](adr/0002-caller-is-held-to-every-rate.md).

tokenrate is a standalone Go library (`github.com/xmidt-org/tokenrate`)
that limits how fast each Caller may make requests. Both **who** is limited
(the Token's principal) and **how fast** (its Rate Capabilities) come from
the Token, but tokenrate never sees the Token itself. The service passes in
the principal and the capability strings after authentication, and gets
back a decision.

tokenrate has **no dependency on bascule or JWT libraries**. It ships an
HTTP middleware (standard library only) that applies the decision; the
service supplies just the function that finds the principal and
capabilities in a request. tr1d1um, and later scytale, place it after
bascule's middleware.

## Out of scope

- **A cluster-wide limit.** Each instance limits on its own; issuers
  account for the instance count.
- **Authentication, CIDR checks, Caller Origin and endpoint checks.**
  Those are in bascule.
- **Extracting the Token from a request.** That depends on the service's
  authentication library, so the service supplies it (see
  [Integrating](#integrating)).
- **A general-purpose rate limiter.** The bucket is an internal package.
- **Limits scoped to a network** ("caller X from CIDR Y"). Not supported
  yet; see [Open decisions](#open-decisions).

## Capability format

`{prefix}<count>/<window>`, e.g. `prefix:rate:50/1m`.

- The prefix is a string or regular expression, which may contain
  subexpressions. The remainder is the rate. (These are the same semantics
  as bascule's `basculecaps.WithPrefixes`, but tokenrate doesn't import it.)
- `<count>`: a positive base-10 integer.
- `<window>`: a positive Go duration (`time.ParseDuration`), e.g. `1m`,
  `1h30m` or `24h`. Days (`1d`) are not a unit.
- Anything else is **malformed**: a missing `/`, `0/1s`, `10/0s`, `-5/1s`,
  `abc/1s`, `10/1x` or `10/1d`.
- A deployment bounds the window (`WithMinWindow`, `WithMaxWindow`; the
  defaults are `1m` and `24h`). A rate whose window is outside the bounds
  is rescaled to the nearer bound at the same calls per second, rounded to
  the nearest call but never below one: `100/1s` is held as `6000/1m`, and
  `100/48h` as `50/24h`. Overrides are exempt.
- A Token may carry several rates. Every one applies.

## Behavior on each request

Given principal `p` and the Token's capability strings `t`:

1. **Select** `t`'s capabilities that match a prefix, and parse each one.
   Each malformed one emits a Capability Warning (in every mode) and is
   otherwise ignored. Each valid one is rescaled into the Window Bounds.
2. **Resolve:** if a Resolver is set, call it with `p` and the rates from
   step 1. Its result is what `t` counts as carrying from here on, and is
   trusted (no rescaling). If it errs or returns an invalid rate, fail
   (`reason=resolver-failed`, `Err` set) and stop.
3. **Missing:** if `t` had no Rate Capabilities and nothing was resolved:
   - Required → fail (`reason=no-rate-capability`).
   - Correct If Present → allow (Unrestricted). Don't touch `p`'s
     allowances.
4. **Remember:** record (or refresh) each of `t`'s rates as a Remembered
   Rate for `p`, stamped `now`. Drop any of `p`'s Remembered Rates not
   presented for more than 2× their window. The limits are every remaining
   Remembered Rate. If there are none, `t` counted as having rates that
   left nothing to apply, so hold it to zero (fail).
5. **Spend:** take one call from `p`'s allowance at **every** limit. If any
   has no allowance left, fail (`reason=rate-exceeded`) with `Limit` the
   one with the longest wait, and spend nothing. Otherwise spend all.

Then:

- **Fail + Enforcing** → `Allowed: false` with the `Reason`
  (`NoRateCapability`, `RateExceeded` or `ResolverFailed`), plus
  `RetryAfter` for `RateExceeded`.
- **Fail + Permissive** → `Allowed: true`. A rate-exceeded call is spent at
  every limit anyway, and a `would-reject` warning is added.

The Resolver runs outside the Limiter's locks, so it may be slow or call
out, but it runs on every check.

## The allowance (GCRA)

Use GCRA (the Generic Cell Rate Algorithm), the leaky bucket stored as a
single timestamp. Per Caller and per rate, store one *theoretical arrival
time* (TAT):

```text
interval = window / count        // time one call "costs"
tolerance = window               // so Burst = count
allow if now >= TAT - tolerance + interval  (with TAT = max(TAT, now))
on allow: TAT = max(TAT, now) + interval
retryAfter = (TAT - tolerance + interval) - now
```

A request is checked against every applicable rate before any is spent,
so a refusal by one leaves the others untouched.

## State and memory

- Per Caller: one allowance per rate (rate → TAT, plus when it was last
  presented).
- An allowance is dropped once its rate is no longer remembered and its
  TAT is in the past: full and unpresented, it carries no information. A
  Caller with no allowances left is deleted.
- Bound the number of Callers (`WithMaxCallers`, default e.g. 100 000).
  When full, evict the least recently used Caller. Eviction only ever makes
  a Caller's next request more lenient.
- Sweep expired state lazily on access, plus a periodic sweep tied to the
  limiter's lifetime (`Start`/`Stop`, or a context).
- Safe for concurrent use. Shard by principal hash so a single lock isn't a
  hot spot.

## Suggested API

```go
package tokenrate

func New(opts ...Option) (*Limiter, error)

func WithPrefixes(prefixes ...string) Option         // required; none = error from New
func WithResolver(r Resolver) Option                  // the final say over a Token's rates
func WithMaxCallers(n int) Option
func WithMinWindow(d time.Duration) Option            // default 1m
func WithMaxWindow(d time.Duration) Option            // default 24h
func WithClock(func() time.Time) Option               // for tests

// Mode options. Each sets the whole mode; the last one applied wins.
// Default: WithCorrectIfPresent.
func WithRequired() Option
func WithCorrectIfPresent() Option
func WithPermissiveRequired() Option
func WithPermissiveCorrectIfPresent() Option

// Resolver decides what rates a Token counts as carrying.  Ceilings,
// Overrides and vouching for rateless Callers are written here.
type Resolver func(ctx context.Context, principal string, provided []Rate) ([]Rate, error)

// Check decides one request and spends from the Caller's allowances.
func (l *Limiter) Check(ctx context.Context, principal string, capabilities []string) Decision

// Middleware applies a Limiter to HTTP requests; Extract is the only
// service-specific part.
type Extractor func(*http.Request) (principal string, capabilities []string)
type Middleware struct {
	Limiter       *Limiter
	Extract       Extractor
	WarningHeader string                        // default X-Webpa-Capability-Warning
	Observe       func(*http.Request, Decision) // optional, for metrics or logging
}
func (m Middleware) Wrap(next http.Handler) http.Handler

type Decision struct {
	Allowed    bool
	Reason     Reason        // None, NoRateCapability, RateExceeded, ResolverFailed
	Limits     []Rate        // every rate applied, shortest window first
	Limit      Rate          // the rate that refused; zero otherwise
	RetryAfter time.Duration // set when Reason == RateExceeded
	Warnings   []Warning
	Err        error         // the Resolver's error when Reason == ResolverFailed
}

type Warning struct {
	Reason string     // "malformed", "would-reject"
	Params [][2]string // ordered key/value pairs
}
func (w Warning) String() string // e.g. `malformed; kind=rate; cap="prefix:rate:10/0s"`

type Rate struct{ Count int; Window time.Duration }
func ParseRate(string) (Rate, error) // same grammar as capabilities
```

The Resolver is fixed at construction, but it is a function, so what it
returns can change at runtime; its rates take effect on the next check.

## Warnings

`Warning.String()` uses the same text format as bascule's Capability
Warnings, so a service can pass the string straight into bascule's warning
header, or anywhere else:

```text
malformed; kind=rate; cap="prefix:rate:10/0s"
would-reject; kind=rate; reason=no-rate-capability
would-reject; kind=rate; reason=rate-exceeded; limit="50/1s"
```

Quoting: a value that is an HTTP token is written bare; anything else is
quoted, escaping only `"` and `\`. A rate contains `/`, which isn't a token
character, so it is quoted.

## Integrating

`Middleware` does the HTTP glue, so every service applies a Decision the
same way. It is placed **after** the authentication middleware, and the
service supplies only the `Extractor`. For tr1d1um that is bascule:

```go
handler := tokenrate.Middleware{
	Limiter: limiter,
	Extract: func(r *http.Request) (string, []string) {
		token, ok := bascule.Get(r.Context())
		if !ok {
			return "", nil // unauthenticated: the Limiter's mode decides
		}

		caps, _ := bascule.GetCapabilities(token)
		return token.Principal(), caps
	},
}.Wrap(next)
```

For each request the middleware:

1. Calls `Extract`, then `Limiter.Check`.
2. Calls `Observe`, if set, with the Decision.
3. Adds one `WarningHeader` per Capability Warning, whether or not the
   request goes on.
4. Allowed → calls `next`. `RateExceeded` → 429 Too Many Requests, with
   `Retry-After` in whole seconds rounded up when `RetryAfter` is set.
   `NoRateCapability` → 403 Forbidden. `ResolverFailed` → 503 Service
   Unavailable; `Observe` sees `Err`.

It writes its own warning headers because bascule's middleware has already
handed the request on by the time this runs. A service that is not HTTP,
or that wants different responses, calls `Check` and applies the Decision
itself.

## Acceptance tests

Table-driven, testify, with an injected clock. The behavior tests lower the
minimum window to `1s` so they run on short windows.

Parsing:

- `50/1s`, `600/1m`, `100/24h`, `4/1h30m` → valid.
- `0/1s`, `10/0s`, `-1/1s`, `1.5/1s`, `abc/1s`, `10/1x`, `10`, `10/1d`
  → malformed.
- Prefix with subexpressions → the rate is still extracted correctly.

Behavior:

- `5/1s`: 5 immediate calls allowed, the 6th returns `RateExceeded` with a
  `RetryAfter` of about 200ms; after 200ms one more is allowed.
- Several rates in one Token → all apply: `3/1s` with `5/1m` allows 3,
  then 2 more a second later, then refuses with `Limit` `5/1m`.
- Token A `5/1s`, then Token B `20/1s`, same principal → both apply to
  both Tokens; their calls draw from the same allowances.
- A cut: Token A `20/1s`, then Token B `2/1s` once → A is held to 2.
- After 2× the window without presenting `20/1s`, it no longer applies.
- Only a malformed rate → every call fails (Enforcing), with a warning.
- A malformed rate alongside a valid one → the valid one applies, and a
  warning is still emitted.
- Window Bounds `1s` to `1h`: `100/1ms` → held as `100000/1s`; `100/2h` →
  held as `50/1h`, so only 50 calls burst; `5/1s` and `5/1h` → unchanged;
  an Override of `10/1ms` still applies. With the defaults, `100/1s` →
  `6000/1m`, `100/25h` → `96/24h`, `1/720h` → `1/24h`.
- No Rate Capability: Correct If Present → allowed; Required →
  `NoRateCapability`.
- Resolver: sees the principal, context and the Token's valid, bounded
  rates. A ceiling it appends applies alongside the Token's, unrescaled,
  and is remembered. An Override it returns replaces the Token's, looser
  or stricter, and vouches for a rateless Token under Required. Returning
  nothing for a rateless Token keeps it Unrestricted; returning nothing for
  a Token with rates holds it to zero. An error or an invalid rate →
  `ResolverFailed` with `Err`, nothing spent; Permissive → allowed with a
  `would-reject` warning.
- Permissive modes: every failing case above → `Allowed: true` plus a
  `would-reject` warning, and the call is still spent.
- A different principal → an independent allowance.
- MaxCallers reached → the least recently used Caller is evicted.
- Concurrent calls (`-race`) → never more than Burst allowed from a full
  allowance.

Middleware:

- Allowed → `next` runs, warnings in the header. `RateExceeded` → 429 with
  `Retry-After` rounded up, or no `Retry-After` at a zero limit.
  `NoRateCapability` → 403. `ResolverFailed` → 503. Permissive → `next`
  runs with a `would-reject` warning. `Observe` sees every Decision. A nil `Limiter` or `Extract`
  panics in `Wrap`.

## Open decisions

- **Network-scoped limits.** tr1d1um's proposed `rate_limits` config
  matches a claim *and* a CIDR. Overrides here are per principal only.
  Recommended: keep tokenrate free of networking. If it's needed, the
  service resolves the Caller Origin (via bascule) and picks which limiter
  or Override applies before calling `Check`.
- **Tokens with no rate during a rollout.** Under Correct If Present, a
  Caller who also holds a rate-limited Token can avoid the limit by using
  an older Token without one. Recommended: accept this (it's what
  Unrestricted means) and use Required once every Token carries a rate.
