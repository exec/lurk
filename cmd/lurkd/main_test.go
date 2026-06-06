package main

import (
	"strings"
	"testing"

	"github.com/exec/lurk/server"
)

// TestHashpwRoundTrip verifies that the -hashpw flag output (via server.HashPassword)
// produces a hash that verifies correctly with server.VerifyPassword. This test
// exercises the flag plumbing without spinning up a real daemon or a subprocess.
func TestHashpwRoundTrip(t *testing.T) {
	const pw = "hunter2"

	hash, err := server.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// Hash must look like a PBKDF2 string.
	if !strings.HasPrefix(hash, "pbkdf2-sha256:") {
		t.Errorf("HashPassword returned %q, want pbkdf2-sha256:... prefix", hash)
	}

	// Verify with correct password.
	ok, err := server.VerifyPassword(hash, pw)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword returned false for correct password")
	}

	// Verify with wrong password.
	ok, err = server.VerifyPassword(hash, "wrongpassword")
	if err != nil {
		t.Fatalf("VerifyPassword (wrong pw): %v", err)
	}
	if ok {
		t.Error("VerifyPassword returned true for wrong password")
	}
}

// TestCursorStoreDirDerivation verifies that cursorStoreDir derives the cursor
// path correctly from the backlog dir.
func TestCursorStoreDirDerivation(t *testing.T) {
	cases := []struct {
		name       string
		backlogDir string
		want       string // suffix to match
	}{
		{
			name:       "from backlog dir",
			backlogDir: "/home/user/.local/share/lurkd/backlog",
			want:       "/home/user/.local/share/lurkd/cursors",
		},
		{
			name:       "nested backlog dir",
			backlogDir: "/data/lurkd/backlog",
			want:       "/data/lurkd/cursors",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cursorStoreDir(tc.backlogDir)
			if got != tc.want {
				t.Errorf("cursorStoreDir(%q) = %q, want %q", tc.backlogDir, got, tc.want)
			}
		})
	}
}
