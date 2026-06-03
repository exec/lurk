package irc

import (
	"reflect"
	"testing"
)

// FuzzParse asserts that Parse never panics on arbitrary input, and that any
// line it accepts round-trips: serializing the parsed message and re-parsing it
// yields the same command and parameters. The seed corpus runs under plain
// `go test`; explore further with `go test -run=x -fuzz=FuzzParse ./irc`.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"PING :tok",
		":nick!user@host PRIVMSG #chan :hello world",
		"@a=1;b=2 :src CMD a b :trailing",
		"@time=2020-01-01T00:00:00.000Z FOO",
		"CMD",
		":src 001 me :welcome",
		"@+typing=active :n TAGMSG #c",
		"",
		"   CMD   a    b   :  c  ",
		"CMD a :",
		"CMD ::x",
		":nick QUIT",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		m, err := Parse(line)
		if err != nil {
			return // a rejected line has nothing to round-trip
		}
		out, serr := m.Serialize()
		if serr != nil {
			return // some parsed messages aren't serializable (e.g. an empty non-final param)
		}
		m2, err2 := Parse(out)
		if err2 != nil {
			t.Fatalf("re-parse of serialized %q (from %q) failed: %v", out, line, err2)
		}
		if m.Command != m2.Command {
			t.Fatalf("command changed across round-trip: %q -> %q (line %q)", m.Command, m2.Command, line)
		}
		if !reflect.DeepEqual(m.Params, m2.Params) {
			t.Fatalf("params changed across round-trip: %#v -> %#v (line %q)", m.Params, m2.Params, line)
		}
	})
}
