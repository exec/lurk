package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// command.go is the slash-command parser. It turns one editor line into an
// action value (consumed by app.go's applyAction, tui-core) plus an optional
// Cmd. The split of responsibility, agreed with tui-core, is:
//
//   - Lines that change buffer lifecycle or send a normal PRIVMSG return an
//     action (actionSend / actionOpen / actionSwitch / actionClose); the core's
//     applyAction performs the protocol side effect and local echo. This keeps
//     the PRIVMSG-send-and-echo path in exactly one place.
//   - Protocol-only commands with no buffer effect (/nick /topic /names /raw)
//     call the client directly here and return an actionInfo status line (or
//     actionNone), because routing them through actionSend would misuse the
//     echo path.
//   - /quit returns the tea.Quit Cmd (after sending QUIT) so the program exits.
//
// The command set and the // literal-escape are modeled on senpai
// (reference/senpai/commands.go); the parsing is reimplemented for our action
// model rather than copied.

// command describes one slash command: how many arguments it takes, a usage
// hint shown on misuse, a one-line description, and the handler that turns the
// argument string into an action (+ Cmd). minArgs/maxArgs bound the number of
// whitespace-split fields; maxArgs == -1 means "rest of line is one argument".
type command struct {
	minArgs int
	maxArgs int
	usage   string
	desc    string
	handle  func(m model, args []string, rest string) (action, tea.Cmd)
}

// argsUnlimited is the maxArgs sentinel for "take the rest of the line as a
// single trailing argument" (used by /me, /msg, /topic, /raw, /part reason).
const argsUnlimited = -1

// commands is the registry, keyed by upper-case command name. It is built once
// at init. Handlers receive the already-split args plus rest (the untouched
// remainder after the command word) for commands that want the raw trailing
// text.
var commands map[string]*command

func init() {
	commands = map[string]*command{
		"JOIN": {
			minArgs: 1, maxArgs: 2,
			usage: "<channel> [key]",
			desc:  "join a channel",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				if m.cli != nil {
					_ = m.cli.Join(args[0])
				}
				// Open and focus the channel buffer optimistically; the JOIN
				// echo / NAMES will populate it.
				return action{kind: actionOpen, target: args[0], bufferKind: BufferChannel}, nil
			},
		},
		"PART": {
			minArgs: 0, maxArgs: argsUnlimited,
			usage:  "[channel] [reason]",
			desc:   "leave a channel (defaults to the current one)",
			handle: cmdPart,
		},
		"MSG": {
			minArgs: 2, maxArgs: 2,
			usage: "<target> <message>",
			desc:  "send a message to a target without opening its buffer",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				return action{kind: actionSend, target: args[0], text: args[1]}, nil
			},
		},
		"QUERY": {
			minArgs: 1, maxArgs: 2,
			usage:  "<nick> [message]",
			desc:   "open a private-message buffer with a user",
			handle: cmdQuery,
		},
		"NICK": {
			minArgs: 1, maxArgs: 1,
			usage: "<nickname>",
			desc:  "change your nickname",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				if i := strings.IndexAny(args[0], " :"); i >= 0 {
					return usageError("NICK", commands["NICK"]), nil
				}
				if m.cli != nil {
					_ = m.cli.SetNick(args[0])
				}
				return action{kind: actionNone}, nil
			},
		},
		"ME": {
			minArgs: 1, maxArgs: argsUnlimited,
			usage: "<action text>",
			desc:  "send a CTCP ACTION (\"* you wave\") to the current buffer",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				// CTCP ACTION framing: \x01ACTION <text>\x01. Routed as a normal
				// send so the core's echo path renders it; the view layer detects
				// the \x01ACTION prefix to format it as an action line.
				text := fmt.Sprintf("\x01ACTION %s\x01", rest)
				return action{kind: actionSend, text: text}, nil
			},
		},
		"TOPIC": {
			minArgs: 0, maxArgs: argsUnlimited,
			usage:  "[new topic]",
			desc:   "show or set the topic of the current channel",
			handle: cmdTopic,
		},
		"NAMES": {
			minArgs: 0, maxArgs: 0,
			usage:  "",
			desc:   "request the member list of the current channel",
			handle: cmdNames,
		},
		"WHOIS": {
			minArgs: 0, maxArgs: 1,
			usage:  "[nick]",
			desc:   "look up a user (defaults to the current PM correspondent)",
			handle: cmdWhois,
		},
		"AWAY": {
			minArgs: 0, maxArgs: argsUnlimited,
			usage:  "[reason]",
			desc:   "set an away status, or clear it when given no reason",
			handle: cmdAway,
		},
		"CLOSE": {
			minArgs: 0, maxArgs: 1,
			usage:  "[buffer]",
			desc:   "close the current buffer (parts a channel first)",
			handle: cmdClose,
		},
		"QUIT": {
			minArgs: 0, maxArgs: argsUnlimited,
			usage: "[reason]",
			desc:  "disconnect and exit lurk",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				if m.cli != nil {
					_ = m.cli.Quit(rest)
				}
				return action{kind: actionNone}, tea.Quit
			},
		},
		"RAW": {
			minArgs: 1, maxArgs: argsUnlimited,
			usage: "<line>",
			desc:  "send a raw protocol line to the server",
			handle: func(m model, args []string, rest string) (action, tea.Cmd) {
				if m.cli != nil {
					if err := m.cli.SendRaw(rest); err != nil {
						return infoAction(fmt.Sprintf("raw: %v", err)), nil
					}
				}
				return action{kind: actionNone}, nil
			},
		},
		"HELP": {
			minArgs: 0, maxArgs: 1,
			usage:  "[command]",
			desc:   "list keybindings and commands, or show usage for one command",
			handle: cmdHelp,
		},
	}
}

// cmdHelp implements /help. With no argument it prints the keybinding digest and
// the list of commands; with a command name it prints that command's usage.
func cmdHelp(m model, args []string, rest string) (action, tea.Cmd) {
	if len(args) == 1 {
		name := strings.ToUpper(strings.TrimPrefix(args[0], "/"))
		cmd, ok := commands[name]
		if !ok {
			return infoAction(fmt.Sprintf("no such command /%s", strings.ToLower(name))), nil
		}
		return infoAction(strings.TrimSpace(fmt.Sprintf("/%s %s — %s", strings.ToLower(name), cmd.usage, cmd.desc))), nil
	}
	text := "keys: " + m.keys.summary() + "\ncommands: " + commandNames() + " (try /help <command>)"
	return infoAction(text), nil
}

// commandNames returns the registered command names as a sorted, "/"-prefixed,
// space-joined string for the /help listing.
func commandNames() string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, "/"+strings.ToLower(name))
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// runLine parses and dispatches one submitted editor line. It is the entry
// point input.go calls on Enter. The returned action is applied by the core;
// the Cmd (if any) is returned up to Bubble Tea.
//
// Rules, mirroring senpai:
//   - "" -> nothing.
//   - A leading "//" is a literal message beginning with a slash: the first
//     slash is dropped and the rest sent as a normal PRIVMSG.
//   - "/word args" is a command; an unknown word yields an unknown-command
//     info line, and bad arguments yield the command's usage hint.
//   - Anything else is a normal message to the active buffer.
func runLine(m model, line string) (action, tea.Cmd) {
	if line == "" {
		return action{kind: actionNone}, nil
	}

	name, rest, isCmd := splitCommand(line)
	if !isCmd {
		// Plain message (or // literal) to the active buffer; empty target means
		// "active buffer" per the core contract.
		return action{kind: actionSend, text: rest}, nil
	}
	if name == "" {
		return infoAction("a lone / is not a command; use // to send a literal slash"), nil
	}

	cmd, ok := commands[name]
	if !ok {
		return infoAction(fmt.Sprintf("unknown command /%s — type /help for the command list", strings.ToLower(name))), nil
	}

	args := splitArgs(rest, cmd.maxArgs)
	if len(args) < cmd.minArgs {
		return usageError(name, cmd), nil
	}
	return cmd.handle(m, args, rest)
}

// splitCommand splits a line into an upper-cased command name and the remaining
// argument text. It reports isCmd=false for non-command lines: a line not
// starting with '/', or a "//"-escaped literal (whose leading slash is stripped
// and returned as rest). This matches senpai's parseCommand semantics.
func splitCommand(line string) (name, rest string, isCmd bool) {
	if line == "" || line[0] != '/' {
		return "", line, false
	}
	if len(line) > 1 && line[1] == '/' {
		// "//text" -> literal "/text".
		return "", line[1:], false
	}
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		i = len(line)
	}
	name = strings.ToUpper(line[1:i])
	rest = strings.TrimLeft(line[i:], " ")
	return name, rest, true
}

// splitArgs splits s into at most max whitespace-separated fields. A max of
// argsUnlimited (-1) returns the whole trimmed string as a single field, and a
// positive max keeps the final field's interior spaces intact (so the trailing
// argument — a message or reason — is preserved verbatim). It is the analogue of
// senpai's fieldsN.
func splitArgs(s string, max int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if max == argsUnlimited {
		return []string{s}
	}
	if max <= 1 {
		return []string{s}
	}
	fields := make([]string, 0, max)
	for len(fields) < max-1 {
		i := strings.IndexByte(s, ' ')
		if i < 0 {
			break
		}
		fields = append(fields, s[:i])
		s = strings.TrimLeft(s[i+1:], " ")
		if s == "" {
			break
		}
	}
	if s != "" {
		fields = append(fields, s)
	}
	return fields
}

// cmdPart leaves a channel. With no channel argument it parts the active buffer
// (if it is a channel); a leading channel argument overrides, and the remainder
// is the part reason. Closing the buffer is left to the JOIN/PART echo + the
// core's routing; here we only emit the protocol and a close request.
func cmdPart(m model, args []string, rest string) (action, tea.Cmd) {
	channel := m.activeBuffer().Title
	reason := ""
	if len(args) > 0 {
		if isChannel(args[0]) {
			channel = args[0]
			reason = strings.TrimSpace(strings.TrimPrefix(rest, args[0]))
		} else {
			reason = rest
		}
	}
	if !isChannel(channel) {
		return infoAction("not a channel; use /part <channel>"), nil
	}
	if m.cli != nil {
		if reason != "" {
			_ = m.cli.PartReason(reason, channel)
		} else {
			_ = m.cli.Part(channel)
		}
	}
	return action{kind: actionClose, target: channel}, nil
}

// cmdQuery opens (or focuses) a PM buffer with a nick. A channel argument is
// rejected — channels are joined, not queried. An optional trailing message is
// sent to the nick after the buffer opens.
func cmdQuery(m model, args []string, rest string) (action, tea.Cmd) {
	target := args[0]
	if isChannel(target) {
		return infoAction("cannot /query a channel — use /join"), nil
	}
	if len(args) > 1 && m.cli != nil {
		_ = m.cli.Privmsg(target, args[1])
	}
	return action{kind: actionOpen, target: target, bufferKind: BufferPM}, nil
}

// cmdTopic shows or sets the current channel's topic. With no argument it asks
// the server for the topic (RPL_TOPIC routes to the buffer); with an argument it
// sets the topic.
func cmdTopic(m model, args []string, rest string) (action, tea.Cmd) {
	channel := m.activeBuffer().Title
	if !isChannel(channel) {
		return infoAction("not a channel; switch to a channel buffer first"), nil
	}
	if m.cli == nil {
		return action{kind: actionNone}, nil
	}
	if len(args) == 0 {
		_ = m.cli.RequestTopic(channel)
		return action{kind: actionNone}, nil
	}
	// Set: the rest of the line is the new topic (spaces preserved).
	_ = m.cli.SetTopic(channel, rest)
	return action{kind: actionNone}, nil
}

// cmdWhois looks up a user. With an explicit nick it whoises that nick; with no
// argument it defaults to the current PM correspondent (the active buffer's
// title when it is a PM). The WHOIS reply numerics route to the active buffer.
func cmdWhois(m model, args []string, rest string) (action, tea.Cmd) {
	nick := ""
	if len(args) > 0 {
		// WHOIS takes a single nick; ignore any extra words the user typed so we
		// don't send a space-bearing (and thus bogus) target.
		if f := strings.Fields(args[0]); len(f) > 0 {
			nick = f[0]
		}
	} else if b := m.activeBuffer(); b.Kind == BufferPM {
		nick = b.Title
	}
	if nick == "" {
		return infoAction("usage: /whois <nick>"), nil
	}
	if m.cli != nil {
		_ = m.cli.Whois(nick)
	}
	return action{kind: actionNone}, nil
}

// cmdAway sets or clears the client's away status. With a reason it marks the
// client away; with no argument it clears the away status. A local info line
// confirms the change (the server's 305/306 numerics also route to the server
// buffer).
func cmdAway(m model, args []string, rest string) (action, tea.Cmd) {
	if m.cli != nil {
		_ = m.cli.Away(rest)
	}
	if rest == "" {
		return infoAction("marked back"), nil
	}
	return infoAction("marked away: " + rest), nil
}

// cmdNames requests the member list of the active channel. The RPL_NAMREPLY /
// RPL_ENDOFNAMES replies route through the event bridge to the buffer.
func cmdNames(m model, args []string, rest string) (action, tea.Cmd) {
	channel := m.activeBuffer().Title
	if !isChannel(channel) {
		return infoAction("not a channel; switch to a channel buffer first"), nil
	}
	if m.cli != nil {
		_ = m.cli.Names(channel)
	}
	return action{kind: actionNone}, nil
}

// cmdClose closes a buffer. A buffer argument names which (defaults to active).
// If the target is a channel we PART it first so the server state matches;
// closing the server buffer (index 0) is a no-op handled by the core.
func cmdClose(m model, args []string, rest string) (action, tea.Cmd) {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	channel := target
	if channel == "" {
		channel = m.activeBuffer().Title
	}
	if isChannel(channel) && m.cli != nil {
		_ = m.cli.Part(channel)
	}
	return action{kind: actionClose, target: target}, nil
}

// usageError builds the info action shown when a command is misused: its name
// and usage hint. It is the single place the "usage:" wording lives.
func usageError(name string, cmd *command) action {
	if cmd.usage == "" {
		return infoAction(fmt.Sprintf("usage: /%s", strings.ToLower(name)))
	}
	return infoAction(fmt.Sprintf("usage: /%s %s", strings.ToLower(name), cmd.usage))
}

// infoAction wraps a status string as a local info line for the active buffer.
func infoAction(text string) action {
	return action{kind: actionInfo, text: text}
}
