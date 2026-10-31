package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/config"
)

func submitPreference(m model, line string) model {
	m.input.SetValue(line)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return updated.(model)
}

func TestPreferencesScopedToNetwork(t *testing.T) {
	m := newModel(client.New(client.Config{Nick: "me", Highlights: []string{" Deploy ", ""}, Ignores: []string{"Spammer"}}), nil)
	a := m.activeNet()
	b := m.addNetwork("second", client.New(client.Config{Nick: "me", Highlights: []string{"Release"}, Ignores: []string{"Troll"}}))
	ba, _ := m.ensureBufferIn(a, "#go", BufferChannel)
	bb, _ := m.ensureBufferIn(b, "#go", BufferChannel)
	// Both networks are in the background; routing must use the event's owner.
	m = routeEventOn(m, b, evt(t, ":Spammer!s@h PRIVMSG #go :deploy"))
	m = routeEventOn(m, a, evt(t, ":Troll!t@h PRIVMSG #go :release"))
	if len(ba.lines) != 1 || len(bb.lines) != 1 || ba.Highlight || bb.Highlight || m.bell {
		t.Fatal("preferences leaked into another network's live messages")
	}
	m = routeEventOn(m, a, evt(t, ":Spammer!s@h PRIVMSG #go :deploy"))
	m = routeEventOn(m, b, evt(t, ":Troll!t@h PRIVMSG me :release"))
	if len(ba.lines) != 1 || m.netBuffer(b, "Troll") != nil {
		t.Fatal("ignored channel or PM message was delivered")
	}
	m = routeEventOn(m, a, evt(t, ":alice!a@h PRIVMSG #go :deploy"))
	m = routeEventOn(m, b, evt(t, ":alice!a@h PRIVMSG #go :release"))
	if !ba.Highlight || !bb.Highlight || !m.bell {
		t.Fatal("configured highlights did not work on both networks")
	}
	m.switchTo(m.bufferIndexIn(b, "#go"))
	m = submitPreference(m, "/ignore local")
	m = submitPreference(m, "/highlight personal")
	if a.isIgnored("local") || a.highlights["personal"] || !b.isIgnored("LOCAL") || !b.highlights["personal"] {
		t.Fatal("commands did not target only the active network")
	}
	act, _ := runLine(m, "/ignore")
	if !strings.Contains(act.text, "local troll") || strings.Contains(act.text, "spammer") {
		t.Fatalf("active network ignore listing = %q", act.text)
	}
	act, _ = runLine(m, "/highlight")
	if !strings.Contains(act.text, "personal release") || strings.Contains(act.text, "deploy") {
		t.Fatalf("active network highlight listing = %q", act.text)
	}
}

func TestHistoryUsesOwningNetworkPreferences(t *testing.T) {
	m, a, b := twoNetModel(t)
	a.ignored["alice"] = true
	a.highlights["deploy"] = true
	b.ignored["bob"] = true
	b.highlights["release"] = true
	for _, tc := range []struct {
		net        *network
		keep, drop string
	}{
		{a, ":bob!b@h PRIVMSG #go :deploy", ":alice!a@h PRIVMSG #go :release"},
		{b, ":alice!a@h PRIVMSG #go :release", ":bob!b@h PRIVMSG #go :deploy"},
	} {
		buf, _ := m.ensureBufferIn(tc.net, "#go", BufferChannel)
		keep := evt(t, tc.keep)
		m = m.mergeHistory(buf, []client.Event{keep, evt(t, tc.drop)}, true)
		want, _ := defaultTheme.formatLine(keep, "me", tc.net.highlights)
		if len(buf.lines) != 2 || buf.lines[1] != want {
			t.Fatalf("history used another network's preferences: %q", buf.lines)
		}
	}
	if m.bell {
		t.Fatal("history raised a mention bell")
	}
}

func TestPreferencesPersistBySavedName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := &config.File{Networks: []config.Network{
		{Name: "Work", Addr: "shared.bouncer:6697", SASL: config.SASL{Mechanism: "PLAIN", Username: "work", Password: "secret"}},
		{Name: "Personal", Addr: "shared.bouncer:6697", Highlights: []string{"release"}, Ignores: []string{"noise"}},
	}}
	if err := config.Save(want, path); err != nil {
		t.Fatal(err)
	}
	m := newModel(client.New(client.Config{Nick: "me", Server: "shared.bouncer:6697"}), nil)
	m.configPath, m.activeNet().savedName = path, "work" // case-insensitive profile lookup
	// External edits after startup must survive preference saves.
	want.Defaults.Realname = "New default"
	want.Networks[0].Channels = []string{"#new"}
	if err := config.Save(want, path); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		line                string
		ignores, highlights []string
	}{
		{"/ignore Spammer", []string{"spammer"}, nil},
		{"/highlight Deploy", []string{"spammer"}, []string{"deploy"}},
		{"/unignore SPAMMER", nil, []string{"deploy"}},
		{"/unhighlight DEPLOY", nil, nil},
	} {
		m = submitPreference(m, step.line)
		want.Networks[0].Ignores, want.Networks[0].Highlights = step.ignores, step.highlights
		got, err := config.LoadFrom(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s did not persist only the requested setting", step.line)
		}
	}
	// /connect loads both settings for the second network, then saves to its own
	// profile even when the dialed address is shared with the initial network.
	n := want.Networks[1]
	updated, _ := m.Update(connectedMsg{name: n.Name, cli: client.New(client.Config{
		Nick: "me", Server: n.Addr, Highlights: n.Highlights, Ignores: n.Ignores,
	})})
	m = updated.(model)
	if !m.activeNet().highlights["release"] || !m.activeNet().isIgnored("noise") {
		t.Fatal("secondary network did not load its preferences")
	}
	m = submitPreference(m, "/ignore Second")
	want.Networks[1].Ignores = []string{"noise", "second"}
	got, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("secondary network save changed another profile")
	}
	// A new client/UI session restores the saved settings.
	n = got.Networks[1]
	restarted := newModel(client.New(client.Config{Nick: "me", Highlights: n.Highlights, Ignores: n.Ignores}), nil)
	if !restarted.activeNet().isIgnored("SECOND") || !restarted.activeNet().highlights["release"] {
		t.Fatal("preferences were lost after restart")
	}
}

func TestPreferenceSaveFailureKeepsSessionAndConfig(t *testing.T) {
	for _, failure := range []string{"malformed config", "removed profile", "write denied"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			store := &config.File{Networks: []config.Network{{Name: "Work", Addr: "h:1"}}}
			if failure == "removed profile" {
				store.Networks = nil
			}
			if err := config.Save(store, path); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "malformed config":
				if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "write denied":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory write permissions")
				}
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{"/ignore new", "/unignore old", "/highlight new", "/unhighlight old"} {
				m := newModel(client.New(client.Config{Nick: "me", Ignores: []string{"old"}, Highlights: []string{"old"}}), nil)
				m.configPath, m.activeNet().savedName = path, "Work"
				m = submitPreference(m, line)
				if !strings.Contains(bufText(m.activeBuffer()), "unchanged:") {
					t.Fatalf("%s hid the persistence error", line)
				}
				if !reflect.DeepEqual(sortedSetKeys(m.activeNet().ignored), []string{"old"}) ||
					!reflect.DeepEqual(sortedSetKeys(m.activeNet().highlights), []string{"old"}) {
					t.Fatalf("%s changed the session after a failed save", line)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("failed saves modified the existing config")
			}
		})
	}
}

func TestAdHocPreferencesStayInSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m := newTestModel()
	m.configPath = path // the binary supplies a path even for -server sessions
	m = submitPreference(m, "/ignore nick")
	m = submitPreference(m, "/highlight word")
	if !m.activeNet().isIgnored("nick") || !m.activeNet().highlights["word"] {
		t.Fatal("ad-hoc session preferences did not take effect")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ad-hoc session wrote a saved profile: %v", err)
	}
}
