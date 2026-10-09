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

**Denied**:
The **Resolver** returned an error, or an invalid rate, so the request is refused whatever the Token says. It is how a deployment requires a rate, blocks a Caller, or refuses anything else it decides to. The error is returned to the service to log, not shown to the Caller.

**Window Bounds**:
The shortest and longest window a **Rate Capability** is held at, by default one minute and 24 hours. A rate with a window outside them is rescaled to the nearer bound at the same calls per second, rounded to the nearest call but never below one: `100/1s` is held as `6000/1m`, and `100/48h` as `50/24h`. This keeps a tiny window from churning state and a huge window from granting a huge **Burst**. They do not apply to **Overrides**, which a deployment configures itself.

### Limits

**Caller Rates**:
The set of rates a **Caller** is held to: every **Remembered Rate**. A request must fit all of them, each from its own allowance, so `1000/1m` alongside `100000/24h` allows neither 1001 in a minute nor 100001 in a day.
_Avoid_: Caller Rate (singular; the old largest-wins rule)

**Remembered Rate**:
One rate a **Caller** has presented, as a **Rate Capability** or as the **Resolver** decided, kept so it keeps applying to every request the Caller makes, whichever Token they carry. It is forgotten once no request has presented it for twice its window.

**Resolver**:
A function the deployment supplies with the final say over the rates a **Token** counts as carrying. It sees the principal and the Token's valid rates and returns the rates to use, which are trusted (not subject to the **Window Bounds**) and treated exactly as if the Token had carried them. It is where ceilings, **Overrides** and vouching for rateless Callers live, instead of in this library.

**Burst**:
How many calls a **Caller** may make at once from a full allowance. For one rate it is always the count: `100/24h` allows 100 calls immediately, then refills over the day. With several rates, the smallest count bounds it.

**Override**:
A **Resolver** policy for one **Caller**: returning fixed rates regardless of what the Token says, whether stricter or looser. Since adding a rate can only tighten, this is how a Caller is loosened.
_Avoid_: configured rate limit (in this repo)

### Outcomes

**Unrestricted**:
The state of a **Token** with no **Rate Capability**, for which the **Resolver** (if any) returned no rates: no rate limit applies. A Resolver that returns rates for such a Token makes it count as carrying them instead, and one that returns an error has it **Denied**. There is no mode that changes this; the library always enforces exactly the rates it is given.
_Avoid_: Required, Correct If Present, Permissive, Enforcing (the old modes)

**Capability Warning**:
A note for the **Caller** describing a problem with their **Token** or request, e.g. a **Malformed Capability** or a limit that would have been exceeded. Returned whether or not the request is blocked.

## Example dialogue

> **Dev:** Caller `abc` has a Token saying `1000/1m` and `100000/24h`. Which applies?
> **Domain expert:** Both: they are the **Caller Rates**. They can burst 1000, make at most 1000 in any minute, and at most 100000 in any day.
> **Dev:** They also sent one request with an older Token saying `200/1m`.
> **Domain expert:** Then `200/1m` is a **Remembered Rate** too, and applies to every request they make, including ones with the new Token, until they haven't presented it for two minutes.
> **Dev:** We cut them to `10/1m`, but they keep using the old Token.
> **Domain expert:** The first time they use the new one, `10/1m` applies to everything. If they never use it, have the **Resolver** return `10/1m` for them: an **Override**.
> **Dev:** And a Token with no rate at all?
> **Domain expert:** **Unrestricted**, unless the **Resolver** returns rates for it, in which case it counts as carrying them, or an error, in which case it is **Denied**. Requiring a rate is a two-line Resolver.
