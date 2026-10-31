package tui

import (
	"fmt"
	"maps"

	"github.com/exec/lurk/config"
)

// SavedConfig enables preference persistence in Run. Path is the saved-network
// config file; Network is the initial client's saved profile name (empty for an
// ad-hoc connection). Networks opened with /connect use their saved names.
type SavedConfig struct {
	Path    string
	Network string
}

// setPreference saves before changing the session, so a failed write leaves the
// old setting intact. Re-read the file to preserve edits made since startup.
func (m model) setPreference(kind, word string, enabled bool) error {
	net := m.activeNet()
	words := net.highlights
	if kind == "ignore" {
		words = net.ignored
	}
	words = maps.Clone(words)
	var store *config.File
	var saved config.Network
	if m.configPath != "" && net.savedName != "" {
		var err error
		store, err = config.LoadFrom(m.configPath)
		if err != nil {
			return err
		}
		var ok bool
		saved, ok = store.Get(net.savedName)
		if !ok {
			return fmt.Errorf("saved network %q no longer exists", net.savedName)
		}
		words = wordSet(saved.Highlights)
		if kind == "ignore" {
			words = wordSet(saved.Ignores)
		}
	}
	if enabled {
		words[asciiLower(word)] = true
	} else {
		delete(words, asciiLower(word))
	}
	if store != nil {
		if kind == "ignore" {
			saved.Ignores = sortedSetKeys(words)
		} else {
			saved.Highlights = sortedSetKeys(words)
		}
		store.Upsert(saved, "")
		if err := config.Save(store, m.configPath); err != nil {
			return err
		}
	}
	if kind == "ignore" {
		net.ignored = words
	} else {
		net.highlights = words
	}
	return nil
}
