---
status: accepted
---
<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# The Caller Rate is the largest recently-presented rate, per instance

A Caller can hold several valid Tokens with different rates, for example during a rate change. We limit per **Caller** (the Token's principal), not per Token, with one shared allowance held to the **largest** rate the Caller has presented. Each rate is remembered until it hasn't been presented for twice its window. Twice the window is long enough that a full allowance would have refilled, so forgetting loses nothing, and token expiry bounds how long an old, higher rate can linger. Limits are per instance, not cluster-wide: the issuer multiplies by the instance count, so no shared state is needed. Deployment **Overrides** replace the Caller Rate outright, because configuration is more trusted than a Token.

## Considered Options

- **Key the allowance per Token.** Rejected: a Caller could multiply their limit just by fetching more Tokens.
- **Key it by Caller plus the requested resources/parameters.** Rejected: the caller chooses the key, so every new combination gets a fresh allowance and the limit stops limiting.
- **Judge each request only by its own Token's rate.** Rejected: simple, but the "largest seen" rule gives one predictable limit per Caller.
- **A cluster-wide limit with shared state.** Rejected: a per-instance approximation is good enough and avoids a shared store.
- **Ignore malformed rates.** Rejected: a typo would make the Token Unrestricted. A malformed rate counts as zero instead.

## Consequences

- A Caller can keep a higher, older rate alive by continuing to use the old Token until it expires. An Override is the immediate fix.
- Losing state (a restart or eviction) only ever makes the limiter more lenient.
