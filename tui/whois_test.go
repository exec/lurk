package tui

import (
	"strings"
	"testing"

	"lurk/client"
	"lurk/irc"
)

// TestFormatNumericWhois verifies that WHOIS reply numerics surface the data
// carried in their middle parameters (host, idle seconds, channels, account),
// rather than only the trailing label — the bug where "/whois" rendered lines
// like "seconds idle, signon time" with no actual values.
func TestFormatNumericWhois(t *testing.T) {
	tm := newTheme()
	cases := []struct {
		line     string
		contains []string
		absent   string // a substring that must NOT appear (e.g. a bare label)
	}{
		{
			line:     ":srv 311 me asdf2 ident host.example * :Real Name",
			contains: []string{"asdf2", "ident@host.example", "Real Name"},
		},
		{
			// idle seconds and signon must show, not just the label.
			line:     ":srv 317 me asdf2 3600 1700000000 :seconds idle, signon time",
			contains: []string{"asdf2", "3600s"},
			absent:   "seconds idle, signon time",
		},
		{
			line:     ":srv 338 me asdf2 ident@host 203.0.113.7 :Actual user@host, Actual IP",
			contains: []string{"asdf2", "203.0.113.7", "ident@host"},
			absent:   "Actual user@host, Actual IP",
		},
		{
			line:     ":srv 319 me asdf2 :#test #go",
			contains: []string{"asdf2", "#test", "#go"},
		},
		{
			line:     ":srv 330 me asdf2 someacct :is logged in as",
			contains: []string{"asdf2", "someacct"},
		},
		{
			line:     ":srv 301 me asdf2 :out to lunch",
			contains: []string{"asdf2", "out to lunch"},
		},
		{
			// an error numeric should still name its subject.
			line:     ":srv 401 me nobody :No such nick/channel",
			contains: []string{"nobody", "No such nick"},
		},
	}
	for _, c := range cases {
		m, err := irc.Parse(c.line)
		if err != nil {
			t.Fatalf("parse %q: %v", c.line, err)
		}
		got := stripANSI(tm.formatNumeric(client.Event{Message: m}))
		for _, want := range c.contains {
			if !strings.Contains(got, want) {
				t.Errorf("formatNumeric(%q) = %q, want it to contain %q", c.line, got, want)
			}
		}
		if c.absent != "" && strings.Contains(got, c.absent) {
			t.Errorf("formatNumeric(%q) = %q, must not contain the bare label %q", c.line, got, c.absent)
		}
	}
}
