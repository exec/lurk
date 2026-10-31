package tui

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

const historyPageSize = 50
const historyTimeout = 15 * time.Second

type historyLine struct {
	id string
	at time.Time
}

type historyQuery struct {
	sub, target, ref string
	from             time.Time
	limit, budget    int
	deadline         time.Time
}

type historyBatch struct {
	target                  string
	query                   *historyQuery
	events                  []client.Event
	count                   int
	oldest                  time.Time
	end, truncated, discard bool
	deadline                time.Time
}

type historyState struct {
	queue               []historyQuery
	pending             *historyQuery
	batches             map[string]*historyBatch
	discovered, offline bool
}

type historyStartMsg struct{ net *network }
type historyTickMsg struct{}

func (b *Buffer) alignHistory() {
	if len(b.lineMeta) != len(b.lines) {
		b.lineMeta = make([]historyLine, len(b.lines))
		b.seenIDs = nil
	}
}

func historyRef(line historyLine, cli *client.Client) string {
	types := "msgid,timestamp"
	if cli != nil {
		if advertised, ok := cli.ISupport("MSGREFTYPES"); ok {
			types = advertised
		}
	}
	for _, kind := range strings.Split(types, ",") {
		if kind == "msgid" && line.id != "" {
			return "msgid=" + line.id
		}
		if kind == "timestamp" && !line.at.IsZero() {
			return "timestamp=" + line.at.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return ""
}

func (b *Buffer) historyRef(newest bool) string {
	var cli *client.Client
	if b.net != nil {
		cli = b.net.cli
	}
	if !newest {
		if ref := historyRef(b.historyBefore, cli); ref != "" {
			return ref
		}
	}
	for n := range b.lineMeta {
		i := n
		if newest {
			i = len(b.lineMeta) - 1 - n
		}
		if ref := historyRef(b.lineMeta[i], cli); ref != "" {
			return ref
		}
	}
	return ""
}

func (m model) queueHistory(net *network, q historyQuery) model {
	if net == nil || net.cli == nil || !net.cli.CapEnabled("draft/chathistory") || net.history.offline {
		return m
	}
	h := &net.history
	if h.pending != nil && h.pending.budget > 0 && equalFold(h.pending.target, q.target) && h.pending.sub == q.sub {
		return m
	}
	for _, queued := range h.queue {
		if equalFold(queued.target, q.target) && queued.sub == q.sub {
			return m
		}
	}
	if len(h.queue) < maxAutoBuffers {
		h.queue = append(h.queue, q)
	}
	return m.startHistory(net)
}

func (m model) startHistory(net *network) model {
	h := &net.history
	for h.pending == nil && len(h.queue) > 0 {
		q := h.queue[0]
		h.queue = h.queue[1:]
		q.limit = historyPageSize
		if value, ok := net.cli.ISupport("CHATHISTORY"); ok {
			if limit, err := strconv.Atoi(value); err == nil && limit > 0 {
				q.limit = min(q.limit, limit)
			}
		}
		q.limit = min(q.limit, q.budget)
		if q.limit <= 0 {
			continue
		}
		var err error
		switch q.sub {
		case "BEFORE":
			err = net.cli.ChatHistoryBefore(q.target, q.ref, q.limit)
		case "AFTER":
			err = net.cli.ChatHistoryAfter(q.target, q.ref, q.limit)
		case "TARGETS":
			err = net.cli.ChatHistoryTargets(q.from, time.Unix(0, 0), q.limit)
		default:
			err = net.cli.ChatHistoryLatest(q.target, q.limit)
		}
		if err != nil {
			m = appendInfo(m, m.serverBuffer(net), "history: "+err.Error())
			continue
		}
		q.deadline = time.Now().Add(historyTimeout)
		h.pending = &q
	}
	return m
}

func (m model) discoverHistory(net *network) model {
	if net == nil || !m.hasNetwork(net) || net.history.discovered {
		return m
	}
	net.history.discovered = true
	return m.queueHistory(net, historyQuery{sub: "TARGETS", from: time.Now().Add(10 * time.Second), budget: maxAutoBuffers})
}

func (m *model) requestOlder(b *Buffer) {
	if b.Kind == BufferServer || b.net == nil || b.historyEnded {
		return
	}
	if !b.historyLoaded {
		*m = m.queueHistory(b.net, historyQuery{sub: "LATEST", target: b.Title, budget: scrollbackLimit - len(b.lines)})
		return
	}
	if ref := b.historyRef(false); ref != "" {
		*m = m.queueHistory(b.net, historyQuery{sub: "BEFORE", target: b.Title, ref: ref,
			budget: scrollbackLimit - len(b.lines)})
	}
}

func (m *model) historyTick() tea.Cmd {
	if m.historyTicking {
		return nil
	}
	for _, net := range m.networks {
		if net.history.pending != nil || len(net.history.batches) != 0 {
			m.historyTicking = true
			return tea.Tick(historyTimeout, func(time.Time) tea.Msg { return historyTickMsg{} })
		}
	}
	return nil
}

func (m model) expireHistory() model {
	now := time.Now()
	for _, net := range m.networks {
		h := &net.history
		if h.pending != nil && !now.Before(h.pending.deadline) {
			h.pending = nil
			m = appendInfo(m, m.serverBuffer(net), "history request timed out; you can retry")
		}
		for ref, batch := range h.batches {
			if !now.Before(batch.deadline) {
				delete(h.batches, ref)
			}
		}
		m = m.startHistory(net)
	}
	return m
}

func (m model) routeHistory(net *network, ev client.Event) (model, bool) {
	h := &net.history
	switch ev.Command() {
	case client.EventReconnecting:
		if !h.offline {
			*h = historyState{offline: true}
			for _, b := range m.buffers {
				if b.net == net {
					b.catchupRef = b.historyRef(true)
					b.historyEnded = false
					if b.Kind == BufferChannel {
						b.historyLoaded = false
					}
				}
			}
		}
		return m, false
	case client.EventReconnected:
		h.offline = false
		m = m.discoverHistory(net)
		for _, b := range m.buffers {
			if b.net == net && (b.Kind == BufferPM || b.historyLoaded) && b.catchupRef != "" {
				m = m.queueHistory(net, historyQuery{sub: "AFTER", target: b.Title, ref: b.catchupRef, budget: scrollbackLimit})
				b.catchupRef = ""
			}
		}
		return m, false
	case irc.FAIL:
		if ev.Param(0) == irc.CHATHISTORY && h.pending != nil {
			failed := h.pending
			h.pending = nil
			for ref, batch := range h.batches {
				if batch.query == failed {
					delete(h.batches, ref)
				}
			}
			m = m.startHistory(net)
		}
		return m, false
	case irc.BATCH:
		ref := ev.Param(0)
		if strings.HasPrefix(ref, "+") {
			kind := ev.Param(1)
			if (kind != "chathistory" && kind != "draft/chathistory-targets") || len(h.batches) >= 16 {
				return m, false
			}
			if h.batches == nil {
				h.batches = make(map[string]*historyBatch)
			}
			batch := &historyBatch{target: ev.Param(2), deadline: time.Now().Add(historyTimeout)}
			_, batch.end = ev.Message.Tags["draft/chathistory-end"]
			if h.pending != nil && equalFold(h.pending.target, batch.target) {
				batch.query = h.pending
				batch.discard = h.pending.budget <= 0
			}
			h.batches[ref[1:]] = batch
			return m, true
		}
		if strings.HasPrefix(ref, "-") {
			if batch := h.batches[ref[1:]]; batch != nil {
				delete(h.batches, ref[1:])
				return m.finishHistory(net, batch), true
			}
		}
		return m, false
	}
	batch := h.batches[ev.Message.Tags.Get("batch")]
	if batch == nil {
		if ev.BatchType() == "chathistory" || ev.BatchType() == "draft/chathistory-targets" {
			return m, true
		}
		return m, false
	}
	if batch.discard {
		return m, true
	}
	if ev.Command() == irc.CHATHISTORY && ev.Param(0) == "TARGETS" {
		batch.count++
		ts, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(ev.Param(2), "timestamp="))
		if err != nil || batch.query == nil || batch.query.sub != "TARGETS" {
			return m, true
		}
		if batch.oldest.IsZero() || ts.Before(batch.oldest) {
			batch.oldest = ts
		}
		target := ev.Param(1)
		if target == "" || isChannel(target) || strings.ContainsAny(target, " ,:") || client.SanitizeTerminal(target) != target {
			return m, true
		}
		b := m.netBuffer(net, target)
		if b == nil {
			if len(m.buffers) >= maxAutoBuffers {
				return m, true
			}
			b, _ = m.ensureBufferIn(net, target, BufferPM)
		}
		sub, ref := "LATEST", ""
		if ref = b.historyRef(true); ref != "" {
			sub = "AFTER"
		}
		return m.queueHistory(net, historyQuery{sub: sub, target: target, ref: ref, budget: scrollbackLimit}), true
	}
	if ev.Command() == irc.PRIVMSG || ev.Command() == irc.NOTICE {
		batch.count++
		if len(batch.events) < 100 {
			batch.events = append(batch.events, ev)
		} else {
			batch.truncated = true
		}
	}
	return m, true
}

func (m model) finishHistory(net *network, batch *historyBatch) model {
	q := batch.query
	if batch.target != "" {
		b := m.netBuffer(net, batch.target)
		if b != nil && !batch.discard {
			if len(batch.events) > 0 && (q == nil || q.sub == "LATEST" || q.sub == "BEFORE") {
				if (q != nil && q.sub == "BEFORE") || historyRef(b.historyBefore, net.cli) == "" {
					ev := batch.events[0]
					b.historyBefore = historyLine{id: ev.Message.Tags.Get("msgid"), at: ev.Time()}
				}
			}
			m = m.mergeHistory(b, batch.events, q != nil && q.sub == "BEFORE")
			b.historyLoaded = true
			if q != nil && q.sub == "BEFORE" {
				b.historyEnded = batch.end || batch.count < q.limit || len(b.lines) >= scrollbackLimit
			}
			if batch.end && (q == nil || q.sub == "LATEST") {
				b.historyEnded = true
			}
			if q == nil && b.catchupRef != "" && !net.history.offline {
				m = m.queueHistory(net, historyQuery{sub: "AFTER", target: b.Title, ref: b.catchupRef, budget: scrollbackLimit})
				b.catchupRef = ""
			}
		} else if q != nil {
			q.budget = 0
		}
	}
	if batch.truncated {
		m = appendInfo(m, m.serverBuffer(net), "history reply exceeded the page limit")
	}
	if q == nil || net.history.pending != q {
		return m
	}
	net.history.pending = nil
	q.budget -= batch.count
	if !batch.end && !batch.truncated && batch.count >= q.limit && q.budget > 0 {
		switch q.sub {
		case "AFTER":
			if n := len(batch.events); n > 0 {
				ev := batch.events[n-1]
				ref := historyRef(historyLine{id: ev.Message.Tags.Get("msgid"), at: ev.Time()}, net.cli)
				if ref != "" && ref != q.ref {
					q.ref = ref
					m = m.queueHistory(net, *q)
				}
			}
		case "TARGETS":
			if !batch.oldest.IsZero() && batch.oldest.Before(q.from) {
				q.from = batch.oldest.Add(-time.Nanosecond)
				m = m.queueHistory(net, *q)
			}
		}
	}
	return m.startHistory(net)
}

func (m *model) cancelHistory(b *Buffer) {
	if b.net == nil {
		return
	}
	h := &b.net.history
	h.queue = slices.DeleteFunc(h.queue, func(q historyQuery) bool { return equalFold(q.target, b.Title) })
	if h.pending != nil && equalFold(h.pending.target, b.Title) {
		h.pending.budget = 0
	}
	for _, batch := range h.batches {
		if equalFold(batch.target, b.Title) {
			batch.discard = true
			batch.events = nil
		}
	}
}

// mergeHistory keeps live/history overlap out of scrollback and logs, and keeps
// the reader on the same wrapped row while older messages are inserted above it.
func (m model) mergeHistory(b *Buffer, events []client.Event, older bool) model {
	b.alignHistory()
	stick := b.vpReady && b.vp.AtBottom() && !older
	offset, anchor := 0, 0
	if b.vpReady {
		offset = b.vp.YOffset()
		anchor = max(0, sort.Search(len(b.lines), func(i int) bool { return b.wrappedOffsetOf(i) > offset })-1)
		offset -= b.wrappedOffsetOf(anchor)
	}
	for _, ev := range events {
		id := ev.Message.Tags.Get("msgid")
		if b.net.isIgnored(ev.Nick()) || (id != "" && b.seenIDs[id]) {
			continue
		}
		at := ev.Time()
		pos := len(b.lines)
		for i, line := range b.lineMeta {
			if !line.at.IsZero() && line.at.After(at) {
				pos = i
				break
			}
		}
		if !b.gotHistory {
			m.insertHistoryLine(b, pos, defaultTheme.dim.Render("── history ──"), historyLine{})
			if pos <= anchor {
				anchor++
			}
			pos++
			b.gotHistory = true
		}
		row, _ := defaultTheme.formatLine(ev, b.net.nick(), b.net.highlights)
		m.insertHistoryLine(b, pos, row, historyLine{id: id, at: at})
		if pos <= anchor {
			anchor++
		}
		m.logger.Log(bufferScope(b), b.Title, stripANSI(row))
		if len(b.lines) > scrollbackLimit {
			drop := len(b.lines) - scrollbackLimit
			for _, line := range b.lineMeta[:drop] {
				delete(b.seenIDs, line.id)
			}
			b.lines = b.lines[drop:]
			b.lineMeta = b.lineMeta[drop:]
			b.readMarker = max(0, b.readMarker-drop)
			anchor = max(0, anchor-drop)
			if b == m.activeBuffer() {
				m.shiftSearchMatches(drop)
			}
		}
	}
	b.dirty = true
	if b.vpReady {
		b.refresh()
		if stick {
			b.vp.GotoBottom()
		} else {
			b.vp.SetYOffset(b.wrappedOffsetOf(min(anchor, len(b.lines))) + offset)
			if older {
				b.vp.PageUp()
			}
		}
	}
	return m
}

func (m *model) insertHistoryLine(b *Buffer, pos int, row string, info historyLine) {
	b.lines = slices.Insert(b.lines, pos, row)
	b.lineMeta = slices.Insert(b.lineMeta, pos, info)
	if info.id != "" {
		if b.seenIDs == nil {
			b.seenIDs = make(map[string]bool)
		}
		b.seenIDs[info.id] = true
	}
	if b.readMarker > 0 && pos <= b.readMarker {
		b.readMarker++
	}
	if b == m.activeBuffer() {
		for i := range m.searchMatches {
			if m.searchMatches[i] >= pos {
				m.searchMatches[i]++
			}
		}
	}
}
