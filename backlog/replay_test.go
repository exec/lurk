package backlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIngestUpstreamMessageIDs(t *testing.T) {
	s := newTestStore(t)
	ev := makeEvent("PRIVMSG", "bob!u@h", []string{"#c", "same text"})
	ev.Message.Tags = map[string]string{"msgid": "upstream-1"}
	id, stored := s.Ingest(1, ev)
	if !stored || id == "" || id == "upstream-1" {
		t.Fatal("first delivery did not receive its own stored ID")
	}
	if got, stored := s.Ingest(1, ev); stored || got != id {
		t.Fatalf("duplicate = (%q, %v), want (%q, false)", got, stored, id)
	}
	// IDs are scoped to the network and conversation, never message text.
	if _, stored := s.Ingest(2, ev); !stored {
		t.Fatal("an ID from another network was treated as a duplicate")
	}
	ev.Message.Params[0] = "#other"
	if _, stored := s.Ingest(1, ev); !stored {
		t.Fatal("an ID from another conversation was treated as a duplicate")
	}
	ev.Message.Params[0] = "#c"
	ev.Message.Tags["msgid"] = "upstream-2"
	if _, stored := s.Ingest(1, ev); !stored {
		t.Fatal("identical text with a different ID was discarded")
	}
	delete(ev.Message.Tags, "msgid")
	for range 2 {
		if _, stored := s.Ingest(1, ev); !stored {
			t.Fatal("a message without an upstream ID was discarded")
		}
	}
	if got := len(s.Latest(1, "#c", 10)); got != 4 {
		t.Fatalf("stored %d messages, want 4", got)
	}
}

func TestReplayIdentitySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev := makeEvent("NOTICE", "bob!u@h", []string{"#c", "stored notice"})
	ev.Message.Tags = map[string]string{"msgid": "upstream-1"}
	id, stored := s.Ingest(1, ev)
	if !stored {
		t.Fatal("initial delivery was not stored")
	}
	s.Close()
	s, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if got, stored := s.Ingest(1, ev); got != id || stored {
		t.Fatalf("replay after restart = (%q, %v), want (%q, false)", got, stored, id)
	}
	data, err := os.ReadFile(filepath.Join(dir, "1", "#c.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Fatal("replay appended a duplicate JSONL record")
	}
}

func TestConcurrentReplayStoredOnce(t *testing.T) {
	s := newTestStore(t)
	ev := makeEvent("PRIVMSG", "bob!u@h", []string{"#c", "same message"})
	ev.Message.Tags = map[string]string{"msgid": "upstream-1"}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { s.Ingest(1, ev) })
	}
	wg.Wait()
	if got := len(s.Latest(1, "#c", 20)); got != 1 {
		t.Fatalf("concurrent deliveries stored %d copies, want 1", got)
	}
}
