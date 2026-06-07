package client

import (
	"strings"
	"testing"
)

// TestBouncerListNetworks verifies that BouncerListNetworks sends the exact
// expected wire line "BOUNCER LISTNETWORKS".
func TestBouncerListNetworks(t *testing.T) {
	cfg := Config{Nick: "testuser"}
	c, done := bindClient(t, cfg, func(srv *mockServer) {
		doMinimalReg(srv)
		srv.expect("BOUNCER LISTNETWORKS")
		srv.close()
	})
	_ = c.BouncerListNetworks()
	<-done
}

// TestBouncerDelNetwork verifies that BouncerDelNetwork(42) sends
// "BOUNCER DELNETWORK 42".
func TestBouncerDelNetwork(t *testing.T) {
	cfg := Config{Nick: "testuser"}
	c, done := bindClient(t, cfg, func(srv *mockServer) {
		doMinimalReg(srv)
		srv.expect("BOUNCER DELNETWORK 42")
		srv.close()
	})
	_ = c.BouncerDelNetwork(42)
	<-done
}

// TestBouncerAddNetworkEmpty verifies that BouncerAddNetwork with a nil or empty
// attrs map returns an error immediately (without sending anything), because the
// soju.im/bouncer-networks server rejects an empty attribute list with FAIL.
func TestBouncerAddNetworkEmpty(t *testing.T) {
	cfg := Config{Nick: "testuser"}
	c, done := bindClient(t, cfg, func(srv *mockServer) {
		doMinimalReg(srv)
		// No BOUNCER ADDNETWORK line should arrive; close immediately.
		srv.close()
	})
	err := c.BouncerAddNetwork(nil)
	if err == nil {
		t.Error("BouncerAddNetwork(nil) should return an error for empty attrs, got nil")
	}
	err2 := c.BouncerAddNetwork(map[string]string{})
	if err2 == nil {
		t.Error("BouncerAddNetwork(empty map) should return an error for empty attrs, got nil")
	}
	<-done
}

// TestBouncerAddNetworkAttrs verifies that BouncerAddNetwork with a single-key
// attrs map serialises the key=value pair correctly, including escaping.
func TestBouncerAddNetworkAttrs(t *testing.T) {
	tests := []struct {
		name       string
		attrs      map[string]string
		wantSuffix string // expected substring in the serialised line
	}{
		{
			name:       "simple name",
			attrs:      map[string]string{"name": "Libera.Chat"},
			wantSuffix: "name=Libera.Chat",
		},
		{
			name:       "value with space",
			attrs:      map[string]string{"realname": "My Name"},
			wantSuffix: "realname=My\\sName",
		},
		{
			name:       "value with semicolon",
			attrs:      map[string]string{"name": "Net;Work"},
			wantSuffix: "name=Net\\:Work",
		},
		{
			name:       "value with backslash",
			attrs:      map[string]string{"name": `back\slash`},
			wantSuffix: `name=back\\slash`,
		},
		{
			name:       "value with space and semicolon",
			attrs:      map[string]string{"name": "foo; bar"},
			wantSuffix: `name=foo\:\sbar`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Nick: "testuser"}
			c, done := bindClient(t, cfg, func(srv *mockServer) {
				doMinimalReg(srv)
				line := srv.readLine()
				if !strings.HasPrefix(line, "BOUNCER ADDNETWORK ") {
					srv.fail("want BOUNCER ADDNETWORK prefix, got %q", line)
				}
				attrPart := strings.TrimPrefix(line, "BOUNCER ADDNETWORK ")
				if !strings.Contains(attrPart, tt.wantSuffix) {
					srv.fail("attr part %q does not contain %q", attrPart, tt.wantSuffix)
				}
				srv.close()
			})
			_ = c.BouncerAddNetwork(tt.attrs)
			<-done
		})
	}
}

// TestBouncerChangeNetwork verifies that BouncerChangeNetwork serialises the
// exact expected line "BOUNCER CHANGENETWORK <netid> <attrs>".
func TestBouncerChangeNetwork(t *testing.T) {
	cfg := Config{Nick: "testuser"}
	c, done := bindClient(t, cfg, func(srv *mockServer) {
		doMinimalReg(srv)
		line := srv.readLine()
		if !strings.HasPrefix(line, "BOUNCER CHANGENETWORK 7 ") {
			srv.fail("want BOUNCER CHANGENETWORK 7 ..., got %q", line)
		}
		if !strings.Contains(line, "nick=newnick") {
			srv.fail("want nick=newnick in %q", line)
		}
		srv.close()
	})
	_ = c.BouncerChangeNetwork(7, map[string]string{"nick": "newnick"})
	<-done
}

// TestEncodeBouncerAttrs is a pure-unit test for the escaping function,
// independent of any network I/O.
func TestEncodeBouncerAttrs(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"nil", nil, ""},
		{"empty", map[string]string{}, ""},
		{"one plain", map[string]string{"host": "irc.example.com"}, "host=irc.example.com"},
		{"escape semicolon", map[string]string{"x": "a;b"}, `x=a\:b`},
		{"escape space", map[string]string{"x": "hello world"}, `x=hello\sworld`},
		{"escape backslash", map[string]string{"x": `a\b`}, `x=a\\b`},
		{"escape CR", map[string]string{"x": "a\rb"}, `x=a\rb`},
		{"escape LF", map[string]string{"x": "a\nb"}, `x=a\nb`},
		// Both special chars in one value.
		{"space and semi", map[string]string{"x": "foo; bar"}, `x=foo\:\sbar`},
		// Backslash then semicolon: both get escaped independently.
		// `a\;b` → 'a' + '\\' (escaped \) + '\:' (escaped ;) + 'b' = "a\\\:b" raw.
		// As a Go raw string: `a\\\:b`.
		{"backslash then semi", map[string]string{"x": `a\;b`}, `x=a\\\:b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeBouncerAttrs(tt.attrs)
			if tt.attrs == nil || len(tt.attrs) == 0 {
				if got != "" {
					t.Errorf("encodeBouncerAttrs(%v) = %q, want %q", tt.attrs, got, tt.want)
				}
				return
			}
			// For single-key maps we can do an exact match.
			if len(tt.attrs) == 1 {
				if got != tt.want {
					t.Errorf("encodeBouncerAttrs(%v) = %q, want %q", tt.attrs, got, tt.want)
				}
			}
		})
	}
}

// TestParseBouncerAttrs is a pure-unit test for the sanitizing wrapper.
// It verifies that:
//   - normal attr values pass through unchanged;
//   - terminal control bytes (ESC, C1, bidi marks) in attr values are stripped;
//   - the wire-escape sequences (\s \: \\ \r \n) are still correctly unescaped
//     (SanitizeTerminal must not be called on the raw wire string before
//     unescaping, since backslash escapes contain only ASCII-safe bytes).
func TestParseBouncerAttrs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		key   string
		want  string
	}{
		{
			name:  "clean value passes through",
			input: "name=Libera.Chat",
			key:   "name",
			want:  "Libera.Chat",
		},
		{
			name:  "wire-escaped space is unescaped",
			input: `realname=My\sName`,
			key:   "realname",
			want:  "My Name",
		},
		{
			name:  "wire-escaped semicolon is unescaped",
			input: `name=Net\:Work`,
			key:   "name",
			want:  "Net;Work",
		},
		{
			// ESC byte smuggled in a value — SanitizeTerminal strips the ESC (0x1b)
			// but leaves the remaining ASCII bytes, so "name=\x1b[Ahello" becomes
			// "[Ahello" (the CSI sequence letter and payload remain, ESC is gone).
			// This is correct: the threat is the ESC introducer that triggers terminal
			// parsing; without it the printable bytes are inert.
			name:  "ESC byte stripped from name",
			input: "name=\x1b[Ahello",
			key:   "name",
			want:  "[Ahello",
		},
		{
			// C1 control in valid UTF-8 encoding (U+009B CSI, encoded as \xc2\x9b).
			// SanitizeTerminal drops runes in the range 0x80–0x9F.
			name:  "C1 CSI control stripped (valid UTF-8)",
			input: "host=\xc2\x9bowned.example.com",
			key:   "host",
			want:  "owned.example.com",
		},
		{
			// Trojan-Source bidi override (U+202E RIGHT-TO-LEFT OVERRIDE).
			name:  "bidi override stripped from realname",
			input: "realname=alice‮bob",
			key:   "realname",
			want:  "alicebob",
		},
		{
			// Empty input returns an empty map without panicking.
			name:  "empty input",
			input: "",
			key:   "name",
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ParseBouncerAttrs(tt.input)
			got := m[tt.key]
			if got != tt.want {
				t.Errorf("ParseBouncerAttrs(%q)[%q] = %q, want %q", tt.input, tt.key, got, tt.want)
			}
			// No value in the map may contain an ESC byte.
			for k, v := range m {
				for _, r := range v {
					if r == 0x1b {
						t.Errorf("ParseBouncerAttrs(%q): key %q still contains ESC in value %q", tt.input, k, v)
					}
				}
			}
		})
	}
}

// doMinimalReg drives a bare-minimum registration for the bouncer test helpers:
// CAP LS (no special caps needed), NICK/USER, welcome. This is used by tests
// that only care about lines sent after registration.
func doMinimalReg(srv *mockServer) {
	srv.expect("CAP LS 302")
	srv.expect("NICK testuser")
	srv.expectPrefix("USER ")
	srv.send("CAP * LS :")
	srv.expect("CAP END")
	srv.send(":server 001 testuser :Welcome")
}
