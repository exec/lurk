package isupport

import "testing"

func TestCaseMappingFromToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  CaseMapping
	}{
		{"ascii", CaseASCII},
		{"ASCII", CaseASCII},
		{"rfc1459", CaseRFC1459},
		{"rfc1459-strict", CaseRFC1459Strict},
		{"unknown-future", CaseRFC1459}, // unrecognised -> default
	} {
		feat := Parse([]string{"CASEMAPPING=" + tc.token})
		if got := feat.CaseMapping(); got != tc.want {
			t.Errorf("CASEMAPPING=%s -> %v, want %v", tc.token, got, tc.want)
		}
	}

	// Absent token defaults to rfc1459.
	var empty ISupport
	if got := empty.CaseMapping(); got != CaseRFC1459 {
		t.Errorf("absent CASEMAPPING -> %v, want CaseRFC1459", got)
	}
}

func TestFold(t *testing.T) {
	for _, tc := range []struct {
		name string
		cm   CaseMapping
		in   string
		want string
	}{
		// Plain ASCII letters fold the same under every mapping.
		{"ascii letters", CaseASCII, "NickName", "nickname"},
		{"rfc1459 letters", CaseRFC1459, "NickName", "nickname"},

		// The discriminating case: brackets vs braces.
		{"ascii brackets untouched", CaseASCII, "[Nick]", "[nick]"},
		{"rfc1459 brackets fold", CaseRFC1459, "[Nick]", "{nick}"},

		// Full bracket set under rfc1459.
		{"rfc1459 all specials", CaseRFC1459, "[]\\^", "{}|~"},
		{"rfc1459 caret folds", CaseRFC1459, "a^b", "a~b"},

		// rfc1459-strict folds brackets but NOT caret.
		{"strict folds brackets", CaseRFC1459Strict, "[]\\", "{}|"},
		{"strict leaves caret", CaseRFC1459Strict, "a^b", "a^b"},

		// ascii leaves all of []\\^ alone.
		{"ascii leaves specials", CaseASCII, "[]\\^", "[]\\^"},

		// Already-folded input is returned unchanged (fast path).
		{"idempotent", CaseRFC1459, "{nick}", "{nick}"},

		// Folding is ASCII-only: multi-byte UTF-8 (here 'Ü') passes through
		// untouched; only the ASCII 'b','r' are already lowercase.
		{"high bytes untouched", CaseRFC1459, "Über", "Über"},
		// ASCII portions of a UTF-8 string still fold.
		{"ascii within utf8", CaseRFC1459, "ÜBER", "Über"},

		// Empty string.
		{"empty", CaseRFC1459, "", ""},
	} {
		if got := tc.cm.Fold(tc.in); got != tc.want {
			t.Errorf("%s: %v.Fold(%q) = %q, want %q", tc.name, tc.cm, tc.in, got, tc.want)
		}
	}
}

// TestFoldEquality demonstrates the intended use: two names that differ only by
// case (and bracket/brace folding) compare equal under the right mapping.
func TestFoldEquality(t *testing.T) {
	// Under rfc1459, "[Nick]" and "{nick}" are the SAME name.
	if CaseRFC1459.Fold("[Nick]") != CaseRFC1459.Fold("{nick}") {
		t.Errorf("rfc1459 should treat [Nick] and {nick} as equal")
	}
	// Under ascii they are DIFFERENT names.
	if CaseASCII.Fold("[Nick]") == CaseASCII.Fold("{nick}") {
		t.Errorf("ascii should treat [Nick] and {nick} as distinct")
	}
}

func TestCaseMappingString(t *testing.T) {
	for _, tc := range []struct {
		cm   CaseMapping
		want string
	}{
		{CaseASCII, "ascii"},
		{CaseRFC1459, "rfc1459"},
		{CaseRFC1459Strict, "rfc1459-strict"},
	} {
		if got := tc.cm.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.cm, got, tc.want)
		}
	}
}

// TestFoldNoMutation ensures Fold does not alias or mutate the input string's
// backing storage (it operates on a copy).
func TestFoldNoMutation(t *testing.T) {
	in := "ABC"
	out := CaseRFC1459.Fold(in)
	if in != "ABC" {
		t.Errorf("input mutated: %q", in)
	}
	if out != "abc" {
		t.Errorf("Fold(ABC) = %q", out)
	}
}
