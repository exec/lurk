package tui

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/chatlog"
	"github.com/exec/lurk/client"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

func historyModel(t *testing.T) (model, *conn.Conn) {
	t.Helper()
	a, b := net.Pipe()
	srv := conn.NewConn(b, conn.Options{})
	c := client.New(client.Config{Nick: "me", Caps: []string{"draft/chathistory", "batch", "server-time", "echo-message"}})
	events := c.Events()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = c.Close(); _ = srv.Close() })
	go func() {
		for range 3 {
			<-srv.Messages()
		}
		msg, _ := irc.Parse("CAP * LS :draft/chathistory batch server-time echo-message")
		_ = srv.WriteMessage(msg)
		<-srv.Messages()
		msg, _ = irc.Parse("CAP * ACK :draft/chathistory batch server-time echo-message")
		_ = srv.WriteMessage(msg)
		<-srv.Messages()
		for _, raw := range []string{":s 001 me :welcome", ":s 005 me CHATHISTORY=2 MSGREFTYPES=msgid,timestamp :supported", ":s 376 me :end"} {
			msg, _ = irc.Parse(raw)
			_ = srv.WriteMessage(msg)
		}
	}()
	if err := c.ConnectConn(ctx, conn.NewConn(a, conn.Options{})); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-events:
			if ev.Command() == "376" {
				m := newModel(c, events)
				m.networks[0].name = "test"
				m.width, m.height, m.ready = 80, 12, true
				return layout(m), srv
			}
		case <-ctx.Done():
			t.Fatal("registration timed out")
		}
	}
}

func historyWire(t *testing.T, srv *conn.Conn, want string) {
	t.Helper()
	select {
	case msg := <-srv.Messages():
		if msg == nil {
			t.Fatal("server closed")
		}
		line, err := msg.Serialize()
		line = strings.TrimRight(line, "\r\n")
		if err != nil {
			t.Fatal(err)
		}
		if want != "" && line != want {
			t.Fatalf("wire = %q, want %q", line, want)
		}
		if want == "" && (msg.Command != irc.CHATHISTORY || msg.Param(0) != "TARGETS") {
			t.Fatalf("expected TARGETS, got %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func historyEvent(t *testing.T, id int, batch string) client.Event {
	t.Helper()
	tag := ""
	if batch != "" {
		tag = ";batch=" + batch
	}
	return evt(t, fmt.Sprintf("@msgid=m%d;time=2026-10-28T12:00:%02dZ%s :bob!u@h PRIVMSG #c :message-%d", id, id, tag, id))
}

func historyReply(t *testing.T, m model, target string, ids ...int) model {
	t.Helper()
	m = routeEvent(m, evt(t, ":s BATCH +h chathistory "+target))
	for _, id := range ids {
		m = routeEvent(m, historyEvent(t, id, "h"))
	}
	return routeEvent(m, evt(t, ":s BATCH -h"))
}

func TestHistoryPagingMergesAndDeduplicates(t *testing.T) {
	m, srv := historyModel(t)
	_, i := m.ensureBuffer("#c", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	logDir := t.TempDir()
	m.logger = chatlog.New(logDir)
	t.Cleanup(m.logger.Close)
	m = routeEvent(m, historyEvent(t, 3, ""))
	m = routeEvent(m, historyEvent(t, 4, ""))
	m = historyReply(t, m, "#c", 2, 3)
	b := m.activeBuffer()
	if strings.Count(bufText(b), "message-3") != 1 {
		t.Fatal("live/history overlap was duplicated")
	}
	runSearch(&m, "message-3")
	b.vp.GotoTop()
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = tm.(model)
	historyWire(t, srv, "CHATHISTORY BEFORE #c msgid=m2 2")
	m = historyReply(t, m, "#c", 0, 1)
	text := bufText(b)
	prev := -1
	for id := range 5 {
		at := strings.Index(text, fmt.Sprintf("message-%d", id))
		if at <= prev {
			t.Fatalf("history out of order: %s", text)
		}
		prev = at
	}
	if len(m.searchMatches) != 1 || !strings.Contains(b.lines[m.searchMatches[0]], "message-3") {
		t.Fatal("history insertion broke search indices")
	}
	if m.bell || b.Highlight || b.Unread != 0 {
		t.Fatal("history raised activity")
	}
	data, err := os.ReadFile(filepath.Join(logDir, "test", "#c.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "message-3") != 1 {
		t.Fatal("duplicate history reached the log")
	}
	b.vp.GotoTop()
	m.requestOlder(b)
	historyWire(t, srv, "CHATHISTORY BEFORE #c msgid=m0 2")
	m = historyReply(t, m, "#c")
	if !b.historyEnded {
		t.Fatal("empty page did not mark history exhausted")
	}
	m.requestOlder(b)
	if m.activeNet().history.pending != nil {
		t.Fatal("exhausted history requested again")
	}
}

func TestHistoryReconnectFillsMoreThanOnePage(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			m, srv := historyModel(t)
			_, i := m.ensureBuffer("#c", BufferChannel)
			m.switchTo(i)
			m = routeEvent(m, historyEvent(t, 1, ""))
			m = routeEvent(m, client.Event{Message: &irc.Message{Command: client.EventReconnecting, Params: []string{"lost"}}})
			if early {
				m = historyReply(t, m, "#c", 4, 5)
			}
			m = routeEvent(m, client.Event{Message: &irc.Message{Command: client.EventReconnected, Params: []string{"back"}}})
			historyWire(t, srv, "")
			m = routeEvent(m, evt(t, ":s BATCH +t draft/chathistory-targets"))
			m = routeEvent(m, evt(t, ":s BATCH -t"))
			if !early {
				m = historyReply(t, m, "#c", 4, 5)
			}
			historyWire(t, srv, "CHATHISTORY AFTER #c msgid=m1 2")
			m = historyReply(t, m, "#c", 2, 3)
			historyWire(t, srv, "CHATHISTORY AFTER #c msgid=m3 2")
			m = historyReply(t, m, "#c", 4, 5)
			historyWire(t, srv, "CHATHISTORY AFTER #c msgid=m5 2")
			m = historyReply(t, m, "#c")
			text := bufText(m.activeBuffer())
			prev := -1
			for id := 1; id <= 5; id++ {
				needle := fmt.Sprintf("message-%d", id)
				at := strings.Index(text, needle)
				if at <= prev || strings.Count(text, needle) != 1 {
					t.Fatalf("catch-up missing, duplicated, or out of order: %s", text)
				}
				prev = at
			}
			if m.activeNet().history.pending != nil {
				t.Fatal("catch-up did not finish")
			}
		})
	}
}

func TestHistoryDiscoversPMs(t *testing.T) {
	m, srv := historyModel(t)
	m = m.discoverHistory(m.activeNet())
	historyWire(t, srv, "")
	m = routeEvent(m, evt(t, ":s BATCH +t draft/chathistory-targets"))
	m = routeEvent(m, evt(t, "@batch=t :s CHATHISTORY TARGETS bob 2026-10-28T12:00:01Z"))
	m = routeEvent(m, evt(t, ":s BATCH -t"))
	historyWire(t, srv, "CHATHISTORY LATEST bob * 2")
	m = routeEvent(m, evt(t, ":s BATCH +p chathistory bob"))
	m = routeEvent(m, evt(t, "@batch=p;msgid=pm;time=2026-10-28T12:00:01Z :bob!u@h PRIVMSG oldnick :missed PM"))
	m = routeEvent(m, evt(t, ":s BATCH -p"))
	b := m.buffer("bob")
	if b == nil || !strings.Contains(bufText(b), "missed PM") || m.buffer("oldnick") != nil {
		t.Fatal("PM history did not use the batch's conversation target")
	}
	if b.Unread != 0 || b.Highlight || m.bell {
		t.Fatal("recovered PM rang or raised false unread activity")
	}
}

func TestHistoryFailuresReleaseQueue(t *testing.T) {
	m, srv := historyModel(t)
	net := m.activeNet()
	m = m.queueHistory(net, historyQuery{sub: "BEFORE", target: "#a", ref: "msgid=old", budget: 2})
	historyWire(t, srv, "CHATHISTORY BEFORE #a msgid=old 2")
	m = m.queueHistory(net, historyQuery{sub: "LATEST", target: "bob", budget: 2})
	m = routeEvent(m, evt(t, ":s FAIL CHATHISTORY INVALID_TARGET BEFORE #a :no access"))
	historyWire(t, srv, "CHATHISTORY LATEST bob * 2")
	net.history.pending.deadline = time.Now().Add(-time.Second)
	m = m.expireHistory()
	if net.history.pending != nil {
		t.Fatal("timeout left a request pending")
	}
	m = m.queueHistory(net, historyQuery{sub: "LATEST", target: "bob", budget: 2})
	historyWire(t, srv, "CHATHISTORY LATEST bob * 2")
	m = historyReply(t, m, "bob")
	if net.history.pending != nil {
		t.Fatal("request could not finish after a timeout")
	}
}

func TestHistoryMetadataIsBoundedAndClearable(t *testing.T) {
	m := newTestModel()
	b, i := m.ensureBuffer("#c", BufferChannel)
	m.switchTo(i)
	for n := range scrollbackLimit + 5 {
		b.addLine("row", historyLine{id: strconv.Itoa(n), at: time.Unix(int64(n+1), 0)})
	}
	if len(b.lineMeta) != scrollbackLimit || len(b.seenIDs) != scrollbackLimit || b.seenIDs["0"] {
		t.Fatal("message metadata grew beyond scrollback")
	}
	cmdClear(m, nil, "")
	if len(b.lineMeta) != 0 || len(b.seenIDs) != 0 || b.historyRef(false) != "" {
		t.Fatal("clear left stale history references")
	}
}

func TestHistoryIsolatedAcrossNetworks(t *testing.T) {
	m, a, b := twoNetModel(t)
	_, ia := m.ensureBufferIn(a, "#c", BufferChannel)
	_, ib := m.ensureBufferIn(b, "#c", BufferChannel)
	m.switchTo(ib)
	m = routeEventOn(m, b, historyEvent(t, 3, ""))
	m = routeEventOn(m, a, evt(t, ":s BATCH +h chathistory #c"))
	m = routeEventOn(m, a, historyEvent(t, 3, "h"))
	m = routeEventOn(m, a, evt(t, ":s BATCH -h"))
	for _, i := range []int{ia, ib} {
		if strings.Count(bufText(m.buffers[i]), "message-3") != 1 {
			t.Fatal("history crossed networks or deduplicated across networks")
		}
	}
}

func TestHistoryCancelledReplyIsDiscarded(t *testing.T) {
	for _, closeBuffer := range []bool{false, true} {
		t.Run(fmt.Sprint(closeBuffer), func(t *testing.T) {
			m, srv := historyModel(t)
			_, i := m.ensureBuffer("#c", BufferChannel)
			m.switchTo(i)
			m = m.queueHistory(m.activeNet(), historyQuery{sub: "LATEST", target: "#c", budget: 2})
			historyWire(t, srv, "CHATHISTORY LATEST #c * 2")
			m = routeEvent(m, evt(t, ":s BATCH +h chathistory #c"))
			if closeBuffer {
				m.closeBuffer(i)
			} else {
				cmdClear(m, nil, "")
			}
			m = routeEvent(m, historyEvent(t, 1, "h"))
			m = routeEvent(m, evt(t, ":s BATCH -h"))
			if b := m.buffer("#c"); b != nil && (closeBuffer || len(b.lines) != 0 || b.historyRef(false) != "") {
				t.Fatal("late history resurrected a closed or cleared buffer")
			}
			if m.activeNet().history.pending != nil {
				t.Fatal("cancelled request remained pending")
			}
		})
	}
}

func TestHistoryNonAdvancingPageStops(t *testing.T) {
	m, srv := historyModel(t)
	m.ensureBuffer("#c", BufferChannel)
	m = m.queueHistory(m.activeNet(), historyQuery{sub: "AFTER", target: "#c", ref: "msgid=m1", budget: 100})
	historyWire(t, srv, "CHATHISTORY AFTER #c msgid=m1 2")
	m = historyReply(t, m, "#c", 1, 1)
	if m.activeNet().history.pending != nil {
		t.Fatal("non-advancing history looped")
	}
}

func TestHistoryTimestampReference(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 123456789, time.UTC)
	if ref := historyRef(historyLine{at: at}, nil); ref != "timestamp=2026-10-28T12:00:00.123Z" {
		t.Fatalf("timestamp ref = %q", ref)
	}
	if ref := historyRef(historyLine{id: "id", at: at}, nil); ref != "msgid=id" {
		t.Fatalf("message ID ref = %q", ref)
	}
}
