package sasl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// scram256Mechanism implements SASL SCRAM-SHA-256 (RFC 5802 + RFC 7677).
//
// SCRAM is a challenge/response protocol in which the client and server
// exchange nonces and a salted key derivation so neither the raw password nor
// a reversible encoding of it is ever sent across the wire. Even over a
// plaintext connection the server learns only a keyed proof, not the password
// itself — a meaningful security improvement over SASL PLAIN.
//
// Exchange (four messages, two client, two server):
//
//  1. client-first-message  → "n,,n=<user>,r=<cnonce>"
//  2. server-first-message  ← "r=<cnonce><snonce>,s=<salt-b64>,i=<iters>"
//  3. client-final-message  → "c=<gs2-b64>,r=<cnonce><snonce>,p=<proof-b64>"
//  4. server-final-message  ← "v=<server-sig-b64>"
//
// The GS2 header is "n,," (no channel binding), corresponding to the plain
// "SCRAM-SHA-256" mechanism name (not "SCRAM-SHA-256-PLUS").
// The server signature in step 4 is verified to detect rogue authenticators.
//
// Implemented per:
//   - RFC 5802 §3 — SCRAM algorithm
//   - RFC 7677 §3 — SCRAM-SHA-256 mechanism name and hash binding
type scram256Mechanism struct {
	username string
	password string

	// clientNonce is randomly generated in Start and threaded through steps.
	clientNonce string

	// clientFirstMessageBare is the "n=...,r=..." part (no GS2 header), kept
	// for inclusion in the AuthMessage computation in step 3.
	clientFirstMessageBare string

	// serverFirstMessage is the verbatim step-2 server message, also needed
	// in the AuthMessage.
	serverFirstMessage string

	// expectedServerSig is the server signature computed in step 3; it is
	// compared against the "v=" attribute in step 4 to verify the server.
	expectedServerSig []byte

	// state: 0=pre-start, 1=awaiting-server-first, 2=awaiting-server-final.
	state int
}

// Scram returns a SASL SCRAM-SHA-256 [RFC 5802 / RFC 7677] Mechanism for the
// given username and password. No channel binding is used ("n,," GS2 header),
// so the mechanism negotiates as "SCRAM-SHA-256" (not "SCRAM-SHA-256-PLUS").
//
// SCRAM is recommended over PLAIN when available: it never transmits the
// password in any recoverable form, even on a plaintext link.
func Scram(username, password string) Mechanism {
	return &scram256Mechanism{username: username, password: password}
}

func (s *scram256Mechanism) Name() string { return "SCRAM-SHA-256" }

// Start produces the client-first-message as the initial response (the
// Conversation driver sends this in reply to the server's first AUTHENTICATE
// + challenge). The message is "n,,n=<user>,r=<cnonce>".
func (s *scram256Mechanism) Start() ([]byte, bool) {
	s.clientNonce = scramNonce()
	s.clientFirstMessageBare = "n=" + scramEncodeUsername(s.username) + ",r=" + s.clientNonce
	s.state = 1
	return []byte("n,," + s.clientFirstMessageBare), true
}

// Next processes the server's multi-step challenge:
//   - state 1: server-first-message → derive keys, emit client-final-message.
//   - state 2: server-final-message → verify server signature, signal done.
func (s *scram256Mechanism) Next(challenge []byte) ([]byte, error) {
	switch s.state {
	case 1:
		return s.serverFirst(string(challenge))
	case 2:
		return s.serverFinal(string(challenge))
	default:
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 unexpected challenge in state %d", s.state)
	}
}

// serverFirst handles the server-first-message (step 2). It:
//  1. Parses r=, s=, i= attributes.
//  2. Verifies the server nonce starts with the client nonce.
//  3. Derives SaltedPassword via Hi (PBKDF2-HMAC-SHA-256).
//  4. Computes ClientProof and ServerSignature.
//  5. Returns the client-final-message with the embedded proof.
func (s *scram256Mechanism) serverFirst(msg string) ([]byte, error) {
	s.serverFirstMessage = msg

	attrs, err := scramParseAttrs(msg)
	if err != nil {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-first-message: %w", err)
	}

	// RFC 5802 §7: if the server-first-message contains an 'm' attribute
	// (mandatory extension), the client MUST fail authentication — it does not
	// understand the extension and must not proceed.
	if ext, ok := attrs["m"]; ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server requires unsupported mandatory extension: %q", ext)
	}

	fullNonce, ok := attrs["r"]
	if !ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-first-message missing 'r'")
	}
	if !strings.HasPrefix(fullNonce, s.clientNonce) {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server nonce does not begin with client nonce")
	}

	saltB64, ok := attrs["s"]
	if !ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-first-message missing 's'")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 invalid salt: %w", err)
	}

	iterStr, ok := attrs["i"]
	if !ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-first-message missing 'i'")
	}
	iters, err := scramParsePositiveInt(iterStr)
	if err != nil {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 invalid iteration count: %w", err)
	}

	// RFC 5802 §3 key derivation.
	saltedPassword := scramHi([]byte(s.password), salt, iters)
	clientKey := scramHMAC(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(saltedPassword, []byte("Server Key"))

	// client-final-message-without-proof: GS2 header base64, nonce.
	// GS2 header for "no channel binding" is the literal bytes "n,,".
	gs2Header := base64.StdEncoding.EncodeToString([]byte("n,,"))
	cfmWithoutProof := "c=" + gs2Header + ",r=" + fullNonce

	// AuthMessage = client-first-message-bare + "," + server-first-message + "," + cfmWithoutProof
	authMessage := s.clientFirstMessageBare + "," + s.serverFirstMessage + "," + cfmWithoutProof

	clientSig := scramHMAC(storedKey[:], []byte(authMessage))
	clientProof := make([]byte, len(clientKey))
	for i := range clientKey {
		clientProof[i] = clientKey[i] ^ clientSig[i]
	}

	// Precompute the expected server signature so we can verify step 4.
	s.expectedServerSig = scramHMAC(serverKey, []byte(authMessage))

	cfm := cfmWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)
	s.state = 2
	return []byte(cfm), nil
}

// serverFinal handles the server-final-message (step 4). It verifies the
// server signature to confirm the server holds the same credential. A
// mismatch means the server cannot be trusted (wrong password hash store,
// MITM, or rogue authenticator).
func (s *scram256Mechanism) serverFinal(msg string) ([]byte, error) {
	s.state = 3
	attrs, err := scramParseAttrs(msg)
	if err != nil {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-final-message: %w", err)
	}
	// An 'e' attribute signals a server-side error.
	if e, ok := attrs["e"]; ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server error: %s", e)
	}
	vB64, ok := attrs["v"]
	if !ok {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server-final-message missing 'v'")
	}
	serverSig, err := base64.StdEncoding.DecodeString(vB64)
	if err != nil {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 invalid server signature base64: %w", err)
	}
	if !hmac.Equal(serverSig, s.expectedServerSig) {
		return nil, fmt.Errorf("sasl: SCRAM-SHA-256 server signature verification failed")
	}
	// No bytes to send; the Conversation layer will receive 903 next.
	return nil, nil
}

// scramHMAC computes HMAC-SHA-256(key, data).
func scramHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// scramHi is the RFC 5802 §2.2 Hi(str, salt, i) function — PBKDF2 with
// HMAC-SHA-256 and a single output block. It derives the salted password from
// the plaintext password, a server-supplied salt, and an iteration count.
func scramHi(password, salt []byte, iterations int) []byte {
	// U1 = HMAC(password, salt || INT(1))
	mac := hmac.New(sha256.New, password)
	mac.Write(salt)
	mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	result := make([]byte, len(u))
	copy(result, u)
	// Ui = HMAC(password, U(i-1)); accumulate XOR.
	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range result {
			result[j] ^= u[j]
		}
	}
	return result
}

// scramNonce generates a random 18-byte value encoded as base64url (24 chars).
// It panics only when the system random source is broken, which is
// unrecoverable for any security-sensitive operation.
func scramNonce() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic("sasl: SCRAM-SHA-256: failed to generate nonce: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// scramEncodeUsername applies RFC 5802 §5.1 attribute-value encoding for the
// username field: "=" → "=3D" and "," → "=2C". Full SASLprep / Unicode
// normalization (RFC 4013) is NOT implemented; only ASCII-clean usernames are
// supported, which covers the overwhelming majority of IRC accounts.
func scramEncodeUsername(u string) string {
	u = strings.ReplaceAll(u, "=", "=3D")
	u = strings.ReplaceAll(u, ",", "=2C")
	return u
}

// scramParseAttrs parses a SCRAM attribute string "k=v,k=v,..." into a map.
// Each token must have exactly one '=' with a single-character key. Extra '='
// signs in the value are allowed (base64 padding, error messages).
func scramParseAttrs(s string) (map[string]string, error) {
	attrs := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || len(k) != 1 {
			return nil, fmt.Errorf("malformed attribute %q", part)
		}
		attrs[k] = v
	}
	return attrs, nil
}

// Iteration count bounds for SCRAM-SHA-256.
//
// minScramIters enforces the RFC 7677 §3 mandatory minimum of 4096 iterations.
// A server advertising fewer is either misconfigured or attempting to weaken the
// key derivation to allow an offline brute-force attack.
//
// maxScramIters caps the upper bound to prevent a malicious server from
// advertising an astronomically large count (e.g. 99999999999999) that would
// cause scramHi to spin for an impractical duration, effectively a DoS. One
// million iterations is already well beyond any legitimate deployment.
const (
	minScramIters = 4096
	maxScramIters = 1_000_000
)

// scramParsePositiveInt parses a decimal integer string, enforcing
// minScramIters ≤ n ≤ maxScramIters for the SCRAM iteration count.
func scramParsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit character %q", c)
		}
		n = n*10 + int(c-'0')
		// Guard against overflow before multiplying again next iteration: if n
		// already exceeds maxScramIters, stop accumulating (the final check will
		// reject it). This also prevents int overflow on a very long digit string.
		if n > maxScramIters {
			return 0, fmt.Errorf("iteration count %d exceeds maximum %d", n, maxScramIters)
		}
	}
	if n < minScramIters {
		return 0, fmt.Errorf("iteration count %d is below RFC 7677 minimum %d", n, minScramIters)
	}
	return n, nil
}
