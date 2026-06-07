package server

// Phase 5 golden-transcript tests for the CHATHISTORY command handler.
//
// Test setup:
//   - makeTestStore populates a backlog.Store with a fixed set of PRIVMSG entries.
//   - pipeServerCHBound creates a Server (with the store) and starts a session
//     goroutine that sets session.netid=netid before the run loop (via
//     serveConnInternalNetid, a test seam on Server). The client end is a
//     *conn.Conn.
//   - registerClient runs NICK/USER and drains the welcome burst.
//   - Each sub-test then sends one or more CHATHISTORY lines and asserts the
//     exact BATCH framing.
//
// Assertions:
//   - BATCH open:  Command=BATCH, Param(0)="+<ref>", Param(1)="chathistory",
//                  Param(2)=<target or "">
//   - Each entry:  Tags["batch"]==batchRef, Tags["time"] non-empty,
//                  Tags["msgid"] non-empty, Command=PRIVMSG/NOTICE
//   - BATCH close: Command=BATCH, Param(0)="-<ref>" (same ref as open)
//   - Empty result: open immediately followed by close (no entry lines).

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/client"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// ─── store setup helpers ──────────────────────────────────────────────────────

// makeTestStore builds a backlog.Store in t.TempDir() and ingests a list of
// PRIVMSG entries into (netid, target) at one-second intervals from baseTime.
// Returns the store and the stored entries (with their assigned MsgIDs).
func makeTestStore(t *testing.T, netid int, target string, msgs []string, baseTime time.Time) (*backlog.Store, []backlog.Entry) {
	t.Helper()
	dir := t.TempDir()
	st, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(st.Close)
	for i, body := range msgs {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		rawMsg := &irc.Message{
			Source:  "alice!a@host",
			Command: "PRIVMSG",
			Params:  []string{target, body},
			Tags:    irc.Tags{"time": ts.UTC().Format(time.RFC3339Nano)},
		}
		ev := &client.Event{Message: rawMsg}
		st.Ingest(netid, ev)
	}
	// Retrieve stored entries to get their server-assigned MsgIDs.
	entries := st.Latest(netid, target, len(msgs)+10)
	if len(entries) < len(msgs) {
		t.Fatalf("makeTestStore: ingested %d entries but Latest returned %d", len(msgs), len(entries))
	}
	return st, entries[len(entries)-len(msgs):]
}

// ─── server+pipe setup helpers ────────────────────────────────────────────────

// pipeServerCHBound creates a net.Pipe, builds a Server with the given store,
// and starts serveConnInternalNetid (the Phase 5 test seam) in a goroutine.
// The session's netid is set to netid before the run loop. Returns the
// client-side *conn.Conn. isTLS=false (no auth needed in these tests).
func pipeServerCHBound(t *testing.T, store *backlog.Store, netid int) *conn.Conn {
	t.Helper()
	return pipeServerCHBoundWith(t, &Config{}, store, netid, false)
}

// pipeServerCHBoundWith is the parameterized variant.
func pipeServerCHBoundWith(t *testing.T, cfg *Config, store *backlog.Store, netid int, isTLS bool) *conn.Conn {
	t.Helper()
	cRaw, sRaw := net.Pipe()
	clientConn := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = sRaw.Close()
	})
	srv := New(cfg)
	if store != nil {
		srv.store = store
	}
	go func() {
		_ = srv.serveConnInternalNetid(sRaw, isTLS, netid)
	}()
	return clientConn
}

// doRegister performs the minimum registration handshake on c, draining
// everything up to and including ERR_NOMOTD.
func doRegister(t *testing.T, c *conn.Conn) {
	t.Helper()
	sendLine(t, c, "NICK chatuser")
	sendLine(t, c, "USER chatuser 0 * :Chat User")
	skipTo(t, c, irc.ERR_NOMOTD)
}

// doRegisterWithCaps registers and negotiates the given list of caps.
func doRegisterWithCaps(t *testing.T, c *conn.Conn, caps ...string) {
	t.Helper()
	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c) // CAP LS reply
	if len(caps) > 0 {
		sendLine(t, c, "CAP REQ :"+strings.Join(caps, " "))
		_ = recvMsg(t, c) // ACK
	}
	sendLine(t, c, "NICK chatuser")
	sendLine(t, c, "USER chatuser 0 * :Chat User")
	sendLine(t, c, "CAP END")
	skipTo(t, c, irc.ERR_NOMOTD)
}

// ─── BATCH assertion helpers ──────────────────────────────────────────────────

// batchResult collects an entire BATCH open/entries/close sequence.
type batchResult struct {
	ref     string         // the batch reference token (without leading +/-)
	target  string         // param(2) of the open line (may be "" for TARGETS)
	btype   string         // param(1) of the open line (e.g. "chathistory")
	entries []*irc.Message // messages between open and close
}

// recvBatch reads the next BATCH sequence from c: one open line, zero or more
// entry lines, and one close line. The open and close must have matching refs.
func recvBatch(t *testing.T, c *conn.Conn) batchResult {
	t.Helper()
	// Expect BATCH open.
	open := skipTo(t, c, irc.BATCH)
	ref0 := open.Param(0)
	if !strings.HasPrefix(ref0, "+") {
		t.Fatalf("BATCH open: expected +<ref>, got %q", ref0)
	}
	ref := ref0[1:]
	result := batchResult{
		ref:    ref,
		btype:  open.Param(1),
		target: open.Param(2),
	}

	// Read until we see BATCH -<ref>.
	for {
		msg := recvMsg(t, c)
		if msg.Command == irc.BATCH {
			closeRef := msg.Param(0)
			if strings.HasPrefix(closeRef, "-") {
				if closeRef[1:] != ref {
					t.Fatalf("BATCH close ref mismatch: got %q, want -%q", closeRef, ref)
				}
				return result
			}
			// Nested BATCH open: treat it as an entry for now.
			result.entries = append(result.entries, msg)
			continue
		}
		result.entries = append(result.entries, msg)
	}
}

// assertBatchEntry checks that msg carries the required CHATHISTORY entry tags:
// @time (non-empty), @msgid (non-empty), @batch (== batchRef).
func assertBatchEntry(t *testing.T, msg *irc.Message, batchRef string) {
	t.Helper()
	if msg.Tags["time"] == "" {
		t.Errorf("entry %s: missing @time tag; tags=%v", msg.Command, msg.Tags)
	}
	if msg.Tags["msgid"] == "" {
		t.Errorf("entry %s: missing @msgid tag; tags=%v", msg.Command, msg.Tags)
	}
	if msg.Tags["batch"] != batchRef {
		t.Errorf("entry %s: @batch=%q, want %q", msg.Command, msg.Tags["batch"], batchRef)
	}
}

// ─── CHATHISTORY LATEST tests ─────────────────────────────────────────────────

// TestCHLatestStar verifies that CHATHISTORY LATEST <target> * <limit>
// returns the newest-N entries in a well-formed BATCH.
func TestCHLatestStar(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	store, entries := makeTestStore(t, 1, "#lurk",
		[]string{"msg0", "msg1", "msg2", "msg3", "msg4"}, base)
	_ = entries

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #lurk * 3")
	batch := recvBatch(t, c)

	if batch.btype != "chathistory" {
		t.Errorf("BATCH type = %q, want chathistory", batch.btype)
	}
	if batch.target != "#lurk" {
		t.Errorf("BATCH target = %q, want #lurk", batch.target)
	}
	// Limit 3 → newest 3: msg2, msg3, msg4.
	if len(batch.entries) != 3 {
		t.Fatalf("LATEST * limit=3: got %d entries, want 3", len(batch.entries))
	}
	for _, e := range batch.entries {
		assertBatchEntry(t, e, batch.ref)
		if e.Command != "PRIVMSG" {
			t.Errorf("entry command = %q, want PRIVMSG", e.Command)
		}
	}
	// Chronological order: msg2 first, msg4 last.
	if batch.entries[0].Param(1) != "msg2" {
		t.Errorf("first entry = %q, want msg2", batch.entries[0].Param(1))
	}
	if batch.entries[2].Param(1) != "msg4" {
		t.Errorf("last entry = %q, want msg4", batch.entries[2].Param(1))
	}
}

// TestCHLatestEmptyBatch verifies that an empty result still sends a
// well-formed BATCH open+close with no entry lines.
func TestCHLatestEmptyBatch(t *testing.T) {
	dir := t.TempDir()
	store, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	// No entries ingested.

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #empty * 10")
	batch := recvBatch(t, c)

	if batch.btype != "chathistory" {
		t.Errorf("BATCH type = %q, want chathistory", batch.btype)
	}
	if len(batch.entries) != 0 {
		t.Errorf("empty LATEST: got %d entries, want 0", len(batch.entries))
	}
}

// TestCHLatestCasemapping verifies that #Chan is treated the same as #chan
// (ascii casefold).
func TestCHLatestCasemapping(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Ingest under "#chan" (lowercased by safeName).
	store, _ := makeTestStore(t, 1, "#chan", []string{"hello"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// Query with uppercase #Chan — must find the same entry.
	sendLine(t, c, "CHATHISTORY LATEST #Chan * 5")
	batch := recvBatch(t, c)
	if len(batch.entries) != 1 {
		t.Errorf("casemapping: got %d entries for #Chan, want 1", len(batch.entries))
	}
}

// ─── CHATHISTORY BEFORE tests ────────────────────────────────────────────────

// TestCHBefore verifies CHATHISTORY BEFORE <target> msgid=<id> <limit> returns
// entries strictly older than the ref (exclusive) in chronological order.
func TestCHBefore(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#before",
		[]string{"m0", "m1", "m2", "m3", "m4"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// BEFORE m3 → m0,m1,m2.
	ref := "msgid=" + stored[3].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY BEFORE #before %s 10", ref))
	batch := recvBatch(t, c)

	if len(batch.entries) != 3 {
		t.Fatalf("BEFORE m3: got %d entries, want 3", len(batch.entries))
	}
	for i, e := range batch.entries {
		assertBatchEntry(t, e, batch.ref)
		want := fmt.Sprintf("m%d", i)
		if e.Param(1) != want {
			t.Errorf("[%d] = %q, want %q", i, e.Param(1), want)
		}
	}
	// Verify ref itself (m3) is NOT included.
	for _, e := range batch.entries {
		if e.Tags["msgid"] == stored[3].MsgID {
			t.Errorf("BEFORE: ref entry (m3) must not appear in results")
		}
	}
}

// TestCHBeforeLimit verifies the limit is respected.
func TestCHBeforeLimit(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#blim",
		[]string{"a", "b", "c", "d", "e"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// BEFORE e (index 4) with limit 2 → c,d (newest 2 before e).
	ref := "msgid=" + stored[4].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY BEFORE #blim %s 2", ref))
	batch := recvBatch(t, c)

	if len(batch.entries) != 2 {
		t.Fatalf("BEFORE limit=2: got %d, want 2", len(batch.entries))
	}
	if batch.entries[0].Param(1) != "c" || batch.entries[1].Param(1) != "d" {
		t.Errorf("BEFORE limit=2: got %q,%q want c,d",
			batch.entries[0].Param(1), batch.entries[1].Param(1))
	}
}

// ─── CHATHISTORY AFTER tests ──────────────────────────────────────────────────

// TestCHAfter verifies CHATHISTORY AFTER returns entries strictly newer than ref.
func TestCHAfter(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#after",
		[]string{"p0", "p1", "p2", "p3"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// AFTER p1 → p2, p3.
	ref := "msgid=" + stored[1].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY AFTER #after %s 10", ref))
	batch := recvBatch(t, c)

	if len(batch.entries) != 2 {
		t.Fatalf("AFTER p1: got %d, want 2", len(batch.entries))
	}
	if batch.entries[0].Param(1) != "p2" || batch.entries[1].Param(1) != "p3" {
		t.Errorf("AFTER p1: got %q,%q want p2,p3",
			batch.entries[0].Param(1), batch.entries[1].Param(1))
	}
	// Verify p1 itself is NOT included.
	for _, e := range batch.entries {
		if e.Tags["msgid"] == stored[1].MsgID {
			t.Errorf("AFTER: ref entry (p1) must not appear in results")
		}
	}
	for _, e := range batch.entries {
		assertBatchEntry(t, e, batch.ref)
	}
}

// ─── CHATHISTORY AROUND tests ─────────────────────────────────────────────────

// TestCHAround verifies CHATHISTORY AROUND returns entries centered on the ref.
func TestCHAround(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#around",
		[]string{"r0", "r1", "r2", "r3", "r4", "r5", "r6"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// AROUND r3 limit=5 → r1,r2,r3,r4,r5 (2 before, pivot, 2 after).
	ref := "msgid=" + stored[3].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY AROUND #around %s 5", ref))
	batch := recvBatch(t, c)

	if len(batch.entries) != 5 {
		t.Fatalf("AROUND r3 limit=5: got %d, want 5", len(batch.entries))
	}
	// Verify r3 (the pivot) is among the results.
	foundPivot := false
	for _, e := range batch.entries {
		assertBatchEntry(t, e, batch.ref)
		if e.Tags["msgid"] == stored[3].MsgID {
			foundPivot = true
		}
	}
	if !foundPivot {
		t.Errorf("AROUND: pivot r3 not found in results")
	}
	// Chronological order.
	for i := 1; i < len(batch.entries); i++ {
		prev := batch.entries[i-1].Tags["time"]
		curr := batch.entries[i].Tags["time"]
		if curr < prev {
			t.Errorf("AROUND: out of order at [%d]: %q < %q", i, curr, prev)
		}
	}
}

// ─── CHATHISTORY BETWEEN tests ────────────────────────────────────────────────

// TestCHBetween verifies CHATHISTORY BETWEEN returns [fromRef, toRef).
func TestCHBetween(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#between",
		[]string{"x0", "x1", "x2", "x3", "x4"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// BETWEEN x1 x4 (exclusive end) → x1, x2, x3.
	from := "msgid=" + stored[1].MsgID
	to := "msgid=" + stored[4].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #between %s %s 10", from, to))
	batch := recvBatch(t, c)

	if len(batch.entries) != 3 {
		t.Fatalf("BETWEEN x1 x4: got %d, want 3 (x1,x2,x3)", len(batch.entries))
	}
	for i, e := range batch.entries {
		assertBatchEntry(t, e, batch.ref)
		want := fmt.Sprintf("x%d", i+1)
		if e.Param(1) != want {
			t.Errorf("[%d] = %q, want %q", i, e.Param(1), want)
		}
	}
	// Verify x4 (the exclusive end) is NOT included.
	for _, e := range batch.entries {
		if e.Tags["msgid"] == stored[4].MsgID {
			t.Errorf("BETWEEN: exclusive end x4 must not appear in results")
		}
	}
}

// TestCHBetweenReversedTimestampBounds verifies that CHATHISTORY BETWEEN rejects
// reversed (toRef <= fromRef) timestamp bounds with FAIL INVALID_PARAMS rather
// than silently triggering a full JSONL scan that returns an empty batch.
func TestCHBetweenReversedTimestampBounds(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	t0 := "timestamp=2024-01-01T00:00:00Z"
	t1 := "timestamp=2024-06-01T00:00:00Z"

	cases := []struct {
		name string
		from string
		to   string
	}{
		{"reversed", t1, t0}, // toRef strictly before fromRef
		{"equal", t0, t0},    // toRef == fromRef
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := pipeServerCHBound(t, store, 1)
			doRegister(t, c)

			sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #foo %s %s 5", tc.from, tc.to))
			msg := recvMsg(t, c)
			assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
			if !strings.Contains(msg.Param(1), "INVALID_PARAMS") {
				t.Errorf("BETWEEN %s: FAIL code = %q, want INVALID_PARAMS", tc.name, msg.Param(1))
			}
		})
	}
}

// TestCHBetweenValidTimestampBounds verifies that BETWEEN with a valid
// (fromRef strictly before toRef) timestamp window is not rejected.
func TestCHBetweenValidTimestampBounds(t *testing.T) {
	// Use a recent base within the clamp window so that entries get distinct
	// 1-second-apart timestamps; stored[0].Time is strictly before stored[3].Time.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	store, stored := makeTestStore(t, 1, "#bvalid",
		[]string{"v0", "v1", "v2", "v3"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// Use timestamp refs that bracket v1 and v2 (from strictly before v1's time,
	// to strictly after v2's time but before v3's time).
	from := fmt.Sprintf("timestamp=%s", stored[0].Time.UTC().Format(time.RFC3339))
	to := fmt.Sprintf("timestamp=%s", stored[3].Time.UTC().Format(time.RFC3339))
	sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #bvalid %s %s 10", from, to))
	batch := recvBatch(t, c)

	// Must get a BATCH, not a FAIL.
	if batch.btype != "chathistory" {
		t.Errorf("valid BETWEEN: BATCH type = %q, want chathistory", batch.btype)
	}
}

// TestCHBetweenMsgIDRefsUnaffected verifies that the reversed-bounds guard
// does not affect msgid= refs (we cannot compare msgids for ordering).
func TestCHBetweenMsgIDRefsUnaffected(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#bmsgid",
		[]string{"m0", "m1", "m2", "m3"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// Supply msgid refs in "reversed" logical order — guard must not fire;
	// the store handles these by returning an empty result.
	from := "msgid=" + stored[3].MsgID
	to := "msgid=" + stored[0].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #bmsgid %s %s 10", from, to))
	batch := recvBatch(t, c)

	// No FAIL — the response must be a BATCH (empty result is fine).
	if batch.btype != "chathistory" {
		t.Errorf("msgid BETWEEN: expected BATCH response, got BATCH type %q", batch.btype)
	}
}

// ─── CHATHISTORY TARGETS tests ────────────────────────────────────────────────

// TestCHTargets verifies CHATHISTORY TARGETS returns target info in a BATCH.
func TestCHTargets(t *testing.T) {
	// Use a recent base within the clamp window so ingested entries retain their
	// supplied timestamps; from/to must bracket the clamped times.
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	dir := t.TempDir()
	store, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	// Ingest into two targets.
	ingestAt := func(target, body string, ts time.Time) {
		rawMsg := &irc.Message{
			Source:  "u!u@h",
			Command: "PRIVMSG",
			Params:  []string{target, body},
			Tags:    irc.Tags{"time": ts.UTC().Format(time.RFC3339Nano)},
		}
		store.Ingest(1, &client.Event{Message: rawMsg})
	}
	ingestAt("#alpha", "hi", base)
	ingestAt("#beta", "hey", base.Add(time.Minute))

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	from := fmt.Sprintf("timestamp=%s", base.Add(-time.Minute).UTC().Format(time.RFC3339))
	to := fmt.Sprintf("timestamp=%s", base.Add(time.Hour).UTC().Format(time.RFC3339))
	sendLine(t, c, fmt.Sprintf("CHATHISTORY TARGETS %s %s 10", from, to))
	batch := recvBatch(t, c)

	if batch.btype != "chathistory" {
		t.Errorf("TARGETS BATCH type = %q, want chathistory", batch.btype)
	}
	if len(batch.entries) != 2 {
		t.Fatalf("TARGETS: got %d entries, want 2", len(batch.entries))
	}
	// Each entry should have @time and @batch tags.
	for _, e := range batch.entries {
		if e.Tags["batch"] != batch.ref {
			t.Errorf("TARGETS entry: @batch=%q, want %q", e.Tags["batch"], batch.ref)
		}
		if e.Tags["time"] == "" {
			t.Errorf("TARGETS entry: missing @time tag")
		}
	}
}

// TestCHTargetsRejectsNonTimestampRefs verifies that CHATHISTORY TARGETS
// rejects msgid= and "*" refs with FAIL INVALID_PARAMS. The spec requires
// timestamp= refs for TARGETS; silently accepting a msgid= or "*" ref would
// use a zero time as the bound, widening the effective query window.
func TestCHTargetsRejectsNonTimestampRefs(t *testing.T) {
	dir := t.TempDir()
	store, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	ts := "timestamp=2024-01-01T00:00:00Z"

	cases := []struct {
		name    string
		fromRef string
		toRef   string
	}{
		{"both msgid", "msgid=abc", "msgid=def"},
		{"from msgid", "msgid=abc", ts},
		{"to msgid", ts, "msgid=def"},
		{"from star", "*", ts},
		{"to star", ts, "*"},
		{"both star", "*", "*"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := pipeServerCHBound(t, store, 1)
			doRegister(t, c)

			sendLine(t, c, fmt.Sprintf("CHATHISTORY TARGETS %s %s 5", tc.fromRef, tc.toRef))
			msg := recvMsg(t, c)
			assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
			if !strings.Contains(msg.Param(1), "INVALID_PARAMS") {
				t.Errorf("TARGETS non-timestamp ref: FAIL code = %q, want INVALID_PARAMS", msg.Param(1))
			}
		})
	}
}

// TestCHTargetsTimestampRefsAccepted verifies that CHATHISTORY TARGETS with
// valid timestamp= refs on both sides is not rejected by the new guard.
func TestCHTargetsTimestampRefsAccepted(t *testing.T) {
	base := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	from := fmt.Sprintf("timestamp=%s", base.Add(-time.Minute).UTC().Format(time.RFC3339))
	to := fmt.Sprintf("timestamp=%s", base.Add(time.Hour).UTC().Format(time.RFC3339))
	sendLine(t, c, fmt.Sprintf("CHATHISTORY TARGETS %s %s 10", from, to))
	batch := recvBatch(t, c)

	// Empty store → empty batch, but it must be a well-formed BATCH, not a FAIL.
	if batch.btype != "chathistory" {
		t.Errorf("TARGETS timestamp refs: BATCH type = %q, want chathistory", batch.btype)
	}
}

// ─── Limit ceiling tests ──────────────────────────────────────────────────────

// TestCHLimitCeiling verifies that a client-supplied limit above maxCHATHISTORYLimit
// is clamped to the server maximum.
func TestCHLimitCeiling(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Ingest more than maxCHATHISTORYLimit entries.
	var msgs []string
	for i := 0; i < maxCHATHISTORYLimit+20; i++ {
		msgs = append(msgs, fmt.Sprintf("msg%d", i))
	}
	store, _ := makeTestStore(t, 1, "#ceil", msgs, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// Request with a huge limit.
	sendLine(t, c, fmt.Sprintf("CHATHISTORY LATEST #ceil * %d", maxCHATHISTORYLimit*10))
	batch := recvBatch(t, c)

	// Must not exceed maxCHATHISTORYLimit.
	if len(batch.entries) > maxCHATHISTORYLimit {
		t.Errorf("limit ceiling: got %d entries, must be <= %d", len(batch.entries), maxCHATHISTORYLimit)
	}
}

// ─── Malformed / hostile input tests ─────────────────────────────────────────

// TestCHMalformedSubcommand verifies that an unknown subcommand yields FAIL
// INVALID_PARAMS and does not panic.
func TestCHMalformedSubcommand(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY EVILCMD #target msgid=foo 10")
	msg := recvMsg(t, c)
	assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
	if !strings.Contains(msg.Param(1), "INVALID_PARAMS") {
		t.Errorf("FAIL code = %q, want INVALID_PARAMS", msg.Param(1))
	}
}

// TestCHMissingParams verifies that too few parameters yield FAIL NEED_MORE_PARAMS.
func TestCHMissingParams(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// LATEST with no args.
	sendLine(t, c, "CHATHISTORY LATEST")
	msg := recvMsg(t, c)
	assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
	if !strings.Contains(msg.Param(1), "NEED_MORE_PARAMS") {
		t.Errorf("LATEST no-args: FAIL code = %q, want NEED_MORE_PARAMS", msg.Param(1))
	}

	// BEFORE with too few args.
	sendLine(t, c, "CHATHISTORY BEFORE #foo")
	msg = recvMsg(t, c)
	assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
	if !strings.Contains(msg.Param(1), "NEED_MORE_PARAMS") {
		t.Errorf("BEFORE too-few-args: FAIL code = %q, want NEED_MORE_PARAMS", msg.Param(1))
	}
}

// TestCHInvalidLimit verifies that a non-integer limit yields FAIL INVALID_PARAMS.
func TestCHInvalidLimit(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #foo * notanumber")
	msg := recvMsg(t, c)
	assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
	if !strings.Contains(msg.Param(1), "INVALID_PARAMS") {
		t.Errorf("invalid limit: FAIL code = %q, want INVALID_PARAMS", msg.Param(1))
	}
}

// TestCHUnboundSessionFails verifies that CHATHISTORY on an unbound (netid=0)
// session yields FAIL NO_BOUND_NETWORK.
func TestCHUnboundSessionFails(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	// netid=0 → unbound.
	c := pipeServerCHBound(t, store, 0)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #foo * 5")
	msg := recvMsg(t, c)
	assertMsg(t, msg, irc.FAIL, "CHATHISTORY")
	if !strings.Contains(msg.Param(1), "NO_BOUND_NETWORK") {
		t.Errorf("unbound: FAIL code = %q, want NO_BOUND_NETWORK", msg.Param(1))
	}
}

// TestCHBeforeRegistrationFails verifies that CHATHISTORY before registration
// yields ERR_NOTREGISTERED.
func TestCHBeforeRegistrationFails(t *testing.T) {
	dir := t.TempDir()
	store, _ := backlog.NewStore(dir)
	t.Cleanup(store.Close)

	c := pipeServerCHBound(t, store, 1)
	// Do NOT register.

	sendLine(t, c, "CHATHISTORY LATEST #foo * 5")
	msg := recvMsg(t, c)
	assertMsg(t, msg, irc.ERR_NOTREGISTERED)
}

// ─── Event-playback gate tests ────────────────────────────────────────────────

// TestCHEventPlaybackGate verifies that without draft/event-playback, only
// PRIVMSG and NOTICE entries appear; with the cap, all types appear.
// In v1 the store only stores PRIVMSG/NOTICE, but we test the filter logic
// by using a custom scenario where we inject mixed types.
//
// Since the v1 store only stores PRIVMSG/NOTICE, this test ingests PRIVMSG and
// NOTICE, confirms both appear WITHOUT event-playback (they're the allowed set),
// and also tests that the gate doesn't accidentally drop them.
func TestCHEventPlaybackGate(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := backlog.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	// Ingest a PRIVMSG and a NOTICE.
	makeMsg := func(cmd, body string, ts time.Time) *client.Event {
		return &client.Event{Message: &irc.Message{
			Source:  "u!u@h",
			Command: cmd,
			Params:  []string{"#gate", body},
			Tags:    irc.Tags{"time": ts.UTC().Format(time.RFC3339Nano)},
		}}
	}
	store.Ingest(1, makeMsg("PRIVMSG", "privmsg-body", base))
	store.Ingest(1, makeMsg("NOTICE", "notice-body", base.Add(time.Second)))

	// Without event-playback cap: PRIVMSG and NOTICE are the allowed set,
	// so both should appear (no entries are filtered out in v1).
	t.Run("WithoutEventPlayback", func(t *testing.T) {
		c := pipeServerCHBound(t, store, 1)
		doRegister(t, c) // no caps negotiated

		sendLine(t, c, "CHATHISTORY LATEST #gate * 10")
		batch := recvBatch(t, c)
		if len(batch.entries) != 2 {
			t.Fatalf("without event-playback: got %d entries, want 2 (PRIVMSG+NOTICE)", len(batch.entries))
		}
		cmds := make(map[string]bool)
		for _, e := range batch.entries {
			cmds[e.Command] = true
		}
		if !cmds["PRIVMSG"] || !cmds["NOTICE"] {
			t.Errorf("without event-playback: expected both PRIVMSG and NOTICE; cmds=%v", cmds)
		}
	})

	// With event-playback cap: same result for v1 (no additional event types stored).
	t.Run("WithEventPlayback", func(t *testing.T) {
		c := pipeServerCHBound(t, store, 1)
		doRegisterWithCaps(t, c, "draft/event-playback")

		sendLine(t, c, "CHATHISTORY LATEST #gate * 10")
		batch := recvBatch(t, c)
		if len(batch.entries) != 2 {
			t.Fatalf("with event-playback: got %d entries, want 2", len(batch.entries))
		}
	})
}

// ─── Server-time and msgid on every entry ─────────────────────────────────────

// TestCHEveryEntryHasTimeAndMsgid is an explicit check that every replayed
// line carries non-empty @time and @msgid tags.
func TestCHEveryEntryHasTimeAndMsgid(t *testing.T) {
	base := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	store, _ := makeTestStore(t, 1, "#tags",
		[]string{"a", "b", "c"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #tags * 10")
	batch := recvBatch(t, c)

	if len(batch.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(batch.entries))
	}
	for i, e := range batch.entries {
		if e.Tags["time"] == "" {
			t.Errorf("entry[%d]: @time is empty", i)
		}
		if e.Tags["msgid"] == "" {
			t.Errorf("entry[%d]: @msgid is empty", i)
		}
		if e.Tags["batch"] == "" {
			t.Errorf("entry[%d]: @batch is empty", i)
		}
	}
}

// ─── Unique batch reference per query ─────────────────────────────────────────

// TestCHUniqueBatchRefs verifies that two consecutive CHATHISTORY queries
// produce different batch reference tokens.
func TestCHUniqueBatchRefs(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, _ := makeTestStore(t, 1, "#uniq", []string{"m"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	sendLine(t, c, "CHATHISTORY LATEST #uniq * 1")
	b1 := recvBatch(t, c)
	sendLine(t, c, "CHATHISTORY LATEST #uniq * 1")
	b2 := recvBatch(t, c)

	if b1.ref == b2.ref {
		t.Errorf("batch refs should be unique but both = %q", b1.ref)
	}
}

// ─── CHATHISTORY before registration ─────────────────────────────────────────

// TestCHLatestWithMsgIDRef verifies CHATHISTORY LATEST with a msgid= ref
// (acts like AFTER — returns entries newer than that ref).
func TestCHLatestWithMsgIDRef(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	store, stored := makeTestStore(t, 1, "#latestref",
		[]string{"q0", "q1", "q2", "q3"}, base)

	c := pipeServerCHBound(t, store, 1)
	doRegister(t, c)

	// LATEST with msgid= of q1 → entries after q1 = q2, q3.
	ref := "msgid=" + stored[1].MsgID
	sendLine(t, c, fmt.Sprintf("CHATHISTORY LATEST #latestref %s 10", ref))
	batch := recvBatch(t, c)

	if len(batch.entries) != 2 {
		t.Fatalf("LATEST msgid=q1: got %d entries, want 2 (q2,q3)", len(batch.entries))
	}
	if batch.entries[0].Param(1) != "q2" || batch.entries[1].Param(1) != "q3" {
		t.Errorf("LATEST msgid=q1: got %q,%q want q2,q3",
			batch.entries[0].Param(1), batch.entries[1].Param(1))
	}
}
