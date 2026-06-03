package client

import "strings"

// CTCPAction reports whether body is a CTCP ACTION (the "/me" form) and, if so,
// returns the inner action text with the \x01ACTION … \x01 framing removed — so a
// front-end can render "* nick waves" instead of a normal "<nick> …" message
// line. A non-ACTION body (a plain message, or another CTCP type such as VERSION)
// returns ("", false).
//
// The returned text is still peer-supplied and must be passed through
// SanitizeTerminal before display, the same as any other message body.
func CTCPAction(body string) (text string, ok bool) {
	const prefix = "\x01ACTION "
	if !strings.HasPrefix(body, prefix) {
		return "", false
	}
	text = strings.TrimPrefix(body, prefix)
	text = strings.TrimSuffix(text, "\x01")
	return text, true
}
