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
using the Rate Capabilities the Token carries, e.g. `x1:webpa:rate:50/1m`.

tokenrate has no dependency on bascule, HTTP or JWT libraries. A service
passes in the principal and capability strings after authenticating a
request, and decides how to apply the decision it gets back.

## Install

```sh
go get github.com/xmidt-org/tokenrate
```

## Usage

```go
limiter, err := tokenrate.New(
	tokenrate.WithPrefixes("x1:webpa:rate:"),
	tokenrate.WithRequired(),
)
if err != nil {
	return err
}

limiter.Start() // optional: periodically sweep idle state
defer limiter.Stop()

d := limiter.Check(token.Principal(), capabilities)
for _, w := range d.Warnings {
	rw.Header().Add("X-Webpa-Capability-Warning", w.String())
}

switch {
case d.Allowed:
	next.ServeHTTP(rw, r)
case d.Reason == tokenrate.RateExceeded:
	rw.Header().Set("Retry-After", seconds(d.RetryAfter)) // round up
	rw.WriteHeader(http.StatusTooManyRequests)
default: // tokenrate.NoRateCapability
	rw.WriteHeader(http.StatusForbidden)
}
```

- [CONTEXT.md](CONTEXT.md) defines the terms.
- [docs/design.md](docs/design.md) describes the behavior.
- [ADR 0001](docs/adr/0001-caller-rate-is-largest-remembered.md) explains
  why the Caller Rate is the largest rate a Caller has recently presented.

## Code of Conduct

This project and everyone participating in it are governed by the [XMiDT Code Of Conduct](https://xmidt.io/code_of_conduct/).
By participating, you agree to this Code.

## Contributing

Refer to [CONTRIBUTING.md](CONTRIBUTING.md).
