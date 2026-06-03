package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"lurk/client"
	"lurk/irc"
)

// cve_regression_test.go checks Lurk against the classes of crafted-input crashes
// that have historically hit other IRC clients (notably the irssi NULL-deref/OOB
// series), confirming the equivalent inputs are handled without a panic. Go's
// bounds checking rules out the C memory-safety variants, but the *logic* (a
// sourceless TOPIC, an empty nick, a bogus timestamp, more windows than fit the
// terminal) is the same surface, so these are worth pinning as regressions.

// feedAndRender routes one parsed line into the model and renders a frame,
// returning the updated model. A panic in either step fails the calling test.
func feedAndRender(t *testing.T, m model, line string) model {
	t.Helper()
	msg, err := irc.Parse(line)
	if err != nil {
		return m // an unparseable line is simply ignored here
	}
	tm, _ := m.Update(ircMsg{ev: client.Event{Message: msg}})
	m = tm.(model)
	_ = m.View()
	return m
}

// TestCVEClassCraftedInputsDoNotPanic feeds the parsing-logic analogues of known
// IRC-client crash CVEs through the full route+render path.
func TestCVEClassCraftedInputsDoNotPanic(t *testing.T) {
	lines := []string{
		// CVE-2018-5206: a TOPIC change with no sender (no source prefix).
		"TOPIC #c :a new topic with no setter",
		// CVE-2017-10965: a message carrying an invalid/garbage server-time tag.
		"@time=not-a-timestamp :bob!b@h PRIVMSG #c :hi",
		"@time=9999999999999-99-99T99:99:99Z :bob!b@h PRIVMSG #c :hi",
		// CVE-2018-7052 / empty-nick: a sourceless message, and a bare "!@" source.
		"PRIVMSG #c :sourceless message",
		":!@ PRIVMSG #c :degenerate source mask",
		"PRIVMSG :only a trailing param",
		// CVE-2017-9468 class: CTCP-ish bodies with no/weird framing.
		":x!y@z PRIVMSG #c :\x01ACTION\x01",
		":x!y@z PRIVMSG #c :\x01\x01",
		// CVE-2018-5205: incomplete escape sequences in the body.
		":x!y@z PRIVMSG #c :tail\x1b",
		":x!y@z PRIVMSG #c :osc\x1b]52;c;",
		// Sourceless membership/metadata events.
		"JOIN #c",
		"PART #c",
		"QUIT",
		"NICK newname",
		"MODE #c +o",
	}
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	for _, line := range lines {
		m = feedAndRender(t, m, line)
	}
}

// TestRenderDegenerateDimensions is the analogue of CVE-2018-7050 (a crash when
// the number of windows exceeds the available space): it opens many buffers and
// renders at a range of tiny / lopsided terminal sizes, including a channel
// buffer (which adds the nicklist column) at one-cell dimensions.
func TestRenderDegenerateDimensions(t *testing.T) {
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	// Open far more buffers than a tiny terminal can show.
	for i := 0; i < 40; i++ {
		m = feedAndRender(t, m, fmt.Sprintf(":s!s@h PRIVMSG #chan%d :hi", i))
	}

	sizes := []struct{ w, h int }{
		{1, 1}, {2, 2}, {1, 24}, {80, 1}, {3, 3}, {5, 2},
		{18, 1}, {19, 2}, {0, 0}, {sidebarWidth, 1}, {200, 3},
	}
	for _, sz := range sizes {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		m = tm.(model)
		_ = m.View()
		// Also exercise switching to a (channel) buffer so the nicklist column is
		// laid out at the degenerate size, then render again.
		m.switchTo(len(m.buffers) - 1)
		m = layout(m)
		_ = m.View()
	}
}
