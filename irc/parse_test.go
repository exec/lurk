package irc

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    *Message
		wantErr error
	}{
		{
			name: "simple command no params",
			line: "PING",
			want: &Message{Command: "PING"},
		},
		{
			name: "command with params",
			line: "JOIN #chan key",
			want: &Message{Command: "JOIN", Params: []string{"#chan", "key"}},
		},
		{
			name: "trailing param with spaces",
			line: "PRIVMSG #ch :Hello there world",
			want: &Message{Command: "PRIVMSG", Params: []string{"#ch", "Hello there world"}},
		},
		{
			name: "source and trailing",
			line: ":nick!u@h PRIVMSG #ch :Hello",
			want: &Message{Source: "nick!u@h", Command: "PRIVMSG", Params: []string{"#ch", "Hello"}},
		},
		{
			name: "spec example with tags",
			line: "@aaa=bbb;ccc;example.com/ddd=eee :nick!u@h PRIVMSG #ch :Hello",
			want: &Message{
				Tags:    Tags{"aaa": "bbb", "ccc": "", "example.com/ddd": "eee"},
				Source:  "nick!u@h",
				Command: "PRIVMSG",
				Params:  []string{"#ch", "Hello"},
			},
		},
		{
			name: "client-only tag retains plus",
			line: "@+typing=active TAGMSG #ch",
			want: &Message{
				Tags:    Tags{"+typing": "active"},
				Command: "TAGMSG",
				Params:  []string{"#ch"},
			},
		},
		{
			name: "numeric command preserved",
			line: ":server.example 001 nick :Welcome",
			want: &Message{Source: "server.example", Command: "001", Params: []string{"nick", "Welcome"}},
		},
		{
			name: "empty trailing param",
			line: "PRIVMSG #ch :",
			want: &Message{Command: "PRIVMSG", Params: []string{"#ch", ""}},
		},
		{
			name: "trailing param starting with colon",
			line: "PRIVMSG #ch ::leading-colon",
			want: &Message{Command: "PRIVMSG", Params: []string{"#ch", ":leading-colon"}},
		},
		{
			name: "tags but no value vs empty value",
			line: "@a;b= CMD",
			want: &Message{Tags: Tags{"a": "", "b": ""}, Command: "CMD"},
		},
		{
			name: "surplus spaces between fields tolerated",
			line: ":src   CMD   a    b",
			want: &Message{Source: "src", Command: "CMD", Params: []string{"a", "b"}},
		},
		{
			name: "tag value escapes decoded",
			line: `@msg=a\sb\:c\\d CMD`,
			want: &Message{Tags: Tags{"msg": "a b;c\\d"}, Command: "CMD"},
		},
		{
			name: "CRLF stripped",
			line: "PING token\r\n",
			want: &Message{Command: "PING", Params: []string{"token"}},
		},
		{
			name: "bare LF terminator tolerated",
			line: "PING token\n",
			want: &Message{Command: "PING", Params: []string{"token"}},
		},
		{
			name: "bare CR terminator tolerated",
			line: "PING token\r",
			want: &Message{Command: "PING", Params: []string{"token"}},
		},
		{
			name: "source only server name",
			line: ":irc.example.net PONG",
			want: &Message{Source: "irc.example.net", Command: "PONG"},
		},
		{
			name: "command lowercased preserved verbatim",
			line: "privmsg #x :hi",
			want: &Message{Command: "privmsg", Params: []string{"#x", "hi"}},
		},
		{
			name:    "empty line",
			line:    "",
			wantErr: ErrEmptyMessage,
		},
		{
			name:    "tags only",
			line:    "@a=b",
			wantErr: ErrEmptyMessage,
		},
		{
			name:    "source only no command",
			line:    ":nick!u@h",
			wantErr: ErrEmptyMessage,
		},
		{
			name:    "tags and source no command",
			line:    "@a=b :src",
			wantErr: ErrEmptyMessage,
		},
		{
			name:    "only spaces",
			line:    "   ",
			wantErr: ErrEmptyMessage,
		},
		{
			name:    "embedded NUL rejected",
			line:    "PRIVMSG #ch :hel\x00lo",
			wantErr: ErrBadLineChar,
		},
		{
			name:    "embedded LF rejected (smuggled second line)",
			line:    "PRIVMSG #ch :hi\nQUIT",
			wantErr: ErrBadLineChar,
		},
		{
			name:    "embedded CR rejected",
			line:    "PRIVMSG #ch :hi\rthere",
			wantErr: ErrBadLineChar,
		},
		{
			// Only one trailing terminator is peeled; a second CR is left and
			// then rejected as a bad char, so we never silently swallow it.
			name:    "double terminator leaves bad char",
			line:    "PING token\r\r\n",
			wantErr: ErrBadLineChar,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.line)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%q) err = %v, want %v", tt.line, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected err: %v", tt.line, err)
			}
			if len(tt.want.Tags) == 0 {
				// Normalize: a non-nil empty map and nil compare equal for our purposes.
				if len(got.Tags) != 0 {
					t.Errorf("Parse(%q) tags = %v, want none", tt.line, got.Tags)
				}
				got.Tags = nil
				tt.want.Tags = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) =\n  %#v\nwant\n  %#v", tt.line, got, tt.want)
			}
		})
	}
}

func TestSourceHelpers(t *testing.T) {
	tests := []struct {
		source           string
		nick, user, host string
	}{
		{"nick!user@host", "nick", "user", "host"},
		{"nick@host", "nick", "", "host"},
		{"nick!user", "nick", "", ""},
		{"server.example.net", "server.example.net", "", ""},
		{"", "", "", ""},
	}
	for _, tt := range tests {
		m := &Message{Source: tt.source}
		if got := m.Nick(); got != tt.nick {
			t.Errorf("Nick(%q) = %q, want %q", tt.source, got, tt.nick)
		}
		if got := m.User(); got != tt.user {
			t.Errorf("User(%q) = %q, want %q", tt.source, got, tt.user)
		}
		if got := m.Host(); got != tt.host {
			t.Errorf("Host(%q) = %q, want %q", tt.source, got, tt.host)
		}
	}
}

func TestParam(t *testing.T) {
	m := &Message{Params: []string{"a", "b"}}
	if got := m.Param(0); got != "a" {
		t.Errorf("Param(0) = %q", got)
	}
	if got := m.Param(5); got != "" {
		t.Errorf("Param(5) = %q, want empty", got)
	}
	if got := m.Param(-1); got != "" {
		t.Errorf("Param(-1) = %q, want empty", got)
	}
}

// TestParseParamCountCap verifies that Parse never returns more than
// maxParamCount parameters. Without the cap, a caller that passes an
// unbounded string directly to Parse (bypassing the conn-layer budget)
// could receive a Params slice with thousands of entries.
func TestParseParamCountCap(t *testing.T) {
	// Build a line with maxParamCount+20 single-char non-trailing params.
	// "CMD a a a a ..." with no ':' prefix so each token is a regular param.
	total := maxParamCount + 20
	var b strings.Builder
	b.WriteString("CMD")
	for i := 0; i < total; i++ {
		b.WriteString(" a")
	}
	line := b.String()

	m, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(m.Params) > maxParamCount {
		t.Errorf("Parse returned %d params for a %d-param line, want at most %d",
			len(m.Params), total, maxParamCount)
	}
	// All stored params must be the expected value.
	for i, p := range m.Params {
		if p != "a" {
			t.Errorf("Params[%d] = %q, want \"a\"", i, p)
		}
	}
}
