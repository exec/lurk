// Package server implements lurkd's client-facing IRC server: the TLS listener,
// the server-side registration / CAP / SASL responder, and the session
// multiplexer that bridges attached lurk clients to persistent upstream
// connections. It reuses the protocol packages (irc, conn, cap, sasl, isupport,
// client) for both directions and is standard-library only — lurkd is a headless
// daemon and must never import the charm UI libraries that only tui/ and cmd/lurk
// are permitted to use. The TestNoCharmDependency guard enforces that.
//
// This file is the on-disk daemon configuration. It mirrors the config package's
// conventions exactly (indented JSON, 0600 perms, atomic temp-file-then-rename,
// XDG-aware path) but describes a server rather than a client: a TLS listener, the
// bouncer-auth credential, and the upstream networks (each with a lurkd-allocated
// integer netid and server-side-only upstream credentials).
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the on-disk lurkd configuration document. The zero value is a usable
// empty config (no networks, no listener address).
type Config struct {
	Listen      Listen      `json:"listen"`
	BouncerAuth BouncerAuth `json:"bouncer_auth"`
	Networks    []Network   `json:"networks"`
}

// Listen describes the daemon's TLS listener. The listener is TLS-only by
// invariant (no plaintext bind); TLSCert/TLSKey are a PEM path pair.
type Listen struct {
	Addr    string `json:"addr"`
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
}

// BouncerAuth is the credential a client presents to attach to the bouncer
// itself (distinct from any upstream network credential). The password is stored
// only as a self-describing PBKDF2-HMAC-SHA256 digest of the form
// "pbkdf2-sha256:<iter>:<saltB64>:<dkB64>"; see the security phase for the
// hash/verify helpers.
type BouncerAuth struct {
	User         string `json:"user"`
	PasswordHash string `json:"password_hash"`
}

// Identity is the nick/user/realname lurkd registers with on an upstream network.
type Identity struct {
	Nick     string `json:"nick,omitempty"`
	User     string `json:"user,omitempty"`
	Realname string `json:"realname,omitempty"`
}

// SASL holds upstream-network SASL credentials. These live server-side only and
// are never round-tripped to an attached client. An empty Mechanism disables it.
type SASL struct {
	Mechanism string `json:"mechanism,omitempty"` // "PLAIN" or "EXTERNAL"
	Authcid   string `json:"authcid,omitempty"`
	Password  string `json:"password,omitempty"`
}

// Network is one upstream network lurkd maintains a persistent connection to.
// NetID is a stable lurkd-allocated integer (the soju.im/bouncer-networks model):
// config authors may omit it and let the daemon number networks at load.
type Network struct {
	NetID    int      `json:"netid"`
	Name     string   `json:"name"`
	Addr     string   `json:"addr"` // host:port
	TLS      bool     `json:"tls,omitempty"`
	Identity Identity `json:"identity"`
	SASL     SASL     `json:"sasl,omitempty"`
	Channels []string `json:"channels,omitempty"` // autojoin
}

// Path returns the resolved config-file path: $LURKD_CONFIG if set, else
// $XDG_CONFIG_HOME/lurkd/config.json, else ~/.config/lurkd/config.json — the same
// XDG-aware resolution the client config uses, under a "lurkd" subdirectory.
func Path() (string, error) {
	if p := os.Getenv("LURKD_CONFIG"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "lurkd", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("server: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "lurkd", "config.json"), nil
}

// Load reads the config from the resolved Path, returning the document and the
// path it was read from. A missing file yields an empty *Config and no error;
// malformed JSON is an error. Networks are assigned netids (see assignNetIDs).
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	c, err := LoadFrom(path)
	return c, path, err
}

// LoadFrom reads a config file from an explicit path. A missing file yields an
// empty *Config and no error. Every network is guaranteed a positive netid on
// return.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("server: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("server: parse %s: %w", path, err)
	}
	c.assignNetIDs()
	return &c, nil
}

// Save writes c to path as indented JSON with owner-only permissions (0600),
// creating the parent directory (0700) if needed. The write is atomic — temp file
// in the same directory then rename — so a crash mid-write cannot truncate an
// existing config. Upstream and bouncer credentials are stored in cleartext/hash
// at 0600, never sent to an attached client.
func Save(c *Config, path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("server: encode: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("server: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("server: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("server: chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("server: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("server: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("server: rename into place: %w", err)
	}
	return nil
}

// assignNetIDs gives every network a stable positive integer netid: ids already
// set are preserved, and any network with netid 0 is assigned the next unused id.
// This realizes the lurkd-allocated-netid model — config authors may omit netids
// and let the daemon number them deterministically at load.
func (c *Config) assignNetIDs() {
	used := make(map[int]bool, len(c.Networks))
	for _, n := range c.Networks {
		if n.NetID > 0 {
			used[n.NetID] = true
		}
	}
	next := 1
	for i := range c.Networks {
		if c.Networks[i].NetID > 0 {
			continue
		}
		for used[next] {
			next++
		}
		c.Networks[i].NetID = next
		used[next] = true
	}
}

// NextNetID returns the smallest unused positive netid, for BOUNCER ADDNETWORK to
// allocate the next network's id.
func (c *Config) NextNetID() int {
	used := make(map[int]bool, len(c.Networks))
	for _, n := range c.Networks {
		used[n.NetID] = true
	}
	id := 1
	for used[id] {
		id++
	}
	return id
}
