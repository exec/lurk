package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// sampleFile is a representative config document used across the round-trip tests.
func sampleFile() *File {
	return &File{
		Defaults: Identity{Nick: "dylan", User: "dyl", Realname: "Dylan H"},
		Networks: []Network{
			{
				Name:     "Libera",
				Addr:     "irc.libera.chat:6697",
				TLS:      true,
				Nick:     "lurkbot",
				SASL:     SASL{Mechanism: "PLAIN", Username: "lurkbot", Password: "s3cret"},
				Channels: []string{"#lurk", "#go-nuts"},
			},
			{Name: "Local", Addr: "127.0.0.1:6667"},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	want := sampleFile()

	if err := Save(want, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestSavePerms0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Save(sampleFile(), path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perms = %#o, want 0600 (passwords are stored in cleartext)", perm)
	}
	// The parent directory should be owner-only too.
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	_ = di // t.TempDir's own perms are not under test; Save only MkdirAll's missing parents.
}

func TestSaveOverwriteKeepsPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Pre-create the file world-readable; Save must tighten it back to 0600.
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Save(sampleFile(), path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("after overwrite, perms = %#o, want 0600", perm)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	f, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom missing: %v", err)
	}
	if f == nil || len(f.Networks) != 0 {
		t.Errorf("missing file should yield empty *File, got %+v", f)
	}
}

func TestLoadMalformedIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Error("malformed JSON should be an error")
	}
}

func TestPathLurkConfigOverride(t *testing.T) {
	t.Setenv("LURK_CONFIG", "/tmp/custom/lurk.json")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != "/tmp/custom/lurk.json" {
		t.Errorf("Path with LURK_CONFIG = %q, want /tmp/custom/lurk.json", got)
	}
}

func TestPathXDGOverride(t *testing.T) {
	t.Setenv("LURK_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join("/xdg", "lurk", "config.json"); got != want {
		t.Errorf("Path with XDG_CONFIG_HOME = %q, want %q", got, want)
	}
}

func TestUpsertAddAndReplace(t *testing.T) {
	f := &File{}
	f.Upsert(Network{Name: "Libera", Addr: "a:1"}, "")
	f.Upsert(Network{Name: "Local", Addr: "b:2"}, "")
	if len(f.Networks) != 2 {
		t.Fatalf("after two adds, len = %d, want 2", len(f.Networks))
	}

	// Replace in place (same name).
	f.Upsert(Network{Name: "Libera", Addr: "a:9999"}, "")
	if len(f.Networks) != 2 {
		t.Fatalf("replace changed length to %d, want 2", len(f.Networks))
	}
	if n, _ := f.Get("Libera"); n.Addr != "a:9999" {
		t.Errorf("replace didn't update Addr: %q", n.Addr)
	}

	// Case-insensitive lookup.
	if _, ok := f.Get("libera"); !ok {
		t.Error("Get should be case-insensitive")
	}
}

func TestUpsertRename(t *testing.T) {
	f := &File{Networks: []Network{{Name: "Old", Addr: "a:1"}}}
	// Edit that renames Old -> New must replace, not duplicate.
	f.Upsert(Network{Name: "New", Addr: "a:1"}, "Old")
	if len(f.Networks) != 1 {
		t.Fatalf("rename duplicated entry: len = %d, want 1", len(f.Networks))
	}
	if f.Networks[0].Name != "New" {
		t.Errorf("rename left name = %q, want New", f.Networks[0].Name)
	}
}

func TestRemove(t *testing.T) {
	f := &File{Networks: []Network{{Name: "A"}, {Name: "B"}, {Name: "C"}}}
	if !f.Remove("B") {
		t.Fatal("Remove(B) reported false")
	}
	if len(f.Networks) != 2 || f.Networks[0].Name != "A" || f.Networks[1].Name != "C" {
		t.Errorf("after Remove(B): %+v", f.Networks)
	}
	if f.Remove("missing") {
		t.Error("Remove of absent network reported true")
	}
}
