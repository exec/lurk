// Package chatlog appends per-target chat logs to plain-text files. It is a
// small, stdlib-only helper the front-ends use to persist conversations: one
// file per channel/PM/buffer, each line prefixed with the date so a day-spanning
// log stays unambiguous. Files are opened lazily and reused.
package chatlog

import (
	"fmt"
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
}

// New returns a Logger writing under dir (created on first write). A nil Logger
// is a valid no-op, so callers can hold a *Logger that is nil when logging is
// disabled.
func New(dir string) *Logger {
	return &Logger{dir: dir, files: make(map[string]*os.File), now: time.Now}
}

// Log appends one line to target's log file, prefixed with the current date. A
// nil Logger does nothing. Errors (e.g. a full disk) are swallowed: logging must
// never disrupt the chat session.
func (l *Logger) Log(target, line string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.fileLocked(target)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %s\n", l.now().Format("2006-01-02"), line)
}

// fileLocked returns the (cached) file for target, opening it if needed. The
// caller holds l.mu.
func (l *Logger) fileLocked(target string) (*os.File, error) {
	key := safeName(target)
	if f, ok := l.files[key]; ok {
		return f, nil
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, key+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
	if out == "" {
		return "server"
	}
	return out
}
