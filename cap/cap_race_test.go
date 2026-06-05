package cap

import (
	"sync"
	"testing"

	"lurk/irc"
)

// TestNegotiatorConcurrentIsEnabled exercises the data race the internal mutex
// guards: the run-loop mutates the enabled/available maps via Receive while
// another goroutine (e.g. the TUI checking message-tags on every TAGMSG) reads
// them via IsEnabled. Before the lock was added this raced "concurrent map read
// and map write" and the -race detector flags it; with the lock it is clean.
//
// Run with: go test -race -run TestNegotiatorConcurrent -count=5 ./cap/
func TestNegotiatorConcurrentIsEnabled(t *testing.T) {
	n := NewNegotiator([]string{"message-tags", "server-time", "sasl", "batch"})
	n.Start()
	// Advertise the wanted caps so Receive will REQ them and ACKs will populate
	// the enabled map (the writer side of the race).
	n.Receive(capMsg("*", "LS", ":message-tags server-time sasl batch"))

	const ackers = 4
	const readers = 4
	const iters = 2000

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Writers: repeatedly ACK/NAK and process CAP NEW/DEL, mutating the maps.
	for w := 0; w < ackers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				n.Receive(capMsg("*", "ACK", ":message-tags batch"))
				n.Receive(capMsg("*", "NEW", ":new-cap-x"))
				n.Receive(capMsg("*", "DEL", ":batch"))
				n.Receive(&irc.Message{Command: "CAP", Params: []string{"*", "ACK", "batch"}})
			}
		}()
	}

	// Readers: hammer the read-only query paths concurrently.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				_ = n.IsEnabled("message-tags")
				_ = n.Enabled()
				_ = n.Available()
				_ = n.NeedSASL()
				_ = n.SASLMechs()
				_ = n.State()
			}
		}()
	}

	close(start)
	wg.Wait()
}
