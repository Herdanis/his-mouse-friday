package daemon

import (
	"database/sql"
	"time"
)

type Channel struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Type        string
}

type Message struct {
	ID          int64     `json:"id"`
	ChannelID   int64     `json:"channel_id"`
	ThreadID    int64     `json:"thread_id"`
	FromProject string    `json:"from_project"`
	ToProject   string    `json:"to_project"`
	Content     string    `json:"content"`
	Status      string    `json:"status"`
	TS          time.Time `json:"ts"`
}

type Comms struct {
	Store *Store
}

func (c *Comms) PostMessage(channelID, threadID int64, from, to, content, status string) (Message, error) {
	if status == "" {
		status = "message"
	}
	now := time.Now().UTC()
	res, err := c.Store.db.Exec(
		`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, status, ts)
		 VALUES(?,?,?,?,?,?,?)`,
		channelID, nullIfZero(threadID), from, to, content, status, now)
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()
	return Message{ID: id, ChannelID: channelID, ThreadID: threadID, FromProject: from, ToProject: to, Content: content, Status: status, TS: now}, nil
}

func (c *Comms) ReadChannel(channelID int64, since time.Time) ([]Message, error) {
	rows, err := c.Store.db.Query(
		`SELECT id, channel_id, IFNULL(thread_id,0), from_project, IFNULL(to_project,''), content, status, ts
		 FROM messages WHERE channel_id=? AND ts > ? ORDER BY ts ASC`, channelID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// GetMessage fetches a single message by id (used to resolve a thread's root
// when a reply omits `to`).
func (c *Comms) GetMessage(id int64) (Message, error) {
	var m Message
	err := c.Store.db.QueryRow(
		`SELECT id, channel_id, IFNULL(thread_id,0), from_project, IFNULL(to_project,''), content, status, ts
		 FROM messages WHERE id=?`, id).
		Scan(&m.ID, &m.ChannelID, &m.ThreadID, &m.FromProject, &m.ToProject, &m.Content, &m.Status, &m.TS)
	return m, err
}

func (c *Comms) ReadThread(threadID int64) ([]Message, error) {
	rows, err := c.Store.db.Query(
		`SELECT id, channel_id, IFNULL(thread_id,0), from_project, IFNULL(to_project,''), content, status, ts
		 FROM messages WHERE id=? OR thread_id=? ORDER BY ts ASC`, threadID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.ThreadID, &m.FromProject, &m.ToProject, &m.Content, &m.Status, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetOrCreateGeneralChannel returns the single global "general" channel where
// all agents live. It is created on store init; this just looks it up.
func (c *Comms) GetOrCreateGeneralChannel() (Channel, error) {
	var ch Channel
	err := c.Store.db.QueryRow(
		`SELECT c.id, c.workspace_id, c.name, c.type
		 FROM channels c JOIN workspaces w ON c.workspace_id=w.id
		 WHERE c.name='general' AND w.name='__global__'`).
		Scan(&ch.ID, &ch.WorkspaceID, &ch.Name, &ch.Type)
	return ch, err
}
