package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newTestModel builds a model with no live client, suitable for exercising the
// pure parsing/completion/history logic. The active buffer is the server buffer
// (index 0); tests that need a channel context add one via ensureBuffer.
func newTestModel() model {
	return newModel(nil, nil)
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		line      string
		wantName  string
		wantRest  string
		wantIsCmd bool
	}{
		{"hello world", "", "hello world", false},
		{"", "", "", false},
		{"//literal", "", "/literal", false},                 // // escapes to a literal slash
		{"//", "", "/", false},                               // bare // -> literal "/"
		{"/join #go", "JOIN", "#go", true},                   // upper-cased name
		{"/JOIN #go", "JOIN", "#go", true},                   // already upper
		{"/me waves   slowly", "ME", "waves   slowly", true}, // interior spaces in rest preserved
		{"/names", "NAMES", "", true},                        // no args
		{"/", "", "", true},                                  // lone slash: command with empty name
		{"/quit   bye now", "QUIT", "bye now", true},         // leading rest spaces trimmed
	}
	for _, tc := range tests {
		name, rest, isCmd := splitCommand(tc.line)
		if name != tc.wantName || rest != tc.wantRest || isCmd != tc.wantIsCmd {
			t.Errorf("splitCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.line, name, rest, isCmd, tc.wantName, tc.wantRest, tc.wantIsCmd)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want []string
	}{
		{"", 2, nil},
		{"   ", 2, nil},
		{"alpha", argsUnlimited, []string{"alpha"}},
		{"alpha beta gamma", argsUnlimited, []string{"alpha beta gamma"}}, // rest as one
		{"alpha beta gamma", 2, []string{"alpha", "beta gamma"}},          // last field keeps spaces
		{"target hello there friend", 2, []string{"target", "hello there friend"}},
		{"one two", 1, []string{"one two"}}, // max 1 -> whole string
		{"#chan key extra", 2, []string{"#chan", "key extra"}},
		{"a  b  c", 3, []string{"a", "b", "c"}}, // collapse multiple spaces between fields
	}
	for _, tc := range tests {
		got := splitArgs(tc.s, tc.max)
		if !equalStrings(got, tc.want) {
			t.Errorf("splitArgs(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
		}
	}
}

func TestRunLinePlainMessage(t *testing.T) {
	m := newTestModel()
	act, cmd := runLine(m, "hello channel")
	if act.kind != actionSend {
		t.Fatalf("plain line kind = %v, want actionSend", act.kind)
	}
	if act.target != "" {
		t.Errorf("plain line target = %q, want empty (active buffer)", act.target)
	}
	if act.text != "hello channel" {
		t.Errorf("plain line text = %q, want %q", act.text, "hello channel")
	}
	if cmd != nil {
		t.Errorf("plain line cmd = %v, want nil", cmd)
	}
}

func TestRunLineLiteralSlash(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "//not a command")
	if act.kind != actionSend {
		t.Fatalf("// literal kind = %v, want actionSend", act.kind)
	}
	if act.text != "/not a command" {
		t.Errorf("// literal text = %q, want %q", act.text, "/not a command")
	}
}

func TestRunLineUnknownCommand(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/bogus arg")
	if act.kind != actionInfo {
		t.Fatalf("unknown command kind = %v, want actionInfo", act.kind)
	}
	if act.text == "" {
		t.Error("unknown command should produce a non-empty info message")
	}
}

func TestRunLineLoneSlash(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/")
	if act.kind != actionInfo {
		t.Fatalf("lone slash kind = %v, want actionInfo", act.kind)
	}
}

func TestRunLineJoinOpensBuffer(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/join #go")
	if act.kind != actionOpen {
		t.Fatalf("/join kind = %v, want actionOpen", act.kind)
	}
	if act.target != "#go" {
		t.Errorf("/join target = %q, want %q", act.target, "#go")
	}
	if act.bufferKind != BufferChannel {
		t.Errorf("/join bufferKind = %v, want BufferChannel", act.bufferKind)
	}
}

func TestRunLineMsg(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/msg bob hi there bob")
	if act.kind != actionSend {
		t.Fatalf("/msg kind = %v, want actionSend", act.kind)
	}
	if act.target != "bob" {
		t.Errorf("/msg target = %q, want %q", act.target, "bob")
	}
	if act.text != "hi there bob" {
		t.Errorf("/msg text = %q, want %q", act.text, "hi there bob")
	}
}

func TestRunLineMsgUsage(t *testing.T) {
	m := newTestModel()
	// Only one arg -> below minArgs (2) -> usage hint.
	act, _ := runLine(m, "/msg bob")
	if act.kind != actionInfo {
		t.Fatalf("/msg with too few args kind = %v, want actionInfo (usage)", act.kind)
	}
}

func TestRunLineQuery(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/query alice")
	if act.kind != actionOpen || act.target != "alice" || act.bufferKind != BufferPM {
		t.Fatalf("/query = %+v, want open PM buffer for alice", act)
	}
	// Querying a channel is rejected.
	act2, _ := runLine(m, "/query #go")
	if act2.kind != actionInfo {
		t.Errorf("/query of a channel should be an info error, got %v", act2.kind)
	}
}

func TestRunLineMeBuildsCTCPAction(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/me waves slowly")
	if act.kind != actionSend {
		t.Fatalf("/me kind = %v, want actionSend", act.kind)
	}
	want := "\x01ACTION waves slowly\x01"
	if act.text != want {
		t.Errorf("/me text = %q, want %q", act.text, want)
	}
}

func TestRunLineQuitReturnsQuitCmd(t *testing.T) {
	m := newTestModel()
	act, cmd := runLine(m, "/quit see ya")
	if act.kind != actionNone {
		t.Errorf("/quit kind = %v, want actionNone", act.kind)
	}
	if cmd == nil {
		t.Fatal("/quit should return a non-nil (tea.Quit) command")
	}
	// The returned Cmd should be tea.Quit, i.e. produce a tea.QuitMsg.
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("/quit command did not produce a tea.QuitMsg")
	}
}

func TestRunLineCloseFromChannel(t *testing.T) {
	m := newTestModel()
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	act, _ := runLine(m, "/close")
	if act.kind != actionClose {
		t.Fatalf("/close kind = %v, want actionClose", act.kind)
	}
}

func TestRunLinePartNonChannel(t *testing.T) {
	m := newTestModel()
	// Active is the server buffer (not a channel) and no arg -> error.
	act, _ := runLine(m, "/part")
	if act.kind != actionInfo {
		t.Fatalf("/part from server buffer kind = %v, want actionInfo", act.kind)
	}
}

func TestRunLineNamesNonChannel(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/names")
	if act.kind != actionInfo {
		t.Fatalf("/names from server buffer kind = %v, want actionInfo", act.kind)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
