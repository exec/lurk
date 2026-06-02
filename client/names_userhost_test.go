package client

import "testing"

func TestNamReplyUserhostInNames(t *testing.T) {
	s := newState()
	s.mergeISupport([]string{"PREFIX=(qaohv)~&@%+"})
	s.applyNamReply("#lurk", "@dylan!~u@host.example +alice!a@1.2.3.4 bob!b@h")
	got := map[string]string{}
	for _, m := range s.channel("#lurk").members {
		got[m.Nick] = m.Prefixes
	}
	want := map[string]string{"dylan": "@", "alice": "+", "bob": ""}
	for nick, pfx := range want {
		m, ok := got[nick]
		if !ok {
			t.Fatalf("missing bare nick %q; got %v", nick, got)
		}
		if m != pfx {
			t.Errorf("%s prefix=%q want %q", nick, m, pfx)
		}
	}
}
