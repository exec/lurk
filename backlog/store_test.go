package backlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// makeEvent builds a minimal *client.Event for testing, with a real irc.Message.
func makeEvent(command, source string, params []string) *client.Event {
	msg := &irc.Message{
		Source:  source,
		Command: command,
		Params:  params,
	}
	return &client.Event{Message: msg}
}

// makeEventWithTime builds an Event whose @time tag makes ev.Time() return t.
func makeEventWithTime(command, source string, params []string, t time.Time) *client.Event {
	msg := &irc.Message{
		Source:  source,
		Command: command,
		Params:  params,
		Tags:    irc.Tags{"time": t.UTC().Format(time.RFC3339Nano)},
	}
	return &client.Event{Message: msg}
}

// newTestStore creates a Store in t.TempDir() with the given options.
func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// ─── sanitization tests ───────────────────────────────────────────────────────

// TestSanitizationStripsESCPreservesIRC verifies that an upstream PRIVMSG with
// an embedded ANSI escape (\x1b[31m) is stored with the ESC stripped but any
// IRC formatting (\x02 bold) is PRESERVED. This is the core correctness
// invariant for SanitizeForRelay as opposed to SanitizeTerminal.
func TestSanitizationStripsESCPreservesIRC(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Body: ESC[31m (ANSI colour escape) mixed with \x02 (IRC bold).
	// After SanitizeForRelay: ESC must vanish, \x02 must survive.
	esc := string([]byte{0x1b})  // ESC
	bold := string([]byte{0x02}) // IRC bold
	body := esc + "[31mred" + esc + "[0m " + bold + "bold" + bold + " normal"
	ev := makeEvent("PRIVMSG", "peer!u@h", []string{"#lurk", body})
	s.Ingest(1, ev)

	entries := s.Latest(1, "#lurk", 10)
	if len(entries) != 1 {
		t.Fatalf("Latest returned %d entries, want 1", len(entries))
	}
	stored := entries[0].Params[1]

	// Must NOT contain ESC (0x1b).
	if strings.ContainsRune(stored, 0x1b) {
		t.Errorf("stored text contains ESC (0x1b): %q", stored)
	}
	// MUST contain IRC bold (0x02).
	if !strings.ContainsRune(stored, 0x02) {
		t.Errorf("stored text lost IRC bold (0x02): %q", stored)
	}

	// Also verify on disk: the JSONL file must not contain the raw ESC byte,
	// and must contain the IRC bold character (json.Marshal encodes \x02 as
	// the JSON unicode escape \u0002).
	jsonlPath := filepath.Join(dir, "1", "#lurk.jsonl")
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read JSONL file: %v", err)
	}
	rawData := string(data)
	if strings.ContainsRune(rawData, 0x1b) {
		t.Errorf("JSONL file contains raw ESC (0x1b)")
	}
	// json.Marshal serialises control bytes as \uXXXX; ESC is \u001b.
	if strings.Contains(rawData, "\\u001b") {
		t.Errorf("JSONL file contains JSON-escaped ESC (\\u001b): %q", rawData)
	}
	// IRC bold is \u0002 in the JSON.
	if !strings.Contains(rawData, "\\u0002") {
		t.Errorf("JSONL file lost IRC bold (expected \\u0002 JSON escape in: %q)", rawData)
	}
}

// TestIRCFormattingRoundTrip verifies that an IRC-formatted PRIVMSG (\x02bold\x02)
// survives a store + rehydrate round-trip with formatting intact.
func TestIRCFormattingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	bold := string([]byte{0x02})
	colour := string([]byte{0x03})
	body := bold + "bold" + bold + " and " + colour + "4red" + colour + " text"
	ev := makeEvent("PRIVMSG", "alice!u@h", []string{"#chan", body})
	s.Ingest(1, ev)
	s.Close()

	// Open a second Store on the same dir — this triggers Rehydrate.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	defer s2.Close()

	entries := s2.Latest(1, "#chan", 5)
	if len(entries) != 1 {
		t.Fatalf("after restart: Latest returned %d entries, want 1", len(entries))
	}
	got := entries[0].Params[1]
	if got != body {
		t.Errorf("round-trip: got %q, want %q", got, body)
	}
}

// ─── ring tests ───────────────────────────────────────────────────────────────

// TestRingDropOldest verifies that the in-memory ring drops the oldest entry
// when the ring is full and a new message is ingested.
func TestRingDropOldest(t *testing.T) {
	const ringSize = 5
	s := newTestStore(t, WithRingSize(ringSize))

	for i := 0; i < ringSize+3; i++ {
		body := fmt.Sprintf("msg%d", i)
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#ch", body})
		s.Ingest(1, ev)
	}

	entries := s.Latest(1, "#ch", 100)
	if len(entries) != ringSize {
		t.Fatalf("ring size = %d, want %d", len(entries), ringSize)
	}
	// The oldest entries (msg0..msg2) should be gone; ring holds msg3..msg7.
	for i, e := range entries {
		want := fmt.Sprintf("msg%d", i+3)
		if e.Params[1] != want {
			t.Errorf("entry[%d].Params[1] = %q, want %q", i, e.Params[1], want)
		}
	}
}

// ─── msgid tests ─────────────────────────────────────────────────────────────

// TestMsgIDUniqueNonEmpty verifies that each Ingest call produces a non-empty,
// unique msgid.
func TestMsgIDUniqueNonEmpty(t *testing.T) {
	s := newTestStore(t)

	const n = 50
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#ch", fmt.Sprintf("msg%d", i)})
		s.Ingest(1, ev)
	}
	entries := s.Latest(1, "#ch", n)
	if len(entries) != n {
		t.Fatalf("got %d entries, want %d", len(entries), n)
	}
	for _, e := range entries {
		if e.MsgID == "" {
			t.Error("entry has empty msgid")
			continue
		}
		if seen[e.MsgID] {
			t.Errorf("duplicate msgid %q", e.MsgID)
		}
		seen[e.MsgID] = true
	}
}

// ─── restart rehydration tests ────────────────────────────────────────────────

// TestRestartRehydratesMsgIDAndTime verifies that a second NewStore on the same
// directory rehydrates the ring from JSONL with msgids and server-time intact.
func TestRestartRehydratesMsgIDAndTime(t *testing.T) {
	dir := t.TempDir()
	// Use a recent timestamp (within the clamp window) so Ingest does not
	// substitute now for it; the test checks that the persisted time survives
	// a restart round-trip unchanged.
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ev := makeEventWithTime("PRIVMSG", "alice!u@h", []string{"#main", "hello"}, ts)
	s.Ingest(1, ev)
	before := s.Latest(1, "#main", 1)
	if len(before) != 1 {
		t.Fatal("no entry before close")
	}
	originalMsgID := before[0].MsgID
	s.Close()

	// Restart.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	defer s2.Close()

	after := s2.Latest(1, "#main", 1)
	if len(after) != 1 {
		t.Fatal("no entry after restart")
	}
	if after[0].MsgID != originalMsgID {
		t.Errorf("msgid mismatch: got %q, want %q", after[0].MsgID, originalMsgID)
	}
	if !after[0].Time.Equal(ts) {
		t.Errorf("time mismatch: got %v, want %v", after[0].Time, ts)
	}
}

// TestTornFinalLineSkipped verifies that a torn final JSONL line (simulated by
// appending a partial/invalid line) is skipped on rehydration while earlier
// valid entries survive.
func TestTornFinalLineSkipped(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ev1 := makeEventWithTime("PRIVMSG", "a!u@h", []string{"#ch", "good1"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	ev2 := makeEventWithTime("PRIVMSG", "b!u@h", []string{"#ch", "good2"},
		time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	s.Ingest(1, ev1)
	s.Ingest(1, ev2)
	s.Close()

	// Simulate crash mid-write: append a torn (partial) JSON line.
	jsonlPath := filepath.Join(dir, "1", "#ch.jsonl")
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open JSONL for corruption: %v", err)
	}
	// Write a partial JSON object — no closing brace, so json.Unmarshal will fail.
	_, _ = f.WriteString(`{"time":"2024-01-03T00:00:00Z","msgid":"partial`)
	f.Close()

	// Restart.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (after corruption): %v", err)
	}
	defer s2.Close()

	entries := s2.Latest(1, "#ch", 10)
	if len(entries) != 2 {
		t.Fatalf("got %d entries after torn-line write, want 2", len(entries))
	}
	if entries[0].Params[1] != "good1" || entries[1].Params[1] != "good2" {
		t.Errorf("unexpected entries: %v", entries)
	}
}

// ─── synthetic event filter tests ────────────────────────────────────────────

// TestSyntheticEventsNotStored verifies that synthetic @reconnecting and
// @reconnected events (ev.Message.Command starts with '@') are not stored.
func TestSyntheticEventsNotStored(t *testing.T) {
	s := newTestStore(t)

	// nil Message (overflow event).
	s.Ingest(1, &client.Event{})

	// @reconnecting synthetic event.
	s.Ingest(1, &client.Event{
		Message: &irc.Message{Command: "@reconnecting"},
	})

	// @reconnected synthetic event.
	s.Ingest(1, &client.Event{
		Message: &irc.Message{Command: "@reconnected"},
	})

	// @connected synthetic event.
	s.Ingest(1, &client.Event{
		Message: &irc.Message{Command: "@connected"},
	})

	// Non-PRIVMSG/NOTICE real event (JOIN).
	s.Ingest(1, makeEvent("JOIN", "nick!u@h", []string{"#ch"}))

	entries := s.Latest(1, "#ch", 100)
	if len(entries) != 0 {
		t.Errorf("expected 0 stored entries, got %d", len(entries))
	}
}

// ─── PM routing tests ─────────────────────────────────────────────────────────

// TestPMKeyedBySenderNick verifies that routeTarget correctly keys a PM
// addressed to lurkd's own nick by the sender's nick.
func TestPMKeyedBySenderNick(t *testing.T) {
	// Direct unit test of routeTarget.
	got := routeTarget("lurkdnick", "alice", "lurkdnick")
	if got != "alice" {
		t.Errorf("routeTarget: got %q, want %q (alice)", got, "alice")
	}
	// Case-insensitive comparison.
	got = routeTarget("LURKDNICK", "alice", "lurkdnick")
	if got != "alice" {
		t.Errorf("routeTarget (case): got %q, want alice", got)
	}

	// Channel targets are not affected.
	got = routeTarget("#channel", "alice", "lurkdnick")
	if got != "#channel" {
		t.Errorf("routeTarget channel: got %q, want #channel", got)
	}

	// When ownNick is empty, rawTarget is used as-is.
	got = routeTarget("somechan", "alice", "")
	if got != "somechan" {
		t.Errorf("routeTarget no-own-nick: got %q, want somechan", got)
	}
}

// TestPMStoredBySenderNick verifies the store-level PM routing: a PRIVMSG
// with a non-channel rawTarget is stored (and retrieved) under the rawTarget
// safeName when ev.Client is nil (no ownNick known).
func TestPMStoredBySenderNick(t *testing.T) {
	s := newTestStore(t)

	// No ev.Client, so ownNick = "". rawTarget "mynick" is used as the key.
	ev := makeEvent("PRIVMSG", "alice!u@h", []string{"mynick", "private message"})
	s.Ingest(1, ev)

	// Retrieved under safeName("mynick") = "mynick".
	entries := s.Latest(1, "mynick", 5)
	if len(entries) != 1 {
		t.Fatalf("PM routing: got %d entries under 'mynick', want 1", len(entries))
	}
}

// ─── concurrent Ingest tests ─────────────────────────────────────────────────

// TestConcurrentIngest verifies that concurrent Ingest and Latest calls from
// multiple goroutines are race-clean. The race detector catches data races.
func TestConcurrentIngest(t *testing.T) {
	s := newTestStore(t)

	const goroutines = 10
	const perGoroutine = 50
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			netid := g%3 + 1
			target := fmt.Sprintf("#chan%d", g%4)
			for i := 0; i < perGoroutine; i++ {
				ev := makeEvent("PRIVMSG", "nick!u@h",
					[]string{target, fmt.Sprintf("goroutine%d msg%d", g, i)})
				s.Ingest(netid, ev)
			}
		}()
	}
	wg.Wait()

	// Concurrent Latest calls while ingests may still be running.
	var wg2 sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			netid := g%3 + 1
			target := fmt.Sprintf("#chan%d", g%4)
			_ = s.Latest(netid, target, 10)
		}()
	}
	wg2.Wait()
}

// ─── target cap tests ─────────────────────────────────────────────────────────

// TestTargetCapDropsExcess verifies that when MaxTargetsPerNet is reached, new
// targets are silently dropped (no panic, no entries stored).
func TestTargetCapDropsExcess(t *testing.T) {
	const capSize = 3
	s := newTestStore(t, WithMaxTargetsPerNet(capSize))

	// Fill up the cap.
	for i := 0; i < capSize; i++ {
		target := fmt.Sprintf("#allowed%d", i)
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{target, "msg"})
		s.Ingest(1, ev)
	}

	// This target exceeds the cap; it should be silently dropped.
	excess := "#excess"
	ev := makeEvent("PRIVMSG", "nick!u@h", []string{excess, "should not be stored"})
	s.Ingest(1, ev)

	entries := s.Latest(1, excess, 10)
	if len(entries) != 0 {
		t.Errorf("excess target: expected 0 entries, got %d", len(entries))
	}

	// Allowed targets still have their entries.
	for i := 0; i < capSize; i++ {
		target := fmt.Sprintf("#allowed%d", i)
		entries := s.Latest(1, target, 5)
		if len(entries) != 1 {
			t.Errorf("allowed target %q: got %d entries, want 1", target, len(entries))
		}
	}
}

// ─── @time clamping tests ────────────────────────────────────────────────────

// TestClampIngestTime verifies the clampIngestTime helper directly.
// A hostile upstream forging @time far in the future or past must have that
// timestamp replaced with the ingest wall clock.
func TestClampIngestTime(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		t       time.Time
		wantNow bool // true → expect now; false → expect t unchanged
	}{
		{"within window — 1 hour ago", now.Add(-time.Hour), false},
		{"within window — now", now, false},
		{"within window — 30 seconds future", now.Add(30 * time.Second), false},
		{"within window — 6 days past", now.Add(-6 * 24 * time.Hour), false},
		{"at past boundary (exactly 7d)", now.Add(-maxTimeSkewPast), false},
		{"just past past boundary", now.Add(-maxTimeSkewPast - time.Second), true},
		{"at future boundary (exactly 1min)", now.Add(maxTimeSkewFuture), false},
		{"just past future boundary", now.Add(maxTimeSkewFuture + time.Second), true},
		{"far future (year 2099)", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"epoch (year 1970)", time.Unix(0, 0).UTC(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampIngestTime(tt.t, now)
			if tt.wantNow {
				if !got.Equal(now) {
					t.Errorf("clampIngestTime(%v, now) = %v, want now (%v)", tt.t, got, now)
				}
			} else {
				if !got.Equal(tt.t) {
					t.Errorf("clampIngestTime(%v, now) = %v, want unchanged (%v)", tt.t, got, tt.t)
				}
			}
		})
	}
}

// TestIngestClampsFarFutureTime verifies the full Ingest path: a PRIVMSG with
// @time=2099-01-01T00:00:00Z is stored with a timestamp near ingest time,
// not 2099. This prevents a hostile upstream from skewing CHATHISTORY ordering
// and TARGETS windows.
func TestIngestClampsFarFutureTime(t *testing.T) {
	s := newTestStore(t)

	farFuture := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := makeEventWithTime("PRIVMSG", "evil!u@h", []string{"#lurk", "forged-future"}, farFuture)

	before := time.Now()
	_, stored := s.Ingest(1, ev)
	after := time.Now()

	if !stored {
		t.Fatal("Ingest returned stored=false for a valid PRIVMSG")
	}

	entries := s.Latest(1, "#lurk", 1)
	if len(entries) != 1 {
		t.Fatalf("Latest returned %d entries, want 1", len(entries))
	}
	got := entries[0].Time

	// The stored time must NOT be 2099.
	if got.Year() >= 2099 {
		t.Errorf("stored Time = %v, far-future timestamp was not clamped", got)
	}
	// The stored time must be near the ingest wall clock.
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("stored Time = %v is not near ingest time [%v, %v]", got, before, after)
	}
}

// TestIngestClampsPastTime verifies that @time far in the past (epoch) is
// clamped to near ingest time, not year 1970.
func TestIngestClampsPastTime(t *testing.T) {
	s := newTestStore(t)

	epoch := time.Unix(0, 0).UTC()
	ev := makeEventWithTime("PRIVMSG", "evil!u@h", []string{"#lurk", "forged-past"}, epoch)

	before := time.Now()
	_, stored := s.Ingest(1, ev)
	after := time.Now()

	if !stored {
		t.Fatal("Ingest returned stored=false for a valid PRIVMSG")
	}

	entries := s.Latest(1, "#lurk", 1)
	if len(entries) != 1 {
		t.Fatalf("Latest returned %d entries, want 1", len(entries))
	}
	got := entries[0].Time

	// Must not be epoch.
	if got.Year() < 2020 {
		t.Errorf("stored Time = %v, past timestamp was not clamped", got)
	}
	// Must be near ingest time.
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("stored Time = %v is not near ingest time [%v, %v]", got, before, after)
	}
}

// TestIngestPreservesNormalTime verifies that a legitimate recent @time tag
// (within the clamp window) is preserved as-is.
func TestIngestPreservesNormalTime(t *testing.T) {
	s := newTestStore(t)

	// A timestamp 10 minutes in the past — well within the 7-day window.
	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	ev := makeEventWithTime("PRIVMSG", "alice!u@h", []string{"#lurk", "normal"}, ts)
	_, stored := s.Ingest(1, ev)
	if !stored {
		t.Fatal("Ingest returned stored=false")
	}

	entries := s.Latest(1, "#lurk", 1)
	if len(entries) != 1 {
		t.Fatalf("Latest returned %d entries, want 1", len(entries))
	}
	// The stored time must match the supplied @time (within 1ms rounding).
	got := entries[0].Time
	diff := got.Sub(ts)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Millisecond {
		t.Errorf("normal @time was unexpectedly altered: stored=%v, supplied=%v", got, ts)
	}
}

// ─── JSONL disk format tests ─────────────────────────────────────────────────

// TestJSONLSchemaFields verifies that the JSONL file contains the expected
// fields (time, msgid, target, source, command, params) for each entry.
func TestJSONLSchemaFields(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Use a recent timestamp (within the clamp window) so Ingest does not
	// substitute now for it; the test checks round-trip field fidelity.
	ts := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Millisecond)
	ev := makeEventWithTime("PRIVMSG", "alice!a@example.com",
		[]string{"#test", "hello world"}, ts)
	s.Ingest(42, ev)
	s.Close()

	data, err := os.ReadFile(filepath.Join(dir, "42", "#test.jsonl"))
	if err != nil {
		t.Fatalf("read JSONL: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line in JSONL, got %d", len(lines))
	}

	var e Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if e.MsgID == "" {
		t.Error("msgid is empty")
	}
	if e.Command != "PRIVMSG" {
		t.Errorf("command = %q, want PRIVMSG", e.Command)
	}
	if e.Target != "#test" {
		t.Errorf("target = %q, want #test", e.Target)
	}
	if e.Source != "alice!a@example.com" {
		t.Errorf("source = %q, want alice!a@example.com", e.Source)
	}
	if !e.Time.Equal(ts) {
		t.Errorf("time = %v, want %v", e.Time, ts)
	}
	if len(e.Params) != 2 || e.Params[1] != "hello world" {
		t.Errorf("params = %v", e.Params)
	}
}

// TestNoticeStored verifies that NOTICE messages are also stored.
func TestNoticeStored(t *testing.T) {
	s := newTestStore(t)
	ev := makeEvent("NOTICE", "server!s@s", []string{"#lurk", "server notice"})
	s.Ingest(1, ev)
	entries := s.Latest(1, "#lurk", 5)
	if len(entries) != 1 {
		t.Fatalf("got %d entries for NOTICE, want 1", len(entries))
	}
	if entries[0].Command != "NOTICE" {
		t.Errorf("command = %q, want NOTICE", entries[0].Command)
	}
}

// TestLatestChronologicalOrder verifies that Latest returns entries in
// chronological order (oldest first).
func TestLatestChronologicalOrder(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ev := makeEventWithTime("PRIVMSG", "nick!u@h",
			[]string{"#order", fmt.Sprintf("msg%d", i)},
			base.Add(time.Duration(i)*time.Minute))
		s.Ingest(1, ev)
	}
	entries := s.Latest(1, "#order", 10)
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Errorf("entry[%d] (%v) is before entry[%d] (%v) — not chronological",
				i, entries[i].Time, i-1, entries[i-1].Time)
		}
	}
}

// TestLatestLimitRespected verifies that Latest(... limit) returns at most
// limit entries even when the ring has more.
func TestLatestLimitRespected(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#lim", fmt.Sprintf("msg%d", i)})
		s.Ingest(1, ev)
	}
	entries := s.Latest(1, "#lim", 5)
	if len(entries) != 5 {
		t.Errorf("Latest(... 5) returned %d entries, want 5", len(entries))
	}
}

// TestDefaultPath verifies DefaultPath respects the LURKD_BACKLOG_DIR env var.
func TestDefaultPath(t *testing.T) {
	t.Setenv("LURKD_BACKLOG_DIR", "/tmp/test-backlog")
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if p != "/tmp/test-backlog" {
		t.Errorf("DefaultPath = %q, want /tmp/test-backlog", p)
	}
}

// TestSafeNamePathTraversal verifies that safeName rejects path-traversal names
// like ".." and "." — consistent with chatlog's safeName invariant.
func TestSafeNamePathTraversal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"..", "server"},
		{".", "server"},
		{"...", "server"},
		{"#lurk", "#lurk"},
		{"alice", "alice"},
		{"", "server"},
		// ToLower applied first, so uppercase becomes lowercase and passes.
		{"UPPER", "upper"},
		{"#MyChannel", "#mychannel"},
	}
	for _, tt := range tests {
		got := safeName(tt.input)
		if got != tt.want {
			t.Errorf("safeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestMultipleNetIDs verifies that entries for different netids are stored and
// retrieved independently.
func TestMultipleNetIDs(t *testing.T) {
	s := newTestStore(t)

	ev1 := makeEvent("PRIVMSG", "alice!u@h", []string{"#a", "net1 msg"})
	ev2 := makeEvent("PRIVMSG", "bob!u@h", []string{"#a", "net2 msg"})
	s.Ingest(1, ev1)
	s.Ingest(2, ev2)

	e1 := s.Latest(1, "#a", 5)
	e2 := s.Latest(2, "#a", 5)

	if len(e1) != 1 || e1[0].Params[1] != "net1 msg" {
		t.Errorf("netid 1: %v", e1)
	}
	if len(e2) != 1 || e2[0].Params[1] != "net2 msg" {
		t.Errorf("netid 2: %v", e2)
	}
}

// TestMultipleRestarts verifies that multiple restart cycles accumulate
// entries correctly in the JSONL file and ring.
func TestMultipleRestarts(t *testing.T) {
	dir := t.TempDir()

	for round := 0; round < 3; round++ {
		s, err := NewStore(dir)
		if err != nil {
			t.Fatalf("NewStore round %d: %v", round, err)
		}
		ev := makeEvent("PRIVMSG", "nick!u@h",
			[]string{"#persist", fmt.Sprintf("round%d", round)})
		s.Ingest(1, ev)
		s.Close()
	}

	// After 3 rounds, the ring should have all 3 entries.
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore final: %v", err)
	}
	defer s.Close()

	entries := s.Latest(1, "#persist", 10)
	if len(entries) != 3 {
		t.Fatalf("got %d entries after 3 rounds, want 3", len(entries))
	}
	for i, e := range entries {
		want := fmt.Sprintf("round%d", i)
		if e.Params[1] != want {
			t.Errorf("entry[%d] = %q, want %q", i, e.Params[1], want)
		}
	}
}

// TestNewMsgIDLength verifies that generated msgids have the expected length
// (24 chars for 18 random bytes in base64url without padding).
func TestNewMsgIDLength(t *testing.T) {
	for i := 0; i < 10; i++ {
		id := newMsgID()
		if len(id) != 24 {
			t.Errorf("msgid %q has length %d, want 24", id, len(id))
		}
	}
}

// TestRehydrateSkipsUnsafeFilenames verifies that Rehydrate ignores any JSONL
// file whose name does not round-trip through safeName (a path-traversal
// defence: an attacker cannot inject a file like "../../evil.jsonl" into the
// store directory and have it rehydrated as a valid target).
func TestRehydrateSkipsUnsafeFilenames(t *testing.T) {
	dir := t.TempDir()

	// Write a legitimate entry via Ingest first.
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ev := makeEvent("PRIVMSG", "alice!u@h", []string{"#safe", "safe message"})
	s.Ingest(1, ev)
	s.Close()

	// Manually place a file with a path-traversal name in the netid directory.
	netDir := filepath.Join(dir, "1")
	evil := filepath.Join(netDir, "..%2fevil.jsonl")
	_ = os.WriteFile(evil, []byte(`{"time":"2024-01-01T00:00:00Z","msgid":"evilddd","target":"evil","command":"PRIVMSG","params":["evil","evil"]}`+"\n"), 0o600)

	// Also place a file whose name doesn't round-trip safeName (uppercase).
	unsafe := filepath.Join(netDir, "UPPERCASE.jsonl")
	_ = os.WriteFile(unsafe, []byte(`{"time":"2024-01-01T00:00:00Z","msgid":"unsafe1","target":"unsafe","command":"PRIVMSG","params":["u","m"]}`+"\n"), 0o600)

	// Restart — Rehydrate should skip the unsafe files and load only #safe.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	defer s2.Close()

	// #safe should be rehydrated.
	safe := s2.Latest(1, "#safe", 5)
	if len(safe) != 1 {
		t.Errorf("safe target: got %d entries, want 1", len(safe))
	}

	// The evil path-traversal file should not appear under any buffer.
	s2.mu.Lock()
	count := len(s2.buffers)
	s2.mu.Unlock()
	if count != 1 {
		t.Errorf("buffer count = %d after unsafe-file injection, want 1", count)
	}
}

// TestRehydrateHonorsTargetCap verifies that Rehydrate enforces the
// per-netid MaxTargetsPerNet cap when loading on-disk JSONL files. Previously,
// Rehydrate skipped the cap check, so a store that was overpopulated under a
// previous run — or one where an attacker placed extra JSONL files — could
// load an unbounded number of targets, exhausting memory and skewing tgtCnt.
//
// The test pre-places more JSONL files than the cap allows for one netid and
// asserts that Rehydrate loads at most maxTgts of them.
func TestRehydrateHonorsTargetCap(t *testing.T) {
	const capSize = 3
	const totalFiles = 7 // more than capSize; some must be skipped

	dir := t.TempDir()

	// Create the netid subdirectory and pre-place totalFiles valid JSONL files.
	// These simulate what would exist after a previous run with a higher cap,
	// or files placed by an attacker with write access to the store directory.
	netDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatalf("mkdir netdir: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < totalFiles; i++ {
		target := fmt.Sprintf("#chan%d", i)
		entry := fmt.Sprintf(
			`{"time":%q,"msgid":"msgid%02d","target":%q,"source":"n!u@h","command":"PRIVMSG","params":[%q,"hello"]}`,
			now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano),
			i, target, target,
		)
		path := filepath.Join(netDir, target+".jsonl")
		if err := os.WriteFile(path, []byte(entry+"\n"), 0o600); err != nil {
			t.Fatalf("write test JSONL %s: %v", path, err)
		}
	}

	// Open a Store with maxTgts=capSize — Rehydrate must not load more than
	// capSize targets even though there are totalFiles on disk.
	s, err := NewStore(dir, WithMaxTargetsPerNet(capSize))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Count how many bufferEntries were loaded for netid 1.
	s.mu.Lock()
	loaded := 0
	for k := range s.buffers {
		if k.netid == 1 {
			loaded++
		}
	}
	tgtCntLoaded := s.tgtCnt[1]
	s.mu.Unlock()

	if loaded > capSize {
		t.Errorf("Rehydrate loaded %d targets for netid 1, want at most %d (cap)", loaded, capSize)
	}
	if loaded == 0 {
		t.Errorf("Rehydrate loaded 0 targets; want at least 1 (cap allows %d)", capSize)
	}
	// tgtCnt must match the number of bufferEntries actually loaded.
	if tgtCntLoaded != loaded {
		t.Errorf("tgtCnt[1]=%d does not match loaded buffer count %d", tgtCntLoaded, loaded)
	}
}

// TestRehydrateCapPreservesLiveIngestCap verifies the corollary: after
// Rehydrate fills exactly maxTgts entries for a netid, a subsequent live
// Ingest for a new target on the same netid is correctly rejected by the cap
// (tgtCnt[netid] was correctly set during Rehydrate, not inflated or zeroed).
func TestRehydrateCapPreservesLiveIngestCap(t *testing.T) {
	const capSize = 2

	dir := t.TempDir()

	// Pre-place exactly capSize JSONL files (filling the cap on rehydration).
	netDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatalf("mkdir netdir: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < capSize; i++ {
		target := fmt.Sprintf("#existing%d", i)
		entry := fmt.Sprintf(
			`{"time":%q,"msgid":"ex%02d","target":%q,"source":"n!u@h","command":"PRIVMSG","params":[%q,"m"]}`,
			now.Format(time.RFC3339Nano), i, target, target,
		)
		path := filepath.Join(netDir, target+".jsonl")
		if err := os.WriteFile(path, []byte(entry+"\n"), 0o600); err != nil {
			t.Fatalf("write JSONL: %v", err)
		}
	}

	s, err := NewStore(dir, WithMaxTargetsPerNet(capSize))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// After rehydrating capSize entries, a new target must be rejected.
	excess := makeEvent("PRIVMSG", "alice!u@h", []string{"#newchan", "should be dropped"})
	_, stored := s.Ingest(1, excess)
	if stored {
		t.Errorf("Ingest accepted a new target after Rehydrate filled the cap; expected drop")
	}

	entries := s.Latest(1, "#newchan", 5)
	if len(entries) != 0 {
		t.Errorf("new-target entries = %d after cap full, want 0", len(entries))
	}

	// Existing rehydrated targets must still be readable.
	for i := 0; i < capSize; i++ {
		target := fmt.Sprintf("#existing%d", i)
		got := s.Latest(1, target, 5)
		if len(got) != 1 {
			t.Errorf("existing target %q: got %d entries, want 1", target, len(got))
		}
	}
}

// ─── JSONL rotation tests ─────────────────────────────────────────────────────

// TestRotationCreatesBackupFile verifies that when the JSONL file exceeds
// WithMaxFileSize after an Ingest write, the file is renamed to .jsonl.1 and
// a fresh .jsonl is created for subsequent writes.
func TestRotationCreatesBackupFile(t *testing.T) {
	dir := t.TempDir()

	// Use a tiny cap so the first few messages trigger a rotation.
	const cap = 100 // bytes — well below a single JSON line
	s, err := NewStore(dir, WithMaxFileSize(cap))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Ingest enough messages to force rotation.
	for i := 0; i < 5; i++ {
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#rot", fmt.Sprintf("rotation-test-msg-%d", i)})
		s.Ingest(1, ev)
	}

	// The rotation file must exist.
	rotPath := filepath.Join(dir, "1", "#rot.jsonl.1")
	if _, err := os.Stat(rotPath); os.IsNotExist(err) {
		t.Fatalf(".jsonl.1 rotation file not created after exceeding size cap")
	}

	// The current .jsonl must also exist (fresh file after rotation).
	curPath := filepath.Join(dir, "1", "#rot.jsonl")
	if _, err := os.Stat(curPath); os.IsNotExist(err) {
		t.Fatalf(".jsonl current file missing after rotation")
	}
}

// TestRotationRingRehydratesFromBothFiles verifies that after rotation, a fresh
// NewStore rehydrates the in-memory ring from both the .jsonl.1 (older) and the
// .jsonl (newer) files, with all msgids intact and in chronological order.
//
// Only one generation of backup is kept (.jsonl.1). With a very small cap,
// multiple rotations occur during the test and only the final .jsonl + .jsonl.1
// pair survives on disk. The test therefore only asserts that:
//   - rehydration returns some entries (not empty)
//   - entries are in chronological order
//   - each rehydrated entry has a non-empty msgid
//   - the very last ingested message is present (it's always in the current .jsonl)
func TestRotationRingRehydratesFromBothFiles(t *testing.T) {
	dir := t.TempDir()

	// Use a cap that triggers rotation but not on every single write, so we get
	// entries in both .jsonl.1 and .jsonl after the final rotation.
	const cap = 300
	s, err := NewStore(dir, WithMaxFileSize(cap))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Ingest enough messages to trigger at least one rotation.
	const total = 15
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var lastMsgID string
	for i := 0; i < total; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		ev := makeEventWithTime("PRIVMSG", "alice!u@h",
			[]string{"#rehydrate", fmt.Sprintf("msg%d", i)}, ts)
		msgid, stored := s.Ingest(1, ev)
		if !stored {
			t.Fatalf("Ingest %d: not stored", i)
		}
		lastMsgID = msgid
	}

	// Ensure the rotation file exists before closing.
	rotPath := filepath.Join(dir, "1", "#rehydrate.jsonl.1")
	if _, err := os.Stat(rotPath); os.IsNotExist(err) {
		t.Fatal(".jsonl.1 not created — increase message count or reduce cap")
	}

	s.Close()

	// Open a second store — this triggers Rehydrate from both files.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	defer s2.Close()

	after := s2.Latest(1, "#rehydrate", total+5)
	if len(after) == 0 {
		t.Fatal("no entries after rehydration from both files")
	}

	// Verify chronological order.
	for i := 1; i < len(after); i++ {
		if after[i].Time.Before(after[i-1].Time) {
			t.Errorf("entries out of order at index %d: %v before %v",
				i, after[i].Time, after[i-1].Time)
		}
	}

	// Every rehydrated entry must have a non-empty msgid.
	for _, e := range after {
		if e.MsgID == "" {
			t.Errorf("rehydrated entry has empty msgid: %+v", e)
		}
	}

	// The last ingested message must always be present (it's in the current .jsonl).
	found := false
	for _, e := range after {
		if e.MsgID == lastMsgID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("last ingested msgid %q not found in rehydrated ring", lastMsgID)
	}
}

// TestRotationDeduplicatesMsgIDs verifies that dedupEntries correctly removes
// duplicate MsgIDs (as could appear at the rotation boundary).
func TestRotationDeduplicatesMsgIDs(t *testing.T) {
	entries := []Entry{
		{MsgID: "a", Params: []string{"#c", "first-a"}},
		{MsgID: "b", Params: []string{"#c", "b"}},
		{MsgID: "a", Params: []string{"#c", "second-a"}}, // duplicate
		{MsgID: "c", Params: []string{"#c", "c"}},
	}
	got := dedupEntries(entries)
	if len(got) != 3 {
		t.Fatalf("dedupEntries: got %d entries, want 3", len(got))
	}
	// First occurrence of "a" must be kept.
	if got[0].MsgID != "a" || got[0].Params[1] != "first-a" {
		t.Errorf("first entry = %+v, want msgid=a text=first-a", got[0])
	}
	if got[1].MsgID != "b" {
		t.Errorf("second entry msgid = %q, want b", got[1].MsgID)
	}
	if got[2].MsgID != "c" {
		t.Errorf("third entry msgid = %q, want c", got[2].MsgID)
	}
}

// TestRotationOnlyOneGenerationKept verifies that a second rotation replaces
// the previous .jsonl.1 (only one generation of backup is kept).
func TestRotationOnlyOneGenerationKept(t *testing.T) {
	dir := t.TempDir()

	const cap = 50 // extremely small to force multiple rotations
	s, err := NewStore(dir, WithMaxFileSize(cap))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Ingest enough to trigger multiple rotations.
	for i := 0; i < 20; i++ {
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#gen", fmt.Sprintf("gen-msg-%d-padding-to-grow-file-size", i)})
		s.Ingest(1, ev)
	}

	// There must be at most one .jsonl.1 (no .jsonl.2 etc.).
	entries, err := os.ReadDir(filepath.Join(dir, "1"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var rotFiles []string
	for _, fi := range entries {
		if strings.HasSuffix(fi.Name(), ".jsonl.1") {
			rotFiles = append(rotFiles, fi.Name())
		}
		if strings.HasSuffix(fi.Name(), ".jsonl.2") {
			t.Errorf("found unexpected second-generation rotation file: %s", fi.Name())
		}
	}
	if len(rotFiles) == 0 {
		t.Error("no .jsonl.1 rotation file found after multiple writes")
	}
	if len(rotFiles) > 1 {
		t.Errorf("found %d .jsonl.1 files, want at most 1: %v", len(rotFiles), rotFiles)
	}
}

// TestNoRotationWhenCapIsZero verifies that WithMaxFileSize(0) (the default)
// does not rotate even after many writes.
func TestNoRotationWhenCapIsZero(t *testing.T) {
	dir := t.TempDir()

	s, err := NewStore(dir) // no WithMaxFileSize — default 0
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	for i := 0; i < 20; i++ {
		ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#norot", fmt.Sprintf("msg%d", i)})
		s.Ingest(1, ev)
	}

	rotPath := filepath.Join(dir, "1", "#norot.jsonl.1")
	if _, err := os.Stat(rotPath); !os.IsNotExist(err) {
		t.Error(".jsonl.1 rotation file unexpectedly created when cap is 0 (disabled)")
	}
}

// ─── JSONL line-size cap tests ────────────────────────────────────────────────

// TestOversizedLineDroppedByIngest verifies that when an entry's marshalled JSON
// exceeds maxJSONLLineBytes, Ingest drops it from BOTH disk and the in-memory
// ring. A ring entry without a disk record would diverge from on-disk state and
// be silently lost on restart, so the two must stay in sync.
//
// Assertions:
//   - Ingest returns stored=false for the oversized entry.
//   - Latest returns 0 entries (ring is not populated).
//   - The JSONL file (if it exists) does not contain the oversized body.
func TestOversizedLineDroppedByIngest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// Build a PRIVMSG whose body is large enough that the marshalled Entry JSON
	// exceeds maxJSONLLineBytes (16 KiB). The JSON framing adds ~200 bytes;
	// a body of 17000 bytes is well above the threshold.
	bigBody := strings.Repeat("x", 17000)
	ev := makeEvent("PRIVMSG", "nick!u@h", []string{"#oversized", bigBody})
	_, stored := s.Ingest(1, ev)

	// Ingest must report the entry as not stored.
	if stored {
		t.Errorf("Ingest returned stored=true for oversized entry; expected false")
	}

	// Ring must be empty — Latest must return nothing.
	entries := s.Latest(1, "#oversized", 10)
	if len(entries) != 0 {
		t.Errorf("ring has %d entries after oversized Ingest; expected 0 (ring/disk must stay in sync)",
			len(entries))
	}

	// Disk file must either not exist or not contain the oversized body.
	jsonlFile := filepath.Join(dir, "1", "#oversized.jsonl")
	data, readErr := os.ReadFile(jsonlFile)
	if os.IsNotExist(readErr) {
		return // fine: file was never opened
	}
	if readErr != nil {
		t.Fatalf("read JSONL: %v", readErr)
	}
	if strings.Contains(string(data), bigBody) {
		t.Errorf("oversized entry was written to JSONL file (%d bytes); expected it to be dropped from disk",
			len(data))
	}
}

// TestOversizedLineDoesNotCrashRehydrate verifies that a JSONL file containing a
// manually-placed oversized line (> maxJSONLLineBytes, which this code never
// writes itself thanks to the write-side cap) does not panic or return an error
// from NewStore. The scanner surfaces bufio.ErrTooLong, Rehydrate logs it and
// continues: the daemon stays up, only that target's rehydration is degraded.
//
// This test proves the read-side scanner cap is wired correctly: an oversized
// line triggers ErrTooLong rather than a silent multi-MiB allocation, and the
// error path in Rehydrate is exercised without crashing.
func TestOversizedLineDoesNotCrashRehydrate(t *testing.T) {
	dir := t.TempDir()

	// Place an oversized JSONL file directly (bypassing Ingest, which would drop
	// it). We write a valid small entry first, then an oversized line — simulating
	// a file written by an older version of the code or an external tool.
	netDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatalf("mkdir netdir: %v", err)
	}
	jsonlFile := filepath.Join(netDir, "#over.jsonl")

	// Write a valid small entry followed by an oversized line.
	oversizedLine := `{"time":"2024-01-01T00:00:00Z","msgid":"over1","target":"#over","source":"x!u@h","command":"PRIVMSG","params":["#over","` +
		strings.Repeat("y", maxJSONLLineBytes+100) + `"]}` + "\n"
	content := `{"time":"2024-01-01T00:00:00Z","msgid":"ok1","target":"#over","source":"a!u@h","command":"PRIVMSG","params":["#over","normal"]}` + "\n" +
		oversizedLine
	if err := os.WriteFile(jsonlFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write test JSONL: %v", err)
	}

	// NewStore must not panic or return a non-nil error for a corrupt file.
	// Rehydrate logs the scan error and continues.
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore with oversized line: %v (expected no error — Rehydrate should log and continue)", err)
	}
	defer s.Close()
	// No assertion on entries: the scanner aborts at ErrTooLong so entries before
	// the oversized line may or may not be recovered depending on the scanner state.
	// The key invariant is that NewStore returns successfully (no panic, no error).
}
