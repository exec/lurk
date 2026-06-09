// Package server — auth.go: PBKDF2-HMAC-SHA256 bouncer-password hash/verify.
//
// The hash format is self-describing and version-safe:
//
//	pbkdf2-sha256:<iter>:<saltB64>:<dkB64>
//
// Per §6.1 of docs/LURKD-DESIGN.md and [B#1]:
//   - 16-byte random salt from crypto/rand
//   - 32-byte derived key
//   - 600000 iterations (OWASP guidance for PBKDF2-HMAC-SHA256)
//   - constant-time compare via crypto/subtle
//   - stdlib-only (no golang.org/x/crypto imports)
//
// Passwords and decoded SASL payloads are NEVER logged or included in error
// messages returned to callers.
package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// pbkdf2Prefix is the algorithm identifier prefix that begins every hash
// string produced by HashPassword. VerifyPassword rejects any other prefix.
const pbkdf2Prefix = "pbkdf2-sha256"

// kdfGate bounds concurrent PBKDF2 computations in the server's SASL
// verification path to one at a time. Each verification costs a full
// 600k-iteration PBKDF2 derivation (~hundreds of milliseconds of CPU);
// without a bound, an unauthenticated peer could open connections up to the
// session cap and churn AUTHENTICATE attempts to burn a core per connection.
// Serializing the KDF caps that at one core regardless of connection count.
// Authentication is rare on a single-user bouncer, so queueing here is
// harmless; the constant-time properties of VerifyPassword are unaffected
// (every attempt still runs the full KDF — no username fast-path).
var kdfGate = make(chan struct{}, 1)

// verifyPasswordGated is VerifyPassword behind kdfGate. The server's SASL
// path uses this instead of calling VerifyPassword directly so concurrent
// hostile AUTHENTICATE bursts cannot multiply KDF CPU cost (see kdfGate).
func verifyPasswordGated(hash, pw string) (bool, error) {
	kdfGate <- struct{}{}
	defer func() { <-kdfGate }()
	return VerifyPassword(hash, pw)
}

// pbkdf2Iters is the iteration count for new hashes. 600 000 is the OWASP
// recommended minimum for PBKDF2-HMAC-SHA256 as of 2023.
const pbkdf2Iters = 600_000

// pbkdf2SaltLen is the number of random salt bytes.
const pbkdf2SaltLen = 16

// pbkdf2KeyLen is the derived-key length in bytes.
const pbkdf2KeyLen = 32

// HashPassword derives a PBKDF2-HMAC-SHA256 hash for pw and returns the
// self-describing string suitable for storage in Config.BouncerAuth.PasswordHash.
// It reads a fresh 16-byte salt from crypto/rand for every call.
//
// The returned string has the form:
//
//	pbkdf2-sha256:<iter>:<saltB64>:<dkB64>
//
// where both base64 fields use standard base64 (not url-safe), with padding.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("server: hash password: generate salt: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iters, pbkdf2KeyLen)
	if err != nil {
		return "", fmt.Errorf("server: hash password: derive key: %w", err)
	}
	saltB64 := base64.StdEncoding.EncodeToString(salt)
	dkB64 := base64.StdEncoding.EncodeToString(dk)
	return fmt.Sprintf("%s:%d:%s:%s", pbkdf2Prefix, pbkdf2Iters, saltB64, dkB64), nil
}

// VerifyPassword checks pw against the stored hash produced by HashPassword.
// It returns (true, nil) if and only if the password is correct. It returns
// (false, nil) for a wrong-but-otherwise-valid password, and (false, error) for
// a malformed or unsupported hash string.
//
// The comparison is constant-time with respect to the derived key so that the
// verifier does not leak whether the hashes share a common prefix via timing.
// The iteration count is taken from the stored hash, which allows safe
// iteration-count upgrades without re-hashing existing credentials.
func VerifyPassword(hash, pw string) (bool, error) {
	// Split into exactly four colon-separated fields.
	parts := strings.SplitN(hash, ":", 4)
	if len(parts) != 4 {
		return false, fmt.Errorf("server: verify password: malformed hash (wrong field count)")
	}
	algo, iterStr, saltB64, dkB64 := parts[0], parts[1], parts[2], parts[3]

	if algo != pbkdf2Prefix {
		return false, fmt.Errorf("server: verify password: unsupported algorithm %q (only pbkdf2-sha256 is accepted)", algo)
	}

	iter, err := strconv.Atoi(iterStr)
	if err != nil || iter <= 0 {
		return false, fmt.Errorf("server: verify password: invalid iteration count")
	}

	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return false, fmt.Errorf("server: verify password: invalid salt encoding")
	}

	storedDK, err := base64.StdEncoding.DecodeString(dkB64)
	if err != nil {
		return false, fmt.Errorf("server: verify password: invalid dk encoding")
	}
	if len(storedDK) == 0 {
		return false, fmt.Errorf("server: verify password: empty stored key")
	}

	// Derive the candidate key with the stored parameters.
	candidateDK, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(storedDK))
	if err != nil {
		return false, fmt.Errorf("server: verify password: derive key: %w", err)
	}

	// Constant-time comparison — must not short-circuit on first differing byte.
	if subtle.ConstantTimeCompare(candidateDK, storedDK) != 1 {
		return false, nil
	}
	return true, nil
}
