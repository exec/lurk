package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"lurk/client"
)

func TestNickRowAccountBadge(t *testing.T) {
	tm := newTheme()
	row := tm.nickRow(client.Member{Nick: "alice", Account: "alice"}, 20, false, false)
	if !strings.Contains(stripANSI(row), "alice·") {
		t.Errorf("logged-in row = %q, want trailing badge", stripANSI(row))
	}

	plain := tm.nickRow(client.Member{Nick: "bob"}, 20, false, false)
	if strings.Contains(stripANSI(plain), "·") {
		t.Errorf("not-logged-in row = %q, should have no badge", stripANSI(plain))
	}
}

func TestNickRowAwayDimmed(t *testing.T) {
	tm := newTheme()
	away := tm.nickRow(client.Member{Nick: "alice", Away: true}, 20, false, false)
	here := tm.nickRow(client.Member{Nick: "alice"}, 20, false, false)
	// The text is identical; only the styling differs (away is faint).
	if stripANSI(away) != "alice" || stripANSI(here) != "alice" {
		t.Fatalf("unexpected text: away=%q here=%q", stripANSI(away), stripANSI(here))
	}
	if away == here {
		t.Error("away row should be styled differently from a present member")
	}
}

func TestNickRowFitsWidthWithBadge(t *testing.T) {
	tm := newTheme()
	// A nick longer than the column, with a badge, must still fit in w cells.
	row := tm.nickRow(client.Member{Nick: "verylongnickname", Account: "x"}, 8, false, false)
	if w := lipgloss.Width(stripANSI(row)); w > 8 {
		t.Errorf("row width = %d, want <= 8 (%q)", w, stripANSI(row))
	}
}

func TestNickRowSelectedNoBadgeStyling(t *testing.T) {
	tm := newTheme()
	// A selected away+account member still renders its label (badge included)
	// without panicking; the reverse bar carries the whole text.
	row := tm.nickRow(client.Member{Nick: "alice", Account: "x", Away: true, Prefixes: "@"}, 20, true, false)
	if got := stripANSI(row); !strings.Contains(got, "@alice·") {
		t.Errorf("selected row = %q, want @alice· label", got)
	}
}
