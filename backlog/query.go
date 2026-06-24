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
// Boundary queries (Before/After/Around/Between) prefer the in-memory ring and
// fall back to an on-disk JSONL scan only when the ring no longer holds the
// target's complete history. Specifically, while a target's ring has not
// reached capacity it contains every entry ever stored for that target (entries
// are appended to ring and disk in lock-step and the ring drops its oldest only
// once full), so it is returned directly — byte-for-byte equivalent to the
// merged disk read but with no file I/O or JSON parsing. See entriesForQuery.
// Once the ring is full, older entries may live only on disk and the on-disk
// scan is used. The ring remains the sole fast path for Latest (newest-N) and
// Targets (newest-entry time per target).
//
// JSONL files are bounded by DefaultRingSize entries per target (drop-oldest
// during Ingest), so a full scan is O(ring-cap) lines — small by design. A
// per-call hard cap (maxScanLines, applied per file) guards against
// pathologically large files that could accumulate before the ring cap takes
// full effect on an older store.
//
// When size-based rotation is enabled (WithMaxFileSize), older entries live in
// <target>.jsonl.1 while the current <target>.jsonl may be nearly empty right
// after a rotation. Every disk read therefore merges the rotation file (older
// entries) with the current file (newer entries), deduplicating by MsgID —
// mirroring the merge Rehydrate performs at startup. Without this, a boundary
// query issued just after a rotation (e.g. CHATHISTORY BEFORE with a msgid
// served from the in-memory ring) would silently miss everything in the
// rotated file.
//
// Targets reads the ring's newest entry via ringNewest under b.mu — O(1) per
// target, no disk I/O — and falls back to a JSONL scan only for targets whose
// ring is empty (unusual; would happen if rehydration found no valid entries).
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
	// Latest is the Time of the most recent entry for this target.
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
	entries := s.entriesForQuery(netid, target)
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
	entries := s.entriesForQuery(netid, target)
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
	entries := s.entriesForQuery(netid, target)
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
	entries := s.entriesForQuery(netid, target)
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
//
// The newest-entry time is read from the in-memory ring via ringNewest (O(1)
// per target, no disk I/O). Only when a target's ring is empty (e.g.
// rehydration loaded zero valid entries) does this method fall back to a full
// JSONL scan for that target.
func (s *Store) Targets(netid int, fromTime, toTime time.Time, limit int) []TargetInfo {
	if limit <= 0 {
		return nil
	}
	if toTime.IsZero() {
		toTime = time.Now()
	}

	// Snapshot (key → *bufferEntry) for this netid under s.mu, then release
	// the global lock before doing any per-entry work. This keeps the critical
	// section short even when there are hundreds of targets.
	s.mu.Lock()
	type kbPair struct {
		k bufferKey
		b *bufferEntry
	}
	pairs := make([]kbPair, 0, len(s.buffers))
	for k, b := range s.buffers {
		if k.netid == netid {
			pairs = append(pairs, kbPair{k, b})
		}
	}
	s.mu.Unlock()

	var results []TargetInfo
	for _, p := range pairs {
		// Fast path: read the newest entry's time from the in-memory ring.
		// ringNewest returns the most recent entry in O(1) with no disk I/O.
		var latest time.Time
		p.b.mu.Lock()
		if newest, ok := ringNewest(p.b); ok {
			latest = newest.Time
		}
		p.b.mu.Unlock()

		if latest.IsZero() {
			// Slow path: ring is empty (target was rehydrated but had no valid
			// JSONL entries, or the ring was trimmed to zero). Fall back to a
			// full JSONL scan (rotation file + current file) to find the
			// latest time.
			path := jsonlPath(s.dir, p.k.netid, p.k.target)
			entries, err := readJSONLWithRotation(path)
			if err != nil || len(entries) == 0 {
				continue
			}
			latest = entries[len(entries)-1].Time
			if latest.IsZero() {
				continue
			}
		}

		// Apply time window filter.
		if !fromTime.IsZero() && latest.Before(fromTime) {
			continue
		}
		if latest.After(toTime) {
			continue
		}
		results = append(results, TargetInfo{Target: p.k.target, Latest: latest})
	}

	// Sort by latest descending.
	sortTargetInfoByLatestDesc(results)

	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// ─── internal query helpers ──────────────────────────────────────────────────

// entriesForQuery returns the entry slice a boundary query (Before/After/
// Around/Between) should operate on, preferring the in-memory ring over disk
// whenever the ring is known to hold the target's complete history.
//
// Invariant: a ring that has not reached capacity contains every entry ever
// stored for the target. Entries are only ever appended (to both ring and disk
// in lock-step) and the ring drops its oldest only once full; rehydration loads
// the newest ≤ringSize entries, so a non-full ring after restart likewise means
// disk held no more than that. Therefore, when 0 < ringLen < ringSize, the ring
// is byte-for-byte equivalent to the merged on-disk read — same entries, same
// chronological order, no duplicates — and every pivot/slice computation below
// yields an identical result, with no file open and no JSON parsing.
//
// Once the ring is full (ringLen == ringSize) older entries may live only on
// disk, so the proven on-disk merge path is used unchanged. The result is that
// CHATHISTORY for any target that has not exceeded ringSize messages — most PMs
// and low-traffic channels, and the common case of a client paging recent
// history — is served entirely from memory, while busy channels fall back to
// the same disk scan as before.
func (s *Store) entriesForQuery(netid int, target string) []Entry {
	safe := safeName(target)
	s.mu.Lock()
	b := s.buffers[bufferKey{netid: netid, target: safe}]
	s.mu.Unlock()
	if b != nil {
		b.mu.Lock()
		if b.ringLen > 0 && b.ringLen < s.ringSize {
			entries := ringReadLatest(b, b.ringLen)
			b.mu.Unlock()
			return entries
		}
		b.mu.Unlock()
	}
	return s.readJSONL(netid, target)
}

// readJSONL reads the on-disk JSONL data for (netid, target) — the rotation
// file <target>.jsonl.1 first (older entries), if present, followed by the
// current <target>.jsonl — and returns all valid entries in chronological
// order, deduplicated by MsgID. Returns nil if neither file exists or has no
// valid entries. Scans at most maxScanLines lines per file.
func (s *Store) readJSONL(netid int, target string) []Entry {
	safe := safeName(target)
	path := jsonlPath(s.dir, netid, safe)
	entries, err := readJSONLWithRotation(path)
	if err != nil {
		// Not found is silently nil; other I/O errors are logged but not fatal.
		if !os.IsNotExist(err) {
			log.Printf("backlog: readJSONL netid=%d target=%q: %v", netid, target, err)
		}
		// entries may still hold rotated entries even when the current file
		// failed to read; serve what we have.
	}
	return entries
}

// readJSONLWithRotation reads the rotation file at path+".1" (older entries),
// if any, followed by the current file at path (newer entries), and returns
// the merged set deduplicated by MsgID — mirroring the merge Rehydrate
// performs at startup. This is the single disk-read path for all boundary
// queries, so entries that were rotated out of the current file remain
// queryable.
//
// If the current file cannot be read, any entries recovered from the rotation
// file are still returned alongside the error; os.IsNotExist on the current
// file with a present rotation file (possible in the brief window between the
// rename and the reopen during rotation) is not treated as an error.
func readJSONLWithRotation(path string) ([]Entry, error) {
	var entries []Entry
	if rotEntries, err := readJSONLFull(path + ".1"); err == nil && len(rotEntries) > 0 {
		entries = append(entries, rotEntries...)
	}
	curEntries, err := readJSONLFull(path)
	if err != nil {
		if os.IsNotExist(err) && len(entries) > 0 {
			return dedupEntries(entries), nil
		}
		return dedupEntries(entries), err
	}
	entries = append(entries, curEntries...)
	return dedupEntries(entries), nil
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
