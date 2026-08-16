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
	ID          int64
	ChannelID   int64
	ThreadID    int64
	FromProject string
	ToProject   string
	Content     string
	TS          time.Time
}

type Comms struct {
	Store *Store
}

func (c *Comms) CreateDMChannel(wsID int64, projA, projB string) (Channel, error) {
	name := projA + "::" + projB
	res, err := c.Store.db.Exec(
		`INSERT INTO channels(workspace_id, name, type) VALUES(?,?,?)
		 ON CONFLICT(workspace_id, name) DO NOTHING`,
		wsID, name, "dm")
	if err != nil {
		return Channel{}, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		var ch Channel
		err = c.Store.db.QueryRow(`SELECT id, workspace_id, name, type FROM channels WHERE workspace_id=? AND name=?`, wsID, name).
			Scan(&ch.ID, &ch.WorkspaceID, &ch.Name, &ch.Type)
		return ch, err
	}
	return Channel{ID: id, WorkspaceID: wsID, Name: name, Type: "dm"}, nil
}

func (c *Comms) PostMessage(channelID, threadID int64, from, to, content string) (Message, error) {
	now := time.Now().UTC()
	res, err := c.Store.db.Exec(
		`INSERT INTO messages(channel_id, thread_id, from_project, to_project, content, ts)
		 VALUES(?,?,?,?,?,?)`,
		channelID, nullIfZero(threadID), from, to, content, now)
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()
	return Message{ID: id, ChannelID: channelID, ThreadID: threadID, FromProject: from, ToProject: to, Content: content, TS: now}, nil
}

func nullIfZero(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

func (c *Comms) ReadChannel(channelID int64, since time.Time) ([]Message, error) {
	rows, err := c.Store.db.Query(
		`SELECT id, channel_id, IFNULL(thread_id,0), from_project, IFNULL(to_project,''), content, ts
		 FROM messages WHERE channel_id=? AND ts > ? ORDER BY ts ASC`, channelID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (c *Comms) ReadThread(threadID int64) ([]Message, error) {
	rows, err := c.Store.db.Query(
		`SELECT id, channel_id, IFNULL(thread_id,0), from_project, IFNULL(to_project,''), content, ts
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
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.ThreadID, &m.FromProject, &m.ToProject, &m.Content, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
