<!--
SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
SPDX-License-Identifier: Apache-2.0
-->

# tokenrate

A library that rate limits Callers using the Rate Capabilities in their Tokens.

[![Build Status](https://github.com/xmidt-org/tokenrate/actions/workflows/ci.yml/badge.svg)](https://github.com/xmidt-org/tokenrate/actions/workflows/ci.yml)
[![codecov.io](http://codecov.io/github/xmidt-org/tokenrate/coverage.svg?branch=main)](http://codecov.io/github/xmidt-org/tokenrate?branch=main)
[![Apache V2 License](http://img.shields.io/badge/license-Apache%20V2-blue.svg)](https://github.com/xmidt-org/tokenrate/blob/main/LICENSE)
[![GitHub Release](https://img.shields.io/github/release/xmidt-org/tokenrate.svg)](https://github.com/xmidt-org/tokenrate/releases)
[![GoDoc](https://pkg.go.dev/badge/github.com/xmidt-org/tokenrate)](https://pkg.go.dev/github.com/xmidt-org/tokenrate)

## Summary

A rate limiter whose subject and limits come from an authenticated Token.
It limits how fast each Caller (the Token's principal) may make requests,
using the Rate Capabilities the Token carries, e.g. `prefix:rate:50/1m`.
A Token may carry several, such as `1000/1m` and `100000/24h`, and every
one applies. A deployment's own policy, such as a ceiling for everyone or
an override for one Caller, is a function it supplies with `WithResolver`.

tokenrate has no dependency on bascule, HTTP or JWT libraries. A service
passes in the principal and capability strings after authenticating a
request, and decides how to apply the decision it gets back.

## Install

```sh
go get github.com/xmidt-org/tokenrate
```

## Usage

Place the middleware after the one that authenticates the request. It
checks the Caller, writes one warning header per Capability Warning, and
answers 429 Too Many Requests (with Retry-After) or 403 Forbidden itself.
The only service-specific part is finding the principal and capabilities in
the request:

```go
limiter, err := tokenrate.New(
	tokenrate.WithPrefixes("prefix:rate:"),
	tokenrate.WithRequired(),
)
if err != nil {
	return err
}

limiter.Start() // optional: periodically sweep idle state
defer limiter.Stop()

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

// With bascule's own middleware, which stores the Token in the context:
protected := basculeMiddleware.Then(handler)
```

Outside HTTP, call `limiter.Check(ctx, principal, capabilities)` and apply
the `Decision` yourself.

A `Resolver` has the final say over the rates a Token counts as carrying,
so the special cases live in your code, not in options:

```go
tokenrate.WithResolver(func(ctx context.Context, principal string, provided []tokenrate.Rate) ([]tokenrate.Rate, error) {
	if override, ok := overrides[principal]; ok {
		return override, nil // replaces whatever the Token says
	}

	if len(provided) == 0 {
		return nil, nil // a Token with no rate stays Unrestricted
	}

	return append(provided, ceiling), nil // nobody else exceeds the ceiling
})
```

See the
[examples](https://pkg.go.dev/github.com/xmidt-org/tokenrate#pkg-examples)
for all three.

- [CONTEXT.md](CONTEXT.md) defines the terms.
- [docs/design.md](docs/design.md) describes the behavior.
- [ADR 0002](docs/adr/0002-caller-is-held-to-every-rate.md) explains why
  a Caller is held to every rate that applies.

## Code of Conduct

This project and everyone participating in it are governed by the [XMiDT Code Of Conduct](https://xmidt.io/code_of_conduct/).
By participating, you agree to this Code.

## Contributing

Refer to [CONTRIBUTING.md](CONTRIBUTING.md).
