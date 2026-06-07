// chathistory.go implements the IRCv3 draft/chathistory server (Phase 5).
//
// Supported subcommands:
//
//	CHATHISTORY LATEST  <target> <ref|*> <limit>
//	CHATHISTORY BEFORE  <target> <ref> <limit>
//	CHATHISTORY AFTER   <target> <ref> <limit>
//	CHATHISTORY AROUND  <target> <ref> <limit>
//	CHATHISTORY BETWEEN <target> <fromRef> <toRef> <limit>
//	CHATHISTORY TARGETS <fromRef> <toRef> <limit>
//
// Results are enclosed in a draft/chathistory BATCH:
//
//	:server BATCH +<ref> chathistory <target>
//	@time=…;msgid=…;batch=<ref> :source PRIVMSG <target> :body
//	…
//	:server BATCH -<ref>
//
// An empty result still sends a well-formed open+close pair per the spec.
//
// Gates:
//   - The session must be registered (caller in dispatch ensures this).
//   - The session must be bound to a netid (netid != 0); unbound sessions get
//     FAIL CHATHISTORY NO_BOUND_NETWORK.
//   - Without draft/event-playback negotiated, only PRIVMSG/NOTICE appear in
//     results. With the cap, all stored event types appear (v1 only stores
//     PRIVMSG/NOTICE anyway, but the filter is correct for future types).
//   - The client-supplied limit is clamped to maxCHATHISTORYLimit.
//
// Casemapping:
//   - Targets are folded via safeName (ascii-lower) before querying the store,
//     matching how the store keys entries. This handles #Chan vs #chan.
//   - TODO(Phase 6): use the upstream's CASEMAPPING (rfc1459 vs ascii) once the
//     upstream state is accessible from the session; for v1 ascii fold is used.
package server

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/irc"
)

// maxCHATHISTORYLimit is the server-side ceiling on client-supplied limit
// parameters. A hostile huge limit must not trigger unbounded work.
const maxCHATHISTORYLimit = 100

// handleCHATHISTORY dispatches a CHATHISTORY command.
// Pre-condition: session is registered (caller has already checked).
func (s *session) handleCHATHISTORY(msg *irc.Message) error {
	sub := strings.ToUpper(msg.Param(0))
	switch sub {
	case "LATEST":
		return s.handleCHLatest(msg)
	case "BEFORE":
		return s.handleCHBefore(msg)
	case "AFTER":
		return s.handleCHAfter(msg)
	case "AROUND":
		return s.handleCHAround(msg)
	case "BETWEEN":
		return s.handleCHBetween(msg)
	case "TARGETS":
		return s.handleCHTargets(msg)
	case "":
		return s.sendFail("CHATHISTORY", "NEED_MORE_PARAMS", "CHATHISTORY requires a subcommand")
	default:
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("Unknown CHATHISTORY subcommand: %s", sub))
	}
}

// handleCHLatest implements CHATHISTORY LATEST <target> <ref|*> <limit>.
// If ref is "*", returns the newest limit entries.
// If ref is a msgid or timestamp, returns entries newer than that ref (same as
// AFTER but tolerates "*").
func (s *session) handleCHLatest(msg *irc.Message) error {
	if msg.Param(1) == "" || msg.Param(3) == "" {
		return s.sendFail("CHATHISTORY", "NEED_MORE_PARAMS", "CHATHISTORY LATEST requires: <target> <ref|*> <limit>")
	}
	target := msg.Param(1)
	refStr := msg.Param(2)
	limitStr := msg.Param(3)

	limit, ok := parseLimit(limitStr)
	if !ok {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", "CHATHISTORY: limit must be a positive integer")
	}

	ref, err := backlog.ParseRef(refStr)
	if err != nil {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("CHATHISTORY: invalid ref: %v", err))
	}

	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}

	var entries []backlog.Entry
	if s.srv.store != nil {
		if ref.IsStar {
			entries = s.srv.store.Latest(s.netid, target, limit)
		} else {
			entries = s.srv.store.After(s.netid, target, ref, limit)
		}
	}

	entries = s.filterEventPlayback(entries)
	return s.sendCHBatch(target, entries)
}

// handleCHBefore implements CHATHISTORY BEFORE <target> <ref> <limit>.
func (s *session) handleCHBefore(msg *irc.Message) error {
	target, ref, limit, err := parseSingleRef(msg, "BEFORE")
	if err != nil {
		return s.sendFail("CHATHISTORY", failCode(err), err.Error())
	}
	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}
	var entries []backlog.Entry
	if s.srv.store != nil {
		entries = s.srv.store.Before(s.netid, target, ref, limit)
	}
	entries = s.filterEventPlayback(entries)
	return s.sendCHBatch(target, entries)
}

// handleCHAfter implements CHATHISTORY AFTER <target> <ref> <limit>.
func (s *session) handleCHAfter(msg *irc.Message) error {
	target, ref, limit, err := parseSingleRef(msg, "AFTER")
	if err != nil {
		return s.sendFail("CHATHISTORY", failCode(err), err.Error())
	}
	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}
	var entries []backlog.Entry
	if s.srv.store != nil {
		entries = s.srv.store.After(s.netid, target, ref, limit)
	}
	entries = s.filterEventPlayback(entries)
	return s.sendCHBatch(target, entries)
}

// handleCHAround implements CHATHISTORY AROUND <target> <ref> <limit>.
func (s *session) handleCHAround(msg *irc.Message) error {
	target, ref, limit, err := parseSingleRef(msg, "AROUND")
	if err != nil {
		return s.sendFail("CHATHISTORY", failCode(err), err.Error())
	}
	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}
	var entries []backlog.Entry
	if s.srv.store != nil {
		entries = s.srv.store.Around(s.netid, target, ref, limit)
	}
	entries = s.filterEventPlayback(entries)
	return s.sendCHBatch(target, entries)
}

// handleCHBetween implements CHATHISTORY BETWEEN <target> <fromRef> <toRef> <limit>.
func (s *session) handleCHBetween(msg *irc.Message) error {
	// Params: [0]=BETWEEN [1]=target [2]=fromRef [3]=toRef [4]=limit
	target := msg.Param(1)
	fromStr := msg.Param(2)
	toStr := msg.Param(3)
	limitStr := msg.Param(4)

	if target == "" || fromStr == "" || toStr == "" || limitStr == "" {
		return s.sendFail("CHATHISTORY", "NEED_MORE_PARAMS", "CHATHISTORY BETWEEN requires: <target> <fromRef> <toRef> <limit>")
	}

	fromRef, err := backlog.ParseRef(fromStr)
	if err != nil {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("CHATHISTORY: invalid fromRef: %v", err))
	}
	toRef, err := backlog.ParseRef(toStr)
	if err != nil {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("CHATHISTORY: invalid toRef: %v", err))
	}

	// When both refs are timestamps we can validate the window before touching
	// the store. A toRef that is at or before fromRef produces an empty result
	// by definition, but still forces a full JSONL scan (up to maxScanLines).
	// Reject it up-front to close the cheap-request IO/CPU amplifier.
	if fromRef.IsTime && toRef.IsTime && !fromRef.Time.Before(toRef.Time) {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", "CHATHISTORY BETWEEN: fromRef must be strictly before toRef")
	}

	limit, ok := parseLimit(limitStr)
	if !ok {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", "CHATHISTORY: limit must be a positive integer")
	}

	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}
	var entries []backlog.Entry
	if s.srv.store != nil {
		entries = s.srv.store.Between(s.netid, target, fromRef, toRef, limit)
	}
	entries = s.filterEventPlayback(entries)
	return s.sendCHBatch(target, entries)
}

// handleCHTargets implements CHATHISTORY TARGETS <fromRef> <toRef> <limit>.
// The BATCH for TARGETS uses "chathistory" with a special target "".
func (s *session) handleCHTargets(msg *irc.Message) error {
	// Params: [0]=TARGETS [1]=fromRef [2]=toRef [3]=limit
	fromStr := msg.Param(1)
	toStr := msg.Param(2)
	limitStr := msg.Param(3)

	if fromStr == "" || toStr == "" || limitStr == "" {
		return s.sendFail("CHATHISTORY", "NEED_MORE_PARAMS", "CHATHISTORY TARGETS requires: <fromRef> <toRef> <limit>")
	}

	fromRef, err := backlog.ParseRef(fromStr)
	if err != nil {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("CHATHISTORY: invalid fromRef: %v", err))
	}
	toRef, err := backlog.ParseRef(toStr)
	if err != nil {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", fmt.Sprintf("CHATHISTORY: invalid toRef: %v", err))
	}

	// The IRCv3 draft/chathistory spec requires timestamp= refs for TARGETS.
	// A msgid= or "*" ref is a client error: reject it rather than silently
	// treating it as a zero time, which would widen the effective query window
	// in an unpredictable way and violates the spec.
	if !fromRef.IsTime || !toRef.IsTime {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", "CHATHISTORY TARGETS: fromRef and toRef must be timestamp= refs")
	}

	limit, ok := parseLimit(limitStr)
	if !ok {
		return s.sendFail("CHATHISTORY", "INVALID_PARAMS", "CHATHISTORY: limit must be a positive integer")
	}

	if s.netid == 0 {
		return s.sendFail("CHATHISTORY", "NO_BOUND_NETWORK", "No network bound to this session")
	}

	// Both refs are guaranteed IsTime by the check above.
	fromTime := fromRef.Time
	toTime := toRef.Time

	var targets []backlog.TargetInfo
	if s.srv.store != nil {
		targets = s.srv.store.Targets(s.netid, fromTime, toTime, limit)
	}

	return s.sendTargetsBatch(targets)
}

// ─── BATCH framing ────────────────────────────────────────────────────────────

// sendCHBatch sends a draft/chathistory BATCH for the given target and entries.
// Even if entries is empty, the open+close pair is always emitted.
// Every replayed entry carries @time, @msgid, and @batch tags.
func (s *session) sendCHBatch(target string, entries []backlog.Entry) error {
	batchRef := newBatchRef()

	// BATCH open: :server BATCH +<ref> chathistory <target>
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"+" + batchRef, "chathistory", target},
	}); err != nil {
		return err
	}

	// Replay each entry.
	for _, e := range entries {
		if err := s.sendCHEntry(batchRef, e); err != nil {
			// On error we must still close the batch to avoid leaving it open.
			// Best-effort: log and close.
			log.Printf("server: CHATHISTORY: error replaying entry: %v", err)
			break
		}
	}

	// BATCH close: :server BATCH -<ref>
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"-" + batchRef},
	})
}

// sendTargetsBatch sends the CHATHISTORY TARGETS response as a BATCH.
// Each target is sent as a CHATHISTORY TARGETS line with @time.
func (s *session) sendTargetsBatch(targets []backlog.TargetInfo) error {
	batchRef := newBatchRef()

	// BATCH open: :server BATCH +<ref> chathistory
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"+" + batchRef, "chathistory"},
	}); err != nil {
		return err
	}

	for _, ti := range targets {
		// Per the IRCv3 chathistory spec, TARGETS results are sent as
		// CHATHISTORY TARGETS <target> <timestamp> lines inside the batch.
		ts := formatServerTime(ti.Latest)
		if err := s.send(&irc.Message{
			Tags: irc.Tags{
				"batch": batchRef,
				"time":  ts,
			},
			Source:  serverName,
			Command: irc.CHATHISTORY,
			Params:  []string{"TARGETS", ti.Target, "timestamp=" + ts},
		}); err != nil {
			log.Printf("server: CHATHISTORY TARGETS: error sending target %q: %v", ti.Target, err)
			break
		}
	}

	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"-" + batchRef},
	})
}

// sendCHEntry sends one replayed backlog entry inside the batch.
// The entry is sent as its original IRC message (Source, Command, Params)
// with @time, @msgid, and @batch tags set.
func (s *session) sendCHEntry(batchRef string, e backlog.Entry) error {
	tags := irc.Tags{
		"time":  formatServerTime(e.Time),
		"msgid": e.MsgID,
		"batch": batchRef,
	}
	return s.send(&irc.Message{
		Tags:    tags,
		Source:  e.Source,
		Command: e.Command,
		Params:  e.Params,
	})
}

// ─── Gates ───────────────────────────────────────────────────────────────────

// filterEventPlayback applies the draft/event-playback gate:
// - Without the cap: only PRIVMSG and NOTICE pass through.
// - With the cap: all stored event types pass through.
//
// In v1 the store only ingests PRIVMSG/NOTICE, so this is currently a no-op.
// The filter is implemented now so future event types are handled correctly
// without changing this code.
func (s *session) filterEventPlayback(entries []backlog.Entry) []backlog.Entry {
	if s.capEnabled["draft/event-playback"] {
		// All event types allowed.
		return entries
	}
	// No event-playback: only PRIVMSG/NOTICE.
	out := entries[:0:0] // nil-safe empty slice sharing no backing array
	for _, e := range entries {
		if e.Command == "PRIVMSG" || e.Command == "NOTICE" {
			out = append(out, e)
		}
	}
	return out
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// failCode extracts the FAIL code from a *parseSingleRefErr; defaults to
// "INVALID_PARAMS" for any other error type.
func failCode(err error) string {
	if e, ok := err.(*parseSingleRefErr); ok {
		return e.code
	}
	return "INVALID_PARAMS"
}

// sendFail sends a standard-replies FAIL message.
// Format: FAIL <command> <code> :<description>
func (s *session) sendFail(command, code, description string) error {
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.FAIL,
		Params:  []string{command, code, description},
	})
}

// parseLimit parses a limit string to a positive integer clamped to
// maxCHATHISTORYLimit. Returns (0, false) if the string is not a valid
// positive integer.
func parseLimit(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > maxCHATHISTORYLimit {
		n = maxCHATHISTORYLimit
	}
	return n, true
}

// parseSingleRefErr is the error type returned by parseSingleRef so callers
// can distinguish NEED_MORE_PARAMS (missing parameter) from INVALID_PARAMS
// (malformed parameter).
type parseSingleRefErr struct {
	code string // "NEED_MORE_PARAMS" or "INVALID_PARAMS"
	msg  string
}

func (e *parseSingleRefErr) Error() string { return e.msg }

// parseSingleRef parses the common CHATHISTORY subcommand shape:
// msg.Param(1)=target, msg.Param(2)=ref, msg.Param(3)=limit.
// Returns a *parseSingleRefErr with the appropriate FAIL code.
func parseSingleRef(msg *irc.Message, sub string) (target string, ref backlog.Ref, limit int, err error) {
	target = msg.Param(1)
	refStr := msg.Param(2)
	limitStr := msg.Param(3)

	if target == "" || refStr == "" || limitStr == "" {
		err = &parseSingleRefErr{
			code: "NEED_MORE_PARAMS",
			msg:  fmt.Sprintf("CHATHISTORY %s requires: <target> <ref> <limit>", sub),
		}
		return
	}

	ref, err = backlog.ParseRef(refStr)
	if err != nil {
		err = &parseSingleRefErr{
			code: "INVALID_PARAMS",
			msg:  fmt.Sprintf("CHATHISTORY: invalid ref: %v", err),
		}
		return
	}

	var ok bool
	limit, ok = parseLimit(limitStr)
	if !ok {
		err = &parseSingleRefErr{
			code: "INVALID_PARAMS",
			msg:  "CHATHISTORY: limit must be a positive integer",
		}
		return
	}
	return
}

// formatServerTime formats t as an RFC3339 timestamp with millisecond
// precision, as required by the IRCv3 server-time spec (@time tag).
func formatServerTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// newBatchRef generates a unique, opaque batch reference token for one BATCH
// open/close pair. Uses 9 random bytes → 12-char base64url string.
func newBatchRef() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		// This should never fail; if it does, use a fallback non-random token.
		// An adversary cannot predict a fallback token from this error path, and
		// the worst outcome is a non-unique batch label — not a security issue.
		log.Printf("server: newBatchRef: crypto/rand failed: %v (using fallback)", err)
		return fmt.Sprintf("b%d", time.Now().UnixNano())
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}
