package tui

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"lurk/client"
)

// style.go owns the visual identity of the TUI: the Lip Gloss theme, stable
// per-nick colorization, and the per-event line formatting that turns a
// client.Event into a styled scrollback row. View composition lives in view.go;
// scrollback storage in buffer.go.
//
// The colorization approach (FNV-hash a nick into a fixed palette, render own
// nick distinctly, dim joins/parts, highlight lines that mention the user) is
// modeled on senpai's reference/senpai/ui/colors.go and buffers.go — studied,
// not copied: senpai is tcell/vaxis-based, so the rendering here is a fresh
// Lip Gloss implementation of the same UX ideas.

// theme groups every Lip Gloss style the view uses. It is built once
// (newTheme) and carried implicitly by the package-level defaultTheme; styles
// are immutable values so sharing one theme across renders is safe.
type theme struct {
	// timestamp prefixes each message line (dim gray "15:04").
	timestamp lipgloss.Style
	// text is the default body style.
	text lipgloss.Style
	// dim renders low-salience meta lines (joins, parts, quits, mode noise).
	dim lipgloss.Style
	// notice renders NOTICE bodies.
	notice lipgloss.Style
	// action renders /me lines.
	action lipgloss.Style
	// highlight renders a line that mentions the user's nick.
	highlight lipgloss.Style
	// info renders local informational lines (command output, errors).
	info lipgloss.Style

	// statusBar is the network/nick/mode bar across the bottom.
	statusBar lipgloss.Style
	// statusKey/statusVal style segments within the status bar.
	statusKey lipgloss.Style

	// sidebar is the buffer-list column container.
	sidebar       lipgloss.Style
	sidebarItem   lipgloss.Style // a normal buffer entry
	sidebarActive lipgloss.Style // the focused buffer entry
	sidebarUnread lipgloss.Style // a buffer with unread activity
	sidebarHigh   lipgloss.Style // a buffer with a highlight (mention)

	// nicklist is the member-list column container.
	nicklist     lipgloss.Style
	nicklistOp   lipgloss.Style // ops/voiced get a tint via prefix
	nicklistTtl  lipgloss.Style // the "Users (n)" title row
	verticalRule lipgloss.Style // 1-cell separator between panes

	// nickPalette is the set of colors nick hashing selects from.
	nickPalette []color.Color
	// self is the color used for the user's own nick.
	self color.Color
}

// defaultTheme is the single theme instance the view renders with. It is a
// dark-terminal palette using ANSI/256 colors so Lip Gloss can downsample
// cleanly on 16-color and monochrome terminals.
var defaultTheme = newTheme()

func newTheme() theme {
	// A 16-ish color nick palette drawn from the 256-color cube, skipping the
	// near-black/near-white extremes and the reds reserved for highlights, so
	// nicks stay legible on a dark background. senpai uses a similar curated
	// base set (reference/senpai/ui/colors.go baseColors).
	palette := []color.Color{
		lipgloss.Color("2"),   // green
		lipgloss.Color("3"),   // yellow
		lipgloss.Color("4"),   // blue
		lipgloss.Color("5"),   // magenta
		lipgloss.Color("6"),   // cyan
		lipgloss.Color("10"),  // bright green
		lipgloss.Color("11"),  // bright yellow
		lipgloss.Color("12"),  // bright blue
		lipgloss.Color("13"),  // bright magenta
		lipgloss.Color("14"),  // bright cyan
		lipgloss.Color("39"),  // sky blue
		lipgloss.Color("42"),  // teal-green
		lipgloss.Color("75"),  // light blue
		lipgloss.Color("141"), // lavender
		lipgloss.Color("178"), // gold
		lipgloss.Color("214"), // orange
	}

	gray := lipgloss.Color("240")
	brightGray := lipgloss.Color("245")

	return theme{
		timestamp: lipgloss.NewStyle().Foreground(gray),
		text:      lipgloss.NewStyle(),
		dim:       lipgloss.NewStyle().Foreground(gray),
		notice:    lipgloss.NewStyle().Foreground(lipgloss.Color("180")),
		action:    lipgloss.NewStyle().Foreground(lipgloss.Color("177")).Italic(true),
		highlight: lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("52")).Bold(true),
		info:      lipgloss.NewStyle().Foreground(brightGray).Italic(true),

		statusBar: lipgloss.NewStyle().
			Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("24")),
		statusKey: lipgloss.NewStyle().Bold(true),

		sidebar:       lipgloss.NewStyle().Foreground(brightGray),
		sidebarItem:   lipgloss.NewStyle().Foreground(brightGray),
		sidebarActive: lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24")).Bold(true),
		sidebarUnread: lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Bold(true),
		sidebarHigh:   lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true),

		nicklist:     lipgloss.NewStyle().Foreground(brightGray),
		nicklistOp:   lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		nicklistTtl:  lipgloss.NewStyle().Foreground(gray).Bold(true),
		verticalRule: lipgloss.NewStyle().Foreground(lipgloss.Color("237")),

		nickPalette: palette,
		self:        lipgloss.Color("231"),
	}
}

// nickColor returns the stable display color for nick. Hashing the (lower-cased)
// nick into a fixed palette means a given nick always renders the same color
// across buffers and sessions, the way senpai's IdentColor does. self renders
// in the dedicated self color so the user can spot their own messages.
func (t theme) nickColor(nick string, self bool) color.Color {
	if self {
		return t.self
	}
	h := fnv.New32()
	_, _ = h.Write([]byte(strings.ToLower(nick)))
	return t.nickPalette[h.Sum32()%uint32(len(t.nickPalette))]
}

// styledNick renders nick in its hashed color (bold), used in message heads.
func (t theme) styledNick(nick string, self bool) string {
	return lipgloss.NewStyle().Foreground(t.nickColor(nick, self)).Bold(true).Render(nick)
}

// formatLine turns a single client.Event into a fully styled scrollback row
// (already including its timestamp prefix). self is the user's current nick,
// used to colorize own messages and detect highlights. The returned highlight
// bool reports whether the line mentions self (so the buffer can bump its
// highlight counter and the row gets the highlight background).
//
// Formatting is per-command: PRIVMSG/NOTICE render a "<nick> body" head+body;
// CTCP ACTION renders as "* nick body"; JOIN/PART/QUIT/NICK/MODE/TOPIC render as
// dim meta lines; numerics and anything else fall back to the trailing text.
func (t theme) formatLine(ev client.Event, self string) (text string, highlight bool) {
	ts := t.timestamp.Render(ev.Time().Local().Format("15:04"))
	nick := ev.Nick()
	isSelf := nick != "" && equalFold(nick, self)

	switch ev.Command() {
	case "PRIVMSG":
		body := ev.Text()
		if action, ok := ctcpAction(body); ok {
			line := fmt.Sprintf("%s %s %s", ts, t.action.Render("*"), t.action.Render(nick+" "+action))
			return t.applyHighlight(line, action, self, isSelf)
		}
		head := t.styledNick(nick, isSelf)
		line := fmt.Sprintf("%s %s %s", ts, head, t.text.Render(body))
		return t.applyHighlight(line, body, self, isSelf)

	case "NOTICE":
		body := ev.Text()
		head := lipgloss.NewStyle().Foreground(t.nickColor(nick, isSelf)).Render("-" + nick + "-")
		line := fmt.Sprintf("%s %s %s", ts, head, t.notice.Render(body))
		return t.applyHighlight(line, body, self, isSelf)

	case "JOIN":
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symJoin+" "+nick+" joined "+ev.Param(0))), false

	case "PART":
		msg := joinReason(ev.Text(), ev.Param(0))
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symPart+" "+nick+" left"+msg)), false

	case "QUIT":
		msg := joinReason(ev.Text(), "")
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symPart+" "+nick+" quit"+msg)), false

	case "NICK":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ "+nick+" is now known as "+ev.Text())), false

	case "TOPIC":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ "+nick+" set the topic: "+ev.Text())), false

	case "MODE":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ mode "+strings.Join(ev.Message.Params, " "))), false

	default:
		// Numerics and unhandled commands: show the trailing text so the server
		// buffer still surfaces MOTD, errors, and replies.
		body := ev.Text()
		if body == "" {
			body = strings.Join(ev.Message.Params, " ")
		}
		return fmt.Sprintf("%s %s", ts, t.dim.Render(body)), false
	}
}

// applyHighlight checks whether body mentions self and, if so, re-renders the
// whole line over the highlight background and reports the highlight. A nil/own
// message never highlights (you don't get pinged by yourself).
func (t theme) applyHighlight(line, body, self string, isSelf bool) (string, bool) {
	if isSelf || self == "" || !mentions(body, self) {
		return line, false
	}
	// Re-render the visible content (stripped of existing ANSI) over the
	// highlight style so the mention stands out as a single banner row.
	return t.highlight.Render(stripANSI(line)), true
}

// formatInfo renders a local informational line (command output / error).
func (t theme) formatInfo(text string) string {
	ts := t.timestamp.Render(time.Now().Format("15:04"))
	return fmt.Sprintf("%s %s", ts, t.info.Render(text))
}

// formatSelfMessage renders a locally-echoed outbound PRIVMSG (used when
// echo-message is not negotiated) so the user sees what they sent immediately.
func (t theme) formatSelfMessage(self, text string) string {
	ts := t.timestamp.Render(time.Now().Format("15:04"))
	if action, ok := ctcpAction(text); ok {
		return fmt.Sprintf("%s %s %s", ts, t.action.Render("*"), t.action.Render(self+" "+action))
	}
	head := t.styledNick(self, true)
	return fmt.Sprintf("%s %s %s", ts, head, t.text.Render(text))
}

// statusLine renders the bottom status bar: network, current nick, active
// buffer, and a scroll indicator. width pads it to the full terminal width.
func (t theme) statusLine(network, nick, buffer string, scrolled bool, width int) string {
	if network == "" {
		network = "(connecting)"
	}
	seg := func(k, v string) string {
		return t.statusKey.Render(k) + ":" + v
	}
	parts := []string{
		seg("net", network),
		seg("nick", nick),
		seg("buf", buffer),
	}
	if scrolled {
		parts = append(parts, t.statusKey.Render("[scrolled]"))
	}
	content := " " + strings.Join(parts, "  ") + " "
	return t.statusBar.Width(width).Render(content)
}

// Glyphs and helpers ---------------------------------------------------------

const (
	symJoin = "+" // join marker in meta lines
	symPart = "-" // part/quit marker in meta lines

	// markUnread / markHigh are the sidebar activity markers.
	markUnread = "•"
	markHigh   = "!"
)

// ctcpAction detects a CTCP ACTION (/me) payload, returning the inner text with
// the \x01ACTION ... \x01 framing removed.
func ctcpAction(body string) (string, bool) {
	const prefix = "\x01ACTION "
	if strings.HasPrefix(body, prefix) {
		inner := strings.TrimPrefix(body, prefix)
		inner = strings.TrimSuffix(inner, "\x01")
		return inner, true
	}
	return "", false
}

// joinReason formats an optional trailing reason for part/quit meta lines,
// avoiding duplicating the channel name (which arrives as Param(0) for PART).
func joinReason(text, channel string) string {
	if text != "" && text != channel {
		return " (" + text + ")"
	}
	return ""
}

// mentions reports whether text contains nick as a whole word (case-insensitive),
// the standard IRC highlight rule. A bare substring match would false-positive
// on e.g. "bobby" mentioning "bob", so word boundaries are required.
func mentions(text, nick string) bool {
	if nick == "" {
		return false
	}
	lt := strings.ToLower(text)
	ln := strings.ToLower(nick)
	for {
		i := strings.Index(lt, ln)
		if i < 0 {
			return false
		}
		beforeOK := i == 0 || !isNickChar(lt[i-1])
		end := i + len(ln)
		afterOK := end >= len(lt) || !isNickChar(lt[end])
		if beforeOK && afterOK {
			return true
		}
		lt = lt[i+1:]
	}
}

// isNickChar reports whether b can appear inside an IRC nick (letters, digits,
// and the special chars RFC 2812 permits), used for highlight word boundaries.
func isNickChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '[' || b == ']' || b == '\\' || b == '`' ||
		b == '^' || b == '{' || b == '}' || b == '_' || b == '|':
		return true
	}
	return false
}

// stripANSI removes SGR escape sequences from s. It is used when re-rendering a
// highlighted line over a single background style, where leftover foreground
// codes from styledNick would otherwise interrupt the banner. It handles the
// CSI ... m form Lip Gloss emits.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// skip until the final byte of the CSI sequence (0x40–0x7e).
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			continue // i now at the final byte; loop's i++ skips it
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
