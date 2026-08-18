package daemon

import (
	"testing"
	"time"
)

func TestComms_CreateDMAndPost(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}

	ch, err := c.CreateDMChannel(1, "companyA/payment", "companyA/user")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Type != "dm" {
		t.Errorf("type: got %q want dm", ch.Type)
	}

	msg, err := c.PostMessage(ch.ID, 0, "companyA/payment", "companyA/user", "add field X", "delivered")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "add field X" || msg.FromProject != "companyA/payment" || msg.Status != "delivered" {
		t.Errorf("got %+v", msg)
	}
}

func TestComms_ReadChannel(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}
	ch, _ := c.CreateDMChannel(1, "a/b", "a/c")
	c.PostMessage(ch.ID, 0, "a/b", "a/c", "first", "message")
	c.PostMessage(ch.ID, 0, "a/b", "a/c", "second", "message")

	msgs, err := c.ReadChannel(ch.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d want 2", len(msgs))
	}
	if msgs[0].Content != "first" {
		t.Errorf("order: got %q", msgs[0].Content)
	}
}

func TestComms_Threading(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}
	ch, _ := c.CreateDMChannel(1, "a/b", "a/c")
	parent, _ := c.PostMessage(ch.ID, 0, "a/b", "a/c", "parent", "message")
	reply, _ := c.PostMessage(ch.ID, parent.ID, "a/c", "a/b", "reply", "message")

	thread, err := c.ReadThread(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ReadThread returns parent + replies.
	if len(thread) != 2 {
		t.Errorf("got %d want 2", len(thread))
	}
	if thread[0].ID != parent.ID || thread[1].ID != reply.ID {
		t.Errorf("order wrong")
	}
}

func TestComms_DMChannelNormalized(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}

	ch1, _ := c.CreateDMChannel(1, "companyA/payment", "companyA/user")
	ch2, _ := c.CreateDMChannel(1, "companyA/user", "companyA/payment")
	if ch1.ID != ch2.ID {
		t.Errorf("A→B and B→A must share one channel: got %d vs %d", ch1.ID, ch2.ID)
	}
	if ch1.Name != ch2.Name {
		t.Errorf("channel name not normalized: %q vs %q", ch1.Name, ch2.Name)
	}
}

// GetOrCreateGeneralChannel returns the single global lobby where all agents
// live. It must be auto-created on store init + idempotent on lookup.
func TestComms_GeneralChannel(t *testing.T) {
	store := newTestStore(t)
	c := &Comms{Store: store}

	ch, err := c.GetOrCreateGeneralChannel()
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "general" || ch.Type != "group" {
		t.Errorf("got %+v, want name=general type=group", ch)
	}
	if ch.ID == 0 {
		t.Fatal("no channel id")
	}

	// Idempotent: a second lookup returns the same channel.
	ch2, _ := c.GetOrCreateGeneralChannel()
	if ch2.ID != ch.ID {
		t.Errorf("non-idempotent: got %d then %d", ch.ID, ch2.ID)
	}

	// Posts to general are readable via ReadChannel(ch.ID).
	c.PostMessage(ch.ID, 0, "ws/a", "ws/b", "lobby msg", "message")
	msgs, _ := c.ReadChannel(ch.ID, time.Time{})
	if len(msgs) != 1 || msgs[0].Content != "lobby msg" {
		t.Errorf("got %+v", msgs)
	}
}
// modernc/sqlite quirk: ON CONFLICT DO NOTHING leaves LastInsertId() holding
// the rowid of the most recent successful INSERT on the connection (not 0).
// CreateDMChannel must detect the existing-channel case via RowsAffected and
// return the real channel id, else PostMessage targets a non-existent channel.
func TestComms_CreateDMChannelExistingAfterUnrelatedInsert(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}

	ch1, err := c.CreateDMChannel(1, "companyA/payment", "companyA/user")
	if err != nil {
		t.Fatal(err)
	}
	// Unrelated INSERT on the same connection bumps LastInsertId() to a rowid
	// that is NOT ch1.ID — this is what tripped the original bug.
	if _, err := c.CreateDMChannel(1, "companyA/billing", "companyA/user"); err != nil {
		t.Fatal(err)
	}
	// Now re-create the original pair: channel already exists, so the INSERT
	// hits ON CONFLICT DO NOTHING. The returned id MUST be ch1.ID, not the
	// stale LastInsertId() left by the billing channel insert.
	ch2, err := c.CreateDMChannel(1, "companyA/payment", "companyA/user")
	if err != nil {
		t.Fatal(err)
	}
	if ch2.ID != ch1.ID {
		t.Fatalf("existing channel id drifted: got %d want %d (stale LastInsertId bug)", ch2.ID, ch1.ID)
	}
}
