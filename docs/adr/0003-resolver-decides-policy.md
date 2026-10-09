---
status: accepted
---
<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# The library always enforces; the Resolver decides policy

tokenrate used to have modes: **Required** or **Correct If Present** decided what a Token with no rate meant, and **Permissive** or **Enforcing** decided whether a failure rejected the request or let it through with a would-reject warning. Four options spelled out the grid, each set the whole mode, and the last one applied won.

Now there are no modes. The library enforces whatever rates the Token counts as carrying. An empty set means no limit, and that is fine: it is what a Token with no rate means unless the deployment says otherwise. The deployment says otherwise through the **Resolver**: it returns rates to add or replace, or an error to deny the request for any reason, including "this Token carries no rate" or "this Caller is blocked". The error goes back in the Decision for the service to log, never to the Caller.

## Considered Options

- **Keep Required as a boolean option.** Rejected: it is a two-line Resolver, and the Resolver can also require a rate only for some Callers or routes, which no option could.
- **Keep Permissive as a boolean option.** Rejected: a dry run is the service's call. It can run the check, log the Decision through `Observe`, and let the request through regardless; the only thing lost is the would-reject warning to the Caller, which nothing consumed.
- **A separate failure reason for Resolver errors versus deliberate denials.** Rejected: the Resolver cannot tell them apart any better than we can, and a Resolver that would rather fail open on a dependency outage can return the provided rates instead of an error.

## Consequences

- One failure path besides the rate itself: `Denied`, with `Err`. The middleware answers 403 Forbidden.
- A Token whose only rates are malformed is still held to zero rather than becoming unlimited. That is not a mode; it keeps a typo from meaning no limit, as ADR 0001 decided.
- Capability Warnings are only ever about malformed rates now.
