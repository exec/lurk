package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"

	"github.com/exec/lurk/config"
)

// launcher_form.go is the add/edit-network form used by the launcher: a list of
// editable fields (text inputs, a TLS toggle, and a SASL-mechanism cycle) and the
// conversion back to a config.Network on save.

// saslMechs is the SASL option cycle shown in the form; index 0 ("") disables it.
var saslMechs = []string{"", "PLAIN", "EXTERNAL"}

type fieldKind int

const (
	kindText fieldKind = iota // a textinput
	kindBool                  // a toggle ([x]/[ ])
	kindMech                  // a cycle over saslMechs
	kindSep                   // a non-interactive section separator (skipped by focus)
)

// formField is one row of the network form.
type formField struct {
	label string
	kind  fieldKind
	input textinput.Model // kindText
	on    bool            // kindBool
	mech  int             // kindMech: index into saslMechs
}

// netForm is the add/edit form's state.
type netForm struct {
	fields []formField
	idx    int    // focused field
	err    string // validation error to display
	title  string // "Add network" or "Edit <name>"
}

// Field indices — must stay in sync with the order built in newNetForm.
const (
	fName = iota
	fAddr
	fTLS
	fNick
	fUser
	fReal
	fPass
	fMech
	fSaslUser
	fSaslPass
	fChannels
	fBounceSep    // non-interactive section separator before the bounce block
	fBounceAddr   // lurkd bouncer host:port (empty = direct connection)
	fBounceNetID  // bouncer-side network id (BOUNCER BIND target)
	fBounceClient // per-client cursor id (@client suffix)
)

// newNetForm builds a form for the given network (zero value when adding). Blank
// identity fields prefill from def; a brand-new network defaults TLS on.
func newNetForm(n config.Network, def config.Identity) *netForm {
	mk := func(val string, secret bool) formField {
		ti := textinput.New()
		ti.Prompt = ""
		ti.SetWidth(40)
		ti.SetValue(val)
		if secret {
			ti.EchoMode = textinput.EchoPassword
		}
		return formField{kind: kindText, input: ti}
	}
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}

	mech := 0
	for i := range saslMechs {
		if saslMechs[i] != "" && strings.EqualFold(saslMechs[i], n.SASL.Mechanism) {
			mech = i
		}
	}

	title := "Add network"
	if n.Name != "" {
		title = "Edit " + n.Name
	}

	f := &netForm{title: title}
	f.fields = []formField{
		fName:         withLabel("Name", mk(n.Name, false)),
		fAddr:         withLabel("Address", mk(n.Addr, false)),
		fTLS:          {label: "TLS", kind: kindBool, on: newTLSDefault(n)},
		fNick:         withLabel("Nick", mk(or(n.Nick, def.Nick), false)),
		fUser:         withLabel("User", mk(or(n.User, def.User), false)),
		fReal:         withLabel("Realname", mk(or(n.Realname, def.Realname), false)),
		fPass:         withLabel("Server pass", mk(n.Pass, true)),
		fMech:         {label: "SASL", kind: kindMech, mech: mech},
		fSaslUser:     withLabel("SASL user", mk(n.SASL.Username, false)),
		fSaslPass:     withLabel("SASL pass", mk(n.SASL.Password, true)),
		fChannels:     withLabel("Channels", mk(strings.Join(n.Channels, " "), false)),
		fBounceSep:    {label: "── Bouncer ──", kind: kindSep},
		fBounceAddr:   withLabel("Bounce addr", mk(n.Bounce.Addr, false)),
		fBounceNetID:  withLabel("Bounce NetID", mk(bounceNetIDStr(n.Bounce.NetID), false)),
		fBounceClient: withLabel("Bounce client", mk(n.Bounce.ClientID, false)),
	}
	f.fields[fName].input.Focus()
	return f
}

// bounceNetIDStr converts a BounceConfig.NetID integer to its display string:
// blank for zero (not configured) or the decimal representation.
func bounceNetIDStr(id int) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("%d", id)
}

// withLabel attaches a label to a freshly-built field.
func withLabel(label string, f formField) formField {
	f.label = label
	return f
}

// newTLSDefault returns the TLS toggle default: on for a brand-new network,
// otherwise the network's stored value.
func newTLSDefault(n config.Network) bool {
	if n.Name == "" && n.Addr == "" {
		return true
	}
	return n.TLS
}

// focus moves the focused field by dir (+1 / -1), wrapping, and updates the
// textinputs' focus state so the cursor follows. kindSep rows are skipped
// so focus never lands on a non-interactive separator.
func (f *netForm) focus(dir int) {
	if f.fields[f.idx].kind == kindText {
		f.fields[f.idx].input.Blur()
	}
	n := len(f.fields)
	f.idx = (f.idx + dir + n) % n
	// Skip over any separator rows so focus always lands on an interactive
	// field even when separators are adjacent.
	for f.fields[f.idx].kind == kindSep {
		f.idx = (f.idx + dir + n) % n
	}
	if f.fields[f.idx].kind == kindText {
		f.fields[f.idx].input.Focus()
	}
}

// toNetwork validates the form and converts it to a config.Network. Name and
// Address are required; passwords keep their exact value (not trimmed). When a
// Bounce addr is set, Bounce NetID is required and must be a positive integer:
// the connection path (cmd/lurk networkToConfig) only routes through the
// bouncer when both are set, so saving an addr without an id would silently
// dial the network directly while looking bouncer-configured.
func (f *netForm) toNetwork() (config.Network, error) {
	val := func(i int) string { return strings.TrimSpace(f.fields[i].input.Value()) }

	name, addr := val(fName), val(fAddr)
	if name == "" || addr == "" {
		return config.Network{}, errors.New("Name and Address are required")
	}

	n := config.Network{
		Name:     name,
		Addr:     addr,
		TLS:      f.fields[fTLS].on,
		Nick:     val(fNick),
		User:     val(fUser),
		Realname: val(fReal),
		Pass:     f.fields[fPass].input.Value(),
		Channels: splitChannels(val(fChannels)),
	}
	if mech := saslMechs[f.fields[fMech].mech]; mech != "" {
		n.SASL = config.SASL{
			Mechanism: mech,
			Username:  val(fSaslUser),
			Password:  f.fields[fSaslPass].input.Value(),
		}
	}
	if baddr := val(fBounceAddr); baddr != "" {
		raw := val(fBounceNetID)
		if raw == "" {
			return config.Network{}, errors.New("Bounce NetID is required when a Bounce addr is set")
		}
		netID, err := strconv.Atoi(raw)
		if err != nil || netID <= 0 {
			return config.Network{}, fmt.Errorf("Bounce NetID %q is not a positive number", raw)
		}
		n.Bounce = config.BounceConfig{
			Addr:     baddr,
			NetID:    netID,
			ClientID: val(fBounceClient),
		}
	}
	return n, nil
}

// splitChannels parses a space-or-comma separated channel list into a slice.
func splitChannels(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
	if len(fields) == 0 {
		return nil
	}
	return fields
}
