package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lurkd", "config.json")
	in := &Config{
		Listen:      Listen{Addr: ":6697", TLSCert: "/etc/lurkd/cert.pem", TLSKey: "/etc/lurkd/key.pem"},
		BouncerAuth: BouncerAuth{User: "dylan", PasswordHash: "pbkdf2-sha256:600000:c2FsdA==:ZGs="},
		Networks: []Network{{
			NetID: 1, Name: "Libera.Chat", Addr: "irc.libera.chat:6697", TLS: true,
			Identity: Identity{Nick: "dylan", User: "dylan", Realname: "Dylan"},
			SASL:     SASL{Mechanism: "PLAIN", Authcid: "dylan", Password: "hunter2"},
			Channels: []string{"#lurk", "#go"},
		}},
	}
	if err := Save(in, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The parent dir is created and the file is owner-only (0600).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %#o, want 0600", perm)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.Listen.Addr != ":6697" || got.Listen.TLSCert == "" {
		t.Errorf("listen round-trip wrong: %+v", got.Listen)
	}
	if got.BouncerAuth.User != "dylan" || got.BouncerAuth.PasswordHash == "" {
		t.Errorf("bouncer auth round-trip wrong: %+v", got.BouncerAuth)
	}
	if len(got.Networks) != 1 {
		t.Fatalf("networks = %d, want 1", len(got.Networks))
	}
	n := got.Networks[0]
	if n.NetID != 1 || n.Name != "Libera.Chat" || !n.TLS ||
		n.SASL.Password != "hunter2" || len(n.Channels) != 2 {
		t.Errorf("network round-trip wrong: %+v", n)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	got, err := LoadFrom(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("a missing config must not error: %v", err)
	}
	if got == nil || len(got.Networks) != 0 {
		t.Errorf("missing config should yield an empty *Config, got %+v", got)
	}
}

func TestLoadMalformedErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Error("malformed JSON should be an error")
	}
}

func TestAssignNetIDsAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// A, C omit netid; B pins 5. After load A and C get unique ids that avoid 5.
	in := &Config{Networks: []Network{{Name: "A"}, {Name: "B", NetID: 5}, {Name: "C"}}}
	if err := Save(in, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for _, n := range got.Networks {
		ids[n.Name] = n.NetID
	}
	if ids["B"] != 5 {
		t.Errorf("B netid = %d, want the pinned 5", ids["B"])
	}
	if ids["A"] == 0 || ids["C"] == 0 {
		t.Errorf("A and C must be assigned ids: %v", ids)
	}
	if ids["A"] == ids["C"] || ids["A"] == 5 || ids["C"] == 5 {
		t.Errorf("assigned ids must be unique and avoid the pinned 5: %v", ids)
	}
}

func TestNextNetID(t *testing.T) {
	c := &Config{Networks: []Network{{NetID: 1}, {NetID: 2}, {NetID: 4}}}
	if got := c.NextNetID(); got != 3 {
		t.Errorf("NextNetID = %d, want 3 (smallest unused)", got)
	}
	c.Networks = append(c.Networks, Network{NetID: 3})
	if got := c.NextNetID(); got != 5 {
		t.Errorf("NextNetID = %d, want 5 after 1-4 are taken", got)
	}
}

func TestPathEnvOverride(t *testing.T) {
	t.Setenv("LURKD_CONFIG", "/tmp/explicit/lurkd.json")
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/tmp/explicit/lurkd.json" {
		t.Errorf("Path = %q, want the LURKD_CONFIG override", p)
	}
}

func TestPathXDG(t *testing.T) {
	t.Setenv("LURKD_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join("/xdg", "lurkd", "config.json") {
		t.Errorf("Path = %q, want the XDG lurkd path", p)
	}
}
