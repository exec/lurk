package client

import "testing"

func TestCTCPAction(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantText string
		wantOK   bool
	}{
		{"action with text", "\x01ACTION waves\x01", "waves", true},
		{"action no trailing marker", "\x01ACTION waves", "waves", true},
		{"action with spaces", "\x01ACTION slowly waves goodbye\x01", "slowly waves goodbye", true},
		{"plain message", "hello there", "", false},
		{"other ctcp", "\x01VERSION\x01", "", false},
		{"empty", "", "", false},
		{"bare action word", "\x01ACTION\x01", "", false}, // no space after ACTION: not matched
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, ok := CTCPAction(c.body)
			if ok != c.wantOK || text != c.wantText {
				t.Errorf("CTCPAction(%q) = (%q, %v), want (%q, %v)", c.body, text, ok, c.wantText, c.wantOK)
			}
		})
	}
}
