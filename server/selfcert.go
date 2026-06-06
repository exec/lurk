// selfcert.go — self-signed TLS certificate generation for lurkd first-run.
//
// When no TLS cert/key is configured, lurkd generates a self-signed ECDSA
// (P-256) certificate and writes it to the config directory so it can be
// reused on subsequent starts. This means lurkd serves TLS out of the box
// and the AUTHENTICATE-requires-TLS gate is satisfied without manual setup.
//
// # Security notes
//
//   - The private key is written at 0600. The public cert is written at 0644
//     (it is not secret). Neither file's contents are logged.
//   - The cert is self-signed with a 10-year validity starting from the
//     generation time; it is not suitable for public-CA trust but is fine for
//     single-user bouncer use.
//   - We use ECDSA P-256. It produces a compact key file and is well supported
//     by all Go TLS stacks (including the lurk client).
//
// # Idempotency
//
// EnsureSelfSignedCert is safe to call on every startup: if both files already
// exist it loads and returns the existing TLS config without regenerating.
// If either file is missing or unreadable, a fresh pair is generated.
package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// certFileName and keyFileName are the filenames written under the config dir.
const certFileName = "lurkd-cert.pem"
const keyFileName = "lurkd-key.pem"

// certValidity is the lifetime of the generated self-signed certificate.
const certValidity = 10 * 365 * 24 * time.Hour

// EnsureSelfSignedCert checks whether cert/key files already exist under dir.
// If they exist and load successfully, it returns the TLS config and the paths.
// If either is missing (first run), it generates a fresh ECDSA P-256 pair,
// writes them to dir at appropriate permissions, updates cfg.Listen with the
// paths, persists cfg to cfgPath via Save, and returns the TLS config.
//
// The dir parameter is injectable for tests (use t.TempDir()). In production
// it is the directory that contains the config file.
func EnsureSelfSignedCert(dir string, cfg *Config, cfgPath string) (*tls.Config, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	// Try to load existing files first (idempotent on repeated starts).
	if fileExists(certPath) && fileExists(keyPath) {
		tlsCfg, err := loadTLSConfig(certPath, keyPath)
		if err != nil {
			// Files exist but are corrupt — regenerate.
			log.Printf("server: existing cert/key at %s / %s could not be loaded (%v) — regenerating", certPath, keyPath, err)
		} else {
			return tlsCfg, nil
		}
	}

	// Generate a fresh self-signed certificate.
	log.Printf("server: generating self-signed TLS certificate in %s", dir)
	tlsCfg, err := generateSelfSignedCert(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server: generate self-signed cert: %w", err)
	}
	log.Printf("server: self-signed certificate written to %s (cert) and %s (key)", certPath, keyPath)

	// Record the paths in the config and persist so the next start reuses them.
	cfg.Listen.TLSCert = certPath
	cfg.Listen.TLSKey = keyPath
	if cfgPath != "" {
		if err := Save(cfg, cfgPath); err != nil {
			// Non-fatal: the cert is already on disk and in use; failure to
			// persist just means the next start regenerates (harmless).
			log.Printf("server: warning: could not save cert paths to config %s: %v", cfgPath, err)
		}
	}

	return tlsCfg, nil
}

// generateSelfSignedCert creates a new ECDSA P-256 key pair and a self-signed
// X.509 certificate, writes them to certPath and keyPath, and returns the
// loaded *tls.Config. keyPath is written at 0600; certPath at 0644.
// Neither the key bytes nor any derived secret are logged.
func generateSelfSignedCert(certPath, keyPath string) (*tls.Config, error) {
	// Generate the private key. We never log or print priv.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ECDSA key: %w", err)
	}

	// Build a self-signed certificate template.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "lurkd",
			Organization: []string{"lurkd self-signed"},
		},
		NotBefore:             now.Add(-time.Minute), // small back-date for clock skew
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	// Encode the key as PKCS#8 PEM (Go's preferred format for ECDSA).
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal ECDSA key: %w", err)
	}

	// Ensure parent directory exists.
	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create dir %s: %w", dir, err)
	}

	// Write the private key at 0600 — must happen before writing the cert so
	// that a crash between the two leaves the cert missing (regeneratable) rather
	// than the key missing (would expose a cert without a key).
	if err := writePEM(keyPath, "EC PRIVATE KEY", privDER, 0o600); err != nil {
		return nil, err
	}

	// Write the certificate at 0644.
	if err := writePEM(certPath, "CERTIFICATE", certDER, 0o644); err != nil {
		_ = os.Remove(keyPath) // best-effort rollback
		return nil, err
	}

	return loadTLSConfig(certPath, keyPath)
}

// writePEM writes data as a PEM block with the given type to path at the
// specified file permissions (mode). The write is atomic (temp file + rename)
// within the same directory. The file content (the private key or cert bytes)
// is never logged.
func writePEM(path, pemType string, der []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pem-*.tmp")
	if err != nil {
		return fmt.Errorf("selfcert: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	// Set permissions before writing any secret bytes.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("selfcert: chmod %s: %w", path, err)
	}

	if err := pem.Encode(tmp, &pem.Block{Type: pemType, Bytes: der}); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("selfcert: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("selfcert: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("selfcert: rename → %s: %w", path, err)
	}
	return nil
}

// fileExists reports whether path names an existing, accessible file.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
