package cli

import "testing"

// Two agents on one thread interleave in time: penny's done lands after mouse's
// dispatch. Chronological order indents it under mouse's task.
func TestGroupConversation_KeepsRepliesWithTheirDispatch(t *testing.T) {
	events := []convEvent{
		{TS: "1", From: "dir:ledger", To: "haydn/penny-pincher", Dispatch: true, Content: "task A"},
		{TS: "2", From: "haydn/penny-pincher", Status: "ack", Content: "working on it A"},
		{TS: "3", From: "dir:ledger", To: "haydn/mouse-for-sale", Dispatch: true, Content: "task B"},
		{TS: "4", From: "haydn/mouse-for-sale", Status: "ack", Content: "working on it B"},
		{TS: "5", From: "haydn/penny-pincher", Status: "done", Content: "backend verdict"},
		{TS: "6", From: "haydn/mouse-for-sale", Status: "done", Content: "frontend verdict"},
	}
	got := groupConversation(events)
	want := []string{"1", "2", "5", "3", "4", "6"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, ts := range want {
		if got[i].TS != ts {
			t.Fatalf("position %d = TS %s, want %s (order: %v)", i, got[i].TS, ts, tsList(got))
		}
	}
}

// A single-agent thread must render exactly as before.
func TestGroupConversation_SingleBranchUnchanged(t *testing.T) {
	events := []convEvent{
		{TS: "1", From: "dir:ledger", To: "haydn/penny-pincher", Dispatch: true},
		{TS: "2", From: "haydn/penny-pincher", Status: "ack"},
		{TS: "3", From: "haydn/penny-pincher", Status: "done"},
	}
	if got := tsList(groupConversation(events)); got != "1 2 3" {
		t.Fatalf("order %q, want \"1 2 3\"", got)
	}
}

// A reply from someone never dispatched to still shows, in place.
func TestGroupConversation_KeepsOrphanReplies(t *testing.T) {
	events := []convEvent{
		{TS: "1", From: "haydn/other", Status: "message"},
		{TS: "2", From: "dir:ledger", To: "haydn/penny-pincher", Dispatch: true},
		{TS: "3", From: "haydn/penny-pincher", Status: "done"},
	}
	if got := tsList(groupConversation(events)); got != "1 2 3" {
		t.Fatalf("order %q, want \"1 2 3\"", got)
	}
}

func tsList(events []convEvent) string {
	out := ""
	for i, e := range events {
		if i > 0 {
			out += " "
		}
		out += e.TS
	}
	return out
}
