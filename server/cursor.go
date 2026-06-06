// cursor.go implements per-client read cursors for the lurkd bouncer.
//
// # Overview
//
// A cursor is a (clientID, netid, target) → (msgid, time) triple that records
// where a particular client device last read in a given channel or PM thread.
// clientID is the @client component of the SASL authcid (e.g. "laptop", "phone"),
// or the string "default" when the session has no @client component.
//
// Cursors are advanced in fanout (from upstream goroutines) as PRIVMSG/NOTICE
// messages are delivered to bound sessions, and flushed to disk:
//   - on clean detach (unregisterBoundSession), and
//   - periodically (default 30s) by a background goroutine.
//
// # Persistence format
//
// Cursors are stored as a single JSON file under the data dir:
//
//	<dir>/cursors.json
//
// at 0600, written atomically (write to a temp file, then rename). The schema
// is a JSON object mapping "<clientID>/<netid>/<safeTarget>" → CursorEntry.
// Writing the whole file on each flush is correct for the small expected size
// (one bouncer user, a handful of clients, a bounded set of channels).
//
// # Concurrency
//
// cursors.mu guards the entire in-memory map. It is acquired briefly from fanout
// goroutines (one per upstream network, concurrent) when advancing cursors and
// from the periodic-flush goroutine and detach path when flushing. No lock is
// held during disk I/O (the flush path snapshots under the lock, then writes
// outside).
//
// # Goroutine lifecycle
//
// The background flush goroutine is started by NewCursorStore and stopped by
// CursorStore.Close. Close is idempotent: calling it multiple times is safe.
// The test injection point is the flushInterval parameter to NewCursorStore
// (choose a short interval like 50ms in tests to exercise the periodic path).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultFlushInterval is the production periodic-flush interval (30 seconds).
const defaultFlushInterval = 30 * time.Second

// defaultClientID is used for sessions that have no @client component in their
// SASL authcid (e.g. bare "user" or "user/network" without a "@client" suffix).
const defaultClientID = "default"

// CursorKey uniquely identifies one cursor: which device (clientID), which
// upstream network (netid), and which conversation target.
type CursorKey struct {
	ClientID string // the @client component of the authcid, or "default"
	NetID    int
	Target   string // channel or PM nick (not safeName-ed; as seen in fanout)
}

// CursorEntry holds the read position for one (clientID, netid, target) triple.
type CursorEntry struct {
	MsgID string    `json:"msgid"`
	Time  time.Time `json:"time"`
}

// cursorFileEntry is the on-disk JSON representation of one cursor entry.
// The key in the JSON object is "<clientID>/<netid>/<target>".
type cursorFileEntry struct {
	MsgID string    `json:"msgid"`
	Time  time.Time `json:"time"`
}

// CursorStore holds per-client read cursors and persists them to disk.
//
// Build with NewCursorStore; call Close when shutting down.
// The zero value is not usable; always use NewCursorStore.
type CursorStore struct {
	dir string

	mu      sync.Mutex
	entries map[CursorKey]CursorEntry

	closeOnce sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewCursorStore creates a CursorStore rooted at dir. It loads any previously
// persisted cursors from dir/cursors.json and starts the background flush
// goroutine. flushInterval is the periodic-flush interval; pass 0 to use the
// default (30s). Pass a short interval in tests to exercise the periodic path.
func NewCursorStore(dir string, flushInterval time.Duration) (*CursorStore, error) {
	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}

	cs := &CursorStore{
		dir:     dir,
		entries: make(map[CursorKey]CursorEntry),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	// Load persisted cursors (ignore missing file — normal on first start).
	if err := cs.load(); err != nil {
		return nil, err
	}

	// Start the background periodic-flush goroutine.
	go cs.flushLoop(flushInterval)

	return cs, nil
}

// Advance updates the cursor for (clientID, netid, target) to msgid/time.
// It is called from the fanout goroutine as a PRIVMSG/NOTICE is delivered to
// a bound session. The clientID comes from session.saslParsed.Client or
// defaultClientID when absent. Safe to call concurrently.
func (cs *CursorStore) Advance(key CursorKey, msgid string, t time.Time) {
	cs.mu.Lock()
	cs.entries[key] = CursorEntry{MsgID: msgid, Time: t}
	cs.mu.Unlock()
}

// Get returns the cursor entry for the given key, and whether it exists.
func (cs *CursorStore) Get(key CursorKey) (CursorEntry, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	e, ok := cs.entries[key]
	return e, ok
}

// Flush persists all cursors to disk atomically. It is called on clean detach
// and by the periodic flush goroutine. Flush is safe to call concurrently.
func (cs *CursorStore) Flush() error {
	// Snapshot under the lock, then write outside the lock.
	cs.mu.Lock()
	snapshot := make(map[string]cursorFileEntry, len(cs.entries))
	for k, v := range cs.entries {
		// Key format: "<clientID>/<netid>/<target>".
		// clientID may not contain '/' (the SASL @client component should not,
		// but we sanitize defensively by replacing '/' with '_' so the key
		// is always round-trip parseable by parseCursorFileKey).
		safeClient := strings.ReplaceAll(k.ClientID, "/", "_")
		fileKey := fmt.Sprintf("%s/%d/%s", safeClient, k.NetID, k.Target)
		snapshot[fileKey] = cursorFileEntry{MsgID: v.MsgID, Time: v.Time}
	}
	cs.mu.Unlock()

	return cs.writeFile(snapshot)
}

// Close stops the background flush goroutine and performs a final flush.
// Safe to call multiple times and from concurrent goroutines (idempotent).
func (cs *CursorStore) Close() {
	cs.closeOnce.Do(func() {
		// Signal the flush goroutine to stop; the flushLoop will do a final
		// flush before closing doneCh.
		close(cs.stopCh)
		// Wait for the background goroutine to complete and do its final flush.
		<-cs.doneCh
	})
}

// ─── internal ────────────────────────────────────────────────────────────────

// flushLoop runs until stopCh is closed, flushing on each tick. On exit it
// performs one final flush (for clean detach write-back) and closes doneCh.
func (cs *CursorStore) flushLoop(interval time.Duration) {
	defer close(cs.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := cs.Flush(); err != nil {
				log.Printf("server: cursor flush: %v", err)
			}
		case <-cs.stopCh:
			// Final flush on shutdown.
			if err := cs.Flush(); err != nil {
				log.Printf("server: cursor final flush: %v", err)
			}
			return
		}
	}
}

// cursorFilePath returns the path to the cursors JSON file.
func (cs *CursorStore) cursorFilePath() string {
	return filepath.Join(cs.dir, "cursors.json")
}

// load reads the cursors JSON file and populates cs.entries.
// A missing file is not an error (normal on first start).
func (cs *CursorStore) load() error {
	path := cs.cursorFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no cursors yet
		}
		return fmt.Errorf("server: cursor load %s: %w", path, err)
	}

	var raw map[string]cursorFileEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		// Corrupt cursor file: log and start fresh (do not block startup).
		log.Printf("server: cursor load %s: unmarshal failed (starting fresh): %v", path, err)
		return nil
	}

	// Parse each key back into a CursorKey.
	for fileKey, entry := range raw {
		key, ok := parseCursorFileKey(fileKey)
		if !ok {
			log.Printf("server: cursor load: skipping malformed key %q", fileKey)
			continue
		}
		cs.entries[key] = CursorEntry{MsgID: entry.MsgID, Time: entry.Time}
	}
	return nil
}

// writeFile atomically writes snapshot to the cursors JSON file.
// Atomic write: write to a uniquely-named temp file alongside the target, then
// rename. Using a unique temp name avoids partial-write collisions when two
// concurrent Flush calls race (detach + periodic tick). os.CreateTemp creates
// the file at 0600 on POSIX; the rename is atomic on POSIX (last rename wins —
// both snapshots are current-enough so this is safe).
func (cs *CursorStore) writeFile(snapshot map[string]cursorFileEntry) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("server: cursor marshal: %w", err)
	}

	if err := os.MkdirAll(cs.dir, 0o700); err != nil {
		return fmt.Errorf("server: cursor mkdir %s: %w", cs.dir, err)
	}

	tmp, err := os.CreateTemp(cs.dir, "cursors.*.tmp")
	if err != nil {
		return fmt.Errorf("server: cursor create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("server: cursor write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("server: cursor close temp: %w", err)
	}

	path := cs.cursorFilePath()
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup
		return fmt.Errorf("server: cursor rename → %s: %w", path, err)
	}
	return nil
}

// parseCursorFileKey parses a "<clientID>/<netid>/<target>" file key back into
// a CursorKey. Returns false when the key is malformed.
func parseCursorFileKey(fileKey string) (CursorKey, bool) {
	// Format: "<clientID>/<netid>/<target>"
	// We split on '/' but only for the first two separators; the target may
	// itself contain '/' (channel names do not, but PMs could in edge cases).
	var clientID string
	var netidStr string
	var target string

	// Find first '/'.
	i := 0
	for i < len(fileKey) && fileKey[i] != '/' {
		i++
	}
	if i >= len(fileKey) {
		return CursorKey{}, false // no first '/'
	}
	clientID = fileKey[:i]
	rest := fileKey[i+1:]

	// Find second '/' (netid separator).
	j := 0
	for j < len(rest) && rest[j] != '/' {
		j++
	}
	if j >= len(rest) {
		return CursorKey{}, false // no second '/'
	}
	netidStr = rest[:j]
	target = rest[j+1:]

	if clientID == "" || netidStr == "" || target == "" {
		return CursorKey{}, false
	}

	netid := 0
	for _, c := range netidStr {
		if c < '0' || c > '9' {
			return CursorKey{}, false
		}
		netid = netid*10 + int(c-'0')
	}

	return CursorKey{ClientID: clientID, NetID: netid, Target: target}, true
}

// clientIDFromSession returns the @client component of the session's authcid,
// or defaultClientID when the session has no @client component.
func clientIDFromSession(sess *session) string {
	if sess.saslParsed != nil && sess.saslParsed.Client != "" {
		return sess.saslParsed.Client
	}
	return defaultClientID
}
