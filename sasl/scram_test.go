package sasl

import (
	"encoding/base64"
	"strings"
	"testing"
)

// RFC 7677 §3 test vector. Fixed inputs allow deterministic verification of
// every computed intermediate value against the published expected outputs.
//
//	user:       user
//	password:   pencil
//	c-nonce:    rOprNGfwEbeRWgbNEkqO
//	s-nonce:    %hvYDpWUa2RaTCAfuxFIlj)hNlF$k0
//	salt:       W22ZaJ0SNY7soEsUEjb6gQ== (base64)
//	iterations: 4096
//
//	SaltedPassword: xKSVEDI6tPlSysH6mUQZOeeOp01r6B3fcJbodRPcYV0=
//	ClientProof:    dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=
//	ServerSig:      6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=

const (
	rfc7677ClientNonce  = "rOprNGfwEbeRWgbNEkqO"
	rfc7677ServerNonce  = "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	rfc7677FullNonce    = rfc7677ClientNonce + rfc7677ServerNonce
	rfc7677SaltB64      = "W22ZaJ0SNY7soEsUEjb6gQ=="
	rfc7677Iters        = "4096"
	rfc7677ClientProof  = "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfc7677ServerSigB64 = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

// newRFC7677Scram returns a scram256Mechanism pre-seeded with the RFC 7677
// fixed nonce (bypassing the random generator). State is set to 1, matching
// what Start() would leave, so Next can be called directly for vector tests.
func newRFC7677Scram() *scram256Mechanism {
	m := &scram256Mechanism{username: "user", password: "pencil"}
	m.clientNonce = rfc7677ClientNonce
	m.clientFirstMessageBare = "n=user,r=" + rfc7677ClientNonce
	m.state = 1
	return m
}

// rfc7677ServerFirst returns the RFC 7677 server-first-message.
func rfc7677ServerFirst() string {
	return "r=" + rfc7677FullNonce + ",s=" + rfc7677SaltB64 + ",i=" + rfc7677Iters
}

func TestScramName(t *testing.T) {
	if got := Scram("u", "p").Name(); got != "SCRAM-SHA-256" {
		t.Errorf("Name() = %q, want SCRAM-SHA-256", got)
	}
}

// TestScramStartProducesClientFirst verifies Start() emits a well-formed
// client-first-message with the required GS2 header and a non-empty nonce.
func TestScramStartProducesClientFirst(t *testing.T) {
	m := Scram("user", "pencil").(*scram256Mechanism)
	payload, hasInitial := m.Start()
	if !hasInitial {
		t.Fatal("Start() hasInitial=false; want true")
	}
	msg := string(payload)
	const wantPrefix = "n,,n=user,r="
	if !strings.HasPrefix(msg, wantPrefix) {
		t.Errorf("client-first = %q; want prefix %q", msg, wantPrefix)
	}
	if strings.TrimPrefix(msg, wantPrefix) == "" {
		t.Error("client-first has empty nonce")
	}
}

// TestScramUsernameEncoding verifies RFC 5802 §5.1 attribute-value encoding.
func TestScramUsernameEncoding(t *testing.T) {
	tests := []struct{ in, want string }{
		{"alice", "alice"},
		{"a=b", "a=3Db"},
		{"a,b", "a=2Cb"},
		{"a=,b", "a=3D=2Cb"},
	}
	for _, tt := range tests {
		if got := scramEncodeUsername(tt.in); got != tt.want {
			t.Errorf("scramEncodeUsername(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestScramHiRFC7677 verifies the Hi (PBKDF2-HMAC-SHA-256) function against
// the SaltedPassword value that is implied by the RFC 7677 §3 test vector (the
// RFC publishes ClientProof and ServerSig; SaltedPassword is the root from
// which both are derived).
func TestScramHiRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString(rfc7677SaltB64)
	got := scramHi([]byte("pencil"), salt, 4096)
	const want = "xKSVEDI6tPlSysH6mUQZOeeOp01r6B3fcJbodRPcYV0="
	if base64.StdEncoding.EncodeToString(got) != want {
		t.Errorf("Hi(SaltedPassword) = %s, want %s",
			base64.StdEncoding.EncodeToString(got), want)
	}
}

// TestScramServerFirstRFC7677 verifies the client-final-message produced by
// serverFirst() against the RFC 7677 §3 ClientProof test vector.
func TestScramServerFirstRFC7677(t *testing.T) {
	m := newRFC7677Scram()
	resp, err := m.Next([]byte(rfc7677ServerFirst()))
	if err != nil {
		t.Fatalf("Next (server-first): %v", err)
	}
	cfm := string(resp)

	wantPrefix := "c=biws,r=" + rfc7677FullNonce + ","
	if !strings.HasPrefix(cfm, wantPrefix) {
		t.Errorf("client-final prefix = %q, want prefix %q", cfm, wantPrefix)
	}
	pIdx := strings.LastIndex(cfm, ",p=")
	if pIdx < 0 {
		t.Fatalf("client-final missing ',p=': %q", cfm)
	}
	if got := cfm[pIdx+3:]; got != rfc7677ClientProof {
		t.Errorf("ClientProof = %q, want %q", got, rfc7677ClientProof)
	}
}

// TestScramServerFinalRFC7677 verifies that the correct RFC 7677 server
// signature is accepted without error.
func TestScramServerFinalRFC7677(t *testing.T) {
	m := newRFC7677Scram()
	if _, err := m.Next([]byte(rfc7677ServerFirst())); err != nil {
		t.Fatalf("Next (server-first): %v", err)
	}
	resp, err := m.Next([]byte("v=" + rfc7677ServerSigB64))
	if err != nil {
		t.Errorf("Next (server-final): unexpected error: %v", err)
	}
	if resp != nil {
		t.Errorf("Next (server-final): expected nil response, got %q", resp)
	}
}

// TestScramServerFinalBadSig verifies that a tampered server signature is
// rejected, detecting a rogue authenticator or wrong credential store.
func TestScramServerFinalBadSig(t *testing.T) {
	m := newRFC7677Scram()
	if _, err := m.Next([]byte(rfc7677ServerFirst())); err != nil {
		t.Fatalf("Next (server-first): %v", err)
	}
	sigBytes, _ := base64.StdEncoding.DecodeString(rfc7677ServerSigB64)
	sigBytes[0] ^= 0xFF
	badSig := base64.StdEncoding.EncodeToString(sigBytes)
	if _, err := m.Next([]byte("v=" + badSig)); err == nil {
		t.Error("expected error for bad server signature, got nil")
	}
}

// TestScramServerFinalError verifies that a server-final 'e' attribute is
// surfaced as an error.
func TestScramServerFinalError(t *testing.T) {
	m := newRFC7677Scram()
	if _, err := m.Next([]byte(rfc7677ServerFirst())); err != nil {
		t.Fatalf("Next (server-first): %v", err)
	}
	_, err := m.Next([]byte("e=unknown-user"))
	if err == nil {
		t.Fatal("expected error for e= in server-final, got nil")
	}
	if !strings.Contains(err.Error(), "unknown-user") {
		t.Errorf("error should include server error text, got %q", err)
	}
}

// TestScramNonceMismatch verifies that a server nonce that doesn't start with
// the client nonce is rejected before any key derivation.
func TestScramNonceMismatch(t *testing.T) {
	m := newRFC7677Scram()
	bad := "r=WRONGNONCE,s=" + rfc7677SaltB64 + ",i=" + rfc7677Iters
	if _, err := m.Next([]byte(bad)); err == nil {
		t.Error("expected error for nonce mismatch, got nil")
	}
}

// TestScramUnexpectedState verifies that Next in state 3 (after server-final)
// returns an error rather than silently proceeding.
func TestScramUnexpectedState(t *testing.T) {
	m := newRFC7677Scram()
	if _, err := m.Next([]byte(rfc7677ServerFirst())); err != nil {
		t.Fatalf("Next (server-first): %v", err)
	}
	if _, err := m.Next([]byte("v=" + rfc7677ServerSigB64)); err != nil {
		t.Fatalf("Next (server-final): %v", err)
	}
	if _, err := m.Next([]byte("anything")); err == nil {
		t.Error("expected error for Next in terminal state, got nil")
	}
}

// TestScramConversationFlow drives a complete SCRAM-SHA-256 exchange through
// the Conversation driver, mirroring how the client run-loop uses it:
//
//  1. Begin()       → "AUTHENTICATE SCRAM-SHA-256"
//  2. Receive("+")  → AUTHENTICATE <client-first-b64>
//  3. Receive(sf)   → AUTHENTICATE <client-final-b64>   (sf = server-first-b64)
//  4. Receive(vf)   → bad sig → error
//
// A correct server signature is tested separately in TestScramServerFinalRFC7677
// against the fixed RFC 7677 vectors; here we exercise the Conversation wrapper
// and verify step 4 rejects a bogus signature.
func TestScramConversationFlow(t *testing.T) {
	conv := NewConversation(Scram("user", "pencil"))

	begin := conv.Begin()
	if len(begin) != 1 || begin[0] != "AUTHENTICATE SCRAM-SHA-256" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE SCRAM-SHA-256]", begin)
	}

	// Step 2: server challenges with "+"; client sends client-first-message.
	lines, done, err := conv.Receive(authMsg("+"))
	if err != nil || done {
		t.Fatalf("after AUTHENTICATE +: done=%v err=%v", done, err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 AUTHENTICATE line, got %d: %v", len(lines), lines)
	}
	cfmB64 := strings.TrimPrefix(lines[0], "AUTHENTICATE ")
	cfmBytes, err := base64.StdEncoding.DecodeString(cfmB64)
	if err != nil {
		t.Fatalf("client-first not valid base64: %v", err)
	}
	cfm := string(cfmBytes)
	if !strings.HasPrefix(cfm, "n,,n=user,r=") {
		t.Fatalf("client-first = %q; want prefix n,,n=user,r=", cfm)
	}
	// Extract the client nonce to build a realistic server-first-message.
	cnonce := strings.TrimPrefix(cfm, "n,,n=user,r=")
	if cnonce == "" {
		t.Fatal("empty client nonce")
	}

	// Step 3: server sends server-first-message (base64-encoded AUTHENTICATE).
	sfPayload := "r=" + cnonce + "ServerNonce,s=" + rfc7677SaltB64 + ",i=" + rfc7677Iters
	sfB64 := base64.StdEncoding.EncodeToString([]byte(sfPayload))
	lines, done, err = conv.Receive(authMsg(sfB64))
	if err != nil || done {
		t.Fatalf("after server-first: done=%v err=%v", done, err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 AUTHENTICATE line, got %d: %v", len(lines), lines)
	}
	cfinalB64 := strings.TrimPrefix(lines[0], "AUTHENTICATE ")
	cfinalBytes, err := base64.StdEncoding.DecodeString(cfinalB64)
	if err != nil {
		t.Fatalf("client-final not valid base64: %v", err)
	}
	if !strings.Contains(string(cfinalBytes), ",p=") {
		t.Fatalf("client-final missing proof: %q", cfinalBytes)
	}

	// Step 4: server sends a bad server-final-message; must be rejected.
	badFinal := base64.StdEncoding.EncodeToString([]byte("v=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="))
	_, _, err = conv.Receive(authMsg(badFinal))
	if err == nil {
		t.Error("expected error for bad server signature, got nil")
	}
}

// TestScramConversationServerError verifies that an 'e=' attribute in the
// server-final-message surfaces as an error through the Conversation layer.
func TestScramConversationServerError(t *testing.T) {
	conv := NewConversation(Scram("user", "pencil"))
	conv.Begin()
	lines, _, _ := conv.Receive(authMsg("+"))

	cfmBytes, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(lines[0], "AUTHENTICATE "))
	cnonce := strings.TrimPrefix(string(cfmBytes), "n,,n=user,r=")

	sfPayload := "r=" + cnonce + "SN,s=" + rfc7677SaltB64 + ",i=4096"
	conv.Receive(authMsg(base64.StdEncoding.EncodeToString([]byte(sfPayload))))

	errorFinal := base64.StdEncoding.EncodeToString([]byte("e=invalid-proof"))
	_, _, err := conv.Receive(authMsg(errorFinal))
	if err == nil {
		t.Fatal("expected error for e= in server-final, got nil")
	}
	if !strings.Contains(err.Error(), "invalid-proof") {
		t.Errorf("error should carry server error text, got %q", err)
	}
}

// TestScramParseAttrs exercises the SCRAM attribute string parser.
func TestScramParseAttrs(t *testing.T) {
	tests := []struct {
		input   string
		wantKey string
		wantVal string
		wantErr bool
	}{
		{"r=abc123,s=salt==,i=4096", "r", "abc123", false},
		{"v=abc=def=", "v", "abc=def=", false},
		{"", "", "", false}, // empty string → empty map, no error
	}
	for _, tt := range tests {
		attrs, err := scramParseAttrs(tt.input)
		if tt.wantErr && err == nil {
			t.Errorf("scramParseAttrs(%q): expected error", tt.input)
			continue
		}
		if !tt.wantErr && err != nil {
			t.Errorf("scramParseAttrs(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if tt.wantKey != "" {
			if got, ok := attrs[tt.wantKey]; !ok || got != tt.wantVal {
				t.Errorf("scramParseAttrs(%q)[%q] = %q, want %q", tt.input, tt.wantKey, got, tt.wantVal)
			}
		}
	}
}

// TestScramParsePositiveInt exercises the iteration-count parser.
func TestScramParsePositiveInt(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"4096", 4096, false},
		{"1", 1, false},
		{"0", 0, true},  // must be >= 1
		{"-1", 0, true}, // leading '-' is not a digit
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		got, err := scramParsePositiveInt(tt.input)
		if tt.wantErr && err == nil {
			t.Errorf("scramParsePositiveInt(%q): expected error", tt.input)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("scramParsePositiveInt(%q): unexpected error: %v", tt.input, err)
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("scramParsePositiveInt(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
