package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/exec/lurk/chatlog"
	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

func TestParseIRCFormatting(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []ircTextSpan
	}{
		{"plain", "café 👩🏽‍💻", []ircTextSpan{{text: "café 👩🏽‍💻"}}},
		{"toggles", "a\x02b\x02c", []ircTextSpan{{text: "a"}, {text: "b", style: ircTextStyle{bold: true}}, {text: "c"}}},
		{"stacked", "\x02\x1d\x1f\x1e\x16\x11x\x0fy", []ircTextSpan{{text: "x", style: ircTextStyle{bold: true, italic: true, underline: true, strike: true, reverse: true, monospace: true}}, {text: "y"}}},
		{"indexed", "\x0304,02red", []ircTextSpan{{text: "red", style: ircTextStyle{fg: "#ff0000", bg: "#00007f"}}}},
		{"single digit", "\x034x", []ircTextSpan{{text: "x", style: ircTextStyle{fg: "#ff0000"}}}},
		{"two digits", "\x031234", []ircTextSpan{{text: "34", style: ircTextStyle{fg: "#0000ff"}}}},
		{"extended", "\x0352x", []ircTextSpan{{text: "x", style: ircTextStyle{fg: "#ff0000"}}}},
		{"foreground default", "\x034,2a\x0399b", []ircTextSpan{{text: "a", style: ircTextStyle{fg: "#ff0000", bg: "#00007f"}}, {text: "b", style: ircTextStyle{bg: "#00007f"}}}},
		{"background default", "\x034,99x", []ircTextSpan{{text: "x", style: ircTextStyle{fg: "#ff0000"}}}},
		{"color reset keeps bold", "\x02\x034a\x03b", []ircTextSpan{{text: "a", style: ircTextStyle{bold: true, fg: "#ff0000"}}, {text: "b", style: ircTextStyle{bold: true}}}},
		{"comma without foreground", "\x03,04x", []ircTextSpan{{text: ",04x"}}},
		{"comma without background", "\x034,text", []ircTextSpan{{text: ",text", style: ircTextStyle{fg: "#ff0000"}}}},
		{"hex", "\x04aB1234,00FF00x", []ircTextSpan{{text: "x", style: ircTextStyle{fg: "#aB1234", bg: "#00FF00"}}}},
		{"hex partial", "\x04ABCDx", []ircTextSpan{{text: "ABCDx"}}},
		{"hex partial background", "\x04FF0000,123x", []ircTextSpan{{text: ",123x", style: ircTextStyle{fg: "#FF0000"}}}},
		{"hex reset", "\x04FF0000a\x04z", []ircTextSpan{{text: "a", style: ircTextStyle{fg: "#FF0000"}}, {text: "z"}}},
		{"unsafe controls", "\x02x\x1b[2J\x07\t\u202ey", []ircTextSpan{{text: "x[2J y", style: ircTextStyle{bold: true}}}},
		{"invalid UTF8", "a\xffb", []ircTextSpan{{text: "a�b"}}},
		{"empty controls", "\x02\x03\x04\x0f", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseIRCFormatting(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
	for i := 0; i < 100; i++ {
		spans := parseIRCFormatting(fmt.Sprintf("\x03%02dx", i))
		if len(spans) != 1 || spans[0].text != "x" {
			t.Fatalf("color %d not consumed", i)
		}
	}
	for _, tc := range []struct {
		code  byte
		style ircTextStyle
	}{
		{0x02, ircTextStyle{bold: true}}, {0x1d, ircTextStyle{italic: true}},
		{0x1f, ircTextStyle{underline: true}}, {0x1e, ircTextStyle{strike: true}},
		{0x16, ircTextStyle{reverse: true}}, {0x11, ircTextStyle{monospace: true}},
	} {
		in := string(tc.code) + "x" + string(tc.code) + "y"
		want := []ircTextSpan{{text: "x", style: tc.style}, {text: "y"}}
		if got := parseIRCFormatting(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("toggle %x: %#v", tc.code, got)
		}
	}
}

func TestFormattingContentCommands(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, line := range []string{
		":bob!u@h NOTICE #c :\x0304red\x0f",
		":bob!u@h PRIVMSG #c :\x01ACTION \x0304red\x0f\x01",
		":bob!u@h TOPIC #c :\x0304red\x0f",
		":bob!u@h PART #c :\x0304red\x0f",
		":bob!u@h QUIT :\x0304red\x0f",
		":s 372 me :\x0304red\x0f",
		":s 311 me bob u h * :\x0304red\x0f",
		":s 301 me bob :\x0304red\x0f",
		":s 322 me #c 2 :\x0304red\x0f",
	} {
		for _, disabled := range []bool{false, true} {
			tm := newTheme()
			tm.disableFormatting = disabled
			ev := evt(t, line)
			out, _ := tm.formatLine(ev, "me", nil)
			if strings.Contains(out, "38;2;255;0;0") == disabled || !strings.Contains(stripANSI(out), "red") || strings.Contains(stripANSI(out), "04red") {
				t.Fatalf("%s disabled=%v: %q", ev.Command(), disabled, out)
			}
		}
	}
	for _, disabled := range []bool{false, true} {
		m := newTestModel()
		m.disableFormatting = disabled
		m = routeEvent(m, evt(t, ":s FAIL JOIN DENIED #c :\x0304red\x0f"))
		b := m.activeBuffer()
		if strings.Contains(b.lines[len(b.lines)-1], "38;2;255;0;0") == disabled {
			t.Fatal("standard replies ignored formatting policy")
		}
		m = openChannelList(m)
		m = routeListReply(m, m.activeNet(), evt(t, ":s 322 me #c 2 :\x0304red\x0f"))
		item := m.chanListAccum[0].(channelItem)
		if strings.Contains(item.Description(), "38;2;255;0;0") == disabled || item.FilterValue() != "#c red" {
			t.Fatal("LIST description/filter ignored formatting policy")
		}
	}
}

func TestIRCFormattingRenderingAndOptOut(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	tm := newTheme()
	raw := "plain \x02bold\x02 \x1ditalic\x1d \x1funder\x1f \x1estrike\x1e \x16reverse\x16 \x0304,02red\x0f"
	want := "plain bold italic under strike reverse red"
	got := tm.formatIRCText(raw, lipgloss.NewStyle())
	if stripANSI(got) != want || !strings.Contains(got, "\x1b[") {
		t.Fatalf("formatting not rendered: %q", got)
	}
	for _, code := range []string{"1", "3", "4", "9", "7", "38;2;255;0;0", "48;2;0;0;127"} {
		if !strings.Contains(got, code) {
			t.Errorf("missing style %s in %q", code, got)
		}
	}
	tm.disableFormatting = true
	if got := tm.formatIRCText(raw, lipgloss.NewStyle()); got != want {
		t.Fatalf("disabled = %q, want %q", got, want)
	}
	tm.disableFormatting = false
	tm.formatIRCText("\x02unclosed bold", lipgloss.NewStyle())
	if got := tm.formatIRCText("next message", lipgloss.NewStyle()); got != "next message" {
		t.Fatalf("style leaked: %q", got)
	}
	if got := plainIRCText("\x0304red\x0f"); got != "red" {
		t.Fatalf("plain text retained color arguments: %q", got)
	}
}

func TestFormattingHistoryViewAndLogsOverPipe(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			m, srv := historyModel(t)
			m.disableFormatting = disabled
			b, i := m.ensureBuffer("#c", BufferChannel)
			m.switchTo(i)
			m = layout(m)
			dir := t.TempDir()
			m.logger = chatlog.New(dir)
			t.Cleanup(m.logger.Close)
			for _, line := range []string{
				":me!u@h JOIN #c",
				"@msgid=live;time=2026-10-31T12:00:02Z :bob!u@h PRIVMSG #c :live \x02bold\x02 \x0304red\x0f",
				":srv BATCH +h chathistory #c",
				"@batch=h;msgid=old;time=2026-10-31T12:00:01Z :bob!u@h PRIVMSG #c :history \x1d\x0304red\x0f",
				":srv BATCH -h",
				":bob!u@h TOPIC #c :\x0304red topic\x0f \x02bold\x02",
			} {
				msg, err := irc.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.WriteMessage(msg); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.After(2 * time.Second)
		pump:
			for {
				select {
				case ev := <-m.activeNet().sub:
					updated, _ := m.Update(ircMsg{net: m.activeNet(), ev: ev})
					m = updated.(model)
					if ev.Command() == irc.TOPIC {
						break pump
					}
				case <-deadline:
					t.Fatal("event pump timed out")
				}
			}
			rows := strings.Join(b.lines, "\n")
			if strings.Contains(rows, "38;2;255;0;0") == disabled {
				t.Fatal("live/history did not use formatting policy")
			}
			if !strings.Contains(stripANSI(rows), "history red") || !strings.Contains(stripANSI(rows), "live bold red") {
				t.Fatalf("bad visible rows: %q", stripANSI(rows))
			}
			if len(matches(b.lines, "history red")) != 1 {
				t.Fatal("formatting broke search")
			}
			for _, width := range []int{30, 80, 120} {
				updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
				m = updated.(model)
				frame := m.View().Content
				if !strings.Contains(stripANSI(frame), "Topic:") || lipgloss.Width(frame) > width {
					t.Fatalf("bad wrapped frame at width %d", width)
				}
				if strings.Contains(frame, "38;2;255;0;0") == disabled {
					t.Fatalf("View lost formatting policy at width %d", width)
				}
			}
			m.logger.Close()
			data, err := os.ReadFile(filepath.Join(dir, "test", "#c.log"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(string(data), "\x1b\x02\x03\x1d") || !strings.Contains(string(data), "history red") {
				t.Fatalf("log contains formatting or lost text: %q", data)
			}
		})
	}
}

func TestIRCFormattingNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out := newTheme().formatIRCText("\x02\x04FF0000,00FF00bold", lipgloss.NewStyle())
	if strings.Contains(out, "38;") || strings.Contains(out, "48;") || !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("NO_COLOR should retain bold without color: %q", out)
	}
}

func TestFormattingDefaultAndDisabledEventPaths(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			m := newModel(client.New(client.Config{Nick: "me"}), nil)
			m.disableFormatting = disabled
			b, _ := m.ensureBuffer("#c", BufferChannel)
			m = routeEvent(m, evt(t, ":bob!u@h PRIVMSG #c :\x02b\x02\x0304red\x0f"))
			if visible := stripANSI(b.lines[0]); !strings.HasSuffix(visible, "bred") {
				t.Fatalf("wrong visible text: %q", visible)
			}
			styled := strings.Contains(b.lines[0], "38;2;255;0;0")
			if styled == disabled {
				t.Fatalf("formatting enabled=%v, disabled=%v", styled, disabled)
			}
			m = echoSelf(m, "#c", "\x0304local\x0f")
			if strings.Contains(b.lines[len(b.lines)-1], "38;2;255;0;0") == disabled {
				t.Fatal("local echo ignored setting")
			}
			m = routeEvent(m, evt(t, ":bob!u@h PRIVMSG #c :m\x02e\x02 \x0304highlight\x0f"))
			if !b.Highlight || !m.bell {
				t.Fatal("formatting hid a mention")
			}
		})
	}
}

func FuzzIRCFormatting(f *testing.F) {
	sgr := regexp.MustCompile("\x1b\\[[0-9;:]*m")
	for _, s := range []string{"plain", "\x0304,99red\x0f", "\x04ff0000,12ab34x", "\x02\x1b]52;c;evil\x07", "\xff\x03,04"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 8192 {
			return
		}
		for _, span := range parseIRCFormatting(s) {
			if !utf8.ValidString(span.text) || client.SanitizeTerminal(span.text) != span.text {
				t.Fatalf("unsafe span %q", span.text)
			}
			for _, c := range []string{span.style.fg, span.style.bg} {
				if c != "" && (len(c) != 7 || c[0] != '#') {
					t.Fatalf("unvalidated color %q", c)
				}
			}
		}
		if strings.ContainsRune(plainIRCText(s), 0x1b) {
			t.Fatal("plain text leaked ESC")
		}
		rendered := newTheme().formatIRCText(s, lipgloss.NewStyle())
		if visible := sgr.ReplaceAllString(rendered, ""); visible != plainIRCText(s) {
			t.Fatalf("rendered unsafe escapes or changed text: %q", rendered)
		}
	})
}
