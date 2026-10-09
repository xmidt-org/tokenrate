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
A **Rate Capability** whose rate cannot be parsed. It is warned about and ignored, but still counts as present (so the **Token** is not **Unrestricted**); a Token whose Rate Capabilities are all malformed, with no other **Caller Rates**, is held to a rate of zero.

**Window Bounds**:
The shortest and longest window a **Rate Capability** is held at, by default one minute and 24 hours. A rate with a window outside them is rescaled to the nearer bound at the same calls per second, rounded to the nearest call but never below one: `100/1s` is held as `6000/1m`, and `100/48h` as `50/24h`. This keeps a tiny window from churning state and a huge window from granting a huge **Burst**. They do not apply to **Overrides**, which a deployment configures itself.

### Limits

**Caller Rates**:
The set of rates a **Caller** is held to: every **Remembered Rate** plus every **Configured Rate**. A request must fit all of them, each from its own allowance, so `1000/1m` alongside `100000/24h` allows neither 1001 in a minute nor 100001 in a day. An **Override** replaces the whole set.
_Avoid_: Caller Rate (singular; the old largest-wins rule)

**Remembered Rate**:
One rate a **Caller** has presented in a **Rate Capability**, kept so it keeps applying to every request the Caller makes, whichever Token they carry. It is forgotten once no request has presented it for twice its window.

**Configured Rate**:
A rate the deployment adds to the **Caller Rates**, for every Caller or for one. It applies alongside what the Token says and can only tighten. It is not subject to the **Window Bounds**.
_Avoid_: default rate, global rate

**Burst**:
How many calls a **Caller** may make at once from a full allowance. For one rate it is always the count: `100/24h` allows 100 calls immediately, then refills over the day. With several rates, the smallest count bounds it.

**Override**:
One or more rates a deployment configures for a specific **Caller**, replacing the **Caller Rates** entirely, whether stricter or looser. It is more trusted than any **Rate Capability** or **Configured Rate**, and is the way to loosen a Caller, since adding a rate can only tighten.
_Avoid_: configured rate limit (in this repo)

### Modes

**Unrestricted**:
The state of a **Token** with no **Rate Capability** and no **Override** for its **Caller**: no rate limit applies, unless the check is **Required**. A deployment may instead hold Unrestricted Tokens to the **Caller Rates** (`WithLimitUnrestricted`), so a Token without a rate is not a way around the **Configured Rates** or the Caller's other Tokens.

**Required**:
A setting under which a **Token** must carry at least one **Rate Capability** (or its **Caller** must have an **Override**); one carrying none fails instead of being **Unrestricted**. The opposite is **Correct If Present**.

**Correct If Present**:
The default setting: a **Token** without **Rate Capabilities** is **Unrestricted**, but one that has them is held to the **Caller Rate**.

**Permissive**:
A mode in which a failing check lets the request through and reports what it would have rejected as a **Capability Warning**. The opposite is **Enforcing**. It combines with **Required** or **Correct If Present**.

**Capability Warning**:
A note for the **Caller** describing a problem with their **Token** or request, e.g. a **Malformed Capability** or a limit that would have been exceeded. Returned whether or not the request is blocked.

## Example dialogue

> **Dev:** Caller `abc` has a Token saying `1000/1m` and `100000/24h`. Which applies?
> **Domain expert:** Both: they are the **Caller Rates**. They can burst 1000, make at most 1000 in any minute, and at most 100000 in any day.
> **Dev:** They also sent one request with an older Token saying `200/1m`.
> **Domain expert:** Then `200/1m` is a **Remembered Rate** too, and applies to every request they make, including ones with the new Token, until they haven't presented it for two minutes.
> **Dev:** We cut them to `10/1m`, but they keep using the old Token.
> **Domain expert:** The first time they use the new one, `10/1m` applies to everything. If they never use it, set an **Override**.
> **Dev:** And a Token with no rate at all?
> **Domain expert:** **Unrestricted** under **Correct If Present**; rejected under **Required**. Unless the deployment limits Unrestricted Tokens, in which case the **Configured Rates** and whatever is remembered for the Caller apply.
