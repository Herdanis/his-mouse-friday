package daemon

import (
	"testing"
	"time"
)

// insertTestChannel inserts a channel row and returns its id, for tests that
// need a channel to post into but don't care about DM semantics (which
// production no longer exercises — wakeAgent posts to the general channel).
func insertTestChannel(t *testing.T, s *Store, wsID int64, name string) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO channels(workspace_id, name, type) VALUES(?,?,?)`,
		wsID, name, "dm")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestComms_PostAndRead(t *testing.T) {
	store := newTestStore(t)
	store.db.Exec(`INSERT INTO workspaces(id, name) VALUES(1, 'companyA')`)
	c := &Comms{Store: store}
	chID := insertTestChannel(t, store, 1, "test")

	msg, err := c.PostMessage(chID, 0, "companyA/payment", "companyA/user", "add field X", "delivered")
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
	chID := insertTestChannel(t, store, 1, "test")
	c.PostMessage(chID, 0, "a/b", "a/c", "first", "message")
	c.PostMessage(chID, 0, "a/b", "a/c", "second", "message")

	msgs, err := c.ReadChannel(chID, time.Time{})
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
	chID := insertTestChannel(t, store, 1, "test")
	parent, _ := c.PostMessage(chID, 0, "a/b", "a/c", "parent", "message")
	reply, _ := c.PostMessage(chID, parent.ID, "a/c", "a/b", "reply", "message")

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

// GetOrCreateGeneralChannel must be auto-created on init + idempotent on lookup.
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
