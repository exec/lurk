package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"lurk/config"
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
