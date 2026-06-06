package server

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

// TestSelfCertFirstRun verifies that EnsureSelfSignedCert generates a cert and
// key on first run, that the key file has 0600 permissions, and that the
// generated pair loads successfully via tls.LoadX509KeyPair.
func TestSelfCertFirstRun(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	cfg := &Config{Listen: Listen{Addr: ":6697"}}

	tlsCfg, err := EnsureSelfSignedCert(dir, cfg, "")
	if err != nil {
		t.Fatalf("EnsureSelfSignedCert first run: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("EnsureSelfSignedCert returned nil *tls.Config")
	}

	// Both files must exist.
	if !fileExists(certPath) {
		t.Errorf("cert file not created at %s", certPath)
	}
	if !fileExists(keyPath) {
		t.Errorf("key file not created at %s", keyPath)
	}

	// Key file must be 0600.
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file permissions = %04o, want 0600", perm)
	}

	// The generated pair must load via tls.LoadX509KeyPair.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("tls.LoadX509KeyPair on generated files: %v", err)
	}

	// Config must have been updated with the paths.
	if cfg.Listen.TLSCert != certPath {
		t.Errorf("cfg.Listen.TLSCert = %q, want %q", cfg.Listen.TLSCert, certPath)
	}
	if cfg.Listen.TLSKey != keyPath {
		t.Errorf("cfg.Listen.TLSKey = %q, want %q", cfg.Listen.TLSKey, keyPath)
	}
}

// TestSelfCertSecondRunReuses verifies that a second call to EnsureSelfSignedCert
// reuses the already-generated files instead of regenerating them.
func TestSelfCertSecondRunReuses(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	cfg := &Config{Listen: Listen{Addr: ":6697"}}

	// First run: generates the pair.
	if _, err := EnsureSelfSignedCert(dir, cfg, ""); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Record mtimes after first run.
	certInfo1, _ := os.Stat(certPath)
	keyInfo1, _ := os.Stat(keyPath)

	// Second run: must reuse (no write).
	cfg2 := &Config{Listen: Listen{Addr: ":6697"}}
	if _, err := EnsureSelfSignedCert(dir, cfg2, ""); err != nil {
		t.Fatalf("second run: %v", err)
	}

	certInfo2, _ := os.Stat(certPath)
	keyInfo2, _ := os.Stat(keyPath)

	// Files must not have been rewritten (mtime unchanged).
	if !certInfo2.ModTime().Equal(certInfo1.ModTime()) {
		t.Error("cert file was rewritten on second run — expected reuse")
	}
	if !keyInfo2.ModTime().Equal(keyInfo1.ModTime()) {
		t.Error("key file was rewritten on second run — expected reuse")
	}
}

// TestSelfCertLoadsViaKeyPair verifies the generated cert/key round-trip
// explicitly through tls.LoadX509KeyPair, matching the doc contract.
func TestSelfCertLoadsViaKeyPair(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{}

	if _, err := EnsureSelfSignedCert(dir, cfg, ""); err != nil {
		t.Fatalf("EnsureSelfSignedCert: %v", err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.Listen.TLSCert, cfg.Listen.TLSKey)
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("loaded certificate has no DER blocks")
	}
}
