package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
)

func TestMenuSendFailurePreservesDraft(t *testing.T) {
	for _, entry := range []menuEntry{
		{label: "Whois", act: miWhois},
		{label: "Op", act: miSetMode, mode: "+o"},
		{label: "Kick", act: miKick},
		{label: "Ban", act: miBan},
	} {
		t.Run(entry.label, func(t *testing.T) {
			m := newModel(client.New(client.Config{Nick: "me"}), nil)
			b, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.menuOpen, m.menuNick = true, "bob"
			m.input.SetValue("unfinished draft")
			m.input.SetCursor(5)
			updated, _ := m.applyMenu(entry)
			m = updated.(model)
			if m.menuOpen || m.focus != focusInput || m.activeBuffer() != b {
				t.Fatal("failed menu action did not return to the conversation editor")
			}
			if m.input.Value() != "unfinished draft" || m.input.Position() != 5 {
				t.Fatal("failed menu action changed the draft or cursor")
			}
			if len(b.lines) != 1 || !strings.Contains(stripANSI(b.lines[0]), "not connected") {
				t.Fatalf("failed menu action did not report its error: %q", b.lines)
			}
		})
	}
}

func TestMenuProtocolRequestsSendSuccessfully(t *testing.T) {
	for _, tc := range []struct {
		entry menuEntry
		wire  string
	}{
		{menuEntry{label: "Whois", act: miWhois}, "WHOIS bob"},
		{menuEntry{label: "Op", act: miSetMode, mode: "+o"}, "MODE #go +o bob"},
		{menuEntry{label: "Kick", act: miKick}, "KICK #go bob"},
		{menuEntry{label: "Ban", act: miBan}, "MODE #go +b bob!*@*"},
	} {
		t.Run(tc.entry.label, func(t *testing.T) {
			m, srv := historyModel(t)
			b, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.menuOpen, m.menuNick = true, "bob"
			m.input.SetValue("unfinished draft")
			updated, _ := m.applyMenu(tc.entry)
			m = updated.(model)
			historyWire(t, srv, tc.wire)
			if m.menuOpen || m.focus != focusInput || m.input.Value() != "unfinished draft" || len(b.lines) != 0 {
				t.Fatal("successful menu action changed the draft, added an error, or left the menu open")
			}
		})
	}
}

// newServerBuffer-backed model with no client: activeBuffer is the server
// buffer (not a channel), so menuEntries should offer only the base actions and
// no operator items (which require a channel + ops).
func baseMenuModel() model {
	return model{
		buffers:  []*Buffer{newServerBuffer("test")},
		menuOpen: true,
		menuNick: "alice",
		input:    newInput(),
	}
}

func TestMenuEntriesBaseActions(t *testing.T) {
	m := baseMenuModel()
	got := menuEntries(m)
	if len(got) != 3 {
		t.Fatalf("want 3 base entries (no channel/ops), got %d: %+v", len(got), got)
	}
	want := []menuActionID{miMessage, miWhois, miInsertNick}
	for i, e := range got {
		if e.act != want[i] {
			t.Errorf("entry %d = %v, want %v", i, e.act, want[i])
		}
	}
}

func TestMenuKeyNavigationAndClose(t *testing.T) {
	m := baseMenuModel()

	// Down then up returns to the top; bounds are respected.
	nm, _ := m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = nm.(model)
	if m.menuSel != 1 {
		t.Fatalf("after down, menuSel=%d want 1", m.menuSel)
	}
	nm, _ = m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = nm.(model)
	if m.menuSel != 0 {
		t.Fatalf("after up, menuSel=%d want 0", m.menuSel)
	}

	// Esc closes the menu and returns focus to the editor.
	nm, _ = m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(model)
	if m.menuOpen || m.focus != focusInput {
		t.Fatalf("after esc: menuOpen=%v focus=%v, want closed+input", m.menuOpen, m.focus)
	}
}

func TestApplyMenuInsertNick(t *testing.T) {
	m := baseMenuModel()
	nm, _ := m.applyMenu(menuEntry{label: "Insert nick", act: miInsertNick})
	m = nm.(model)
	if got := m.input.Value(); got != "alice: " {
		t.Errorf("insert nick into empty editor = %q, want %q", got, "alice: ")
	}
	if m.menuOpen || m.focus != focusInput {
		t.Errorf("applyMenu should close the menu and refocus input")
	}
}

func TestModeName(t *testing.T) {
	cases := map[byte]string{'q': "Founder", 'a': "Admin", 'o': "Op", 'h': "Half-op", 'v': "Voice", 'x': "+x"}
	for in, want := range cases {
		if got := modeName(in); got != want {
			t.Errorf("modeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHighestPrefixIndex(t *testing.T) {
	const symbols = "~&@%+" // founder, admin, op, half-op, voice
	cases := []struct {
		have string
		want int
	}{
		{"", 5},   // hold nothing → past the end
		{"+", 4},  // voice
		{"@", 2},  // op
		{"@+", 2}, // op is the highest held
		{"~@", 0}, // founder
		{"%+", 3}, // half-op
	}
	for _, c := range cases {
		if got := highestPrefixIndex(c.have, symbols); got != c.want {
			t.Errorf("highestPrefixIndex(%q) = %d, want %d", c.have, got, c.want)
		}
	}
}

// TestStatusSubmenuBackOut drives the sub-menu state machine: the "Status…" level
// always ends with Kick + Ban, Esc backs out to the top menu (not closed), and a
// second Esc closes it.
func TestStatusSubmenuBackOut(t *testing.T) {
	m := baseMenuModel()
	m.menuStatus = true
	m.menuSel = 1

	got := currentMenuEntries(m)
	if n := len(got); n < 2 || got[n-2].act != miKick || got[n-1].act != miBan {
		t.Fatalf("status sub-menu should end with Kick, Ban; got %+v", got)
	}

	nm, _ := m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(model)
	if m.menuStatus || !m.menuOpen {
		t.Fatalf("esc in sub-menu should return to the top menu, got menuStatus=%v menuOpen=%v", m.menuStatus, m.menuOpen)
	}

	nm, _ = m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(model)
	if m.menuOpen {
		t.Fatalf("a second esc should close the menu")
	}
}

func TestBanMaskFallback(t *testing.T) {
	m := baseMenuModel() // nil client → member host unknown
	if got := banMask(m, "bob"); got != "bob!*@*" {
		t.Errorf("banMask fallback = %q, want %q", got, "bob!*@*")
	}
}
