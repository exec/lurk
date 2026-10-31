package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/chatlog"
)

func TestHistoryPagingPastIgnoredMessages(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, timestamp := range []bool{false, true} {
			t.Run(fmt.Sprintf("empty_%v_timestamp_%v", empty, timestamp), func(t *testing.T) {
				m, srv := historyModel(t)
				b, i := m.ensureBuffer("#c", BufferChannel)
				m.switchTo(i)
				m = layout(m)
				m.activeNet().ignored["bob"] = true
				logDir := t.TempDir()
				m.logger = chatlog.New(logDir)
				t.Cleanup(m.logger.Close)
				reply := func(ignored bool, ids ...int) {
					m = routeEvent(m, evt(t, ":s BATCH +h chathistory #c"))
					for _, id := range ids {
						ev := historyEvent(t, id, "h")
						if !ignored {
							ev.Message.Source = "alice!u@h"
						}
						if timestamp {
							delete(ev.Message.Tags, "msgid")
						}
						m = routeEvent(m, ev)
					}
					m = routeEvent(m, evt(t, ":s BATCH -h"))
				}
				ref := func(id int) string {
					if timestamp {
						return fmt.Sprintf("timestamp=2026-10-28T12:00:%02d.000Z", id)
					}
					return fmt.Sprintf("msgid=m%d", id)
				}
				pageUp := func(id int) {
					b.vp.GotoTop()
					updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
					m = updated.(model)
					historyWire(t, srv, "CHATHISTORY BEFORE #c "+ref(id)+" 2")
				}
				// Initial JOIN replay may itself contain nothing but ignored traffic.
				reply(empty, 5, 6)
				pageUp(5)
				reply(true, 3, 4)
				pageUp(3)
				reply(true, 1, 2)
				pageUp(1)
				reply(false, 0)
				if !b.historyEnded || !strings.Contains(bufText(b), "message-0") {
					t.Fatal("paging did not reach the older visible conversation")
				}
				data, err := os.ReadFile(filepath.Join(logDir, "test", "#c.log"))
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []int{1, 2, 3, 4} {
					text := fmt.Sprintf("message-%d", id)
					if strings.Contains(bufText(b), text) || strings.Contains(string(data), text) {
						t.Fatalf("ignored history reached the screen or log: %s", text)
					}
				}
				if m.bell || b.Highlight || b.Unread != 0 {
					t.Fatal("paging hidden history raised activity")
				}
				// A newer automatic replay must not move the paging cursor forward.
				reply(false, 7, 8)
				if got := b.historyRef(false); got != ref(0) {
					t.Fatalf("newer replay moved the older cursor to %q", got)
				}
				cmdClear(m, nil, "")
				if got := b.historyRef(false); got != "" {
					t.Fatalf("clear retained the older cursor: %q", got)
				}
				m.requestOlder(b)
				historyWire(t, srv, "CHATHISTORY LATEST #c * 2")
			})
		}
	}
}
