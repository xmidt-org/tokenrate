// SPDX-FileCopyrightText: 2026 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package tokenrate

import "strings"

// Warning reasons.
const (
	// WarningMalformed reports a Rate Capability whose rate cannot be parsed.
	WarningMalformed = "malformed"

	// WarningWouldReject reports a check that failed in a Permissive mode.
	WarningWouldReject = "would-reject"
)

// Warning is a Capability Warning: a note for the Caller describing a problem
// with their Token or request.
type Warning struct {
	// Reason is WarningMalformed or WarningWouldReject.
	Reason string

	// Params are ordered key/value pairs giving the details.
	Params [][2]string
}

// String formats w the same way as bascule's Capability Warnings, e.g.
//
//	malformed; kind=rate; cap="x1:webpa:rate:10/0s"
//
// A value that is an HTTP token is written bare; anything else is quoted,
// escaping only '"' and '\'.
func (w Warning) String() string {
	var b strings.Builder
	b.WriteString(w.Reason)
	for _, p := range w.Params {
		b.WriteString("; ")
		b.WriteString(p[0])
		b.WriteByte('=')
		writeValue(&b, p[1])
	}

	return b.String()
}

func writeValue(b *strings.Builder, v string) {
	if isToken(v) {
		b.WriteString(v)
		return
	}

	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if v[i] == '"' || v[i] == '\\' {
			b.WriteByte('\\')
		}

		b.WriteByte(v[i])
	}

	b.WriteByte('"')
}

// isToken reports whether s is an HTTP token (RFC 9110, section 5.6.2).
func isToken(s string) bool {
	if s == "" {
		return false
	}

	for i := 0; i < len(s); i++ {
		if !isTokenChar(s[i]) {
			return false
		}
	}

	return true
}

func isTokenChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}

	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func malformedWarning(capability string) Warning {
	return Warning{
		Reason: WarningMalformed,
		Params: [][2]string{{"kind", "rate"}, {"cap", capability}},
	}
}

func wouldRejectWarning(reason Reason, limit Rate) Warning {
	w := Warning{
		Reason: WarningWouldReject,
		Params: [][2]string{{"kind", "rate"}, {"reason", reason.String()}},
	}

	if reason == RateExceeded {
		w.Params = append(w.Params, [2]string{"limit", limit.String()})
	}

	return w
}
