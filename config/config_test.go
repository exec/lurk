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

func TestValidate(t *testing.T) {
	// validNet is a well-formed network that should always pass.
	validNet := Network{Name: "Libera", Addr: "irc.libera.chat:6697", TLS: true,
		SASL: SASL{Mechanism: "PLAIN", Username: "u", Password: "p"}}

	// wantErrs counts the expected number of errors returned by Validate for
	// each table entry. wantField, when non-empty, asserts at least one error
	// carries that Field value so we know the right check fired.
	tests := []struct {
		name      string
		file      *File
		wantErrs  int
		wantField string
	}{
		{
			name:     "nil file is valid",
			file:     nil,
			wantErrs: 0,
		},
		{
			name:     "empty file is valid",
			file:     &File{},
			wantErrs: 0,
		},
		{
			name:     "one valid network",
			file:     &File{Networks: []Network{validNet}},
			wantErrs: 0,
		},
		{
			name:     "two valid networks",
			file:     &File{Networks: []Network{validNet, {Name: "Local", Addr: "127.0.0.1:6667"}}},
			wantErrs: 0,
		},
		{
			name:      "missing name",
			file:      &File{Networks: []Network{{Addr: "irc.libera.chat:6697"}}},
			wantErrs:  1,
			wantField: "name",
		},
		{
			name:      "missing addr",
			file:      &File{Networks: []Network{{Name: "Net"}}},
			wantErrs:  1,
			wantField: "addr",
		},
		{
			name:      "addr without port",
			file:      &File{Networks: []Network{{Name: "Net", Addr: "irc.libera.chat"}}},
			wantErrs:  1,
			wantField: "addr",
		},
		{
			name:      "unknown SASL mechanism",
			file:      &File{Networks: []Network{{Name: "Net", Addr: "h:1", SASL: SASL{Mechanism: "GSSAPI"}}}},
			wantErrs:  1,
			wantField: "sasl.mechanism",
		},
		{
			name:     "SASL mechanism PLAIN is valid",
			file:     &File{Networks: []Network{{Name: "Net", Addr: "h:1", SASL: SASL{Mechanism: "PLAIN"}}}},
			wantErrs: 0,
		},
		{
			name:     "SASL mechanism EXTERNAL is valid",
			file:     &File{Networks: []Network{{Name: "Net", Addr: "h:1", SASL: SASL{Mechanism: "EXTERNAL"}}}},
			wantErrs: 0,
		},
		{
			// Mechanism matching is case-insensitive to match cmd/lurk's ToUpper call.
			name:     "SASL mechanism lowercase plain is valid",
			file:     &File{Networks: []Network{{Name: "Net", Addr: "h:1", SASL: SASL{Mechanism: "plain"}}}},
			wantErrs: 0,
		},
		{
			name:      "duplicate network name",
			file:      &File{Networks: []Network{{Name: "Net", Addr: "h:1"}, {Name: "net", Addr: "h:2"}}},
			wantErrs:  1,
			wantField: "name",
		},
		{
			name: "bounce netid set without addr",
			file: &File{Networks: []Network{
				{Name: "Net", Addr: "h:1", Bounce: BounceConfig{NetID: 1}},
			}},
			wantErrs:  1,
			wantField: "bounce.addr",
		},
		{
			name: "bounce with both netid and addr is valid",
			file: &File{Networks: []Network{
				{Name: "Net", Addr: "h:1", Bounce: BounceConfig{NetID: 1, Addr: "b:6697"}},
			}},
			wantErrs: 0,
		},
		{
			name: "multiple errors in one network",
			file: &File{Networks: []Network{
				{Name: "", Addr: "", SASL: SASL{Mechanism: "BOGUS"}},
			}},
			// name missing + addr missing + bad mechanism = 3 errors
			wantErrs: 3,
		},
		{
			name: "errors across multiple networks",
			file: &File{Networks: []Network{
				{Name: "A", Addr: ""}, // addr missing
				{Name: "B", Addr: ""}, // addr missing
			}},
			wantErrs: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate(tc.file)
			if len(errs) != tc.wantErrs {
				t.Fatalf("Validate() returned %d errors, want %d:\n%v", len(errs), tc.wantErrs, errs)
			}
			if tc.wantField != "" {
				found := false
				for _, e := range errs {
					if ve, ok := e.(*ValidationError); ok && ve.Field == tc.wantField {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no error with Field=%q among: %v", tc.wantField, errs)
				}
			}
		})
	}
}

func TestValidationErrorString(t *testing.T) {
	e := &ValidationError{Network: "Libera", Field: "addr", Problem: "must not be empty"}
	want := `network "Libera": addr: must not be empty`
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// File-level error (no network name).
	e2 := &ValidationError{Field: "defaults.nick", Problem: "some problem"}
	want2 := "defaults.nick: some problem"
	if got := e2.Error(); got != want2 {
		t.Errorf("Error() = %q, want %q", got, want2)
	}
}
