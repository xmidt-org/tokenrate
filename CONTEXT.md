<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# tokenrate

A standalone rate limiter whose subject and limits come from an authenticated **Token** (passed in as its principal and capability strings): it limits how fast each **Caller** may make requests, using **Rate Capabilities** the Token carries.

## Language

### Tokens and capabilities

**Token**:
The authenticated result of a request's credentials, e.g. a verified JWT. It identifies its **Caller** and may carry **Capabilities**.
_Avoid_: SAT (internal jargon), JWT (when the format doesn't matter)

**Caller**:
The party a **Token** was issued to, identified by the Token's principal (a JWT's `sub`). All Tokens with the same principal are the same Caller.
_Avoid_: client ID, user, partner

**Capability**:
One flat string carried by a **Token**. A Token's capabilities may serve many services; tokenrate only looks at the ones its **Capability Prefix** selects.

**Capability Prefix**:
A configured string or regular expression that selects which **Capabilities** are **Rate Capabilities**. Nothing is built in; capabilities matching no prefix are ignored silently.

**Rate Capability**:
A **Capability** (e.g. `{prefix}50/1m`) stating how many calls a **Caller** may make per window against **one instance** of the service. Fleet-wide throughput is roughly that × the number of instances; issuers do that math.

**Malformed Capability**:
A **Rate Capability** whose rate cannot be parsed. It still counts as present (so the **Token** is not **Unrestricted**) and counts as a rate of zero.

**Window Bounds**:
The shortest and longest window a **Rate Capability** is held at, by default one minute and 24 hours. A rate with a window outside them is rescaled to the nearer bound at the same calls per second, rounded to the nearest call but never below one: `100/1s` is held as `6000/1m`, and `100/48h` as `50/24h`. This keeps a tiny window from churning state and a huge window from granting a huge **Burst**. They do not apply to **Overrides**, which a deployment configures itself.

### Limits

**Caller Rate**:
The rate a **Caller** is held to: the largest **Rate Capability** among the Caller's **Tokens** that this instance has seen recently. All of a Caller's requests share one allowance at this rate, whichever Token they carry.

**Remembered Rate**:
One rate a **Caller** has presented, kept so it can contribute to the **Caller Rate**. It is forgotten once no request has presented it for twice its window.

**Burst**:
How many calls a **Caller** may make at once from a full allowance. Always equal to the rate's count: `100/24h` allows 100 calls immediately, then refills over the day.

**Override**:
A rate a deployment configures for a specific **Caller**. It is more trusted than any **Rate Capability** and replaces the Caller Rate entirely, whether it is stricter or looser.
_Avoid_: configured rate limit (in this repo)

### Modes

**Unrestricted**:
The state of a **Token** with no **Rate Capability** and no **Override** for its **Caller**: no rate limit applies, unless the check is **Required**.

**Required**:
A setting under which a **Token** must carry at least one **Rate Capability** (or its **Caller** must have an **Override**); one carrying none fails instead of being **Unrestricted**. The opposite is **Correct If Present**.

**Correct If Present**:
The default setting: a **Token** without **Rate Capabilities** is **Unrestricted**, but one that has them is held to the **Caller Rate**.

**Permissive**:
A mode in which a failing check lets the request through and reports what it would have rejected as a **Capability Warning**. The opposite is **Enforcing**. It combines with **Required** or **Correct If Present**.

**Capability Warning**:
A note for the **Caller** describing a problem with their **Token** or request, e.g. a **Malformed Capability** or a limit that would have been exceeded. Returned whether or not the request is blocked.

## Example dialogue

> **Dev:** Caller `abc` sent one request with a Token saying `50/1m` and another with a newer Token saying `200/1m`. Which applies?
> **Domain expert:** Both are **Remembered Rates**, so the **Caller Rate** is `200/1m`, and both Tokens draw from the same allowance.
> **Dev:** We cut them to `10/1m`, but they keep using the old Token.
> **Domain expert:** Then `200/1m` keeps being remembered until that Token expires and stops being accepted; twice its window later it's forgotten. If you need it now, set an **Override**.
> **Dev:** And a Token with no rate at all?
> **Domain expert:** **Unrestricted** under **Correct If Present**; rejected under **Required**.
