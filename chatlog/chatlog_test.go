package chatlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerWritesPerTarget(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.now = func() time.Time { return time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC) }

	l.Log("libera", "#go", "15:04 alice hi")
	l.Log("libera", "#go", "15:05 bob yo")
	l.Log("libera", "bob", "15:06 <bob> pm")
	l.Close()

	chanLog, err := os.ReadFile(filepath.Join(dir, "libera", "#go.log"))
	if err != nil {
		t.Fatalf("read #go log: %v", err)
	}
	got := string(chanLog)
	if !strings.Contains(got, "2026-06-04 15:04 alice hi") || !strings.Contains(got, "2026-06-04 15:05 bob yo") {
		t.Errorf("#go log missing dated lines:\n%s", got)
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n"); lines != 1 {
		t.Errorf("#go log has %d newlines, want 2 lines (1 separator)", lines)
	}

	if _, err := os.Stat(filepath.Join(dir, "libera", "bob.log")); err != nil {
		t.Errorf("per-target file for bob not created: %v", err)
	}
}

func TestLoggerPerms0600(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Log("net", "#x", "line")
	l.Close()
	fi, err := os.Stat(filepath.Join(dir, "net", "#x.log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("log perms = %#o, want 0600", perm)
	}
}

func TestNilLoggerIsNoop(t *testing.T) {
	var l *Logger
	l.Log("net", "#x", "line") // must not panic
	l.Close()
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"#go":        "#go",
		"#Go-Lang":   "#go-lang",
		"a/b\\c:d":   "a_b_c_d",
		"":           "server",
		"  ":         "server",
		"Bob[m]":     "bob_m_",
		"+localchan": "+localchan",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
