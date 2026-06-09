package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/config"
)

// press feeds one key string ("enter", "a", "esc", …) to the launcher model and
// returns the updated model.
func press(t *testing.T, m launcherModel, key string) launcherModel {
	t.Helper()
	var msg tea.KeyPressMsg
	switch key {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEsc}
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	case "up":
		msg = tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	case "space":
		msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	default:
		r := []rune(key)[0]
		msg = tea.KeyPressMsg{Code: r, Text: key}
	}
	tm, _ := m.Update(msg)
	return tm.(launcherModel)
}

// typeText types each rune of s into the model (used to fill text fields).
func typeText(t *testing.T, m launcherModel, s string) launcherModel {
	t.Helper()
	for _, r := range s {
		m = press(t, m, string(r))
	}
	return m
}

func newLauncher(t *testing.T, store *config.File) launcherModel {
	t.Helper()
	path := t.TempDir() + "/config.json"
	return launcherModel{store: store, path: path, mode: modeList}
}

// TestLauncherSelectReturnsNetwork: Enter on a saved network selects it.
func TestLauncherSelectReturnsNetwork(t *testing.T) {
	store := &config.File{Networks: []config.Network{
		{Name: "Libera", Addr: "irc.libera.chat:6697"},
		{Name: "Local", Addr: "127.0.0.1:6667"},
	}}
	m := newLauncher(t, store)
	m.sel = 1
	m = press(t, m, "enter")
	if m.chosen == nil {
		t.Fatal("Enter on a network did not set chosen")
	}
	if m.chosen.Name != "Local" {
		t.Errorf("chosen = %q, want Local", m.chosen.Name)
	}
}

// TestLauncherAddCreatesNetwork: 'a' opens the form, filling Name+Address and
// saving creates a network and persists it.
func TestLauncherAddCreatesNetwork(t *testing.T) {
	store := &config.File{}
	m := newLauncher(t, store)

	m = press(t, m, "a")
	if m.mode != modeForm {
		t.Fatalf("'a' did not open the form (mode = %v)", m.mode)
	}
	// Name field is focused first.
	m = typeText(t, m, "Libera")
	m = press(t, m, "tab") // -> Address
	m = typeText(t, m, "irc.libera.chat:6697")
	m = press(t, m, "enter") // save

	if m.mode != modeList {
		t.Fatalf("after save, mode = %v, want modeList", m.mode)
	}
	if len(store.Networks) != 1 {
		t.Fatalf("save created %d networks, want 1", len(store.Networks))
	}
	got := store.Networks[0]
	if got.Name != "Libera" || got.Addr != "irc.libera.chat:6697" {
		t.Errorf("saved network = %+v", got)
	}

	// Reload from disk to confirm it persisted.
	reloaded, err := config.LoadFrom(m.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Networks) != 1 || reloaded.Networks[0].Name != "Libera" {
		t.Errorf("persisted config = %+v", reloaded.Networks)
	}
}

// TestLauncherAddRequiresNameAndAddr: saving with empty fields shows an error and
// stays in the form.
func TestLauncherFormValidation(t *testing.T) {
	m := newLauncher(t, &config.File{})
	m = press(t, m, "a")
	m = press(t, m, "enter") // save with blank Name/Address
	if m.mode != modeForm {
		t.Fatalf("blank save left form (mode = %v), should stay to show error", m.mode)
	}
	if m.form == nil || m.form.err == "" {
		t.Error("blank save did not set a validation error")
	}
}

// TestLauncherEscCancelsForm: Esc abandons the form without saving.
func TestLauncherEscCancelsForm(t *testing.T) {
	store := &config.File{}
	m := newLauncher(t, store)
	m = press(t, m, "a")
	m = typeText(t, m, "Temp")
	m = press(t, m, "esc")
	if m.mode != modeList {
		t.Fatalf("Esc did not return to list (mode = %v)", m.mode)
	}
	if len(store.Networks) != 0 {
		t.Errorf("Esc still saved a network: %+v", store.Networks)
	}
}

// TestLauncherDeleteConfirmRemoves: 'd' then 'y' removes the selected network.
func TestLauncherDeleteConfirmRemoves(t *testing.T) {
	store := &config.File{Networks: []config.Network{
		{Name: "Libera", Addr: "a:1"},
		{Name: "Local", Addr: "b:2"},
	}}
	m := newLauncher(t, store)
	m.sel = 0
	m = press(t, m, "d")
	if m.mode != modeConfirm {
		t.Fatalf("'d' did not open confirm (mode = %v)", m.mode)
	}
	m = press(t, m, "y")
	if m.mode != modeList {
		t.Fatalf("confirm did not return to list (mode = %v)", m.mode)
	}
	if len(store.Networks) != 1 || store.Networks[0].Name != "Local" {
		t.Errorf("after delete: %+v", store.Networks)
	}
}

// TestLauncherDeleteCancel: 'd' then 'n' keeps the network.
func TestLauncherDeleteCancel(t *testing.T) {
	store := &config.File{Networks: []config.Network{{Name: "Libera", Addr: "a:1"}}}
	m := newLauncher(t, store)
	m = press(t, m, "d")
	m = press(t, m, "n")
	if m.mode != modeList {
		t.Fatalf("cancel left mode = %v", m.mode)
	}
	if len(store.Networks) != 1 {
		t.Errorf("cancel still deleted: %+v", store.Networks)
	}
}

// TestLauncherAddRowEnter: Enter on the trailing "add network" row opens the form
// rather than selecting a network.
func TestLauncherAddRowEnter(t *testing.T) {
	store := &config.File{Networks: []config.Network{{Name: "Libera", Addr: "a:1"}}}
	m := newLauncher(t, store)
	m.sel = len(store.Networks) // the add row
	m = press(t, m, "enter")
	if m.mode != modeForm {
		t.Fatalf("Enter on add row mode = %v, want modeForm", m.mode)
	}
	if m.chosen != nil {
		t.Error("add row should not choose a network")
	}
}

// TestLauncherBounceFieldsRoundTrip: filling the bounce fields in the form
// produces a BounceConfig that round-trips correctly through toNetwork().
// It drives the form via tab navigation and typed text, exercising the full
// Update path (including the kindSep skip), then asserts toNetwork() output.
func TestLauncherBounceFieldsRoundTrip(t *testing.T) {
	// Build a form pre-filled with Name and Address so validation passes, then
	// navigate to the bounce fields and fill them via typed input.
	n := config.Network{Name: "BounceNet", Addr: "irc.libera.chat:6697"}
	f := newNetForm(n, config.Identity{})

	// Advance focus to fBounceAddr. The sep at fBounceSep is skipped
	// automatically by focus(), so it takes (fBounceAddr - 1) steps from
	// fName to land on fBounceAddr (11 steps: indices 1..10 plus the sep
	// skip at 11 → 12).
	for step := 0; step < fBounceAddr-fName-1; step++ {
		f.focus(1)
	}
	if f.idx != fBounceAddr {
		t.Fatalf("focus landed at idx %d, want fBounceAddr=%d", f.idx, fBounceAddr)
	}

	// Type the bounce addr directly into the focused field.
	f.fields[fBounceAddr].input.SetValue("127.0.0.1:7777")
	f.focus(1) // → fBounceNetID
	f.fields[fBounceNetID].input.SetValue("3")
	f.focus(1) // → fBounceClient
	f.fields[fBounceClient].input.SetValue("laptop")

	got, err := f.toNetwork()
	if err != nil {
		t.Fatalf("toNetwork: %v", err)
	}
	if got.Bounce.Addr != "127.0.0.1:7777" {
		t.Errorf("Bounce.Addr = %q, want 127.0.0.1:7777", got.Bounce.Addr)
	}
	if got.Bounce.NetID != 3 {
		t.Errorf("Bounce.NetID = %d, want 3", got.Bounce.NetID)
	}
	if got.Bounce.ClientID != "laptop" {
		t.Errorf("Bounce.ClientID = %q, want laptop", got.Bounce.ClientID)
	}
}

// TestLauncherBounceFieldsEmptyWhenNoAddr: leaving Bounce addr blank means
// toNetwork() produces a zero BounceConfig (no bouncer configured).
func TestLauncherBounceFieldsEmptyWhenNoAddr(t *testing.T) {
	n := config.Network{Name: "Libera", Addr: "irc.libera.chat:6697"}
	f := newNetForm(n, config.Identity{})
	// Bounce fields are all blank for a direct-connection network.
	got, err := f.toNetwork()
	if err != nil {
		t.Fatalf("toNetwork: %v", err)
	}
	if got.Bounce != (config.BounceConfig{}) {
		t.Errorf("Bounce = %+v, want zero value (no bouncer)", got.Bounce)
	}
}

// TestLauncherBounceEditRoundTrip: opening an existing bouncer-connected
// network in the edit form pre-fills the bounce fields, and saving them back
// preserves the full BounceConfig.
func TestLauncherBounceEditRoundTrip(t *testing.T) {
	orig := config.Network{
		Name: "MyBounce",
		Addr: "irc.libera.chat:6697",
		Bounce: config.BounceConfig{
			Addr:     "localhost:7778",
			NetID:    5,
			ClientID: "desktop",
		},
	}
	f := newNetForm(orig, config.Identity{})

	// Verify prefill.
	if got := f.fields[fBounceAddr].input.Value(); got != "localhost:7778" {
		t.Errorf("fBounceAddr prefill = %q, want localhost:7778", got)
	}
	if got := f.fields[fBounceNetID].input.Value(); got != "5" {
		t.Errorf("fBounceNetID prefill = %q, want 5", got)
	}
	if got := f.fields[fBounceClient].input.Value(); got != "desktop" {
		t.Errorf("fBounceClient prefill = %q, want desktop", got)
	}

	// Round-trip.
	got, err := f.toNetwork()
	if err != nil {
		t.Fatalf("toNetwork: %v", err)
	}
	if got.Bounce != orig.Bounce {
		t.Errorf("Bounce = %+v, want %+v", got.Bounce, orig.Bounce)
	}
}

// TestLauncherBounceListFlag: a network with a Bounce addr shows "Bounce" in
// the network list row alongside TLS/SASL.
func TestLauncherBounceListFlag(t *testing.T) {
	store := &config.File{Networks: []config.Network{
		{
			Name: "Bounced",
			Addr: "irc.libera.chat:6697",
			TLS:  true,
			Bounce: config.BounceConfig{
				Addr:  "localhost:7778",
				NetID: 1,
			},
		},
	}}
	m := newLauncher(t, store)
	rendered := renderNetworkRows(m, defaultTheme)
	if !containsPlain(rendered, "Bounce") {
		t.Errorf("network list row missing Bounce flag; rendered:\n%s", rendered)
	}
}

// containsPlain strips ANSI escapes from s and reports whether it contains sub.
func containsPlain(s, sub string) bool {
	return strings.Contains(stripANSI(s), sub)
}

// TestLauncherBounceSepIsSkippedByFocus: Tab never lands on the bounce section
// separator row; focus always moves to an interactive field.
func TestLauncherBounceSepIsSkippedByFocus(t *testing.T) {
	f := newNetForm(config.Network{Name: "x", Addr: "y:1"}, config.Identity{})
	for steps := 0; steps < len(f.fields)*2; steps++ {
		if f.fields[f.idx].kind == kindSep {
			t.Fatalf("focus landed on kindSep at index %d after %d steps", f.idx, steps)
		}
		f.focus(1)
	}
}

// TestLauncherBounceNetIDNonNumericError: a non-empty, non-numeric Bounce NetID
// field produces a validation error from toNetwork() rather than silently
// saving 0 and creating a non-functional bounce configuration.
func TestLauncherBounceNetIDNonNumericError(t *testing.T) {
	f := newNetForm(config.Network{Name: "x", Addr: "y:1"}, config.Identity{})
	f.fields[fBounceAddr].input.SetValue("localhost:7778")
	f.fields[fBounceNetID].input.SetValue("not-a-number")

	_, err := f.toNetwork()
	if err == nil {
		t.Fatal("toNetwork: expected error for non-numeric Bounce NetID, got nil")
	}
	if !strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error message should quote the bad value; got: %v", err)
	}
}

// TestLauncherBounceNetIDRequiredWithAddr: a Bounce addr without a NetID is a
// validation error rather than a silently-direct connection. The connect path
// (cmd/lurk networkToConfig) only routes through the bouncer when NetID > 0 &&
// Addr != "", so saving an addr with a zero NetID used to produce a network
// that wore the "Bounce" badge but dialed the IRC network directly.
func TestLauncherBounceNetIDRequiredWithAddr(t *testing.T) {
	f := newNetForm(config.Network{Name: "x", Addr: "y:1"}, config.Identity{})
	f.fields[fBounceAddr].input.SetValue("localhost:7778")
	// fBounceNetID left blank intentionally.

	if _, err := f.toNetwork(); err == nil {
		t.Fatal("toNetwork: expected error for Bounce addr without NetID, got nil")
	}

	// A non-positive NetID is rejected for the same reason: networkToConfig
	// treats NetID <= 0 as "not bouncer-bound".
	f.fields[fBounceNetID].input.SetValue("0")
	if _, err := f.toNetwork(); err == nil {
		t.Fatal("toNetwork: expected error for Bounce NetID 0, got nil")
	}
}

// TestLauncherBounceBadgeRequiresNetID: the network list's "Bounce" badge keys
// on the same condition the connect path uses (Addr set AND NetID > 0), so a
// legacy config with an addr but no id does not claim a bouncer route it will
// not take.
func TestLauncherBounceBadgeRequiresNetID(t *testing.T) {
	store := &config.File{Networks: []config.Network{
		{
			Name:   "Halfway", // must not itself contain "Bounce"
			Addr:   "irc.libera.chat:6697",
			Bounce: config.BounceConfig{Addr: "localhost:7778"}, // NetID unset
		},
	}}
	m := newLauncher(t, store)
	rendered := renderNetworkRows(m, defaultTheme)
	if containsPlain(rendered, "Bounce") {
		t.Errorf("network list shows Bounce badge for a config that dials direct (NetID 0):\n%s", rendered)
	}
}
