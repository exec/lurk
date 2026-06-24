// Package chatlog appends per-target chat logs to plain-text files. It is a
// small, stdlib-only helper the front-ends use to persist conversations: one
// file per channel/PM/buffer, each line prefixed with the date so a day-spanning
// log stays unambiguous. Files are opened lazily and reused.
package chatlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger appends lines to per-target log files under a directory. It is safe for
// concurrent use. The zero value is not usable; build one with New.
type Logger struct {
	dir string

	mu    sync.Mutex
	files map[string]*os.File
	now   func() time.Time // injectable clock for tests

	// date caching: the date prefix only changes once per day, so the formatted
	// string is cached and reformatted only when the calendar day rolls over,
	// avoiding a time.Format allocation on every logged line.
	cacheY, cacheD int
	cacheM         time.Month
	cacheDate      string
	// buf is a reusable line-assembly buffer (guarded by mu) so Log makes no
	// per-line allocation in steady state.
	buf []byte
}

// New returns a Logger writing under dir (created on first write). A nil Logger
// is a valid no-op, so callers can hold a *Logger that is nil when logging is
// disabled.
func New(dir string) *Logger {
	return &Logger{dir: dir, files: make(map[string]*os.File), now: time.Now}
}

// Log appends one line to the log file for target within scope (the network),
// prefixed with the current date: <dir>/<scope>/<target>.log. A nil Logger does
// nothing. Errors (e.g. a full disk) are swallowed: logging must never disrupt
// the chat session.
func (l *Logger) Log(scope, target, line string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.fileLocked(scope, target)
	if err != nil {
		return
	}

	// Reformat the date prefix only when the day changes.
	t := l.now()
	if y, mo, d := t.Date(); l.cacheDate == "" || y != l.cacheY || mo != l.cacheM || d != l.cacheD {
		l.cacheY, l.cacheM, l.cacheD = y, mo, d
		l.cacheDate = t.Format("2006-01-02")
	}

	l.buf = append(l.buf[:0], l.cacheDate...)
	l.buf = append(l.buf, ' ')
	l.buf = append(l.buf, line...)
	l.buf = append(l.buf, '\n')
	_, _ = f.Write(l.buf)
}

// fileLocked returns the (cached) file for scope/target, opening it (and its
// per-scope subdirectory) if needed. The caller holds l.mu.
func (l *Logger) fileLocked(scope, target string) (*os.File, error) {
	dir := filepath.Join(l.dir, safeName(scope))
	key := filepath.Join(dir, safeName(target)+".log")
	if f, ok := l.files[key]; ok {
		return f, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(key, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.files[key] = f
	return f, nil
}

// Close closes all open log files. A nil Logger does nothing.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		_ = f.Close()
	}
	l.files = map[string]*os.File{}
}

// safeName maps a target (channel/nick/buffer title) to a safe file base name:
// lower-cased, with anything outside a conservative set replaced by '_'. An empty
// or all-stripped name falls back to "server".
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
	// A name made entirely of dots (".", "..", …) survives the character filter but
	// resolves to the log directory itself or its parent once joined to a path.
	// Reject it so a buffer titled "." or ".." can't redirect its log outside the
	// log dir.
	if strings.Trim(out, ".") == "" {
		return "server"
	}
	return out
}
