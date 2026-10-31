package tui

import (
	"strings"

	"github.com/exec/lurk/client"
)

// network is one connected IRC server within the TUI: its client, its event
// stream, and a display name. The model holds a slice of these; each Buffer
// carries a back-pointer to the network it belongs to, so two networks can host
// channels of the same name without colliding. Routing, the nicklist, and
// outbound commands all resolve through the owning network's client.
type network struct {
	name    string // display label (network name, else the dialed address)
	cli     *client.Client
	sub     <-chan client.Event
	history historyState
	// disconnecting distinguishes /disconnect from an unexpected terminal drop.
	disconnecting    bool
	disconnectReason string

	savedName  string // config identity; never the server-controlled display name
	highlights map[string]bool
	ignored    map[string]bool
}

func newNetwork(name string, cli *client.Client, sub <-chan client.Event) *network {
	n := &network{name: name, cli: cli, sub: sub, highlights: wordSet(nil), ignored: wordSet(nil)}
	if cli != nil {
		n.highlights = wordSet(cli.Highlights())
		n.ignored = wordSet(cli.Ignores())
	}
	return n
}

func wordSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			set[asciiLower(w)] = true
		}
	}
	return set
}

func (n *network) isIgnored(nick string) bool {
	return n != nil && n.ignored[asciiLower(nick)]
}

// nick returns the network's current nick, tolerating a nil network/client (the
// nil-client case appears in pure rendering tests).
func (n *network) nick() string {
	if n != nil && n.cli != nil {
		return n.cli.Nick()
	}
	return ""
}

// label returns a display name for the network: its tracked network name once
// known, else the saved profile or seed name.
func (n *network) label() string {
	if n != nil && n.cli != nil {
		if nm := n.cli.Network(); nm != "" {
			return nm
		}
	}
	if n != nil {
		if n.savedName != "" {
			return n.savedName
		}
		return n.name
	}
	return "(server)"
}
