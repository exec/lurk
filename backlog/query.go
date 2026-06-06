// query.go extends the backlog Store with the CHATHISTORY read API:
// Before, After, Around, Between, and Targets. These methods serve the
// CHATHISTORY subcommands defined in the IRCv3 draft/chathistory spec.
//
// # Reference format
//
// A ref is either:
//   - "msgid=<opaque>"    — identifies an entry by its MsgID field
//   - "timestamp=<RFC3339>" — identifies an entry by its Time field
//   - "*"                  — a sentinel meaning "no reference" (for LATEST)
//
// A ref that does not resolve to any stored entry yields an empty result (not
// an error), per the spec: the server MUST return an empty BATCH rather than
// an error when the ref is valid but not found.
//
// # JSONL scan policy
//
// All boundary queries (Before/After/Around/Between) scan the on-disk JSONL
// file rather than the in-memory ring. The ring is only a fast path for Latest
// (newest-N without a reference). JSONL files are bounded by DefaultRingSize
// entries per target (drop-oldest during Ingest), so a full scan is O(ring-cap)
// lines — small by design. A per-call hard cap (maxScanLines) guards against
// pathologically large files that could accumulate before the ring cap takes
// full effect on an older store.
package backlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxScanLines is the hard per-call upper bound on lines read from a JSONL file
// during a boundary query. A store operating within DefaultRingSize will never
// approach this; it is a safety net against unexpectedly large files.
const maxScanLines = 10_000

// Ref is a parsed CHATHISTORY reference — either a msgid or a timestamp — as
// defined by the IRCv3 draft/chathistory spec.
type Ref struct {
	// MsgID is set when the ref has the form "msgid=<id>".
	MsgID string
	// Time is set when the ref has the form "timestamp=<RFC3339>".
	Time time.Time
	// IsMsgID and IsTime indicate which variant is set.
	IsMsgID bool
	IsTime  bool
	// IsStar is set for the "*" sentinel (LATEST with no lower bound).
	IsStar bool
}

// ParseRef parses a CHATHISTORY reference string. Accepted forms:
//
//	"*"                       → IsStar
//	"msgid=<any non-empty>"   → IsMsgID, MsgID set
//	"timestamp=<RFC3339>"     → IsTime, Time set (UTC)
//
// Any other form returns an error; the caller MUST send FAIL INVALID_PARAMS to
// the client rather than panicking or sending garbage.
func ParseRef(s string) (Ref, error) {
	if s == "*" {
		return Ref{IsStar: true}, nil
	}
	if id, ok := strings.CutPrefix(s, "msgid="); ok {
		if id == "" {
			return Ref{}, fmt.Errorf("backlog: ParseRef: empty msgid value in %q", s)
		}
		return Ref{IsMsgID: true, MsgID: id}, nil
	}
	if ts, ok := strings.CutPrefix(s, "timestamp="); ok {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			// Accept sub-second variants too.
			t, err = time.Parse(time.RFC3339Nano, ts)
		}
		if err != nil {
			return Ref{}, fmt.Errorf("backlog: ParseRef: invalid timestamp %q: %w", ts, err)
		}
		return Ref{IsTime: true, Time: t.UTC()}, nil
	}
	return Ref{}, fmt.Errorf("backlog: ParseRef: unrecognised ref %q (want msgid=… or timestamp=…)", s)
}

// TargetInfo is one result row for CHATHISTORY TARGETS: a target name and the
// timestamp of its most recent stored message.
type TargetInfo struct {
	// Target is the normalised routing target (as stored in Entry.Target).
	Target string
	// Latest is the Time of the most recent entry in that target's JSONL.
	Latest time.Time
}

// Before returns up to limit entries strictly OLDER than the entry identified
// by ref, in chronological order (oldest first). If the ref resolves to the
// first entry in the target, the result is empty. If ref does not resolve, the
// result is empty.
//
// The cutoff is exclusive: the referenced entry itself is NOT included.
func (s *Store) Before(netid int, target string, ref Ref, limit int) []Entry {
	if limit <= 0 {
		return nil
	}
	entries := s.readJSONL(netid, target)
	if len(entries) == 0 {
		return nil
	}
	// Find the index of the pivot entry.
	pivot := findPivot(entries, ref)
	if pivot < 0 {
		return nil // ref not found
	}
	// Return up to limit entries strictly before the pivot.
	end := pivot // exclusive
	if end == 0 {
		return nil
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := make([]Entry, end-start)
	copy(out, entries[start:end])
	return out
}

// After returns up to limit entries strictly NEWER than the entry identified
// by ref, in chronological order (oldest first). If ref does not resolve, the
// result is empty.
//
// The cutoff is exclusive: the referenced entry itself is NOT included.
func (s *Store) After(netid int, target string, ref Ref, limit int) []Entry {
	if limit <= 0 {
		return nil
	}
	entries := s.readJSONL(netid, target)
	if len(entries) == 0 {
		return nil
	}
	pivot := findPivot(entries, ref)
	if pivot < 0 {
		return nil
	}
	// Entries strictly after pivot.
	start := pivot + 1
	if start >= len(entries) {
		return nil
	}
	end := start + limit
	if end > len(entries) {
		end = len(entries)
	}
	out := make([]Entry, end-start)
	copy(out, entries[start:end])
	return out
}

// Around returns up to limit entries centered on the entry identified by ref:
// floor(limit/2) entries before and ceil(limit/2) entries after (inclusive of
// the pivot itself). If the ref does not resolve, the result is empty.
//
// The total count is at most limit. If there are fewer entries before the
// pivot, the unused slots are filled from entries after.
func (s *Store) Around(netid int, target string, ref Ref, limit int) []Entry {
	if limit <= 0 {
		return nil
	}
	entries := s.readJSONL(netid, target)
	if len(entries) == 0 {
		return nil
	}
	pivot := findPivot(entries, ref)
	if pivot < 0 {
		return nil
	}

	half := limit / 2
	// before: up to half entries strictly before the pivot.
	beforeStart := pivot - half
	if beforeStart < 0 {
		beforeStart = 0
	}
	// after: pivot + half+1 (to include pivot itself and half after), but total ≤ limit.
	afterEnd := beforeStart + limit
	if afterEnd > len(entries) {
		afterEnd = len(entries)
	}
	// If afterEnd reached the end before consuming the full limit, shift beforeStart back.
	if afterEnd-beforeStart < limit && beforeStart > 0 {
		beforeStart = afterEnd - limit
		if beforeStart < 0 {
			beforeStart = 0
		}
	}
	out := make([]Entry, afterEnd-beforeStart)
	copy(out, entries[beforeStart:afterEnd])
	return out
}

// Between returns up to limit entries in the range [fromRef, toRef).
// The fromRef entry IS included (inclusive start); the toRef entry is NOT
// included (exclusive end). Entries are returned in chronological order.
//
// If fromRef does not resolve, the result is empty. If toRef does not resolve,
// all entries from fromRef to the end of the log (up to limit) are returned.
//
// The spec leaves the inclusive/exclusive boundary choice to the server; we
// choose inclusive-start/exclusive-end so clients can chain requests using
// the last-received msgid as the next fromRef without duplication.
func (s *Store) Between(netid int, target string, fromRef, toRef Ref, limit int) []Entry {
	if limit <= 0 {
		return nil
	}
	entries := s.readJSONL(netid, target)
	if len(entries) == 0 {
		return nil
	}
	from := findPivot(entries, fromRef)
	if from < 0 {
		return nil
	}
	// Determine end: exclusive index of toRef, or end of slice.
	to := findPivot(entries, toRef)
	var end int
	if to < 0 {
		end = len(entries) // toRef not found → include everything from fromRef
	} else {
		end = to // exclusive: do not include toRef itself
	}
	if from >= end {
		return nil
	}
	count := end - from
	if count > limit {
		count = limit
	}
	out := make([]Entry, count)
	copy(out, entries[from:from+count])
	return out
}

// Targets returns up to limit targets for netid whose most recent message falls
// within [fromTime, toTime] (both inclusive), ordered by latest-message time
// descending (most recently active first). This serves CHATHISTORY TARGETS.
//
// fromTime and toTime are the wall-clock times from the timestamp= refs in the
// CHATHISTORY TARGETS command. A zero fromTime is treated as the beginning of
// time; a zero toTime is treated as now.
func (s *Store) Targets(netid int, fromTime, toTime time.Time, limit int) []TargetInfo {
	if limit <= 0 {
		return nil
	}
	if toTime.IsZero() {
		toTime = time.Now()
	}

	// Enumerate known bufferKeys for this netid.
	s.mu.Lock()
	var keys []bufferKey
	for k := range s.buffers {
		if k.netid == netid {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()

	var results []TargetInfo
	for _, k := range keys {
		// Read the JSONL to find the latest entry time.
		path := jsonlPath(s.dir, k.netid, k.target)
		entries, err := readJSONLFull(path)
		if err != nil || len(entries) == 0 {
			continue
		}
		latest := entries[len(entries)-1].Time
		if latest.IsZero() {
			continue
		}
		// Apply time window filter.
		if !fromTime.IsZero() && latest.Before(fromTime) {
			continue
		}
		if latest.After(toTime) {
			continue
		}
		results = append(results, TargetInfo{Target: k.target, Latest: latest})
	}

	// Sort by latest descending.
	sortTargetInfoByLatestDesc(results)

	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// ─── internal query helpers ──────────────────────────────────────────────────

// readJSONL reads the on-disk JSONL file for (netid, target) and returns all
// valid entries in chronological order. Returns nil if the file does not exist
// or has no valid entries. Scans at most maxScanLines lines.
func (s *Store) readJSONL(netid int, target string) []Entry {
	safe := safeName(target)
	path := jsonlPath(s.dir, netid, safe)
	entries, err := readJSONLFull(path)
	if err != nil {
		// Not found is silently nil; other I/O errors are logged but not fatal.
		if !os.IsNotExist(err) {
			log.Printf("backlog: readJSONL netid=%d target=%q: %v", netid, target, err)
		}
		return nil
	}
	return entries
}

// readJSONLFull reads up to maxScanLines lines from path, returning all valid
// entries. A torn final line is silently skipped. If the file does not exist,
// returns (nil, nil).
func readJSONLFull(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	lineCount := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, maxJSONLLineBytes), maxJSONLLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lineCount++
		if lineCount > maxScanLines {
			log.Printf("backlog: readJSONLFull: %s has >%d lines; truncating scan", path, maxScanLines)
			break
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // torn or corrupt line
		}
		if e.MsgID == "" || e.Command == "" {
			continue // structurally invalid
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return entries, fmt.Errorf("scan: %w", err)
	}
	return entries, nil
}

// jsonlPath returns the filesystem path for a (netid, safeTarget) JSONL file.
func jsonlPath(dir string, netid int, safeTarget string) string {
	return filepath.Join(dir, strconv.Itoa(netid), safeTarget+".jsonl")
}

// findPivot returns the index in entries of the first entry that matches ref.
// Returns -1 if the ref does not resolve (not found or IsStar).
//
// For IsMsgID: exact string match on Entry.MsgID.
// For IsTime:  first entry whose Time is >= ref.Time (approximate match).
// For IsStar:  always -1 (caller must handle the star case before calling this).
func findPivot(entries []Entry, ref Ref) int {
	if ref.IsStar || len(entries) == 0 {
		return -1
	}
	if ref.IsMsgID {
		for i, e := range entries {
			if e.MsgID == ref.MsgID {
				return i
			}
		}
		return -1
	}
	if ref.IsTime {
		// For timestamp refs: find the first entry whose time is at or after
		// ref.Time. This is the natural boundary for BEFORE/AFTER queries.
		for i, e := range entries {
			if !e.Time.Before(ref.Time) {
				return i
			}
		}
		return -1
	}
	return -1
}

// sortTargetInfoByLatestDesc sorts in-place by Latest time descending.
// Small n (≤500); insertion sort is fine.
func sortTargetInfoByLatestDesc(items []TargetInfo) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Latest.After(items[j-1].Latest); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
