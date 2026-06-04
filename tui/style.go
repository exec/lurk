package tui

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"lurk/client"
	"lurk/irc"
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
		if isNumericCommand(ev.Command()) {
			return t.formatNumeric(ev), false
		}
		// Unhandled non-numeric command: show the trailing text.
		body := ev.Text()
		if body == "" {
			body = strings.Join(ev.Message.Params, " ")
		}
		return fmt.Sprintf("%s %s", ts, t.dim.Render(sanitize(body))), false
	}
}

// isNumericCommand reports whether cmd is a three-digit numeric reply code.
func isNumericCommand(cmd string) bool {
	if len(cmd) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if cmd[i] < '0' || cmd[i] > '9' {
			return false
		}
	}
	return true
}

// formatNumeric renders a numeric (3-digit) server reply. Every numeric carries
// the recipient's own nick as its first parameter; a trailing-only render would
// drop the data fields the trailing text merely labels — WHOIS idle seconds, the
// actual host/IP, the channel list. This composes the useful values for the
// WHOIS/AWAY family and, for any other numeric, shows every parameter after the
// recipient nick so nothing (e.g. the "<nick>" an error reply is about) is lost.
func (t theme) formatNumeric(ev client.Event) string {
	ts := t.timestamp.Render(ev.Time().Local().Format("15:04"))
	subj := sanitize(ev.Param(1)) // the nick a WHOIS line is about
	text := sanitize(ev.Text())

	// WHOIS replies render as a compact block: RPL_WHOISUSER (311) opens with a
	// header carrying the nick, the detail numerics are shown indented WITHOUT
	// repeating the nick (the burst arrives contiguously, bracketed by the header
	// and the RPL_ENDOFWHOIS footer, so the subject stays clear), and the footer
	// carries the nick again. RPL_AWAY (301) keeps the nick inline and unindented
	// because it also arrives standalone (when you message an away user), not just
	// within a whois.
	var body string
	switch ev.Command() {
	case irc.RPL_WHOISUSER: // <me> <nick> <user> <host> * :<realname>
		body = fmt.Sprintf("whois %s — %s@%s (%s)", subj, sanitize(ev.Param(2)), sanitize(ev.Param(3)), text)
	case irc.RPL_WHOISSERVER: // <me> <nick> <server> :<server info>
		body = whoisDetail("server", fmt.Sprintf("%s (%s)", sanitize(ev.Param(2)), text))
	case irc.RPL_WHOISACCOUNT: // <me> <nick> <account> :is logged in as
		body = whoisDetail("account", sanitize(ev.Param(2)))
	case irc.RPL_WHOISIDLE: // <me> <nick> <secs> [<signon>] :seconds idle, signon time
		body = whoisDetail("idle", idleSummary(ev.Param(2), ev.Param(3)))
	case irc.RPL_WHOISCHANNELS: // <me> <nick> :<channels>
		body = whoisDetail("channels", text)
	case irc.RPL_WHOISACTUALLY: // <me> <nick> [<user@host>] <ip> :Actual ...
		body = whoisDetail("actually", actuallyValue(midParams(ev)))
	case irc.RPL_WHOISOPERATOR, irc.RPL_WHOISSECURE: // <me> <nick> :is an operator / secure
		body = whoisDetail("", text)
	case irc.RPL_AWAY: // <me> <nick> :<away message> — also arrives standalone
		body = subj + " is away: " + text
	case irc.RPL_ENDOFWHOIS: // <me> <nick> :End of /WHOIS list
		body = "end of whois — " + subj
	default:
		// Any other numeric: show all params after the recipient nick so middle
		// data is not dropped (e.g. "<nick> No such nick" for an error reply).
		if p := ev.Message.Params; len(p) >= 2 {
			body = sanitize(strings.Join(p[1:], " "))
		} else {
			body = text
		}
	}
	return fmt.Sprintf("%s %s", ts, t.dim.Render(body))
}

// midParams returns the parameters of a numeric that sit between the subject nick
// (index 1) and the trailing description (the last param) — the data fields a
// reply like RPL_WHOISACTUALLY carries. It returns nil when there are none.
func midParams(ev client.Event) []string {
	p := ev.Message.Params
	if len(p) <= 3 {
		return nil
	}
	return p[2 : len(p)-1]
}

// whoisDetail formats one indented WHOIS detail row: "  · <label> <value>", with
// the label padded to a column so successive rows align. An empty label yields
// "  · <value>" for replies that are already a full phrase (operator/secure).
func whoisDetail(label, value string) string {
	if label == "" {
		return "  · " + value
	}
	return fmt.Sprintf("  · %-8s %s", label, value)
}

// actuallyValue renders the RPL_WHOISACTUALLY data fields (the real host and IP):
// "<host> (<ip>)", collapsing to just the host when the IP is already part of it
// (e.g. "~u@10.0.0.29" already contains "10.0.0.29"), to avoid showing it twice.
func actuallyValue(mid []string) string {
	switch len(mid) {
	case 0:
		return ""
	case 1:
		return sanitize(mid[0])
	default:
		host := sanitize(mid[0])
		ip := sanitize(mid[len(mid)-1])
		if ip == "" || strings.Contains(host, ip) {
			return host
		}
		return host + " (" + ip + ")"
	}
}

// idleSummary renders an RPL_WHOISIDLE value: the idle seconds and, when the
// optional signon timestamp is present and valid, the local sign-on time. The
// "idle" label is supplied by the caller (whoisDetail), so it is not repeated.
func idleSummary(secs, signon string) string {
	out := "?"
	if secs != "" {
		out = sanitize(secs) + "s"
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(signon), 10, 64); err == nil && n > 0 {
		out += ", signed on " + time.Unix(n, 0).Local().Format("2006-01-02 15:04")
	}
	return out
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

// formatInfo renders a local informational line (command output / error). The
// text may be intentionally multi-line (e.g. /help puts the keybindings and the
// command list on separate rows), so it is sanitized per line and rejoined:
// sanitize strips '\n' as a control byte — correct for server message bodies, but
// here the breaks are ours and must survive, or the rows weld together.
func (t theme) formatInfo(text string) string {
	ts := t.timestamp.Render(time.Now().Format("15:04"))
	segs := strings.Split(text, "\n")
	for i, s := range segs {
		segs[i] = sanitize(s)
	}
	return fmt.Sprintf("%s %s", ts, t.info.Render(strings.Join(segs, "\n")))
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
// the \x01ACTION ... \x01 framing removed. It is a thin alias for
// client.CTCPAction, the shared parser used by both front-ends.
func ctcpAction(body string) (string, bool) {
	return client.CTCPAction(body)
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
