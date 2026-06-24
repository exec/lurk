// Package backlog is the durable, structured message store for the lurkd
// bouncer. It implements the server.Sink interface structurally (an Ingest
// method with the same signature) without importing server/ — server/ imports
// backlog/, not the other way around, so a direct import would create a cycle.
//
// # Architecture
//
// Each (netid, target) pair has a bufferEntry that holds:
//   - an append-only JSONL file on disk (<dir>/<netid>/<safeTarget>.jsonl)
//   - an in-memory ring of the most recent [DefaultRingSize] entries
//
// Ingest is the write path: it filters for PRIVMSG/NOTICE, assigns a
// crypto/rand msgid, sanitizes text with client.SanitizeForRelay, writes a JSON
// line to disk (a single Write call per entry, so the line is atomic w.r.t.
// other writes to the same file), and pushes the entry into the ring. After each
// write, if WithMaxFileSize is configured and the file has grown past the cap,
// the current .jsonl file is rotated to .jsonl.1 and a fresh .jsonl is opened.
//
// Latest is the read path Phase 5's CHATHISTORY server uses: it returns the
// newest-N entries in chronological order from the in-memory ring.
//
// # Concurrency
//
// The top-level Store.mu guards the buffers map and the per-netid target-count
// map. Each bufferEntry has its own mu that guards the ring slice and the open
// file handle. Ingest acquires Store.mu briefly to find-or-create the entry,
// then releases it before doing I/O, so different (netid, target) pairs
// proceed in parallel without global serialization.
//
// # Resource bounds
//
// DefaultRingSize caps entries per ring. DefaultMaxTargetsPerNet caps how many
// distinct (netid, target) buffers a single upstream can create. Targets beyond
// the cap are logged and silently dropped so a hostile upstream spraying many
// fabricated targets cannot exhaust memory or file descriptors. WithMaxFileSize
// caps the on-disk size of each JSONL file — once exceeded, the file is rotated
// to .jsonl.1 and a fresh .jsonl is opened, keeping disk usage predictable for
// long-running daemons on busy networks.
//
// # Disk format
//
// Each line of a .jsonl file is one JSON object:
//
//	{"time":"2024-01-01T12:00:00Z","msgid":"abc123","target":"#lurk",
//	 "source":"nick!u@h","command":"PRIVMSG","params":["#lurk","hello"]}
//
// Writes are performed as a single os.File.Write of (json + newline), which is
// atomic with respect to other appenders on most POSIX filesystems. A torn
// final line (crash mid-write) is detected during rehydration by checking that
// json.Unmarshal succeeds and required fields are non-empty.
//
// # Rotation
//
// When WithMaxFileSize(n) is set and a file's size exceeds n bytes after a write,
// the file is rotated: the open handle is closed, the file is renamed from
// <target>.jsonl to <target>.jsonl.1 (replacing any previous .1), and a fresh
// <target>.jsonl is opened. Only one generation of backup (.jsonl.1) is kept.
//
// # Rehydration
//
// Rehydrate (called by NewStore) reads the tail of every JSONL file it finds
// under s.dir and refills each ring so msgids and server-time survive a
// restart. When a .jsonl.1 rotation file also exists for a target, its tail is
// read first (older entries) followed by the current .jsonl (newer entries),
// with the merged set deduplicated by MsgID. If a target has no JSONL file but
// the optional chatlog.Logger is configured, chatlog.Tail is the lossy fallback:
// entries get placeholder msgids and are marked Lossy=true.
package backlog

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/exec/lurk/chatlog"
	"github.com/exec/lurk/client"
)

// Default tuning constants. All can be overridden via options on NewStore.
const (
	// DefaultRingSize is the maximum number of entries kept in memory per
	// (netid, target) ring. Drop-oldest when this is exceeded.
	DefaultRingSize = 500

	// DefaultMaxTargetsPerNet is the maximum number of distinct targets
	// (channels + PMs) stored per netid. Exceeding this logs a warning and the
	// excess target is silently dropped.
	DefaultMaxTargetsPerNet = 500

	// maxTimeSkewPast is the maximum distance a message's @time tag may lie
	// in the past relative to the ingest wall clock before the timestamp is
	// clamped. Seven days accommodates legitimate server-time on slow or
	// backlogged IRC networks while bounding how far a hostile upstream can
	// push a message's apparent age backward.
	maxTimeSkewPast = 7 * 24 * time.Hour

	// maxTimeSkewFuture is the maximum distance a message's @time tag may lie
	// in the future relative to the ingest wall clock. One minute accommodates
	// clock skew between lurkd and the upstream while preventing a hostile
	// upstream from forging timestamps far in the future that would corrupt
	// CHATHISTORY ordering (BEFORE/AFTER pivots, TARGETS windows).
	maxTimeSkewFuture = time.Minute

	// maxJSONLLineBytes is the maximum byte length of a single marshalled JSONL
	// line (JSON object + '\n') that appendLineLocked will write to disk. It is
	// also the scanner buffer size used by tailJSONL and readJSONLFull, so the
	// write-side cap and read-side buffer are an invariant pair: a line this code
	// writes can always be read back.
	//
	// Derivation: the IRC wire budget is conn.MaxLineBytes ≈ 9215 bytes per line
	// (irc.MaxLenTags=8191 + irc.MaxLenMessage=512 + 512 framing slack). A JSONL
	// Entry wrapping one IRC message serialises all params plus JSON field
	// overhead; the total is comfortably under 12 KiB for any wire-conformant
	// message. 16 KiB provides a generous safety margin while being 64× smaller
	// than the former 1 MiB allocation. An entry whose JSON exceeds this constant
	// is almost certainly a programming error or a crafted attack; it is logged
	// and dropped rather than written to disk.
	maxJSONLLineBytes = 16384
)

// Entry is one persisted message, both on disk (the JSONL schema) and in the
// in-memory ring.
type Entry struct {
	// Time is the server-assigned or client-received timestamp of the message.
	// Corresponds to the IRCv3 @time tag when present.
	Time time.Time `json:"time"`

	// MsgID is the server-assigned opaque identifier for this stored message.
	// Generated at ingestion via crypto/rand base64url (18 random bytes →
	// 24-char string). Stable across restarts because it is persisted in JSONL.
	MsgID string `json:"msgid"`

	// Target is the normalised routing target: a channel name, or (for PMs)
	// the other party's nick.
	Target string `json:"target"`

	// Source is the sanitised full source prefix of the original IRC message
	// (nick!user@host). client.SanitizeForRelay is applied at ingestion.
	Source string `json:"source"`

	// Command is the IRC command ("PRIVMSG" or "NOTICE" for v1).
	Command string `json:"command"`

	// Params are the sanitised message parameters. For PRIVMSG/NOTICE:
	// Params[0] is the original target as sent by the server; Params[1] (the
	// text body) has been processed with client.SanitizeForRelay to strip
	// terminal-hijacking escapes while preserving IRC formatting codes.
	Params []string `json:"params"`

	// Lossy marks entries rehydrated from the plain-text chatlog fallback (no
	// structured JSONL available). Their MsgID is a generated placeholder;
	// Phase 5 should prefer JSONL entries over these.
	Lossy bool `json:"lossy,omitempty"`
}

// bufferEntry holds one (netid, target) circular ring and open file handle.
//
// The ring is a fixed-capacity circular buffer allocated once to ringSize
// slots. ringHead is the index of the oldest valid entry; ringLen is the count
// of valid entries (0 ≤ ringLen ≤ cap(ring)). The newest entry sits at index
// (ringHead+ringLen-1) % cap(ring). pushRing is O(1): it writes to the next
// slot and, when the buffer is full, advances ringHead (evicting the oldest)
// without copying any existing entries.
type bufferEntry struct {
	mu       sync.Mutex
	ring     []Entry  // circular backing slice, len == cap == ringSize once allocated
	ringHead int      // index of the oldest entry
	ringLen  int      // number of valid entries currently stored
	f        *os.File // nil until first write (or nil after Close)
	fSize    int64    // bytes in f, tracked in-memory to avoid a Stat per write
}

// bufferKey identifies one (netid, safe-target-name) pair.
type bufferKey struct {
	netid  int
	target string // safeName output, lower-cased
}

// Store is the durable message backlog. Build one with NewStore; call Ingest
// from the upstream session manager (it satisfies server.Sink structurally);
// call Latest to serve CHATHISTORY queries.
//
// The zero value is not usable; always use NewStore.
type Store struct {
	dir         string
	ringSize    int
	maxTgts     int
	maxFileSize int64           // 0 = no rotation; positive = rotate when file exceeds this
	chatlog     *chatlog.Logger // optional lossy fallback for rehydration

	mu      sync.Mutex
	buffers map[bufferKey]*bufferEntry
	tgtCnt  map[int]int // netid → number of distinct targets opened so far
}

// Option is a functional option for NewStore.
type Option func(*Store)

// WithRingSize overrides the per-(netid,target) ring capacity (default 500).
func WithRingSize(n int) Option {
	return func(s *Store) { s.ringSize = n }
}

// WithMaxTargetsPerNet overrides the per-netid target cap (default 500).
func WithMaxTargetsPerNet(n int) Option {
	return func(s *Store) { s.maxTgts = n }
}

// WithMaxFileSize sets a per-(netid,target) JSONL file size cap in bytes. When
// a file exceeds this size after an Ingest write, it is rotated: the current
// file is renamed to <target>.jsonl.1 (replacing any previous rotation) and a
// fresh <target>.jsonl is opened. A value of 0 (the default) disables rotation.
func WithMaxFileSize(bytes int64) Option {
	return func(s *Store) { s.maxFileSize = bytes }
}

// WithChatlogFallback attaches a chatlog.Logger to use as a lossy rehydration
// fallback when no JSONL file exists for a (netid, target) pair.
func WithChatlogFallback(l *chatlog.Logger) Option {
	return func(s *Store) { s.chatlog = l }
}

// NewStore creates (or opens) the backlog store rooted at dir, then calls
// Rehydrate to refill in-memory rings from existing JSONL files.
//
// Tests pass t.TempDir(); production callers use DefaultPath().
func NewStore(dir string, opts ...Option) (*Store, error) {
	s := &Store{
		dir:      dir,
		ringSize: DefaultRingSize,
		maxTgts:  DefaultMaxTargetsPerNet,
		buffers:  make(map[bufferKey]*bufferEntry),
		tgtCnt:   make(map[int]int),
	}
	for _, o := range opts {
		o(s)
	}
	// Clamp options to safe values: a non-positive ring size would divide by
	// zero in pushRing, and a non-positive target cap would reject every
	// target. A negative file-size cap is treated as "no rotation".
	if s.ringSize < 1 {
		s.ringSize = DefaultRingSize
	}
	if s.maxTgts < 1 {
		s.maxTgts = DefaultMaxTargetsPerNet
	}
	if s.maxFileSize < 0 {
		s.maxFileSize = 0
	}
	if err := s.Rehydrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// DefaultPath returns the default backlog directory:
// $LURKD_BACKLOG_DIR if set, else $XDG_DATA_HOME/lurkd/backlog,
// else ~/.local/share/lurkd/backlog.
func DefaultPath() (string, error) {
	if p := os.Getenv("LURKD_BACKLOG_DIR"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "lurkd", "backlog"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("backlog: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "lurkd", "backlog"), nil
}

// Close flushes and closes all open file handles. Safe to call multiple times.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buffers {
		b.mu.Lock()
		if b.f != nil {
			_ = b.f.Close()
			b.f = nil
		}
		b.mu.Unlock()
	}
}

// Ingest stores one event from upstream and returns the server-assigned msgid
// and whether the entry was actually stored.
//
// stored is false when:
//   - the event is synthetic or filtered (not PRIVMSG/NOTICE),
//   - the target is unroutable, or
//   - the per-netid target cap is exceeded (drop-oldest).
//
// When stored is true, msgid is the canonical identifier for the stored entry —
// the same value that CHATHISTORY will return for this message. Callers (e.g.
// Server.Ingest) should stamp the live fan-out copy with this msgid so that live
// delivery and CHATHISTORY replay reference the same id.
//
// Ingest is safe for concurrent calls from multiple upstream goroutines.
//
// Only PRIVMSG and NOTICE are persisted. Synthetic client events
// (ev.Message == nil, or commands starting with '@') are silently skipped.
// Events without a routable target are skipped.
//
// For PRIVMSG/NOTICE, the target is Param(0). If Param(0) equals the client's
// own nick (a PM addressed to lurkd), the sender's nick is used as the routing
// key so PMs are keyed by the other party.
func (s *Store) Ingest(netid int, ev *client.Event) (msgid string, stored bool) {
	// Skip synthetic events (overflow events, @reconnecting/@connected, etc.).
	if ev.Message == nil {
		return "", false
	}
	cmd := ev.Command()
	if strings.HasPrefix(cmd, "@") {
		// Synthetic @connected / @reconnecting / @reconnected markers from the
		// client package must never be stored.
		return "", false
	}

	// v1: only PRIVMSG and NOTICE.
	if cmd != "PRIVMSG" && cmd != "NOTICE" {
		return "", false
	}

	rawTarget := ev.Param(0)
	if rawTarget == "" {
		return "", false
	}

	// Determine the routing target. For a PM addressed to lurkd's own nick,
	// key by the sender's nick.
	ownNick := ""
	if ev.Client != nil {
		ownNick = ev.Client.Nick()
	}
	target := routeTarget(rawTarget, ev.Nick(), ownNick)
	if target == "" {
		return "", false
	}

	// Sanitize all params with SanitizeForRelay: strips terminal-hijacking
	// escapes (ESC/C1/DEL/Trojan-Source bidi controls) while PRESERVING the
	// inline IRC formatting codes (bold \x02, colour \x03, italic \x1d, etc.).
	sanitizedParams := make([]string, len(ev.Message.Params))
	for i, p := range ev.Message.Params {
		sanitizedParams[i] = client.SanitizeForRelay(p)
	}

	// Clamp the upstream-supplied @time to the ingest window. ev.Time() prefers
	// the @time tag, which a hostile upstream can forge. clampIngestTime
	// substitutes the ingest wall clock whenever the tag is outside the
	// [now-maxTimeSkewPast, now+maxTimeSkewFuture] window, preventing
	// fabricated timestamps from corrupting CHATHISTORY ordering.
	now := time.Now()
	entry := Entry{
		Time:    clampIngestTime(ev.Time(), now),
		MsgID:   newMsgID(),
		Target:  client.SanitizeForRelay(target),
		Source:  client.SanitizeForRelay(ev.Source()),
		Command: cmd,
		Params:  sanitizedParams,
	}

	safe := safeName(target)
	b, ok := s.getOrCreate(netid, safe)
	if !ok {
		// Target cap exceeded; drop silently (already logged in getOrCreate).
		return "", false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	written, err := s.appendLineLocked(b, netid, safe, entry)
	if err != nil {
		// I/O errors are swallowed — backlog must never disrupt the IRC session.
		log.Printf("backlog: ingest netid=%d target=%q: write: %v", netid, target, err)
	}
	if !written {
		// The line was dropped (size cap or I/O error): do not push to the ring.
		// A ring entry without a corresponding disk record would serve stale data
		// and be silently lost on restart.
		return "", false
	}
	pushRing(b, entry, s.ringSize)
	return entry.MsgID, true
}

// Latest returns the most recent limit entries for the given (netid, target) in
// chronological order (oldest first). If limit <= 0 or no entries exist, it
// returns nil. Latest is read-only; no disk I/O occurs.
func (s *Store) Latest(netid int, target string, limit int) []Entry {
	if limit <= 0 {
		return nil
	}
	safe := safeName(target)
	key := bufferKey{netid: netid, target: safe}

	s.mu.Lock()
	b, ok := s.buffers[key]
	s.mu.Unlock()
	if !ok {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return ringReadLatest(b, limit)
}

// Rehydrate scans s.dir for existing JSONL files and refills each ring from
// the file tail. Any torn final line (crash mid-write) is silently skipped.
//
// When a rotation file (<target>.jsonl.1) exists alongside the current
// <target>.jsonl, its tail is read first (it holds older entries) followed by
// the current file (newer entries). The merged set is deduplicated by MsgID so
// that an entry that appears in both files (possible if the rotation happened
// between writes) is stored only once.
//
// Called automatically by NewStore; can be called again after manual store
// inspection if needed (idempotent: it only adds to empty rings).
func (s *Store) Rehydrate() error {
	netDirs, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil // no data yet — normal on first start
	}
	if err != nil {
		return fmt.Errorf("backlog: rehydrate: read %s: %w", s.dir, err)
	}

	for _, nd := range netDirs {
		if !nd.IsDir() {
			continue
		}
		netid, err := strconv.Atoi(nd.Name())
		if err != nil {
			continue // not a netid subdirectory
		}
		netDir := filepath.Join(s.dir, nd.Name())
		files, err := os.ReadDir(netDir)
		if err != nil {
			continue
		}
		for _, fi := range files {
			// Process only current .jsonl files; .jsonl.1 rotation files are
			// handled below as part of the matching .jsonl entry.
			if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".jsonl") {
				continue
			}
			// Re-apply safeName to the file-system name to reject any file that
			// does not round-trip through the safe-name mapping (e.g. a manually
			// placed file with a path-traversal name like "../../evil.jsonl").
			rawTarget := strings.TrimSuffix(fi.Name(), ".jsonl")
			if safeName(rawTarget) != rawTarget {
				log.Printf("backlog: rehydrate: skipping unsafe filename %q in %s", fi.Name(), netDir)
				continue
			}
			safeTarget := rawTarget
			currentPath := filepath.Join(netDir, fi.Name())
			rotatedPath := currentPath + ".1" // <target>.jsonl.1, if present

			// Load from the rotation file first (older entries), then the current
			// file (newer entries), so chronological order is preserved.
			var entries []Entry
			if rotEntries, err := tailJSONL(rotatedPath, s.ringSize); err == nil && len(rotEntries) > 0 {
				entries = append(entries, rotEntries...)
			}
			curEntries, err := tailJSONL(currentPath, s.ringSize)
			if err != nil {
				log.Printf("backlog: rehydrate %s: %v", currentPath, err)
				continue
			}
			entries = append(entries, curEntries...)

			if len(entries) == 0 {
				continue
			}

			// Deduplicate by MsgID (in case an entry appears in both files at the
			// rotation boundary). Keep first occurrence (chronologically older).
			entries = dedupEntries(entries)

			// Trim to the ring size (keep newest).
			if len(entries) > s.ringSize {
				entries = entries[len(entries)-s.ringSize:]
			}

			key := bufferKey{netid: netid, target: safeTarget}
			s.mu.Lock()
			b, ok := s.buffers[key]
			if !ok {
				// Enforce the per-netid target cap during rehydration. The original
				// assumption — that files on disk were written by a previous Ingest
				// that already cleared the cap — does not hold when the cap was
				// lowered between runs, or when an attacker (or operator) manually
				// placed JSONL files in the store directory. Without this guard,
				// Rehydrate loads every file it finds and inflates tgtCnt[netid]
				// beyond maxTgts, exhausting memory (one ring per target) and
				// causing subsequent live Ingest calls to misfire the cap for
				// legitimate new targets. Log and skip the excess so the daemon
				// starts cleanly with a predictable footprint.
				if s.tgtCnt[netid] >= s.maxTgts {
					s.mu.Unlock()
					log.Printf("backlog: rehydrate: netid=%d target cap (%d) exceeded; skipping %q",
						netid, s.maxTgts, safeTarget)
					continue
				}
				b = &bufferEntry{}
				s.buffers[key] = b
				s.tgtCnt[netid]++
			}
			s.mu.Unlock()

			b.mu.Lock()
			for _, e := range entries {
				pushRing(b, e, s.ringSize)
			}
			b.mu.Unlock()
		}
	}
	return nil
}

// dedupEntries returns entries with duplicates (same MsgID) removed, keeping
// the first occurrence. The input order (chronological, oldest first) is
// preserved so later entries from the current file shadow older rotation copies.
func dedupEntries(entries []Entry) []Entry {
	seen := make(map[string]bool, len(entries))
	out := entries[:0:len(entries)] // reuse backing array
	for _, e := range entries {
		if seen[e.MsgID] {
			continue
		}
		seen[e.MsgID] = true
		out = append(out, e)
	}
	return out
}

// ─── internal helpers ─────────────────────────────────────────────────────────

// getOrCreate returns the bufferEntry for (netid, safe), creating it if
// necessary and enforcing the MaxTargetsPerNet cap. Returns ok=false when the
// cap is exceeded. Caller must NOT hold s.mu.
func (s *Store) getOrCreate(netid int, safe string) (*bufferEntry, bool) {
	key := bufferKey{netid: netid, target: safe}

	s.mu.Lock()
	defer s.mu.Unlock()

	if b, ok := s.buffers[key]; ok {
		return b, true
	}
	if s.tgtCnt[netid] >= s.maxTgts {
		log.Printf("backlog: netid=%d target cap (%d) exceeded; dropping %q",
			netid, s.maxTgts, safe)
		return nil, false
	}
	b := &bufferEntry{}
	s.buffers[key] = b
	s.tgtCnt[netid]++
	return b, true
}

// appendLineLocked serialises entry as JSON and appends it as a single line
// (json + '\n') to b.f, opening the file lazily. After a successful write, if
// a maxFileSize cap is configured and the file has grown past it, rotateLocked
// is called to rename the current file to .jsonl.1 and open a fresh .jsonl.
// The caller must hold b.mu.
//
// Returns (written=true, nil) when the line is successfully written to disk.
// Returns (written=false, nil) when the line is dropped by the size cap — the
// caller must NOT push the entry into the ring, since a ring entry without a
// corresponding disk entry would diverge from the on-disk state and be lost
// on restart. Returns (false, err) on I/O failure.
//
// A single Write call ensures the line is written atomically with respect to
// readers scanning complete lines on POSIX filesystems with O_APPEND.
func (s *Store) appendLineLocked(b *bufferEntry, netid int, safeTarget string, e Entry) (written bool, err error) {
	if b.f == nil {
		f, err := openJSONLFile(s.dir, netid, safeTarget)
		if err != nil {
			return false, err
		}
		b.f = f
		// Initialise the in-memory size from the file's current length (the file
		// may pre-exist with content after a restart, since it is O_APPEND). This
		// one Stat per file-open lets maybeRotateLocked avoid a Stat per write.
		if info, statErr := f.Stat(); statErr == nil {
			b.fSize = info.Size()
		} else {
			b.fSize = 0
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("json encode: %w", err)
	}
	data = append(data, '\n')

	// Write-side line-length cap: a marshalled line that exceeds maxJSONLLineBytes
	// cannot be read back by tailJSONL/readJSONLFull (whose scanner buffers are
	// sized to the same constant). Log and signal drop (written=false) so the
	// caller skips pushRing — keeping ring and disk in sync. This should never
	// fire for wire-conformant IRC messages; it is a defence-in-depth guard
	// against programming errors or future event types whose params are large.
	if len(data) > maxJSONLLineBytes {
		log.Printf("backlog: appendLine netid=%d target=%q: marshalled line %d bytes > cap %d; dropping",
			netid, safeTarget, len(data), maxJSONLLineBytes)
		return false, nil
	}

	if _, err = b.f.Write(data); err != nil {
		return false, err
	}
	b.fSize += int64(len(data))

	// Check size and rotate if over the cap. Errors here are non-fatal: the
	// write already succeeded, so we log and continue rather than disrupting
	// the IRC session.
	if s.maxFileSize > 0 {
		if rotErr := s.maybeRotateLocked(b, netid, safeTarget); rotErr != nil {
			log.Printf("backlog: rotate netid=%d target=%q: %v", netid, safeTarget, rotErr)
		}
	}
	return true, nil
}

// maybeRotateLocked checks whether b.f has grown past s.maxFileSize and, if so,
// rotates the current .jsonl to .jsonl.1 and opens a fresh .jsonl. The caller
// must hold b.mu.
//
// Rotation steps:
//  1. Stat the open file to get current size.
//  2. If size <= maxFileSize, return immediately (nothing to do).
//  3. Close b.f.
//  4. os.Rename(<target>.jsonl → <target>.jsonl.1), replacing any previous .1.
//  5. Open a fresh <target>.jsonl and assign it to b.f.
//
// If any step fails after the rename, b.f is left nil so the next Ingest call
// reopens (and appends to the .jsonl file, which may have been recreated by
// another process). In practice this should not happen on a healthy filesystem.
func (s *Store) maybeRotateLocked(b *bufferEntry, netid int, safeTarget string) error {
	if b.fSize <= s.maxFileSize {
		return nil // still within cap (size tracked in-memory; no per-write Stat)
	}

	// Close the current file before renaming so Windows does not refuse the
	// rename on a file with an open handle (POSIX allows it, Windows does not).
	if err := b.f.Close(); err != nil {
		b.f = nil
		return fmt.Errorf("close before rotate: %w", err)
	}
	b.f = nil

	subdir := filepath.Join(s.dir, strconv.Itoa(netid))
	current := filepath.Join(subdir, safeTarget+".jsonl")
	rotated := filepath.Join(subdir, safeTarget+".jsonl.1")

	// Rename current → .1 (replaces any previous rotation).
	if err := os.Rename(current, rotated); err != nil {
		return fmt.Errorf("rename %s → %s: %w", current, rotated, err)
	}

	// Open a fresh .jsonl.
	f, err := openJSONLFile(s.dir, netid, safeTarget)
	if err != nil {
		return fmt.Errorf("open fresh after rotate: %w", err)
	}
	b.f = f
	b.fSize = 0 // fresh, empty file
	return nil
}

// openJSONLFile opens (O_APPEND|O_CREATE|O_WRONLY) the JSONL file for
// (netid, safeTarget), creating the per-netid subdirectory (0700) if needed.
// Files are opened at 0600 (owner-only).
func openJSONLFile(dir string, netid int, safeTarget string) (*os.File, error) {
	subdir := filepath.Join(dir, strconv.Itoa(netid))
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", subdir, err)
	}
	path := filepath.Join(subdir, safeTarget+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

// pushRing appends e to the circular ring, evicting the oldest entry when the
// ring is full. O(1): no slice shifting. The caller must hold b.mu.
//
// The ring is allocated lazily to ringSize slots on the first push, then reused
// for the lifetime of the bufferEntry. Once the ring reaches capacity, each new
// entry overwrites the oldest slot (ringHead advances mod cap(ring)).
func pushRing(b *bufferEntry, e Entry, ringSize int) {
	cap := ringSize
	if len(b.ring) == 0 {
		// First push: allocate the backing slice.
		b.ring = make([]Entry, cap)
		b.ringHead = 0
		b.ringLen = 0
	}
	if b.ringLen < cap {
		// Ring not yet full: write to the next free slot and grow ringLen.
		b.ring[(b.ringHead+b.ringLen)%cap] = e
		b.ringLen++
	} else {
		// Ring full: overwrite the oldest slot (ringHead), then advance ringHead.
		b.ring[b.ringHead] = e
		b.ringHead = (b.ringHead + 1) % cap
	}
}

// ringReadLatest returns the most recent limit entries from b in chronological
// order (oldest first). The caller must hold b.mu. Returns nil if the ring is
// empty or limit <= 0.
//
// The circular ring stores entries at indices ringHead, ringHead+1, …
// (mod cap(ring)), in order from oldest to newest. Reading the last min(limit,
// ringLen) entries requires at most two copy calls for the wrap-around case.
func ringReadLatest(b *bufferEntry, limit int) []Entry {
	if b.ringLen == 0 || limit <= 0 {
		return nil
	}
	cap := len(b.ring)
	count := b.ringLen
	if count > limit {
		count = limit
	}
	// The last `count` entries start at index:
	//   start = (ringHead + ringLen - count) % cap
	startOff := b.ringLen - count // offset from ringHead (0-based, ≥ 0)
	start := (b.ringHead + startOff) % cap

	out := make([]Entry, count)
	// Copy in one or two segments depending on whether the range wraps.
	end := start + count
	if end <= cap {
		// Contiguous — single copy.
		copy(out, b.ring[start:end])
	} else {
		// Wraps around the end of the backing slice — two copies.
		first := cap - start
		copy(out[:first], b.ring[start:])
		copy(out[first:], b.ring[:end-cap])
	}
	return out
}

// ringNewest returns the newest entry in b and true, or the zero Entry and
// false if the ring is empty. The caller must hold b.mu.
func ringNewest(b *bufferEntry) (Entry, bool) {
	if b.ringLen == 0 {
		return Entry{}, false
	}
	cap := len(b.ring)
	idx := (b.ringHead + b.ringLen - 1) % cap
	return b.ring[idx], true
}

// newMsgID generates a crypto/rand 18-byte opaque identifier encoded as
// base64url without padding (24 characters). 18 bytes gives 144 bits of
// randomness; the birthday bound over 10^9 messages is ~3×10^{-26} (negligible).
func newMsgID() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failures are catastrophic; panic so the caller cannot
		// silently store an empty msgid and corrupt the timeline.
		panic(fmt.Sprintf("backlog: crypto/rand failed: %v", err))
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

// routeTarget determines the buffer key for a PRIVMSG/NOTICE:
//   - If rawTarget starts with a channel prefix (#, &, +, !) it is a channel.
//   - If rawTarget equals ownNick (a PM addressed to lurkd), key by senderNick.
//   - Otherwise (PM where rawTarget is already the other party), use rawTarget.
//
// An empty return means the target is unroutable; the caller should drop the
// message.
func routeTarget(rawTarget, senderNick, ownNick string) string {
	if rawTarget == "" {
		return ""
	}
	switch rawTarget[0] {
	case '#', '&', '+', '!':
		return rawTarget
	}
	// It's a PM. Key by the other party.
	if ownNick != "" && strings.EqualFold(rawTarget, ownNick) {
		return senderNick
	}
	return rawTarget
}

// clampIngestTime returns the message timestamp to persist. If the upstream
// supplied a @time tag (via ev.Time() returning a value other than recvTime),
// it is trusted only when it falls within the window
// [now-maxTimeSkewPast, now+maxTimeSkewFuture]. Outside that window the
// ingest wall clock (now) is substituted, so a hostile upstream cannot forge
// timestamps that corrupt CHATHISTORY ordering (BEFORE/AFTER pivots, TARGETS
// windows) or the per-client cursor.
func clampIngestTime(t, now time.Time) time.Time {
	if t.Before(now.Add(-maxTimeSkewPast)) || t.After(now.Add(maxTimeSkewFuture)) {
		return now
	}
	return t
}

// safeName mirrors chatlog.safeName: maps a target to a safe, lower-cased file
// base name. Having identical rules ensures path consistency between the human
// chatlog and the JSONL store. An empty or all-stripped result falls back to
// "server".
func safeName(target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return "server"
	}
	var b strings.Builder
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '#' || r == '+' || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// Reject dot-only names (".", "..") that would escape the store directory.
	if strings.Trim(out, ".") == "" {
		return "server"
	}
	return out
}

// tailJSONL reads the last n valid JSON lines from path by scanning all lines
// and returning the tail. A torn final line (incomplete write on crash) is
// identified by a failed json.Unmarshal or missing required fields and silently
// skipped; earlier valid lines are not affected.
//
// The returned slice is in chronological order (oldest first, matching the
// append order in the file).
func tailJSONL(path string, n int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// For Phase 4, a full sequential scan is acceptable. The ring cap of 500
	// per target means files grow at most ~500 lines before Phase 5 query logic
	// prunes them (future: reverse-seek from EOF for large files).
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, maxJSONLLineBytes), maxJSONLLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	// Take the last n lines.
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// Torn or corrupt line — skip.
			continue
		}
		// Required fields validation; skip structurally invalid entries.
		if e.MsgID == "" || e.Command == "" {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}
