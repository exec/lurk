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
	nicklistAway lipgloss.Style // away members rendered faint
	nicklistAcct lipgloss.Style // the "logged-in" badge marker
	verticalRule lipgloss.Style // 1-cell separator between panes

	// nickPalette is the set of colors nick hashing selects from.
	nickPalette []color.Color
	// self is the color used for the user's own nick.
	self color.Color
}

// Catppuccin Mocha palette (https://catppuccin.com) — a dark, harmonious
// 24-bit color scheme designed for legibility on a dark background. We render
// in full truecolor; Bubble Tea's renderer downsamples these hex values to the
// terminal's actual color profile (ANSI256 on Apple Terminal, 16-color on
// older terminals), so capable terminals (iTerm2, Ghostty, Kitty, WezTerm,
// Alacritty, modern Linux terminals, Windows Terminal) get the full palette
// while limited ones still degrade cleanly. Credited in CREDITS.md.
var (
	ctpRosewater = lipgloss.Color("#f5e0dc")
	ctpFlamingo  = lipgloss.Color("#f2cdcd")
	ctpPink      = lipgloss.Color("#f5c2e7")
	ctpMauve     = lipgloss.Color("#cba6f7")
	ctpRed       = lipgloss.Color("#f38ba8")
	ctpPeach     = lipgloss.Color("#fab387")
	ctpYellow    = lipgloss.Color("#f9e2af")
	ctpGreen     = lipgloss.Color("#a6e3a1")
	ctpTeal      = lipgloss.Color("#94e2d5")
	ctpSky       = lipgloss.Color("#89dceb")
	ctpSapphire  = lipgloss.Color("#74c7ec")
	ctpBlue      = lipgloss.Color("#89b4fa")
	ctpLavender  = lipgloss.Color("#b4befe")
	ctpText      = lipgloss.Color("#cdd6f4")
	ctpSubtext0  = lipgloss.Color("#a6adc8")
	ctpOverlay0  = lipgloss.Color("#6c7086")
	ctpSurface0  = lipgloss.Color("#313244")

	// statusAccent is the muted blue backing the status bar and the active
	// buffer — a darkened Catppuccin blue that keeps white text readable.
	statusAccent = lipgloss.Color("#3a4669")
	// highlightBg backs a mention banner: a deep, desaturated red so the white
	// foreground stays legible (and downsamples to a dark-red 256 cell).
	highlightBg = lipgloss.Color("#6c2e3e")
)

// defaultTheme is the single theme instance the view renders with.
var defaultTheme = newTheme()

func newTheme() theme {
	// A curated nick palette spanning the hue wheel for maximum distinguishability
	// when nicks hash into it, drawn from the Catppuccin Mocha accents. The two
	// reds (Red/Maroon) are reserved for highlights and ops, so they're omitted
	// here to avoid confusing a nick with a mention. senpai uses a similar curated
	// base set (reference/senpai/ui/colors.go baseColors).
	palette := []color.Color{
		ctpGreen,
		ctpYellow,
		ctpBlue,
		ctpPink,
		ctpTeal,
		ctpPeach,
		ctpSapphire,
		ctpMauve,
		ctpSky,
		ctpLavender,
		ctpFlamingo,
		ctpRosewater,
	}

	return theme{
		timestamp: lipgloss.NewStyle().Foreground(ctpOverlay0),
		text:      lipgloss.NewStyle(),
		dim:       lipgloss.NewStyle().Foreground(ctpOverlay0),
		notice:    lipgloss.NewStyle().Foreground(ctpYellow),
		action:    lipgloss.NewStyle().Foreground(ctpMauve).Italic(true),
		highlight: lipgloss.NewStyle().Foreground(ctpRosewater).Background(highlightBg).Bold(true),
		info:      lipgloss.NewStyle().Foreground(ctpSubtext0).Italic(true),

		statusBar: lipgloss.NewStyle().
			Foreground(ctpText).
			Background(statusAccent),
		statusKey: lipgloss.NewStyle().Bold(true),

		sidebar:       lipgloss.NewStyle().Foreground(ctpSubtext0),
		sidebarItem:   lipgloss.NewStyle().Foreground(ctpSubtext0),
		sidebarActive: lipgloss.NewStyle().Foreground(ctpText).Background(statusAccent).Bold(true),
		sidebarUnread: lipgloss.NewStyle().Foreground(ctpText).Bold(true),
		sidebarHigh:   lipgloss.NewStyle().Foreground(ctpRed).Bold(true),

		nicklist:     lipgloss.NewStyle().Foreground(ctpSubtext0),
		nicklistOp:   lipgloss.NewStyle().Foreground(ctpRed),
		nicklistTtl:  lipgloss.NewStyle().Foreground(ctpOverlay0).Bold(true),
		nicklistAway: lipgloss.NewStyle().Foreground(ctpOverlay0).Faint(true),
		nicklistAcct: lipgloss.NewStyle().Foreground(ctpTeal),
		verticalRule: lipgloss.NewStyle().Foreground(ctpSurface0),

		nickPalette: palette,
		self:        ctpText,
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
	// Every server-derived field below is sanitized before it is rendered: the
	// values are attacker-controlled and the wire parser only strips NUL/CR/LF,
	// so ESC and the other control bytes that drive terminal escape sequences
	// still arrive here. nick feeds both the display and the self/colour
	// comparisons; a legitimate nick has no control bytes, so sanitizing it is a
	// no-op in practice but closes the spoofing vector.
	nick := sanitize(ev.Nick())
	isSelf := nick != "" && equalFold(nick, self)

	switch ev.Command() {
	case "PRIVMSG":
		// Detect CTCP framing on the raw text (the \x01 markers are control
		// bytes that sanitize would strip), then sanitize the inner payload.
		if action, ok := ctcpAction(ev.Text()); ok {
			action = sanitize(action)
			line := fmt.Sprintf("%s %s %s", ts, t.action.Render("*"), t.action.Render(nick+" "+action))
			return t.applyHighlight(line, action, self, isSelf)
		}
		body := sanitize(ev.Text())
		head := t.styledNick(nick, isSelf)
		line := fmt.Sprintf("%s %s %s", ts, head, t.text.Render(body))
		return t.applyHighlight(line, body, self, isSelf)

	case "NOTICE":
		body := sanitize(ev.Text())
		head := lipgloss.NewStyle().Foreground(t.nickColor(nick, isSelf)).Render("-" + nick + "-")
		line := fmt.Sprintf("%s %s %s", ts, head, t.notice.Render(body))
		return t.applyHighlight(line, body, self, isSelf)

	case "JOIN":
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symJoin+" "+nick+" joined "+sanitize(ev.Param(0)))), false

	case "PART":
		msg := joinReason(sanitize(ev.Text()), sanitize(ev.Param(0)))
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symPart+" "+nick+" left"+msg)), false

	case "QUIT":
		msg := joinReason(sanitize(ev.Text()), "")
		return fmt.Sprintf("%s %s", ts, t.dim.Render(symPart+" "+nick+" quit"+msg)), false

	case "NICK":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ "+nick+" is now known as "+sanitize(ev.Text()))), false

	case "TOPIC":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ "+nick+" set the topic: "+sanitize(ev.Text()))), false

	case "MODE":
		return fmt.Sprintf("%s %s", ts, t.dim.Render("~ mode "+sanitize(strings.Join(ev.Message.Params, " ")))), false

	default:
		// Numerics and unhandled commands: show the trailing text so the server
		// buffer still surfaces MOTD, errors, and replies.
		body := ev.Text()
		if body == "" {
			body = strings.Join(ev.Message.Params, " ")
		}
		return fmt.Sprintf("%s %s", ts, t.dim.Render(sanitize(body))), false
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

// formatStandardReply renders a standard-replies line (FAIL/WARN/NOTE) in a
// readable form, e.g. "! FAIL JOIN ACCOUNT_REQUIRED: You must register…". The
// wire form is "<TYPE> <COMMAND> <code> [context...] :<description>": Param(0)
// is the command the reply concerns, Param(1) the machine-readable code, any
// middle params are extra context, and the trailing param is the human text.
// FAIL is styled most severely, NOTE least.
func (t theme) formatStandardReply(ev client.Event) string {
	ts := t.timestamp.Render(ev.Time().Local().Format("15:04"))

	kind := ev.Command() // FAIL, WARN, or NOTE
	cmd := ev.Param(0)
	code := ev.Param(1)
	desc := ev.Text()

	// Context params sit between the code (index 1) and the trailing description.
	var context string
	if n := len(ev.Message.Params); n > 3 {
		context = " " + strings.Join(ev.Message.Params[2:n-1], " ")
	}

	var marker string
	var style lipgloss.Style
	switch kind {
	case "FAIL":
		marker, style = "✗", lipgloss.NewStyle().Foreground(ctpRed).Bold(true)
	case "WARN":
		marker, style = "⚠", t.notice
	default: // NOTE
		marker, style = "ℹ", t.info
	}

	head := strings.TrimSpace(kind + " " + cmd + " " + code + context)
	body := head
	if desc != "" && desc != code {
		body += ": " + desc
	}
	// cmd/code/context/desc are all server-supplied; sanitize the assembled body
	// before rendering so embedded escape sequences cannot reach the terminal.
	return fmt.Sprintf("%s %s", ts, style.Render(marker+" "+sanitize(body)))
}

// formatInfo renders a local informational line (command output / error).
func (t theme) formatInfo(text string) string {
	ts := t.timestamp.Render(time.Now().Format("15:04"))
	// Info lines are locally composed plain text, but some carry server- or
	// user-supplied fragments (command echoes, raw-send errors), so sanitize.
	return fmt.Sprintf("%s %s", ts, t.info.Render(sanitize(text)))
}

// formatSelfMessage renders a locally-echoed outbound PRIVMSG (used when
// echo-message is not negotiated) so the user sees what they sent immediately.
func (t theme) formatSelfMessage(self, text string) string {
	ts := t.timestamp.Render(time.Now().Format("15:04"))
	// Sanitize the echoed text too: a user can paste attacker-crafted content
	// carrying escape sequences, and the local echo writes straight to the pane.
	if action, ok := ctcpAction(text); ok {
		return fmt.Sprintf("%s %s %s", ts, t.action.Render("*"), t.action.Render(self+" "+sanitize(action)))
	}
	head := t.styledNick(self, true)
	return fmt.Sprintf("%s %s %s", ts, head, t.text.Render(sanitize(text)))
}

// statusLine renders the bottom status bar: network, current nick, active
// buffer, and a scroll indicator. width pads it to the full terminal width.
func (t theme) statusLine(network, nick, buffer string, scrolled bool, typing string, width int) string {
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
	if typing != "" {
		parts = append(parts, t.info.Render(typing))
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

// sanitize neutralizes terminal control sequences in attacker-controlled display
// text before it is stored in scrollback and written to the terminal. It is a
// thin alias for client.SanitizeTerminal, the single shared implementation used
// by both Lurk front-ends (this TUI and the -plain line client); see that
// function for the rationale and exact behavior. It must be applied to raw text
// *before* Lip Gloss styling, never after, or it would strip the renderer's own
// SGR escapes.
func sanitize(s string) string {
	return client.SanitizeTerminal(s)
}

// stripANSI removes ANSI escape sequences from s. Its primary use is re-rendering
// a highlighted line over a single background style, where leftover SGR codes
// from styledNick would otherwise interrupt the banner; it is also used to
// measure visible width for truncation. Server text is sanitized before it is
// styled, so in practice stripANSI only ever sees the renderer's own CSI ... m
// sequences — but it is written to be a correct, conservative stripper for any
// input, recognizing the CSI, OSC, and DCS/PM/APC/SOS string forms (not just
// CSI) so an escape can never slip through.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		// s[i] is ESC. The following byte selects the sequence form.
		if i+1 >= len(s) {
			break // lone trailing ESC: drop it
		}
		switch s[i+1] {
		case '[': // CSI: ESC [ ... <final byte 0x40–0x7e>
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			// i now rests on the final byte (or len); the loop's i++ skips it.
		case ']', 'P', 'X', '^', '_':
			// OSC / DCS / SOS / PM / APC: a string sequence terminated by ST
			// (ESC \) or, for OSC, a BEL (0x07).
			i += 2
			for i < len(s) {
				if s[i] == 0x07 { // BEL terminator
					break
				}
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST
					i++ // consume ESC; the loop's i++ skips the '\'
					break
				}
				i++
			}
		default:
			// Any other Fe/two-byte escape (charset selection, ESC =, …): drop
			// the ESC and its single following byte.
			i++
		}
	}
	return b.String()
}
