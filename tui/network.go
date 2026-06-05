package tui

import "github.com/exec/lurk/client"

// network is one connected IRC server within the TUI: its client, its event
// stream, and a display name. The model holds a slice of these; each Buffer
// carries a back-pointer to the network it belongs to, so two networks can host
// channels of the same name without colliding. Routing, the nicklist, and
// outbound commands all resolve through the owning network's client.
type network struct {
	name string // display label (network name, else the dialed address)
	cli  *client.Client
	sub  <-chan client.Event
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
// known, else the seed name.
func (n *network) label() string {
	if n != nil && n.cli != nil {
		if nm := n.cli.Network(); nm != "" {
			return nm
		}
	}
	if n != nil {
		return n.name
	}
	return "(server)"
}
