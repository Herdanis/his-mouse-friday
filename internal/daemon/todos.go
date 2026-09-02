package daemon

import (
	"fmt"
	"time"
)

// ============================================
// Todos — work items bound to a thread root
// ============================================

type Todo struct {
	ID        int64     `json:"id"`
	ThreadID  int64     `json:"thread_id"`
	Content   string    `json:"content"`
	State     string    `json:"state"` // pending | done
	UpdatedAt time.Time `json:"updated_at"`
}

type TodoStore struct {
	Store *Store
}

// Add creates a work item, or returns the existing one when the same thread
// already has that exact item. An agent that loses track of the id it got back
// re-adds the item verbatim and marks the copy done, stranding the original as
// permanently pending — the count then never reaches full.
func (t *TodoStore) Add(threadID int64, content string) (Todo, error) {
	var existing Todo
	err := t.Store.db.QueryRow(
		`SELECT id, thread_id, content, state, updated_at FROM todos
		 WHERE thread_id=? AND content=? ORDER BY id LIMIT 1`, threadID, content).
		Scan(&existing.ID, &existing.ThreadID, &existing.Content, &existing.State, &existing.UpdatedAt)
	if err == nil {
		return existing, nil
	}
	now := time.Now().UTC()
	res, err := t.Store.db.Exec(
		`INSERT INTO todos(thread_id, content, state, updated_at) VALUES(?,?,'pending',?)`,
		threadID, content, now)
	if err != nil {
		return Todo{}, err
	}
	id, _ := res.LastInsertId()
	return Todo{ID: id, ThreadID: threadID, Content: content, State: "pending", UpdatedAt: now}, nil
}

func (t *TodoStore) Update(id int64, state string) error {
	if state != "pending" && state != "done" {
		return fmt.Errorf("state must be pending|done")
	}
	res, err := t.Store.db.Exec(`UPDATE todos SET state=?, updated_at=? WHERE id=?`, state, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (t *TodoStore) Delete(id int64) error {
	res, err := t.Store.db.Exec(`DELETE FROM todos WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no todo %d", id)
	}
	return nil
}

func (t *TodoStore) List(threadID int64) ([]Todo, error) {
	rows, err := t.Store.db.Query(
		`SELECT id, thread_id, content, state, updated_at FROM todos WHERE thread_id=? ORDER BY id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var td Todo
		if err := rows.Scan(&td.ID, &td.ThreadID, &td.Content, &td.State, &td.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, td)
	}
	return out, rows.Err()
}
