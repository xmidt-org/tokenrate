<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# tokenrate design

> **Status:** implemented. Terms are defined in
> [CONTEXT.md](../CONTEXT.md); the key decisions are in
> [ADR 0001](adr/0001-caller-rate-is-largest-remembered.md).

tokenrate is a standalone Go library (`github.com/xmidt-org/tokenrate`)
that limits how fast each Caller may make requests. Both **who** is limited
(the Token's principal) and **how fast** (its Rate Capabilities) come from
the Token, but tokenrate never sees the Token itself. The service passes in
the principal and the capability strings after authentication, and gets
back a decision.

tokenrate has **no dependency on bascule, HTTP, or JWT libraries**. The
service decides how to apply a decision (status codes, headers, logging).
tr1d1um, and later scytale, call it after bascule has authenticated the
request.

## Out of scope

- **A cluster-wide limit.** Each instance limits on its own; issuers
  account for the instance count.
- **Authentication, CIDR checks, Caller Origin and endpoint checks.**
  Those are in bascule.
- **HTTP.** No status codes, headers or middleware. Each service writes
  that glue itself (see [Integrating](#integrating)).
- **A general-purpose rate limiter.** The bucket is an internal package.
- **Limits scoped to a network** ("caller X from CIDR Y"). Not supported
  yet; see [Open decisions](#open-decisions).

## Capability format

`{prefix}<count>/<window>`, e.g. `x1:webpa:rate:50/1m`.

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
- Rates are compared by calls per second (`count / window`). On a tie, the
  larger count (and therefore the larger Burst) wins.

## Behavior on each request

Given principal `p` and the Token's capability strings `t`:

1. **Select** `t`'s capabilities that match a prefix, and parse each one.
   Each malformed one emits a Capability Warning (in every mode) and counts
   as a rate of zero. Each valid one is rescaled into the Window Bounds.
2. **Override:** if `p` has an Override, the limit is the Override.
   Remembered Rates are neither consulted nor updated. Go to step 5.
3. **Missing:** if `t` has no Rate Capabilities:
   - Correct If Present → allow (Unrestricted). Don't touch `p`'s
     allowance.
   - Required → fail (`reason=no-rate-capability`).
4. **Remember:** take the largest rate in `t`. If it is non-zero, record it
   (or refresh it) as a Remembered Rate for `p`, stamped `now`. Drop any of
   `p`'s Remembered Rates not presented for more than 2× their window. The
   limit is the largest remaining Remembered Rate. If there is none (the
   token's only rates were malformed), the limit is zero.
5. **Spend:** take one call from `p`'s allowance at the limit. A limit of
   zero always fails. If there is no allowance left, fail
   (`reason=rate-exceeded`).

Then:

- **Fail + Enforcing** → `Allowed: false` with the `Reason`
  (`NoRateCapability` or `RateExceeded`), plus `RetryAfter` for
  `RateExceeded`.
- **Fail + Permissive** → `Allowed: true`. The call is still spent, and a
  `would-reject` warning is added.

## The allowance (GCRA)

Use GCRA (the Generic Cell Rate Algorithm), the leaky bucket stored as a
single timestamp. Per Caller, store one *theoretical arrival time* (TAT),
shared by every rate:

```text
interval = window / count        // time one call "costs"
tolerance = window               // so Burst = count
allow if now >= TAT - tolerance + interval  (with TAT = max(TAT, now))
on allow: TAT = max(TAT, now) + interval
retryAfter = (TAT - tolerance + interval) - now
```

Because the TAT is shared and the interval is chosen per request, a Caller
switching between rates never gets more than the larger rate in total.

## State and memory

- Per Caller: the TAT plus Remembered Rates (rate → last presented).
- A Caller with no Remembered Rates left, whose TAT is in the past, is
  deleted. With a full allowance and nothing remembered, its state carries
  no information.
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
func WithOverride(principal string, rate Rate) Option // repeatable
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

// Check decides one request and spends from the Caller's allowance.
func (l *Limiter) Check(principal string, capabilities []string) Decision

type Decision struct {
	Allowed    bool
	Reason     Reason        // None, NoRateCapability, RateExceeded
	Limit      Rate          // the limit applied; zero if Unrestricted
	RetryAfter time.Duration // set when Reason == RateExceeded
	Warnings   []Warning
}

type Warning struct {
	Reason string     // "malformed", "would-reject"
	Params [][2]string // ordered key/value pairs
}
func (w Warning) String() string // e.g. `malformed; kind=rate; cap="x1:webpa:rate:10/0s"`

type Rate struct{ Count int; Window time.Duration }
func ParseRate(string) (Rate, error) // same grammar as capabilities
```

Overrides are fixed at construction. If they must change at runtime,
rebuild the limiter (state is lost, which only makes it more lenient).

## Warnings

`Warning.String()` uses the same text format as bascule's Capability
Warnings, so a service can pass the string straight into bascule's warning
header, or anywhere else:

```text
malformed; kind=rate; cap="x1:webpa:rate:10/0s"
would-reject; kind=rate; reason=no-rate-capability
would-reject; kind=rate; reason=rate-exceeded; limit="50/1s"
```

Quoting: a value that is an HTTP token is written bare; anything else is
quoted, escaping only `"` and `\`. A rate contains `/`, which isn't a token
character, so it is quoted.

## Integrating

The glue lives in the service, not in tokenrate. For tr1d1um it is a small
HTTP middleware placed **after** bascule's middleware:

```go
token, _ := bascule.Get(r.Context())
caps, _ := bascule.GetCapabilities(token)
d := limiter.Check(token.Principal(), caps)
for _, warn := range d.Warnings {
	w.Header().Add("X-Webpa-Capability-Warning", warn.String())
}
switch {
case d.Allowed:
	next.ServeHTTP(w, r)
case d.Reason == tokenrate.RateExceeded:
	w.Header().Set("Retry-After", seconds(d.RetryAfter)) // round up
	w.WriteHeader(http.StatusTooManyRequests)
default: // NoRateCapability
	w.WriteHeader(http.StatusForbidden)
}
```

It writes its own warning headers because bascule's middleware has already
handed the request on by the time this runs.

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
- Two rates in one Token → the larger applies.
- Token A `5/1s`, then Token B `20/1s`, same principal → the limit is 20
  for both Tokens; their calls draw from one allowance.
- After 2× the window without presenting `20/1s`, a Token with `5/1s` is
  held to 5.
- Only a malformed rate → every call fails (Enforcing), with a warning.
- A malformed rate alongside a valid one → the valid one applies, and a
  warning is still emitted.
- Window Bounds `1s` to `1h`: `100/1ms` → held as `100000/1s`; `100/2h` →
  held as `50/1h`, so only 50 calls burst; `5/1s` and `5/1h` → unchanged;
  an Override of `10/1ms` still applies. With the defaults, `100/1s` →
  `6000/1m`, `100/25h` → `96/24h`, `1/720h` → `1/24h`.
- No Rate Capability: Correct If Present → allowed; Required →
  `NoRateCapability`.
- Override looser than the Token → the Override applies. Override stricter
  → the Override applies. Override with no Rate Capability and Required →
  allowed.
- Permissive modes: every failing case above → `Allowed: true` plus a
  `would-reject` warning, and the call is still spent.
- A different principal → an independent allowance.
- MaxCallers reached → the least recently used Caller is evicted.
- Concurrent calls (`-race`) → never more than Burst allowed from a full
  allowance.

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
