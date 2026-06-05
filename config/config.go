// Package config persists Lurk's launcher configuration — the saved networks
// (each bundling its own identity and optional SASL credentials) and a set of
// default identity fields — to a JSON file in the user's config directory.
//
// It is stdlib-only and holds no dependency on the client or TUI packages, so it
// stays a pure, trivially-testable data layer. The cmd/lurk binary converts a
// chosen Network into a client.Config at connect time.
//
// Passwords are stored in cleartext (the same as irssi/WeeChat), so the file is
// written with owner-only permissions (0600); see Save.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SASL holds SASL authentication settings for a network. An empty Mechanism
// disables SASL.
type SASL struct {
	Mechanism string `json:"mechanism,omitempty"` // "PLAIN" or "EXTERNAL"
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

// Network is one saved server entry, bundling the identity used to connect to it.
// Name and Addr are required; the rest are optional (blank identity fields fall
// back to the file's Defaults at connect time).
type Network struct {
	Name              string   `json:"name"`
	Addr              string   `json:"addr"` // host:port
	TLS               bool     `json:"tls,omitempty"`
	Insecure          bool     `json:"insecure,omitempty"`            // skip TLS cert verification
	AllowInsecureAuth bool     `json:"allow_insecure_auth,omitempty"` // permit credentials over plaintext
	Nick              string   `json:"nick,omitempty"`
	User              string   `json:"user,omitempty"`
	Realname          string   `json:"realname,omitempty"`
	Pass              string   `json:"pass,omitempty"` // server PASS
	SASL              SASL     `json:"sasl,omitempty"`
	Channels          []string `json:"channels,omitempty"`   // autojoin
	Highlights        []string `json:"highlights,omitempty"` // extra mention words
}

// Identity holds default nick/user/realname used to prefill a new network form
// and as a fallback when a network leaves those fields blank.
type Identity struct {
	Nick     string `json:"nick,omitempty"`
	User     string `json:"user,omitempty"`
	Realname string `json:"realname,omitempty"`
}

// File is the on-disk configuration document. The zero value is a usable empty
// config (no networks).
type File struct {
	Defaults Identity  `json:"defaults"`
	Networks []Network `json:"networks"`
}

// Path returns the resolved config-file path: $LURK_CONFIG if set, else
// $XDG_CONFIG_HOME/lurk/config.json, else ~/.config/lurk/config.json. The
// ~/.config default is predictable for a terminal tool and honors XDG on Linux.
func Path() (string, error) {
	if p := os.Getenv("LURK_CONFIG"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "lurk", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "lurk", "config.json"), nil
}

// LogDir returns the default chat-log directory: $LURK_LOG_DIR if set, else
// $XDG_DATA_HOME/lurk/logs, else ~/.local/share/lurk/logs.
func LogDir() (string, error) {
	if p := os.Getenv("LURK_LOG_DIR"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "lurk", "logs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "lurk", "logs"), nil
}

// Load reads the config from the resolved Path, returning the document and the
// path it was read from (so callers can Save back to the same place). A missing
// file yields an empty *File and no error; malformed JSON is an error.
func Load() (*File, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	f, err := LoadFrom(path)
	return f, path, err
}

// LoadFrom reads a config file from an explicit path. A missing file yields an
// empty *File and no error.
func LoadFrom(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &f, nil
}

// Save writes f to path as indented JSON with owner-only permissions (0600),
// creating the parent directory (0700) if needed. The write is atomic — it goes
// to a temp file in the same directory which is then renamed into place — so a
// crash mid-write cannot truncate an existing config.
func Save(f *File, path string) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename into place: %w", err)
	}
	return nil
}

// find returns the index of the network named name (case-insensitive), or -1.
func (f *File) find(name string) int {
	for i := range f.Networks {
		if strings.EqualFold(f.Networks[i].Name, name) {
			return i
		}
	}
	return -1
}

// Get returns a copy of the network named name and whether it was found.
func (f *File) Get(name string) (Network, bool) {
	if i := f.find(name); i >= 0 {
		return f.Networks[i], true
	}
	return Network{}, false
}

// Upsert adds n, or replaces an existing network in place. orig is the network's
// prior name (non-empty when editing): it lets an edit that renames the network
// replace the original entry instead of leaving a duplicate. An empty orig keys
// on n.Name.
func (f *File) Upsert(n Network, orig string) {
	key := orig
	if key == "" {
		key = n.Name
	}
	if i := f.find(key); i >= 0 {
		f.Networks[i] = n
		return
	}
	f.Networks = append(f.Networks, n)
}

// Remove deletes the network named name, reporting whether one was removed.
func (f *File) Remove(name string) bool {
	i := f.find(name)
	if i < 0 {
		return false
	}
	f.Networks = append(f.Networks[:i], f.Networks[i+1:]...)
	return true
}
