package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// TestFormatNumericWhois verifies the compact WHOIS rendering: the header (311)
// and footer (318) carry the nick, the detail rows surface their data (idle,
// channels, account, actual host/IP) without repeating the nick, RPL_AWAY keeps
// the nick inline, and bare trailing labels are not shown in place of values.
func TestFormatNumericWhois(t *testing.T) {
	tm := newTheme()
	cases := []struct {
		line     string
		contains []string
		absent   string // a substring that must NOT appear (e.g. a bare label)
	}{
		{
			// 311 header: nick + ident@host + realname.
			line:     ":srv 311 me asdf2 ident host.example * :Real Name",
			contains: []string{"whois", "asdf2", "ident@host.example", "Real Name"},
		},
		{
			// idle seconds and signon show; the bare label does not; no nick repeat.
			line:     ":srv 317 me asdf2 3600 1700000000 :seconds idle, signon time",
			contains: []string{"·", "idle", "3600s", "signed on"},
			absent:   "seconds idle, signon time",
		},
		{
			// distinct host/IP shown as "host (ip)".
			line:     ":srv 338 me asdf2 ident@host 203.0.113.7 :Actual user@host, Actual IP",
			contains: []string{"actually", "ident@host (203.0.113.7)"},
			absent:   "Actual user@host, Actual IP",
		},
		{
			// IP already contained in the host mask collapses to just the host.
			line:     ":srv 338 me asdf2 ~u@10.0.0.29 10.0.0.29 :Actual user@host",
			contains: []string{"~u@10.0.0.29"},
			absent:   "10.0.0.29 10.0.0.29",
		},
		{
			line:     ":srv 319 me asdf2 :@#test #go",
			contains: []string{"channels", "@#test", "#go"},
		},
		{
			line:     ":srv 330 me asdf2 someacct :is logged in as",
			contains: []string{"account", "someacct"},
			absent:   "is logged in as",
		},
		{
			// RPL_AWAY keeps the nick inline (it also arrives standalone).
			line:     ":srv 301 me asdf2 :out to lunch",
			contains: []string{"asdf2", "out to lunch"},
		},
		{
			// footer carries the nick.
			line:     ":srv 318 me asdf2 :End of /WHOIS list",
			contains: []string{"end of whois", "asdf2"},
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
