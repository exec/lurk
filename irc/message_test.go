package irc

import "testing"

// TestMessageClone verifies that Clone produces a deep-independent copy:
// mutating Tags or Params in the clone must not affect the original, and vice
// versa.
func TestMessageClone(t *testing.T) {
	tests := []struct {
		name string
		orig *Message
	}{
		{
			name: "full message",
			orig: &Message{
				Tags:    Tags{"time": "2024-01-01T00:00:00Z", "+typing": "active"},
				Source:  "alice!a@host",
				Command: "PRIVMSG",
				Params:  []string{"#chan", "hello"},
			},
		},
		{
			name: "nil Tags",
			orig: &Message{Command: "PING", Params: []string{"server"}},
		},
		{
			name: "nil Params",
			orig: &Message{Tags: Tags{"batch": "abc"}, Command: "BATCH"},
		},
		{
			name: "empty Tags and Params",
			orig: &Message{Tags: Tags{}, Command: "CAP", Params: []string{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clone := tt.orig.Clone()
			if clone == tt.orig {
				t.Fatal("Clone() returned the same pointer as the original")
			}

			// Scalar fields must match.
			if clone.Source != tt.orig.Source {
				t.Errorf("Source: clone %q != orig %q", clone.Source, tt.orig.Source)
			}
			if clone.Command != tt.orig.Command {
				t.Errorf("Command: clone %q != orig %q", clone.Command, tt.orig.Command)
			}

			// Tags independence: add a key to the clone and confirm it is absent
			// in the original.
			if tt.orig.Tags != nil {
				clone.Tags["__clone_key__"] = "x"
				if tt.orig.Tags.Has("__clone_key__") {
					t.Error("mutating clone Tags leaked into original")
				}
				// Remove the probe key before checking values.
				delete(clone.Tags, "__clone_key__")

				// All original tags must be present in the clone with the same
				// value.
				for k, v := range tt.orig.Tags {
					if cv, ok := clone.Tags[k]; !ok || cv != v {
						t.Errorf("Tags[%q]: clone=%q orig=%q", k, cv, v)
					}
				}
			} else if clone.Tags != nil {
				t.Error("clone.Tags is non-nil but orig.Tags was nil")
			}

			// Params independence: append to the clone's slice; the original
			// must be unchanged.
			if tt.orig.Params != nil {
				origLen := len(tt.orig.Params)
				clone.Params = append(clone.Params, "__extra__")
				if len(tt.orig.Params) != origLen {
					t.Error("appending to clone.Params changed orig.Params length")
				}
				clone.Params = clone.Params[:origLen] // restore for value check

				for i, v := range tt.orig.Params {
					if clone.Params[i] != v {
						t.Errorf("Params[%d]: clone=%q orig=%q", i, clone.Params[i], v)
					}
				}
			} else if clone.Params != nil {
				t.Error("clone.Params is non-nil but orig.Params was nil")
			}
		})
	}
}

// TestMessageCloneNilReceiver verifies that Clone on a nil *Message returns nil
// rather than panicking.
func TestMessageCloneNilReceiver(t *testing.T) {
	var m *Message
	if got := m.Clone(); got != nil {
		t.Errorf("Clone on nil *Message = %v, want nil", got)
	}
}

// TestMessageCloneTagsMapIndependence is a focused regression for the aliasing
// footgun: a caller that stashes a parsed *Message and later has someone else
// write to its Tags must not see the write.
func TestMessageCloneTagsMapIndependence(t *testing.T) {
	orig := &Message{
		Tags:    Tags{"server-time": "2024-01-01"},
		Command: "PRIVMSG",
	}
	clone := orig.Clone()

	// Mutate the clone's tags in both directions.
	clone.Tags["server-time"] = "CHANGED"
	clone.Tags["new-key"] = "new-val"

	if orig.Tags["server-time"] != "2024-01-01" {
		t.Errorf("orig server-time mutated to %q after clone write", orig.Tags["server-time"])
	}
	if orig.Tags.Has("new-key") {
		t.Error("new-key appeared in orig after being set on clone")
	}
}

// TestTagsSet verifies the Set helper stores the key-value pair in the map.
func TestTagsSet(t *testing.T) {
	tags := make(Tags)
	tags.Set("time", "2024-01-01")
	tags.Set("+typing", "active")
	tags.Set("time", "2024-02-01") // overwrite

	if v := tags.Get("time"); v != "2024-02-01" {
		t.Errorf("Get(time) = %q, want 2024-02-01", v)
	}
	if !tags.Has("+typing") {
		t.Error("Has(+typing) = false, want true")
	}
	if v := tags.Get("+typing"); v != "active" {
		t.Errorf("Get(+typing) = %q, want active", v)
	}
}

// TestTagsSetNilSafe verifies that Set on a nil Tags field initialises the map
// rather than panicking, so callers can write to a zero-value Message without a
// separate make call.
func TestTagsSetNilSafe(t *testing.T) {
	// Via a Message field (the primary use case).
	m := &Message{Command: "PRIVMSG"}
	// m.Tags is nil here; Set must initialise it.
	m.Tags.Set("time", "2024-01-01")
	if !m.Tags.Has("time") {
		t.Error("Tags.Set on nil Message.Tags: Has(time) = false, want true")
	}
	if v := m.Tags.Get("time"); v != "2024-01-01" {
		t.Errorf("Tags.Set on nil Message.Tags: Get(time) = %q, want 2024-01-01", v)
	}

	// Via a standalone nil Tags variable.
	var tags Tags
	tags.Set("batch", "b1")
	if !tags.Has("batch") {
		t.Error("Tags.Set on nil Tags variable: Has(batch) = false, want true")
	}
}

// TestTagsSetOnClone verifies the intended idiom: Clone then Set on the clone
// does not affect the original.
func TestTagsSetOnClone(t *testing.T) {
	orig := &Message{
		Tags:    Tags{"batch": "b1"},
		Command: "PRIVMSG",
	}
	clone := orig.Clone()
	clone.Tags.Set("batch", "MODIFIED")
	clone.Tags.Set("extra", "val")

	if orig.Tags.Get("batch") != "b1" {
		t.Errorf("orig batch = %q after clone Set, want b1", orig.Tags.Get("batch"))
	}
	if orig.Tags.Has("extra") {
		t.Error("extra appeared in orig after clone Set")
	}
}
