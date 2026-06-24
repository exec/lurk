package irc

import "testing"

// Representative inbound lines exercising the per-message hot path: a tagged
// PRIVMSG (the common modern case, server-time + msgid), a NAMES reply (many
// params), and a bare PING (no tags, no params — the nil-Params fast path).
var benchLines = []string{
	"@time=2024-05-01T12:34:56.789Z;msgid=abc123 :nick!user@host PRIVMSG #channel :hello there, this is a normal message",
	":irc.example.net 353 me = #channel :alice bob carol dave erin frank grace heidi ivan judy",
	"PING :LAG1234567890",
}

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, line := range benchLines {
			m, err := Parse(line)
			if err != nil {
				b.Fatal(err)
			}
			_ = m
		}
	}
}

func BenchmarkSerialize(b *testing.B) {
	msgs := make([]*Message, len(benchLines))
	for i, line := range benchLines {
		m, err := Parse(line)
		if err != nil {
			b.Fatal(err)
		}
		msgs[i] = m
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, m := range msgs {
			if _, err := m.Serialize(); err != nil {
				b.Fatal(err)
			}
		}
	}
}
