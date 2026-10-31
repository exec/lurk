package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDraftsStayWithBuffers(t *testing.T) {
	m, netA, netB := twoNetModel(t)
	_, a := m.ensureBufferIn(netA, "#same", BufferChannel)
	_, b := m.ensureBufferIn(netB, "#same", BufferChannel)
	m.switchTo(a)
	m.input.SetValue("draft for α")
	m.input.SetCursor(4)
	tm, _ := m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	m = tm.(model)
	if m.input.Value() != "" {
		t.Fatal("a fresh buffer inherited another conversation's draft")
	}
	m.switchTo(b)
	m.input.SetValue("draft for β")
	m.input.SetCursor(7)
	m.switchTo(a)
	if m.input.Value() != "draft for α" || m.input.Position() != 4 {
		t.Error("first network's draft or cursor was not restored")
	}
	m.switchTo(a)
	if m.input.Value() != "draft for α" || m.input.Position() != 4 {
		t.Error("focusing the active buffer changed its input")
	}
	m.switchTo(b)
	if m.input.Value() != "draft for β" || m.input.Position() != 7 {
		t.Error("second network's draft or cursor was not restored")
	}
}

func TestHistoryRestoresDraft(t *testing.T) {
	m := newTestModel()
	m = pushHistory(m, "older")
	m = pushHistory(m, "newer")
	m.input.SetValue("unfinished draft")
	m.input.SetCursor(5)
	for _, key := range []rune{tea.KeyUp, tea.KeyUp, tea.KeyDown, tea.KeyDown, tea.KeyDown} {
		tm, _ := m.Update(tea.KeyPressMsg{Code: key})
		m = tm.(model)
	}
	if m.input.Value() != "unfinished draft" || m.input.Position() != 5 {
		t.Error("history navigation did not restore the draft and cursor")
	}
}

func TestHistoryDraftSurvivesBufferSwitch(t *testing.T) {
	m := newTestModel()
	_, a := m.ensureBuffer("#a", BufferChannel)
	_, b := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(a)
	m = pushHistory(m, "older")
	m.input.SetValue("draft a")
	m.input.SetCursor(2)
	m = historyPrev(m)
	m.switchTo(b)
	m.input.SetValue("draft b")
	m = pushHistory(m, "newer")
	m.switchTo(a)
	if m.input.Value() != "older" {
		t.Fatal("buffer switch lost the recalled history entry")
	}
	m = historyNext(m)
	if m.input.Value() != "newer" {
		t.Fatal("history recall lost its position when another buffer added an entry")
	}
	m = historyNext(m)
	if m.input.Value() != "draft a" || m.input.Position() != 2 {
		t.Error("history recall lost the original draft across a buffer switch")
	}
}

func TestDraftAfterBufferClose(t *testing.T) {
	m := newTestModel()
	_, a := m.ensureBuffer("#a", BufferChannel)
	_, b := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(a)
	m.input.SetValue("draft a")
	m.switchTo(b)
	m.input.SetValue("draft b")
	m.closeBuffer(b)
	if m.input.Value() != "draft a" {
		t.Fatal("closing the active buffer did not restore the next buffer's draft")
	}
	_, b = m.ensureBuffer("#b", BufferChannel)
	m.switchTo(b)
	m.input.SetValue("draft b")
	m.closeBuffer(a)
	if m.input.Value() != "draft b" {
		t.Fatal("closing a background buffer changed the active draft")
	}
	m.closeBuffer(m.active)
	if m.input.Value() != "" {
		t.Fatal("closing the active buffer left its draft on the server buffer")
	}
	_, a = m.ensureBuffer("#a", BufferChannel)
	m.switchTo(a)
	if m.input.Value() != "" {
		t.Error("a reopened buffer inherited the closed buffer's draft")
	}
}

func TestFailedSendDraftSurvivesSwitch(t *testing.T) {
	m, netA, _ := twoNetModel(t)
	_, a := m.ensureBufferIn(netA, "#a", BufferChannel)
	_, b := m.ensureBufferIn(netA, "#b", BufferChannel)
	m.switchTo(a)
	m.input.SetValue("/msg bob retry this")
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(model)
	m.switchTo(b)
	if m.input.Value() != "" {
		t.Error("failed send leaked into another buffer")
	}
	m.switchTo(a)
	if m.input.Value() != "/msg bob retry this" {
		t.Error("switching buffers discarded a failed send")
	}
}

func TestDraftAfterNetworkRemoval(t *testing.T) {
	for _, removeActive := range []bool{false, true} {
		t.Run(map[bool]string{false: "background", true: "active"}[removeActive], func(t *testing.T) {
			m, netA, netB := twoNetModel(t)
			m.input.SetValue("draft a")
			m.input.SetCursor(3)
			m.switchTo(m.bufferIndexIn(netB, "oftc"))
			m.input.SetValue("draft b")
			if removeActive {
				m = m.removeNetwork(netB)
				if m.input.Value() != "draft a" || m.input.Position() != 3 {
					t.Error("removing the active network did not restore the survivor's draft")
				}
			} else {
				m = m.removeNetwork(netA)
				if m.input.Value() != "draft b" {
					t.Error("removing a background network changed the active draft")
				}
			}
		})
	}
}

func TestSentDraftDoesNotReturn(t *testing.T) {
	cli, _ := connectedClient(t, "me")
	t.Cleanup(func() { _ = cli.Close() })
	m := newModel(cli, nil)
	_, a := m.ensureBuffer("#a", BufferChannel)
	_, b := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(a)
	m.input.SetValue("send me")
	m.switchTo(b)
	m.switchTo(a)
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(model)
	m.switchTo(b)
	m.switchTo(a)
	if m.input.Value() != "" {
		t.Error("switching buffers resurrected a sent draft")
	}
}
