package llm

import (
	"encoding/json"
	"strings"
	"unicode/utf16"
)

// Sanitize strips bytes that cannot round-trip through provider JSON
// encoders (pi's sanitizeSurrogates). JS strings are UTF-16, where lone
// surrogates crash provider-side JSON parsing; the Go equivalent of a
// lone surrogate is invalid UTF-8, so this drops every invalid sequence
// (ToValidUTF8 with an empty replacement — removal, not U+FFFD
// substitution, matching pi's delete-not-replace behavior).
func Sanitize(s string) string {
	return strings.ToValidUTF8(s, "")
}

// NormalizeArguments guarantees every tool-call block in a message
// carries marshalable JSON. Providers occasionally emit argument
// fragments that never close into a valid object (truncation, relay
// quirks); a json.RawMessage holding invalid bytes poisons every later
// marshal — session persistence, transcript replay, the provider
// request — with "error calling MarshalJSON for type json.RawMessage".
// Invalid text is wrapped as a JSON string so argument validation
// rejects it with a clean, model-recoverable tool error instead.
func NormalizeArguments(m *Message) {
	for i := range m.Content {
		b := &m.Content[i]
		if b.Kind != BlockToolCall {
			continue
		}
		if len(b.Arguments) == 0 {
			b.Arguments = json.RawMessage("{}")
			continue
		}
		if json.Valid(b.Arguments) {
			continue
		}
		if enc, err := json.Marshal(string(b.Arguments)); err == nil {
			b.Arguments = enc
		} else {
			b.Arguments = json.RawMessage("{}")
		}
	}
}

// ShortHash is pi's shortHash (utils/hash.ts): a fast deterministic
// 64-bit-feeling hash rendered as two base36 halves, used to shorten
// over-long provider item ids while keeping them stable across requests.
// It hashes UTF-16 code units (JS semantics), so non-BMP text matches pi.
func ShortHash(s string) string {
	h1 := uint32(0xdeadbeef)
	h2 := uint32(0x41c6ce57)
	for _, u := range utf16.Encode([]rune(s)) {
		ch := uint32(u)
		h1 = (h1 ^ ch) * 2654435761
		h2 = (h2 ^ ch) * 1597334677
	}
	h1 = (h1^(h1>>16))*2246822507 ^ (h2^(h2>>13))*3266489909
	h2 = (h2^(h2>>16))*2246822507 ^ (h1^(h1>>13))*3266489909
	return toBase36(h2) + toBase36(h1)
}

const base36Digits = "0123456789abcdefghijklmnopqrstuvwxyz"

func toBase36(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte // uint32 max is 6 base36 digits + slack
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = base36Digits[v%36]
		v /= 36
	}
	return string(buf[i:])
}
