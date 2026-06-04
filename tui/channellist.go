package tui

import (
	"fmt"
	"sort"
	"strconv"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"lurk/client"
	"lurk/irc"
)

// channellist.go implements the /list channel-directory modal: a filterable,
// scrollable overlay (bubbles/list) drawn centered over the chat frame via a
// lipgloss Compositor. The flow is: /list sends a LIST and opens the modal in a
// loading state (command.go -> actionListOpen -> openChannelList); the RPL_LIST
// replies are collected by the event bridge (routeListReply) and populate the
// list when RPL_LISTEND arrives; Enter on a row joins that channel.

// channelItem is one row in the channel-directory modal. The fields are already
// terminal-sanitized when the item is built (routeListReply), since they are
// server-controlled and the list renders them straight to the screen.
type channelItem struct {
	name  string
	users int
	topic string
}

// Title, Description, and FilterValue satisfy bubbles/list.DefaultItem. The
// filter matches on the channel name and topic so users can find a channel by
// either.
func (c channelItem) Title() string {
	return fmt.Sprintf("%s  ·  %d users", c.name, c.users)
}
func (c channelItem) Description() string { return c.topic }
func (c channelItem) FilterValue() string { return c.name + " " + c.topic }

// channelListSize returns the inner size (width, height) for the list widget:
// roughly two-thirds of the terminal, clamped to sensible bounds and never
// larger than the available space.
func channelListSize(m model) (w, h int) {
	w = min(max(m.width*2/3, 28), 76)
	w = min(w, m.width-6)
	h = min(max(m.height*2/3, 6), 26)
	h = min(h, m.height-4)
	return w, h
}

// openChannelList resets and opens the channel-list modal, entering the loading
// state until RPL_LISTEND arrives.
func openChannelList(m model) model {
	w, h := channelListSize(m)

	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)

	l := list.New(nil, delegate, w, h)
	l.Title = "Channels"
	l.SetStatusBarItemName("channel", "channels")
	l.SetShowHelp(true)

	m.chanList = l
	m.chanListAccum = nil
	m.chanListLoading = true
	m.chanListOpen = true
	return m
}

// closeChannelList dismisses the modal and stops any in-flight accumulation.
func closeChannelList(m model) model {
	m.chanListOpen = false
	m.chanListLoading = false
	m.chanListAccum = nil
	return m
}

// handleChannelListKey processes a key while the channel-list modal is open. Esc
// closes it (after first letting the list clear an active filter); Enter on a
// row joins that channel; everything else (navigation, filtering via "/") is
// forwarded to the list.
func (m model) handleChannelListKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Only close when there is no filter for the list to clear first.
		if m.chanList.FilterState() == list.Unfiltered {
			// Re-arm the editor caret blink the overlay had taken over.
			return closeChannelList(m), textinput.Blink
		}
	case "enter":
		// Enter while typing a filter applies it (handled by the list); otherwise
		// it selects the highlighted channel and joins it.
		if m.chanList.FilterState() != list.Filtering {
			if it, ok := m.chanList.SelectedItem().(channelItem); ok {
				m = closeChannelList(m)
				if m.cli != nil {
					_ = m.cli.Join(it.name)
				}
				_, i := m.ensureBuffer(it.name, BufferChannel)
				m.switchTo(i)
				m = layout(m)
				return m, textinput.Blink
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.chanList, cmd = m.chanList.Update(msg)
	return m, cmd
}

// routeListReply collects the LIST reply numerics into the modal. When the modal
// is not driving the request (e.g. a raw LIST), it falls back to rendering the
// reply in the active buffer so the data is not lost.
func routeListReply(m model, ev client.Event) model {
	if !m.chanListLoading {
		return appendLine(m, m.activeBuffer(), ev)
	}
	switch ev.Command() {
	case irc.RPL_LIST:
		users, _ := strconv.Atoi(ev.Param(2))
		m.chanListAccum = append(m.chanListAccum, channelItem{
			name:  sanitize(ev.Param(1)),
			users: users,
			topic: sanitize(ev.Text()),
		})
	case irc.RPL_LISTEND:
		m.chanListLoading = false
		items := m.chanListAccum
		// Most-populated channels first — the useful default for browsing.
		sort.SliceStable(items, func(i, j int) bool {
			return items[i].(channelItem).users > items[j].(channelItem).users
		})
		m.chanList.SetItems(items)
		m.chanListAccum = nil
	}
	// rplListStart carries only a header; nothing to collect.
	return m
}

// renderChannelListModal renders the modal box (border + the list, or a loading
// line while the directory is still arriving).
func renderChannelListModal(m model) string {
	t := defaultTheme
	w, _ := channelListSize(m)

	var body string
	if m.chanListLoading {
		body = m.chanList.Styles.Title.Render("Channels") + "\n\n" +
			t.dim.Render(fmt.Sprintf("loading… (%d so far)", len(m.chanListAccum))) + "\n\n" +
			t.dim.Render("esc cancel")
	} else {
		body = m.chanList.View()
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.self).
		Padding(0, 1).
		Width(w)
	return box.Render(body)
}

// overlayChannelList composites the channel-list modal centered over the base
// frame using a lipgloss Compositor (v2's overlay primitive).
func overlayChannelList(m model, base string) string {
	modal := renderChannelListModal(m)
	x := (m.width - lipgloss.Width(modal)) / 2
	y := (m.height - lipgloss.Height(modal)) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	baseLayer := lipgloss.NewLayer(base)
	modalLayer := lipgloss.NewLayer(modal).X(x).Y(y).Z(1)
	return lipgloss.NewCompositor(baseLayer, modalLayer).Render()
}
