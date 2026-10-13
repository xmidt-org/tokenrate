---
status: accepted
---
<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# No cap on the number of Callers

tokenrate used to bound how many Callers it tracked (`WithMaxCallers`). When a new Caller took it past the bound, another Caller was forgotten: first the least recently used, later an arbitrary one. Forgetting a Caller refills their allowance, so their next request is judged as if they had made none.

That makes the bound a way around the limit. Anyone able to present Tokens for enough principals can push the Limiter past it, and every new principal then hands some other Caller, possibly one of their own, a fresh allowance. Legitimate traffic that reaches the bound does the same by accident, silently.

Now there is no bound. A Caller's state is kept until it expires, twice the longest window after their last request, which is at most 48 hours under `DefaultResolver`'s Window Bounds. Only Callers whose Token carries a rate have state, so filling memory takes validly issued Tokens for a great many principals, which is a problem with the issuer, not one a rate limiter can solve. This assumes the limiter runs only after credentials are validated, so the principal is never the client's to choose; the package and `Middleware` docs say so.

## Considered Options

- **Keep the bound and evict.** Rejected: eviction of live state grants allowance, whichever Caller is chosen.
- **Keep the bound and fail closed.** When full, sweep at once, and refuse new Callers if nothing has expired, so no Caller ever gains allowance. Rejected for now: it adds a failure reason and a status code for a condition that needs an issuer compromise to reach, and turns that into a denial of service for new Callers. It remains the option if memory ever needs a hard bound.

## Consequences

- Memory grows with the number of distinct principals presenting a rate within twice the longest window. A deployment that shortens the windows its Resolver allows shortens that horizon.
- Expired state is still dropped by the sweep a check starts when something is due.
