package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/exec/lurk/client"
)

type ircTextStyle struct {
	bold, italic, underline, strike, reverse, monospace bool
	fg, bg                                              string // Validated RGB colors; empty means the enclosing display style.
}

type ircTextSpan struct {
	text  string // Terminal-sanitized text, never peer-supplied ANSI.
	style ircTextStyle
}

// parseIRCFormatting follows https://modern.ircdocs.horse/formatting.html.
// Only IRC controls affect style; every text span still passes through the
// terminal sanitizer. A fresh state per call prevents styling the next message.
func parseIRCFormatting(s string) []ircTextSpan {
	var spans []ircTextSpan
	var style ircTextStyle
	var text strings.Builder
	flush := func() {
		if clean := client.SanitizeTerminal(strings.ToValidUTF8(text.String(), "�")); clean != "" {
			spans = append(spans, ircTextSpan{text: clean, style: style})
		}
		text.Reset()
	}
	for i := 0; i < len(s); {
		code := s[i]
		switch code {
		case 0x02, 0x03, 0x04, 0x0f, 0x11, 0x16, 0x1d, 0x1e, 0x1f:
			flush()
		default:
			text.WriteByte(code)
			i++
			continue
		}
		i++
		switch code {
		case 0x02:
			style.bold = !style.bold
		case 0x1d:
			style.italic = !style.italic
		case 0x1f:
			style.underline = !style.underline
		case 0x1e:
			style.strike = !style.strike
		case 0x16:
			style.reverse = !style.reverse
		case 0x11:
			style.monospace = !style.monospace // Terminal text is already monospaced.
		case 0x0f:
			style = ircTextStyle{}
		case 0x03, 0x04:
			fg, n := readIRCColor(s[i:], code == 0x04)
			if n == 0 {
				style.fg, style.bg = "", ""
				continue
			}
			style.fg = fg
			i += n
			if i < len(s) && s[i] == ',' {
				if bg, n := readIRCColor(s[i+1:], code == 0x04); n > 0 {
					style.bg = bg
					i += n + 1
				}
			}
		}
	}
	flush()
	return spans
}

func readIRCColor(s string, hex bool) (string, int) {
	if hex {
		if len(s) < 6 {
			return "", 0
		}
		for _, c := range []byte(s[:6]) {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return "", 0
			}
		}
		return "#" + s[:6], 6
	}
	if len(s) == 0 || s[0] < '0' || s[0] > '9' {
		return "", 0
	}
	n, value := 1, int(s[0]-'0')
	if len(s) > 1 && s[1] >= '0' && s[1] <= '9' {
		n, value = 2, value*10+int(s[1]-'0')
	}
	if value == 99 {
		return "", n
	}
	return "#" + ircColors[value], n
}

// The extended palette is fixed by the IRC formatting reference above.
var ircColors = [...]string{
	"ffffff", "000000", "00007f", "009300", "ff0000", "7f0000", "9c009c", "fc7f00",
	"ffff00", "00fc00", "009393", "00ffff", "0000ff", "ff00ff", "7f7f7f", "d2d2d2",
	"470000", "472100", "474700", "324700", "004700", "00472c", "004747", "002747", "000047", "2e0047", "470047", "47002a",
	"740000", "743a00", "747400", "517400", "007400", "007449", "007474", "004074", "000074", "4b0074", "740074", "740045",
	"b50000", "b56300", "b5b500", "7db500", "00b500", "00b571", "00b5b5", "0063b5", "0000b5", "7500b5", "b500b5", "b5006b",
	"ff0000", "ff8c00", "ffff00", "b2ff00", "00ff00", "00ffa0", "00ffff", "008cff", "0000ff", "a500ff", "ff00ff", "ff0098",
	"ff5959", "ffb459", "ffff71", "cfff60", "6fff6f", "65ffc9", "6dffff", "59b4ff", "5959ff", "c459ff", "ff66ff", "ff59bc",
	"ff9c9c", "ffd39c", "ffff9c", "e2ff9c", "9cff9c", "9cffdb", "9cffff", "9cd3ff", "9c9cff", "dc9cff", "ff9cff", "ff94d3",
	"000000", "131313", "282828", "363636", "4d4d4d", "656565", "818181", "9f9f9f", "bcbcbc", "e2e2e2", "ffffff",
}

func plainIRCText(s string) string {
	if !strings.ContainsAny(s, "\x02\x03\x04\x0f\x11\x16\x1d\x1e\x1f") {
		return client.SanitizeTerminal(strings.ToValidUTF8(s, "�"))
	}
	var out strings.Builder
	for _, span := range parseIRCFormatting(s) {
		out.WriteString(span.text)
	}
	return out.String()
}

func (t theme) formatIRCText(s string, base lipgloss.Style) string {
	var out strings.Builder
	for _, span := range parseIRCFormatting(s) {
		style := base
		if !t.disableFormatting {
			f := span.style
			if f.bold {
				style = style.Bold(true)
			}
			if f.italic {
				style = style.Italic(true)
			}
			if f.underline {
				style = style.Underline(true).UnderlineSpaces(true)
			}
			if f.strike {
				style = style.Strikethrough(true).StrikethroughSpaces(true)
			}
			if f.reverse {
				style = style.Reverse(true)
			}
			if !noColorSet() {
				if f.fg != "" {
					style = style.Foreground(lipgloss.Color(f.fg))
				}
				if f.bg != "" {
					style = style.Background(lipgloss.Color(f.bg))
				}
			}
		}
		out.WriteString(style.Render(span.text))
	}
	return out.String()
}

func (m model) messageTheme() theme {
	t := defaultTheme
	t.disableFormatting = m.disableFormatting
	return t
}
