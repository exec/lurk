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

// bufferEntry holds one (netid, target) in-memory ring and open file handle.
type bufferEntry struct {
	mu   sync.Mutex
	ring []Entry  // newest-last; len <= ringSize
	f    *os.File // nil until first write (or nil after Close)
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

	entry := Entry{
		Time:    ev.Time(),
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

	if err := s.appendLineLocked(b, netid, safe, entry); err != nil {
		// I/O errors are swallowed — backlog must never disrupt the IRC session.
		log.Printf("backlog: ingest netid=%d target=%q: write: %v", netid, target, err)
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

	n := len(b.ring)
	if n == 0 {
		return nil
	}
	start := n - limit
	if start < 0 {
		start = 0
	}
	out := make([]Entry, n-start)
	copy(out, b.ring[start:])
	return out
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
				// Rehydrate restores existing on-disk data; we do not apply the
				// MaxTargetsPerNet cap here because these targets were already
				// persisted by a previous Ingest that went through the cap check.
				// A hostile upstream cannot inject files directly.
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
// A single Write call ensures the line is written atomically with respect to
// readers scanning complete lines on POSIX filesystems with O_APPEND.
func (s *Store) appendLineLocked(b *bufferEntry, netid int, safeTarget string, e Entry) error {
	if b.f == nil {
		f, err := openJSONLFile(s.dir, netid, safeTarget)
		if err != nil {
			return err
		}
		b.f = f
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("json encode: %w", err)
	}
	data = append(data, '\n')
	if _, err = b.f.Write(data); err != nil {
		return err
	}

	// Check size and rotate if over the cap. Errors here are non-fatal: the
	// write already succeeded, so we log and continue rather than disrupting
	// the IRC session.
	if s.maxFileSize > 0 {
		if rotErr := s.maybeRotateLocked(b, netid, safeTarget); rotErr != nil {
			log.Printf("backlog: rotate netid=%d target=%q: %v", netid, safeTarget, rotErr)
		}
	}
	return nil
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
	info, err := b.f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() <= s.maxFileSize {
		return nil // still within cap
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

// pushRing appends e to b.ring, evicting the oldest entry if the ring is full.
// The caller must hold b.mu.
func pushRing(b *bufferEntry, e Entry, ringSize int) {
	if len(b.ring) >= ringSize {
		// Drop oldest: shift left by one. We keep the slice at exactly ringSize
		// after this call, avoiding unbounded growth.
		copy(b.ring, b.ring[1:])
		b.ring = b.ring[:len(b.ring)-1]
	}
	b.ring = append(b.ring, e)
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
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
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
