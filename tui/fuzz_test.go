package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// FuzzUpdate throws arbitrary inbound IRC events at the TUI update + render path
// (routeEvent -> appendLine -> formatLine/sanitize, plus the width math in
// render/View), asserting it never panics on malformed or hostile server text.
// Explore with `go test -run=x -fuzz=FuzzUpdate ./tui`.
func FuzzUpdate(f *testing.F) {
	seeds := []string{
		":nick!u@h PRIVMSG #c :hello",
		":nick!u@h PRIVMSG me :pm",
		":nick!u@h NOTICE #c :notice",
		":nick!u@h PRIVMSG #c :\x01ACTION waves\x01",
		":nick!u@h JOIN #c",
		":nick!u@h PART #c :bye",
		":nick!u@h QUIT :gone",
		":nick!u@h TOPIC #c :new topic",
		":op!o@h MODE #c +o nick",
		":srv 372 me :motd line",
		":srv FAIL JOIN ACCOUNT_REQUIRED #c :nope",
		"@+typing=active :n TAGMSG #c",
		":evil!e@h PRIVMSG #c :\x1b]52;c;ZXZpbA==\x07\x1b[2Jhi",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		msg, err := irc.Parse(line)
		if err != nil {
			return
		}
		m := newModel(nil, nil) // no live client: routing/formatting must be nil-safe
		// Give it real dimensions so layout/render exercise the width math.
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(model)
		// Feed the event twice (buffer creation then reuse), then render a frame.
		tm, _ = m.Update(ircMsg{ev: client.Event{Message: msg}})
		m = tm.(model)
		tm, _ = m.Update(ircMsg{ev: client.Event{Message: msg}})
		m = tm.(model)
		_ = m.View()
	})
}
