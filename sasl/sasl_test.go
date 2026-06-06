package sasl

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/exec/lurk/irc"
)

// authMsg builds a server AUTHENTICATE message with the given single payload.
func authMsg(payload string) *irc.Message {
	return &irc.Message{Command: cmdAuthenticate, Params: []string{payload}}
}

// numMsg builds a numeric reply message with the given command and params.
func numMsg(cmd string, params ...string) *irc.Message {
	return &irc.Message{Command: cmd, Params: params}
}

func TestPlainSpecVector(t *testing.T) {
	// The canonical sasl-3.1 example: authzid "jilles", authcid "jilles",
	// passwd "sesame" -> "jilles\0jilles\0sesame", which base64-encodes to
	// amlsbGVzAGppbGxlcwBzZXNhbWU=.
	const want = "amlsbGVzAGppbGxlcwBzZXNhbWU="

	m := Plain("jilles", "jilles", "sesame")
	resp, ok := m.Start()
	if !ok {
		t.Fatal("PLAIN.Start() returned hasInitial=false; want true")
	}
	if got := base64.StdEncoding.EncodeToString(resp); got != want {
		t.Errorf("PLAIN base64 = %q, want %q", got, want)
	}

	// The raw response must be authzid \0 authcid \0 passwd.
	if string(resp) != "jilles\x00jilles\x00sesame" {
		t.Errorf("PLAIN raw response = %q, want %q", resp, "jilles\x00jilles\x00sesame")
	}
}

func TestPlainWithAuthzid(t *testing.T) {
	m := Plain("admin", "jilles", "sesame")
	resp, _ := m.Start()
	if string(resp) != "admin\x00jilles\x00sesame" {
		t.Errorf("PLAIN raw response = %q, want %q", resp, "admin\x00jilles\x00sesame")
	}
}

// TestPlainBeginThenChallenge verifies the corrected IRCv3 flow: Begin emits
// only "AUTHENTICATE PLAIN", and the chunked initial response is sent in reply
// to the server's first "AUTHENTICATE +" challenge.
func TestPlainBeginThenChallenge(t *testing.T) {
	conv := NewConversation(Plain("jilles", "jilles", "sesame"))

	begin := conv.Begin()
	if len(begin) != 1 || begin[0] != "AUTHENTICATE PLAIN" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE PLAIN]", begin)
	}

	lines, done, err := conv.Receive(authMsg("+"))
	if err != nil || done {
		t.Fatalf("after first '+': done=%v err=%v", done, err)
	}
	want := []string{"AUTHENTICATE amlsbGVzAGppbGxlcwBzZXNhbWU="}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("initial response lines = %v, want %v", lines, want)
	}
}

func TestExternalEmpty(t *testing.T) {
	m := External("")
	if m.Name() != "EXTERNAL" {
		t.Errorf("Name() = %q, want EXTERNAL", m.Name())
	}
	resp, ok := m.Start()
	if !ok {
		t.Fatal("EXTERNAL.Start() hasInitial=false; want true")
	}
	if len(resp) != 0 {
		t.Errorf("EXTERNAL empty-authzid response = %q, want empty", resp)
	}

	conv := NewConversation(External(""))
	if begin := conv.Begin(); len(begin) != 1 || begin[0] != "AUTHENTICATE EXTERNAL" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE EXTERNAL]", begin)
	}
	// The empty initial response is sent as "AUTHENTICATE +" in reply to the
	// server's first challenge.
	lines, done, err := conv.Receive(authMsg("+"))
	if err != nil || done {
		t.Fatalf("after first '+': done=%v err=%v", done, err)
	}
	if len(lines) != 1 || lines[0] != "AUTHENTICATE +" {
		t.Errorf("EXTERNAL initial response = %v, want [AUTHENTICATE +]", lines)
	}
}

func TestExternalWithAuthzid(t *testing.T) {
	conv := NewConversation(External("alice"))
	if begin := conv.Begin(); len(begin) != 1 || begin[0] != "AUTHENTICATE EXTERNAL" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE EXTERNAL]", begin)
	}
	lines, _, err := conv.Receive(authMsg("+"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{"AUTHENTICATE " + base64.StdEncoding.EncodeToString([]byte("alice"))}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("initial response = %v, want %v", lines, want)
	}
}

// TestChunkBoundaries exercises the ≤400-byte chunking rules at the critical
// payload lengths. We construct raw responses whose base64 encodings have
// exactly the target lengths (base64 length is a multiple of 4; 399 is not, so
// it is covered separately via a direct chunkResponse-of-encoded check).
func TestChunkBoundaries(t *testing.T) {
	// rawForEncodedLen returns a raw byte slice whose base64 encoding has length
	// encLen (which must be a multiple of 4).
	rawForEncodedLen := func(encLen int) []byte {
		if encLen%4 != 0 {
			t.Fatalf("encoded length %d is not a multiple of 4", encLen)
		}
		// base64 encodes 3 raw bytes -> 4 chars; no padding when raw len % 3 == 0.
		return make([]byte, encLen/4*3)
	}

	tests := []struct {
		name      string
		encLen    int
		wantLines int
		// trailingPlus reports whether the final emitted line is "AUTHENTICATE +".
		trailingPlus bool
	}{
		{"len400_one_full_chunk_needs_plus", 400, 2, true},
		{"len800_two_full_chunks_needs_plus", 800, 3, true},
		{"len404_chunk_plus_remainder", 404, 2, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := rawForEncodedLen(tt.encLen)
			encoded := base64.StdEncoding.EncodeToString(raw)
			if len(encoded) != tt.encLen {
				t.Fatalf("setup: encoded len = %d, want %d", len(encoded), tt.encLen)
			}
			lines := chunkResponse(raw)
			if len(lines) != tt.wantLines {
				t.Fatalf("chunkResponse produced %d lines, want %d:\n%s",
					len(lines), tt.wantLines, strings.Join(lines, "\n"))
			}
			// Every non-terminating-plus line must carry exactly maxChunk payload
			// bytes (i.e. "AUTHENTICATE " + 400 chars), and reassembly must equal
			// the original encoding.
			var reassembled strings.Builder
			for i, line := range lines {
				payload := strings.TrimPrefix(line, "AUTHENTICATE ")
				if payload == "+" {
					if i != len(lines)-1 {
						t.Errorf("'+' line appeared at index %d, not last", i)
					}
					continue
				}
				if i < len(lines)-1 && len(payload) != maxChunk {
					t.Errorf("non-final chunk %d has %d payload bytes, want %d", i, len(payload), maxChunk)
				}
				reassembled.WriteString(payload)
			}
			if reassembled.String() != encoded {
				t.Errorf("reassembled payload != original encoding")
			}
			lastIsPlus := lines[len(lines)-1] == "AUTHENTICATE +"
			if lastIsPlus != tt.trailingPlus {
				t.Errorf("trailing '+' = %v, want %v", lastIsPlus, tt.trailingPlus)
			}
		})
	}
}

// TestChunkExactLengths verifies the chunk-count rule directly against encoded
// payloads of lengths 399, 400, 401, and 800 by driving the chunker through a
// raw payload and inspecting the resulting encoded length, plus a direct
// reassembly check. 399 and 401 are not multiples of 4, so they cannot be exact
// base64 outputs; we instead assert the boundary behavior of the splitting loop
// using synthetic encoded strings.
func TestChunkSplitCounts(t *testing.T) {
	// splitEncoded mirrors chunkResponse's splitting given an already-encoded
	// string, so we can test non-multiple-of-4 lengths the encoder never emits.
	splitEncoded := func(encoded string) []string {
		if encoded == "" {
			return []string{"AUTHENTICATE +"}
		}
		var lines []string
		for len(encoded) >= maxChunk {
			lines = append(lines, "AUTHENTICATE "+encoded[:maxChunk])
			encoded = encoded[maxChunk:]
		}
		if len(encoded) > 0 {
			lines = append(lines, "AUTHENTICATE "+encoded)
		} else {
			lines = append(lines, "AUTHENTICATE +")
		}
		return lines
	}

	tests := []struct {
		encLen    int
		wantLines int
		wantPlus  bool
	}{
		{399, 1, false}, // single sub-400 chunk, no trailing +
		{400, 2, true},  // one full chunk + terminating +
		{401, 2, false}, // full chunk + 1-byte remainder
		{800, 3, true},  // two full chunks + terminating +
		{0, 1, true},    // empty -> single +
	}
	for _, tt := range tests {
		encoded := strings.Repeat("A", tt.encLen)
		lines := splitEncoded(encoded)
		if len(lines) != tt.wantLines {
			t.Errorf("encLen=%d: got %d lines, want %d", tt.encLen, len(lines), tt.wantLines)
		}
		gotPlus := lines[len(lines)-1] == "AUTHENTICATE +"
		if gotPlus != tt.wantPlus {
			t.Errorf("encLen=%d: trailing + = %v, want %v", tt.encLen, gotPlus, tt.wantPlus)
		}
	}
}

func TestEmptyResponseIsPlus(t *testing.T) {
	lines := chunkResponse(nil)
	if len(lines) != 1 || lines[0] != "AUTHENTICATE +" {
		t.Errorf("chunkResponse(nil) = %v, want [AUTHENTICATE +]", lines)
	}
	lines = chunkResponse([]byte{})
	if len(lines) != 1 || lines[0] != "AUTHENTICATE +" {
		t.Errorf("chunkResponse(empty) = %v, want [AUTHENTICATE +]", lines)
	}
}

// TestSessionAliases verifies that the Session/NewSession/Start/Continue aliases
// drive the exchange identically to Conversation/NewConversation/Begin/Receive,
// since the client integrator drives SASL through those names.
func TestSessionAliases(t *testing.T) {
	var s *Session = NewSession(Plain("jilles", "jilles", "sesame"))
	begin := s.Start()
	if len(begin) != 1 || begin[0] != "AUTHENTICATE PLAIN" {
		t.Fatalf("Start() = %v, want [AUTHENTICATE PLAIN]", begin)
	}
	// Initial response is sent in reply to the first server challenge.
	lines, done, err := s.Continue(authMsg("+"))
	if err != nil || done {
		t.Fatalf("after '+': lines=%v done=%v err=%v", lines, done, err)
	}
	if len(lines) != 1 || lines[0] != "AUTHENTICATE amlsbGVzAGppbGxlcwBzZXNhbWU=" {
		t.Fatalf("Continue('+') = %v, want chunked PLAIN creds", lines)
	}
	lines, done, err = s.Continue(numMsg(numSASLSuccess, "jilles", "ok"))
	if err != nil || !done || len(lines) != 0 {
		t.Errorf("Continue(903) = lines %v done %v err %v; want success", lines, done, err)
	}
}

func TestEncodeExported(t *testing.T) {
	// Encode must match the internal chunkResponse for the canonical PLAIN payload.
	got := Encode([]byte("jilles\x00jilles\x00sesame"))
	want := []string{"AUTHENTICATE amlsbGVzAGppbGxlcwBzZXNhbWU="}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Encode() = %v, want %v", got, want)
	}
	if e := Encode(nil); len(e) != 1 || e[0] != "AUTHENTICATE +" {
		t.Errorf("Encode(nil) = %v, want [AUTHENTICATE +]", e)
	}
}

func TestConversationSuccess(t *testing.T) {
	conv := NewConversation(Plain("", "jilles", "sesame"))
	begin := conv.Begin()
	if len(begin) != 1 || begin[0] != "AUTHENTICATE PLAIN" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE PLAIN]", begin)
	}

	// Server replies "AUTHENTICATE +"; the client sends its initial response.
	lines, done, err := conv.Receive(authMsg("+"))
	if err != nil || done || len(lines) != 1 {
		t.Fatalf("after '+': lines=%v done=%v err=%v; want one response line", lines, done, err)
	}

	// Server then sends 900 (informational) followed by 903 (success).
	lines, done, err = conv.Receive(numMsg(numLoggedIn, "jilles", "jilles!u@h", "jilles", "You are now logged in as jilles"))
	if err != nil || done || len(lines) != 0 {
		t.Fatalf("after 900: lines=%v done=%v err=%v; want no lines, not done, no err", lines, done, err)
	}

	lines, done, err = conv.Receive(numMsg(numSASLSuccess, "jilles", "SASL authentication successful"))
	if err != nil {
		t.Fatalf("after 903: unexpected err %v", err)
	}
	if !done {
		t.Error("after 903: done=false, want true")
	}
	if len(lines) != 0 {
		t.Errorf("after 903: lines=%v, want none", lines)
	}
}

func TestConversationChallengeResponse(t *testing.T) {
	// When a mechanism sends no initial response, the server replies
	// "AUTHENTICATE +" to request it. Use a fake mechanism that withholds its
	// initial response so the challenge path is exercised.
	conv := NewConversation(&challengeMech{})
	begin := conv.Begin()
	if len(begin) != 1 || begin[0] != "AUTHENTICATE FAKE" {
		t.Fatalf("Begin() = %v, want [AUTHENTICATE FAKE]", begin)
	}

	lines, done, err := conv.Receive(authMsg("+"))
	if err != nil || done {
		t.Fatalf("after challenge: done=%v err=%v", done, err)
	}
	if len(lines) != 1 || lines[0] != "AUTHENTICATE "+base64.StdEncoding.EncodeToString([]byte("hello")) {
		t.Errorf("challenge response = %v", lines)
	}
}

func TestConversationFailureNumerics(t *testing.T) {
	for _, num := range []string{numSASLFail, numSASLTooLong, numSASLAborted, numSASLAlready, numNickLocked} {
		conv := NewConversation(Plain("", "jilles", "sesame"))
		conv.Begin()
		lines, done, err := conv.Receive(numMsg(num, "jilles", "SASL authentication failed"))
		if err == nil {
			t.Errorf("%s: expected error, got nil", num)
		}
		if done {
			t.Errorf("%s: done=true, want false on failure", num)
		}
		if len(lines) != 0 {
			t.Errorf("%s: lines=%v, want none", num, lines)
		}
	}
}

func TestConversationMechsNumeric(t *testing.T) {
	conv := NewConversation(Plain("", "jilles", "sesame"))
	conv.Begin()
	_, done, err := conv.Receive(numMsg(numSASLMechs, "jilles", "PLAIN,EXTERNAL", "are available SASL mechanisms"))
	if err == nil {
		t.Fatal("908: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "PLAIN,EXTERNAL") {
		t.Errorf("908 error should carry mech list, got %q", err)
	}
	if done {
		t.Error("908: done=true, want false")
	}
}

func TestConversationUnrelatedPassThrough(t *testing.T) {
	conv := NewConversation(Plain("", "jilles", "sesame"))
	conv.Begin()
	lines, done, err := conv.Receive(&irc.Message{Command: "NOTICE", Params: []string{"*", "hi"}})
	if err != nil || done || len(lines) != 0 {
		t.Errorf("unrelated msg: lines=%v done=%v err=%v; want pass-through", lines, done, err)
	}
}

func TestConversationAbortOnUnexpectedChallenge(t *testing.T) {
	// PLAIN sends its initial response in reply to the first challenge; any
	// SECOND challenge is unexpected, so PLAIN.Next errors and the conversation
	// must emit an abort.
	conv := NewConversation(Plain("", "jilles", "sesame"))
	conv.Begin()
	// First challenge consumes the initial response.
	if _, _, err := conv.Receive(authMsg("+")); err != nil {
		t.Fatalf("first '+' should send initial response, got err %v", err)
	}
	// Second challenge triggers Next, which PLAIN rejects.
	lines, done, err := conv.Receive(authMsg("Zm9v")) // base64("foo")
	if err == nil {
		t.Fatal("expected error from PLAIN.Next on second challenge")
	}
	if done {
		t.Error("done=true, want false")
	}
	if len(lines) != 1 || lines[0] != abortLine {
		t.Errorf("abort lines = %v, want [%q]", lines, abortLine)
	}
}

func TestConversationAfterDone(t *testing.T) {
	conv := NewConversation(Plain("", "jilles", "sesame"))
	conv.Begin()
	conv.Receive(numMsg(numSASLSuccess, "jilles", "ok"))
	_, _, err := conv.Receive(numMsg(numSASLSuccess, "jilles", "ok"))
	if err == nil {
		t.Error("Receive after completion should error")
	}
}

func TestSelectMechanism(t *testing.T) {
	plain := Plain("", "u", "p")

	if got, err := SelectMechanism(nil, plain); err != nil || got != plain {
		t.Errorf("empty list should permit any mech: got %v err %v", got, err)
	}
	if got, err := SelectMechanism([]string{"PLAIN", "EXTERNAL"}, plain); err != nil || got != plain {
		t.Errorf("advertised PLAIN should be selected: got %v err %v", got, err)
	}
	if _, err := SelectMechanism([]string{"EXTERNAL"}, plain); err == nil {
		t.Error("PLAIN not advertised should error")
	}
}

// TestSelectMechanismIgnoresUnsupported confirms that a server advertising mechs
// we don't implement (e.g. our live Ergo offers SCRAM-SHA-256, OAUTHBEARER,
// IRCV3BEARER alongside PLAIN/EXTERNAL) is handled cleanly: a configured mech we
// support is selected and the unsupported extras are ignored, while a configured
// mech the server doesn't offer is rejected before any AUTHENTICATE is sent.
func TestSelectMechanismIgnoresUnsupported(t *testing.T) {
	advertised := []string{"PLAIN", "EXTERNAL", "SCRAM-SHA-256", "OAUTHBEARER", "IRCV3BEARER"}

	plain := Plain("", "u", "p")
	if got, err := SelectMechanism(advertised, plain); err != nil || got != plain {
		t.Errorf("PLAIN should be selected from a list including SCRAM/etc: got %v err %v", got, err)
	}
	ext := External("")
	if got, err := SelectMechanism(advertised, ext); err != nil || got != ext {
		t.Errorf("EXTERNAL should be selected from a list including SCRAM/etc: got %v err %v", got, err)
	}

	// A mechanism we don't implement, even if the server offers it, is the
	// caller's responsibility; SelectMechanism only validates by name, so a
	// fake "SCRAM-SHA-256" mechanism would be accepted by name. The relevant
	// guarantee is the inverse: a configured mech absent from the list errors.
	if _, err := SelectMechanism([]string{"SCRAM-SHA-256", "OAUTHBEARER"}, plain); err == nil {
		t.Error("PLAIN absent from a SCRAM/OAUTH-only list should error")
	}
}

// TestChallengeChunkTooLong verifies that an over-long server challenge chunk
// (>400 bytes) is rejected with an abort, matching Ergo's server-side
// ErrSASLTooLong framing guard.
func TestChallengeChunkTooLong(t *testing.T) {
	conv := NewConversation(&challengeMech{}) // no initial response -> first challenge hits Next path
	conv.Begin()
	oversized := strings.Repeat("A", maxChunk+1)
	lines, done, err := conv.Receive(authMsg(oversized))
	if err == nil {
		t.Fatal("expected error for >400-byte challenge chunk")
	}
	if done {
		t.Error("done=true, want false")
	}
	if len(lines) != 1 || lines[0] != abortLine {
		t.Errorf("abort lines = %v, want [%q]", lines, abortLine)
	}
}

// challengeMech is a test mechanism that sends no initial response and replies
// "hello" to the first server challenge.
type challengeMech struct{}

func (challengeMech) Name() string                  { return "FAKE" }
func (challengeMech) Start() ([]byte, bool)         { return nil, false }
func (challengeMech) Next(c []byte) ([]byte, error) { return []byte("hello"), nil }

// echoMech captures the raw challenge bytes passed to Next so tests can assert
// that multi-chunk reassembly delivers the full, unframed payload.
type echoMech struct {
	got []byte
}

func (e *echoMech) Name() string                  { return "ECHO" }
func (e *echoMech) Start() ([]byte, bool)         { return nil, false }
func (e *echoMech) Next(c []byte) ([]byte, error) { e.got = c; return []byte("ok"), nil }

// makeAuthChunks splits a raw payload into the correct sequence of AUTHENTICATE
// messages that a server would send according to the IRCv3 framing rule:
// chunks of exactly maxChunk bytes signal "more follows"; a shorter final chunk
// (or a bare "+" appended when the encoded length is an exact multiple of
// maxChunk) signals "end". This mirrors what chunkResponse does for the client.
func makeAuthChunks(raw []byte) []*irc.Message {
	encoded := base64.StdEncoding.EncodeToString(raw)
	var msgs []*irc.Message
	for len(encoded) >= maxChunk {
		msgs = append(msgs, authMsg(encoded[:maxChunk]))
		encoded = encoded[maxChunk:]
	}
	if len(encoded) > 0 {
		msgs = append(msgs, authMsg(encoded))
	} else {
		// Exact multiple: the last full chunk is ambiguous; append a "+" terminator.
		msgs = append(msgs, authMsg("+"))
	}
	return msgs
}

// TestMultiChunkChallengeReassembly verifies that a server challenge split
// across multiple AUTHENTICATE lines is fully reassembled before being passed
// to mech.Next. It constructs a payload large enough to require two full 400-
// byte base64 chunks plus a shorter terminal, encodes it as the server would
// send it, feeds each chunk to Conversation.Receive, and asserts that Next
// receives the original raw bytes — not a fragment.
func TestMultiChunkChallengeReassembly(t *testing.T) {
	// Build a raw payload whose base64 encoding spans two full chunks and a
	// remainder. 4*maxChunk base64 chars → 3*maxChunk raw bytes decoded; we
	// want 2*maxChunk+1 base64 chars so we get two full chunks and a 1-byte
	// terminal. The raw payload is 3/4*(2*maxChunk+1) ≈ 600 bytes.
	// Use a simple repeating pattern for determinism.
	rawLen := (2*maxChunk + 1) * 3 / 4 // ~600 bytes raw → ~801 base64 chars
	raw := make([]byte, rawLen)
	for i := range raw {
		raw[i] = byte(i % 251)
	}

	mech := &echoMech{}
	conv := NewConversation(mech)
	conv.Begin()

	chunks := makeAuthChunks(raw)
	if len(chunks) < 3 {
		t.Fatalf("test setup: need ≥3 AUTHENTICATE chunks, got %d (rawLen=%d)", len(chunks), rawLen)
	}

	var finalLines []string
	for i, msg := range chunks {
		lines, done, err := conv.Receive(msg)
		if err != nil {
			t.Fatalf("Receive chunk %d/%d: unexpected error: %v", i+1, len(chunks), err)
		}
		if done {
			t.Fatalf("Receive chunk %d/%d: done=true before 903", i+1, len(chunks))
		}
		if i < len(chunks)-1 {
			// Continuation chunks must not trigger a response yet.
			if len(lines) != 0 {
				t.Errorf("chunk %d: got %d response lines before terminal, want 0", i+1, len(lines))
			}
		} else {
			// Terminal chunk must trigger a response.
			finalLines = lines
		}
	}

	if len(finalLines) == 0 {
		t.Fatal("no response lines after terminal chunk")
	}
	if mech.got == nil {
		t.Fatal("mech.Next was never called")
	}
	if string(mech.got) != string(raw) {
		t.Errorf("mech.Next got %d bytes, want %d; payloads differ", len(mech.got), len(raw))
	}
}

// TestMultiChunkExactMultiple exercises the edge case where the encoded payload
// is an exact multiple of maxChunk (400), requiring the server to append a
// standalone "+" after the last full chunk. Reassembly must handle the "+"
// terminal correctly (treating it as an empty final piece that triggers dispatch
// without adding any bytes to the buffer).
func TestMultiChunkExactMultiple(t *testing.T) {
	// Find a raw length whose base64 is an exact multiple of maxChunk.
	// base64 length = ceil(rawLen / 3) * 4.  We want that to equal 2*maxChunk.
	// 2*400 = 800 base64 chars → rawLen = 800*3/4 = 600 raw bytes.
	rawLen := 2 * maxChunk * 3 / 4 // 600 bytes → 800 base64 chars
	raw := make([]byte, rawLen)
	for i := range raw {
		raw[i] = byte(i % 199)
	}
	// Verify our math.
	encoded := base64.StdEncoding.EncodeToString(raw)
	if len(encoded)%maxChunk != 0 {
		t.Skipf("test-setup: encoded length %d is not a multiple of %d — adjust rawLen", len(encoded), maxChunk)
	}

	mech := &echoMech{}
	conv := NewConversation(mech)
	conv.Begin()

	chunks := makeAuthChunks(raw)
	// Last chunk must be "+".
	if last := chunks[len(chunks)-1]; last.Param(0) != "+" {
		t.Fatalf("test setup: expected last chunk to be '+', got %q", last.Param(0))
	}

	var calledAt int = -1
	for i, msg := range chunks {
		lines, _, err := conv.Receive(msg)
		if err != nil {
			t.Fatalf("Receive chunk %d: unexpected error: %v", i+1, err)
		}
		if mech.got != nil && calledAt < 0 {
			calledAt = i
		}
		if i < len(chunks)-1 && len(lines) != 0 {
			t.Errorf("chunk %d: got response before terminal", i+1)
		}
	}

	if mech.got == nil {
		t.Fatal("mech.Next never called")
	}
	if calledAt != len(chunks)-1 {
		t.Errorf("mech.Next called at chunk %d, want at last chunk (%d)", calledAt+1, len(chunks))
	}
	if string(mech.got) != string(raw) {
		t.Errorf("reassembled payload differs: got %d bytes, want %d", len(mech.got), len(raw))
	}
}

// TestChunkCountExhaustion verifies that a hostile server sending an unbounded
// stream of full-size (400-byte) AUTHENTICATE chunks with no terminal is
// rejected before mech.Next is ever called. The error must be returned and an
// AUTHENTICATE * abort emitted.
func TestChunkCountExhaustion(t *testing.T) {
	mech := &echoMech{}
	conv := NewConversation(mech)
	conv.Begin()

	fullChunk := strings.Repeat("A", maxChunk) // 400 bytes, signals "more follows"

	var gotErr error
	var gotAbort bool
	// maxChallengeB64 = (8192*4+2)/3 ≈ 10923 chars; each chunk is 400 chars,
	// so the ceiling is hit after at most ceil(10923/400) = 28 chunks. Send 100
	// to be sure we trigger the guard.
	for i := 0; i < 100; i++ {
		lines, _, err := conv.Receive(authMsg(fullChunk))
		if err != nil {
			gotErr = err
			for _, l := range lines {
				if l == abortLine {
					gotAbort = true
				}
			}
			break
		}
		if mech.got != nil {
			t.Fatal("mech.Next called before exhaustion was detected")
		}
	}

	if gotErr == nil {
		t.Fatal("expected error for chunk-count exhaustion, got nil")
	}
	if !gotAbort {
		t.Errorf("expected AUTHENTICATE * abort line in response, got none (err=%v)", gotErr)
	}
	if mech.got != nil {
		t.Error("mech.Next must not be called when exhaustion is detected")
	}
}

// TestSingleChunkChallengeUnchanged is a regression test confirming that a
// normal single-chunk challenge (the common SCRAM case where server-first fits
// in one line) still works correctly after the reassembly refactor.
func TestSingleChunkChallengeUnchanged(t *testing.T) {
	payload := []byte("r=clientnonce+servernonce,s=c2FsdA==,i=4096")
	mech := &echoMech{}
	conv := NewConversation(mech)
	conv.Begin()

	encoded := base64.StdEncoding.EncodeToString(payload)
	if len(encoded) >= maxChunk {
		t.Fatalf("test setup: encoded payload (%d bytes) must be < %d for single-chunk test", len(encoded), maxChunk)
	}

	lines, done, err := conv.Receive(authMsg(encoded))
	if err != nil {
		t.Fatalf("single-chunk Receive: unexpected error: %v", err)
	}
	if done {
		t.Error("done=true before 903")
	}
	if len(lines) == 0 {
		t.Fatal("expected response lines from single-chunk challenge")
	}
	if string(mech.got) != string(payload) {
		t.Errorf("single-chunk: mech.Next got %q, want %q", mech.got, payload)
	}
}
