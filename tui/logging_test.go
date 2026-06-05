package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lurk/chatlog"
)

// TestTUILogsToDisk verifies the model writes buffer lines to the logger when one
// is configured.
func TestTUILogsToDisk(t *testing.T) {
	dir := t.TempDir()
	m := newTestModel()
	m.networks[0].name = "testnet" // a clean scope name for the path assertion
	m.logger = chatlog.New(dir)
	defer m.logger.Close()

	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	m = appendLine(m, m.activeBuffer(), evt(t, ":alice!a@h PRIVMSG #go :hello world"))

	// Logs are filed per network: <dir>/<network>/<target>.log.
	data, err := os.ReadFile(filepath.Join(dir, "testnet", "#go.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "hello world") || !strings.Contains(string(data), "alice") {
		t.Errorf("log missing message content:\n%s", data)
	}
	// The logged line is plain text (no ANSI escapes).
	if strings.ContainsRune(string(data), 0x1b) {
		t.Errorf("log line contains ANSI escapes: %q", data)
	}
}

// TestNoLoggerNoFiles verifies that without a logger, nothing is written.
func TestNoLoggerNoFiles(t *testing.T) {
	m := newTestModel() // logger is nil
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	// Must not panic on a nil logger.
	m = appendLine(m, m.activeBuffer(), evt(t, ":alice!a@h PRIVMSG #go :hi"))
}
