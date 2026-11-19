package store

import (
	"testing"
)

// TestToolCallInFlightAndComplete exercises the P7 two-phase lifecycle: a
// tool call is persisted IN-FLIGHT the instant the model requests it (a
// crash mid-execution leaves the row, and the request builder excludes
// in-flight rows from the wire), then completed with its result. The batch
// fetch (the request builder's pairing path) groups by message id in row
// order and omits messages with no rows.
func TestToolCallInFlightAndComplete(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("tool test", "m1")
	if err != nil {
		t.Fatal(err)
	}
	am, err := s.AppendMessage(c.ID, "assistant", "let me check", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// In-flight: persisted before execution.
	id1, err := s.AppendToolCall(am.ID, "web_browser", `{"command":"read"}`)
	if err != nil {
		t.Fatal(err)
	}
	if id1 < 1 {
		t.Fatalf("AppendToolCall id = %d, want > 0", id1)
	}
	rows, err := s.ToolCallsForMessage(am.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Result != "" || rows[0].Success {
		t.Fatalf("in-flight row = %+v, want Result==\"\" && Success==false", rows[0])
	}
	if rows[0].Tool != "web_browser" || rows[0].Args != `{"command":"read"}` {
		t.Fatalf("row identity = %+v", rows[0])
	}

	// Complete it.
	if err := s.CompleteToolCall(id1, "the page says hello", true); err != nil {
		t.Fatal(err)
	}

	// A second call stays in-flight (the crash case: the row exists but has
	// no outcome).
	id2, err := s.AppendToolCall(am.ID, "web_browser", `{"command":"click"}`)
	if err != nil {
		t.Fatal(err)
	}

	// A third assistant message with a FAILED call (a failure is an outcome
	// too — it is sent back on the wire).
	am2, err := s.AppendMessage(c.ID, "assistant", "trying again", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	id3, err := s.AppendToolCall(am2.ID, "web_browser", `{"command":"navigate"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteToolCall(id3, "navigation failed: net::ERR", false); err != nil {
		t.Fatal(err)
	}

	// Batch fetch: grouped by message id, row order preserved, the
	// in-flight row still distinguishable, and a row-less message is absent
	// from the map.
	am3, err := s.AppendMessage(c.ID, "assistant", "no tools here", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	mm, err := s.ToolCallsForMessages([]int64{am.ID, am2.ID, am3.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mm[am3.ID]; ok {
		t.Fatalf("message with no tool calls appears in the batch map")
	}
	if got := mm[am.ID]; len(got) != 2 {
		t.Fatalf("batch rows for am = %d, want 2", len(got))
	}
	if got := mm[am.ID]; got[0].ID != id1 || got[1].ID != id2 {
		t.Fatalf("batch row order = [%d %d], want [%d %d] (insertion order)", got[0].ID, got[1].ID, id1, id2)
	}
	if got := mm[am.ID]; !got[0].Success || got[0].Result != "the page says hello" {
		t.Fatalf("completed row = %+v", got[0])
	}
	if got := mm[am.ID]; got[1].Success || got[1].Result != "" {
		t.Fatalf("in-flight row = %+v, want no outcome", got[1])
	}
	if got := mm[am2.ID]; len(got) != 1 || got[0].Success || got[0].Result == "" {
		t.Fatalf("failed row = %+v, want one completed-as-failed row", got)
	}
}

// TestToolCallsForMessagesEmpty is the no-tool-activity fast path: an empty
// id list returns an empty map without touching the database.
func TestToolCallsForMessagesEmpty(t *testing.T) {
	s := openTestStore(t)
	mm, err := s.ToolCallsForMessages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mm) != 0 {
		t.Fatalf("map = %+v, want empty", mm)
	}
}
