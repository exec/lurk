package tui

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
)

// TestLiveHistoryPaging drives the TUI's real history requests and merges against
// the opt-in loopback Ergo provisioned by scripts/live-test.sh.
func TestLiveHistoryPaging(t *testing.T) {
	addr := os.Getenv("LURK_TEST_SERVER")
	if addr == "" {
		t.Skip("LURK_TEST_SERVER not set; skipping live history test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialed := make(chan net.Conn, 2)
	reader := client.New(client.Config{Server: addr, Nick: "histreader", AutoReconnect: true,
		Dialer: func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			raw, err := dialer.DialContext(ctx, "tcp", addr)
			if err == nil {
				dialed <- raw
			}
			return raw, err
		}})
	writer := client.New(client.Config{Server: addr, Nick: "histwriter"})
	reader.Events()
	writer.Events()
	defer reader.Close()
	defer writer.Close()
	if err := reader.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	firstConn := <-dialed
	if err := writer.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if !reader.CapEnabled("draft/chathistory") {
		t.Fatal("Ergo did not negotiate history")
	}
	channel := "#history-" + time.Now().Format("150405000")
	if err := reader.Join(channel); err != nil {
		t.Fatal(err)
	}
	if err := writer.Join(channel); err != nil {
		t.Fatal(err)
	}
	m := newModel(reader, reader.Events())
	m.width, m.height, m.ready = 100, 14, true
	m = layout(m)
	pump := func(done func(model) bool) {
		for !done(m) {
			select {
			case ev := <-reader.Events():
				m = routeEvent(m, ev)
			case <-ctx.Done():
				t.Fatalf("live history timed out; buffers=%v", m.buffers)
			}
		}
	}
	pump(func(m model) bool {
		b := m.buffer(channel)
		return b != nil && b.historyLoaded && len(reader.Members(channel)) == 2
	})
	for i := 1; i <= 6; i++ {
		if err := writer.Privmsg(channel, fmt.Sprintf("live-history-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var last client.Event
	for last.Text() != "live-history-06" {
		select {
		case last = <-reader.Events():
		case <-ctx.Done():
			t.Fatal("live messages did not arrive")
		}
	}
	// Keep only the newest live row, then overlap it with a recent-history page.
	_, i := m.ensureBuffer(channel, BufferChannel)
	m.switchTo(i)
	m = layout(m)
	cmdClear(m, nil, "")
	m = routeEvent(m, last)
	m = m.queueHistory(m.activeNet(), historyQuery{sub: "LATEST", target: channel, budget: 2})
	pump(func(m model) bool { return m.activeNet().history.pending == nil })
	b := m.activeBuffer()
	if strings.Count(bufText(b), "live-history-06") != 1 || !strings.Contains(bufText(b), "live-history-05") {
		t.Fatalf("recent history was missing or duplicated: %s", bufText(b))
	}
	b.vp.GotoTop()
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = tm.(model)
	if m.activeNet().history.pending == nil {
		t.Fatal("PageUp did not request older history")
	}
	pump(func(m model) bool { return m.activeNet().history.pending == nil })
	text := bufText(b)
	previous := -1
	for i := 1; i <= 6; i++ {
		needle := fmt.Sprintf("live-history-%02d", i)
		at := strings.Index(text, needle)
		if at <= previous || strings.Count(text, needle) != 1 {
			t.Fatalf("paged live history missing, duplicated, or out of order: %s", text)
		}
		previous = at
	}
	// A history replay must not masquerade as a new incoming message.
	if m.bell || b.Highlight || b.Unread != 0 {
		t.Fatal("live history raised a notification")
	}
	if err := writer.Privmsg(reader.Nick(), "live-PM-recovery"); err != nil {
		t.Fatal(err)
	}
	for received := false; !received; {
		select {
		case ev := <-reader.Events():
			received = ev.Text() == "live-PM-recovery"
		case <-ctx.Done():
			t.Fatal("PM did not arrive")
		}
	}
	m = m.discoverHistory(m.activeNet())
	pump(func(m model) bool { return m.activeNet().history.pending == nil })
	pm := m.buffer(writer.Nick())
	if pm == nil || strings.Count(bufText(pm), "live-PM-recovery") != 1 {
		t.Fatal("TARGETS did not recover the missed PM")
	}

	if err := firstConn.Close(); err != nil {
		t.Fatal(err)
	}
	pump(func(m model) bool { return m.activeNet().history.offline })
	if !strings.Contains(stripANSI(renderStatus(m)), "reconnecting") {
		t.Fatal("socket drop did not show persistent reconnect status")
	}
	for i := 7; i <= 12; i++ {
		if err := writer.Privmsg(channel, fmt.Sprintf("live-history-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	pump(func(m model) bool {
		return !m.activeNet().history.offline && m.activeNet().history.pending == nil &&
			b.historyLoaded && strings.Contains(bufText(b), "live-history-12")
	})
	text = bufText(b)
	if strings.Contains(stripANSI(renderStatus(m)), "reconnecting") {
		t.Fatal("reconnect status did not clear after recovery")
	}
	previous = -1
	for i := 1; i <= 12; i++ {
		needle := fmt.Sprintf("live-history-%02d", i)
		at := strings.Index(text, needle)
		if at <= previous || strings.Count(text, needle) != 1 {
			t.Fatalf("reconnect history missing, duplicated, or out of order: %s", text)
		}
		previous = at
	}
	t.Log("Ergo: PageUp, live/history deduplication, missed-PM discovery, and socket-drop catch-up passed")
}
