package irc

import (
	"errors"
	"testing"
)

func TestSerialize(t *testing.T) {
	tests := []struct {
		name    string
		msg     *Message
		want    string
		wantErr error
	}{
		{
			name: "simple command",
			msg:  &Message{Command: "PING", Params: []string{"token"}},
			want: "PING token\r\n",
		},
		{
			name: "trailing with space",
			msg:  &Message{Command: "PRIVMSG", Params: []string{"#ch", "Hello there"}},
			want: "PRIVMSG #ch :Hello there\r\n",
		},
		{
			name: "trailing empty param",
			msg:  &Message{Command: "PRIVMSG", Params: []string{"#ch", ""}},
			want: "PRIVMSG #ch :\r\n",
		},
		{
			name: "trailing param starting with colon",
			msg:  &Message{Command: "PRIVMSG", Params: []string{"#ch", ":-)"}},
			want: "PRIVMSG #ch ::-)\r\n",
		},
		{
			name: "single-word last param not trailing",
			msg:  &Message{Command: "JOIN", Params: []string{"#chan", "key"}},
			want: "JOIN #chan key\r\n",
		},
		{
			name: "source preserved",
			msg:  &Message{Source: "nick!u@h", Command: "PRIVMSG", Params: []string{"#ch", "Hi there"}},
			want: ":nick!u@h PRIVMSG #ch :Hi there\r\n",
		},
		{
			name: "single tag",
			msg:  &Message{Tags: Tags{"time": "2020"}, Command: "PING"},
			want: "@time=2020 PING\r\n",
		},
		{
			name: "value-less tag is bare key",
			msg:  &Message{Tags: Tags{"account-tag": ""}, Command: "PING"},
			want: "@account-tag PING\r\n",
		},
		{
			name: "tag value escaped",
			msg:  &Message{Tags: Tags{"msg": "a b;c\\d\r\n"}, Command: "CMD"},
			want: `@msg=a\sb\:c\\d\r\n CMD` + "\r\n",
		},
		{
			name:    "empty command rejected",
			msg:     &Message{Params: []string{"x"}},
			wantErr: ErrNoCommand,
		},
		{
			name:    "CR in param rejected",
			msg:     &Message{Command: "PRIVMSG", Params: []string{"#ch", "a\rb"}},
			wantErr: ErrIllegalByte,
		},
		{
			name:    "LF in source rejected",
			msg:     &Message{Source: "a\nb", Command: "PING"},
			wantErr: ErrIllegalByte,
		},
		{
			name:    "NUL in command rejected",
			msg:     &Message{Command: "PI\x00NG"},
			wantErr: ErrIllegalByte,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.msg.Serialize()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Serialize() err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Serialize() unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("Serialize() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSerializeRejectsMisframing covers the guards that prevent Serialize from
// emitting a line that would reframe to a different message on reparse (found via
// FuzzParse: Parse(": :") yields Command ":", which serialized as ":" reparses as
// a bare source). A command must begin with a letter/digit, and a non-final
// param must not begin with ':'.
func TestSerializeRejectsMisframing(t *testing.T) {
	cases := []struct {
		name string
		msg  *Message
	}{
		{"colon command", &Message{Command: ":"}},
		{"tag-lead command", &Message{Command: "@x"}},
		{"punctuation command", &Message{Command: "-bad"}},
		{"non-final colon param", &Message{Command: "CMD", Params: []string{":x", "y"}}},
		// A space in the source would promote the remainder to the command
		// position: ":a PRIVMSG #c :x JOIN" reparses as a PRIVMSG from "a".
		{"source with space", &Message{Source: "a PRIVMSG #c :x", Command: "JOIN", Params: []string{"#y"}}},
		// A space, ';', or '=' in a tag key would end or split the tag
		// segment, shifting the frame of everything after it.
		{"tag key with space", &Message{Tags: Tags{"a b": ""}, Command: "CMD"}},
		{"tag key with semicolon", &Message{Tags: Tags{"a;b": ""}, Command: "CMD"}},
		{"tag key with equals", &Message{Tags: Tags{"a=b": "v"}, Command: "CMD"}},
		{"empty tag key", &Message{Tags: Tags{"": "v"}, Command: "CMD"}},
		// NUL has no tag-value escape sequence; emitted raw it would be an
		// illegal wire byte.
		{"NUL in tag value", &Message{Tags: Tags{"k": "a\x00b"}, Command: "CMD"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.msg.Serialize(); err == nil {
				t.Errorf("Serialize(%+v) = nil error, want a rejection", c.msg)
			}
		})
	}

	// A valid all-digit (numeric) command and a final colon param still serialize.
	if _, err := (&Message{Command: "001", Params: []string{"me", ":-)"}}).Serialize(); err != nil {
		t.Errorf("valid numeric/colon-trailing message rejected: %v", err)
	}
}

// TestRoundTrip verifies Parse and Serialize are inverses for representative
// lines (tag order aside, which is not significant).
func TestRoundTrip(t *testing.T) {
	lines := []string{
		"PING token\r\n",
		"PRIVMSG #ch :Hello there world\r\n",
		":nick!u@h PRIVMSG #ch :Hello world\r\n",
		"@example.com/ddd=eee :nick!u@h PRIVMSG #ch :Hello world\r\n",
		":server.example 001 nick :Welcome to the network\r\n",
		"PRIVMSG #ch :\r\n",
		"JOIN #chan key\r\n",
		`@msg=a\sb\:c\\d CMD` + "\r\n",
	}
	for _, line := range lines {
		m, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q): %v", line, err)
		}
		got, err := m.Serialize()
		if err != nil {
			t.Fatalf("Serialize(%q): %v", line, err)
		}
		if got != line {
			t.Errorf("round trip:\n  in:  %q\n  out: %q", line, got)
		}
	}
}
