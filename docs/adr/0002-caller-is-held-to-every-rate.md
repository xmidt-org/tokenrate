---
status: accepted
supersedes: 0001
---
<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# A Caller is held to every rate that applies, not the largest

ADR 0001 held a Caller to the **largest** rate they had recently presented. That made a Token with several rates mean only the most generous one, so `1000/1m` next to `100000/24h` ignored the daily cap: 1000 a minute is 1.44 million a day. It also meant a rate cut landed only once the old Token expired, and it needed a rule for comparing rates that differ in window and in count.

Now every valid rate applies at once, each with its own allowance. A request must fit all of them. The rates are:

- the Token's own Rate Capabilities, as the deployment's **Resolver** (if any) adjusts them, which are **remembered** for twice their window;
- every rate remembered from the Caller's other Tokens.

The Resolver is one function with the final say over what a Token counts as carrying. It is where ceilings, **Overrides** and vouching for rateless Callers live, rather than as options in this library: adding a rate can only tighten, so an Override that loosens has to replace, and only the deployment knows which it wants.

What ADR 0001 decided about **keying** stands: we limit per Caller (the Token's principal), not per Token; limits are per instance; a malformed rate is warned about rather than ignored, and a Token whose rates are all malformed, with nothing else applying, is held to zero.

## Considered Options

- **Keep the largest rate.** Rejected: tiered limits are the common case for issuers, and the comparison rule was the least obvious part of the design.
- **Apply only the rates on the request's own Token.** Rejected: a Caller holding an old `200/1m` Token and a new `10/1m` one would get 210 a minute until the old one expired. Applying every remembered rate makes a cut land the first time the new Token is used.
- **Options for ceilings, per-Caller rates and Overrides.** Rejected in favor of the Resolver: each special case would need its own option and its own rules for how it combines with the others, and the deployment can write the function it needs.
- **Hold an Unrestricted Token to the deployment's rates too.** Left to the Resolver, since it changes what Unrestricted means: returning rates for a Token that carries none makes it count as carrying them; returning nothing keeps it a free pass.

## Consequences

- A rate **cut** lands immediately, the first time any Token carrying it is used, and applies to every Token the Caller holds.
- A rate **raise** waits until the old, lower rate has not been presented for twice its window. Until then the Caller is still held to it. Stop using the old Token, or set an Override.
- Each rate costs one allowance (a timestamp) per Caller. Forgetting a rate after twice its window still loses nothing: its allowance would be full, and a rate nobody presents any more should not apply.
- Whatever the Resolver returns counts as what the Token carries. A Resolver error denies the request; see [ADR 0003](0003-resolver-decides-policy.md).
