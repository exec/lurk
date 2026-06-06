package bouncer_test

import (
	"strings"
	"testing"

	"github.com/exec/lurk/bouncer"
)

// ─── Attribute escape / unescape round-trip ───────────────────────────────────

func TestEscapeUnescapeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"plain", "hello"},
		{"with space", "hello world"},
		{"with semicolon", "key;value"},
		{"with backslash", `a\b`},
		{"with CR", "a\rb"},
		{"with LF", "a\nb"},
		{"all specials", ";\\ \r\n"},
		{"empty", ""},
		{"only backslash", `\`},
		{"double backslash", `\\`},
		{"unicode", "Libera.Chat 🌐"},
		{"very long", strings.Repeat("a;b\\c d", 200)},
		{"embedded NUL", "a\x00b"}, // NUL is not special but must survive
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			escaped := bouncer.EscapeAttrValue(tc.raw)
			got := bouncer.UnescapeAttrValue(escaped)
			if got != tc.raw {
				t.Errorf("round-trip failed: raw=%q escaped=%q got=%q",
					tc.raw, escaped, got)
			}
		})
	}
}

// TestEscapeSpecificChars verifies the specific escape sequences the spec
// mandates, so a change to attrEscaper doesn't silently break interoperability.
func TestEscapeSpecificChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`\`, `\\`},
		{`;`, `\:`},
		{" ", `\s`},
		{"\r", `\r`},
		{"\n", `\n`},
	}
	for _, tc := range cases {
		got := bouncer.EscapeAttrValue(tc.in)
		if got != tc.want {
			t.Errorf("EscapeAttrValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEncodeAttrsRoundTrip verifies that EncodeAttrs → ParseAttrs recovers the
// original pairs, including values that contain the special characters.
func TestEncodeAttrsRoundTrip(t *testing.T) {
	pairs := [][2]string{
		{"name", "Libera.Chat"},
		{"host", "irc.libera.chat"},
		{"port", "6697"},
		{"nick", "my;nick\\with spaces"},
		{"state", "connected"},
	}
	encoded := bouncer.EncodeAttrs(pairs)
	parsed := bouncer.ParseAttrs(encoded)

	for _, kv := range pairs {
		if got, ok := parsed[kv[0]]; !ok {
			t.Errorf("key %q missing from parsed attrs", kv[0])
		} else if got != kv[1] {
			t.Errorf("key %q: got %q, want %q (encoded=%q)", kv[0], got, kv[1], encoded)
		}
	}
}

// TestEncodeAttrsInjection verifies that an attacker cannot break the encoding
// by embedding semicolons, backslashes, or spaces in values.
func TestEncodeAttrsInjection(t *testing.T) {
	// An attacker supplies a network name that contains ";" to try to inject
	// a second key-value pair.
	malicious := "evil;state=hacked"
	pairs := [][2]string{{"name", malicious}, {"state", "disconnected"}}
	encoded := bouncer.EncodeAttrs(pairs)
	parsed := bouncer.ParseAttrs(encoded)

	// The parsed name must be exactly malicious, not the split version.
	if got := parsed["name"]; got != malicious {
		t.Errorf("injection protection failed: parsed name=%q, want %q", got, malicious)
	}
	// state must still be "disconnected" (the attacker's ";state=hacked" is
	// part of the escaped name value, not an extra key).
	if got := parsed["state"]; got != "disconnected" {
		t.Errorf("injection caused state to become %q", got)
	}
}

// TestParseAttrsEmpty and hostile inputs.
func TestParseAttrsEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantKV map[string]string
	}{
		{"empty", "", map[string]string{}},
		{"only semicolons", ";;;", map[string]string{}},
		{"no value", "key", map[string]string{"key": ""}},
		{"empty key", "=value", map[string]string{}}, // empty key is skipped
		{"duplicate keys last wins", "a=1;a=2", map[string]string{"a": "2"}},
		{"trailing semicolon", "a=1;", map[string]string{"a": "1"}},
		{"leading semicolon", ";a=1", map[string]string{"a": "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bouncer.ParseAttrs(tc.input)
			for k, v := range tc.wantKV {
				if gv, ok := got[k]; !ok {
					t.Errorf("missing key %q", k)
				} else if gv != v {
					t.Errorf("key %q = %q, want %q", k, gv, v)
				}
			}
			// No extra keys.
			for k := range got {
				if _, ok := tc.wantKV[k]; !ok {
					t.Errorf("unexpected key %q = %q", k, got[k])
				}
			}
		})
	}
}

// ─── Command parsing ──────────────────────────────────────────────────────────

func TestParseCmdBIND(t *testing.T) {
	cmd, err := bouncer.ParseCmd([]string{"BIND", "42"})
	if err != nil {
		t.Fatalf("ParseCmd BIND: %v", err)
	}
	if cmd.Sub != bouncer.SubBind {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubBind)
	}
	if cmd.NetID != 42 {
		t.Errorf("NetID = %d, want 42", cmd.NetID)
	}
}

func TestParseCmdBINDCaseInsensitive(t *testing.T) {
	cmd, err := bouncer.ParseCmd([]string{"bind", "1"})
	if err != nil {
		t.Fatalf("ParseCmd bind (lower): %v", err)
	}
	if cmd.Sub != bouncer.SubBind {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubBind)
	}
}

func TestParseCmdLISTNETWORKS(t *testing.T) {
	cmd, err := bouncer.ParseCmd([]string{"LISTNETWORKS"})
	if err != nil {
		t.Fatalf("ParseCmd LISTNETWORKS: %v", err)
	}
	if cmd.Sub != bouncer.SubListNetworks {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubListNetworks)
	}
}

func TestParseCmdADDNETWORK(t *testing.T) {
	attrs := "name=Libera;host=irc.libera.chat;port=6697;tls=1"
	cmd, err := bouncer.ParseCmd([]string{"ADDNETWORK", attrs})
	if err != nil {
		t.Fatalf("ParseCmd ADDNETWORK: %v", err)
	}
	if cmd.Sub != bouncer.SubAddNetwork {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubAddNetwork)
	}
	if cmd.Attrs["name"] != "Libera" {
		t.Errorf("name = %q, want %q", cmd.Attrs["name"], "Libera")
	}
	if cmd.Attrs["port"] != "6697" {
		t.Errorf("port = %q, want %q", cmd.Attrs["port"], "6697")
	}
}

func TestParseCmdCHANGENETWORK(t *testing.T) {
	cmd, err := bouncer.ParseCmd([]string{"CHANGENETWORK", "3", "nick=newnick"})
	if err != nil {
		t.Fatalf("ParseCmd CHANGENETWORK: %v", err)
	}
	if cmd.Sub != bouncer.SubChangeNetwork {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubChangeNetwork)
	}
	if cmd.NetID != 3 {
		t.Errorf("NetID = %d, want 3", cmd.NetID)
	}
	if cmd.Attrs["nick"] != "newnick" {
		t.Errorf("nick attr = %q, want newnick", cmd.Attrs["nick"])
	}
}

func TestParseCmdDELNETWORK(t *testing.T) {
	cmd, err := bouncer.ParseCmd([]string{"DELNETWORK", "7"})
	if err != nil {
		t.Fatalf("ParseCmd DELNETWORK: %v", err)
	}
	if cmd.Sub != bouncer.SubDelNetwork {
		t.Errorf("Sub = %q, want %q", cmd.Sub, bouncer.SubDelNetwork)
	}
	if cmd.NetID != 7 {
		t.Errorf("NetID = %d, want 7", cmd.NetID)
	}
}

// TestParseCmdHostileInputs verifies that malformed or hostile input returns an
// error and never panics.
func TestParseCmdHostileInputs(t *testing.T) {
	cases := []struct {
		name   string
		params []string
	}{
		{"empty params", []string{}},
		{"empty subcommand", []string{""}},
		{"BIND no netid", []string{"BIND"}},
		{"BIND empty netid", []string{"BIND", ""}},
		{"BIND negative netid", []string{"BIND", "-1"}},
		{"BIND zero netid", []string{"BIND", "0"}},
		{"BIND non-numeric", []string{"BIND", "abc"}},
		{"BIND overflow-ish", []string{"BIND", "99999999999999999999"}},
		{"DELNETWORK no netid", []string{"DELNETWORK"}},
		{"DELNETWORK empty netid", []string{"DELNETWORK", ""}},
		{"ADDNETWORK no attrs", []string{"ADDNETWORK"}},
		{"ADDNETWORK empty attrs", []string{"ADDNETWORK", ""}},
		{"CHANGENETWORK no netid", []string{"CHANGENETWORK"}},
		{"CHANGENETWORK bad netid", []string{"CHANGENETWORK", "notanumber", "a=b"}},
		{"unknown subcommand", []string{"FROBNICATENETWORK"}},
		{"junk subcommand with spaces", []string{"A B C"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseCmd panicked: %v", r)
				}
			}()
			_, err := bouncer.ParseCmd(tc.params)
			if err == nil {
				t.Errorf("ParseCmd(%v) succeeded, want error", tc.params)
			}
		})
	}
}

// ─── NetworkInfo → attrs → NetworkInfo round-trip ─────────────────────────────

func TestNetworkInfoRoundTrip(t *testing.T) {
	n := bouncer.NetworkInfo{
		NetID:    5,
		Name:     "My Net;Work", // semicolon in name — must survive
		Host:     "irc.example.com",
		Port:     "6697",
		TLS:      true,
		Nick:     "mynick",
		Username: "myuser",
		Realname: "My Real Name",
		State:    "connected",
	}
	pairs := bouncer.NetworkInfoToAttrs(n)
	encoded := bouncer.EncodeAttrs(pairs)
	parsed := bouncer.ParseAttrs(encoded)
	got := bouncer.NetworkInfoFromAttrs(parsed)
	// NetID is not carried in attrs; check the rest.
	if got.Name != n.Name {
		t.Errorf("Name: got %q, want %q", got.Name, n.Name)
	}
	if got.Host != n.Host {
		t.Errorf("Host: got %q, want %q", got.Host, n.Host)
	}
	if got.TLS != n.TLS {
		t.Errorf("TLS: got %v, want %v", got.TLS, n.TLS)
	}
	if got.State != n.State {
		t.Errorf("State: got %q, want %q", got.State, n.State)
	}
}

// TestNetworkInfoNoPassword verifies that NetworkInfoToAttrs never emits a
// password key, even if one were somehow present in the struct (it isn't —
// NetworkInfo has no password field, this test documents the invariant).
func TestNetworkInfoNoPassword(t *testing.T) {
	n := bouncer.NetworkInfo{NetID: 1, Name: "Test", State: "connected"}
	pairs := bouncer.NetworkInfoToAttrs(n)
	encoded := bouncer.EncodeAttrs(pairs)
	parsed := bouncer.ParseAttrs(encoded)
	for k := range parsed {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "pass") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") {
			t.Errorf("NetworkInfoToAttrs emitted a secret attr %q", k)
		}
	}
}
