package sasl

import "encoding/base64"

// maxChunk is the maximum number of base64 characters carried in a single
// AUTHENTICATE line's payload, per the SASL spec.
const maxChunk = 400

// Encode base64-encodes a raw SASL payload and returns the sequence of
// AUTHENTICATE lines that transmit it, applying the IRCv3 ≤400-byte chunking
// rule (including the trailing "AUTHENTICATE +" when the encoded length is an
// exact multiple of 400, and the single "AUTHENTICATE +" for an empty payload).
//
// Most callers should use a Conversation/Session instead, which calls this
// internally. Encode is exported for callers that drive Mechanism.Start/Next
// themselves and only need the line-encoding rule.
func Encode(payload []byte) []string {
	return chunkResponse(payload)
}

// chunkResponse encodes a raw client response and splits it into the sequence
// of AUTHENTICATE lines required to transmit it, following the IRCv3 chunking
// rules:
//
//   - The response is base64-encoded.
//   - The encoded payload is split into pieces of at most 400 bytes, each sent
//     as "AUTHENTICATE <piece>".
//   - If the encoded length is an exact non-zero multiple of 400, the final
//     400-byte piece is ambiguous (the server cannot tell whether more follows),
//     so a terminating "AUTHENTICATE +" is appended.
//   - An empty response (encoded length 0) is sent as a single "AUTHENTICATE +".
//
// The returned slice always contains at least one line. The trailing-"+" and
// empty-response rules were cross-checked against ircutils.EncodeSASLResponse in
// ergochat/irc-go (the chunker Ergo's server uses); the boundary behavior is
// identical (e.g. an exact-multiple-of-400 payload appends a terminating "+").
func chunkResponse(response []byte) []string {
	encoded := base64.StdEncoding.EncodeToString(response)
	if len(encoded) == 0 {
		return []string{"AUTHENTICATE +"}
	}

	var lines []string
	for len(encoded) >= maxChunk {
		lines = append(lines, "AUTHENTICATE "+encoded[:maxChunk])
		encoded = encoded[maxChunk:]
	}
	if len(encoded) > 0 {
		// A non-empty remainder shorter than maxChunk signals the end by being
		// under the chunk size; no trailing "+" is needed.
		lines = append(lines, "AUTHENTICATE "+encoded)
	} else {
		// The payload divided evenly into maxChunk-sized pieces, so the last full
		// chunk is indistinguishable from a continuation. Terminate explicitly.
		lines = append(lines, "AUTHENTICATE +")
	}
	return lines
}
