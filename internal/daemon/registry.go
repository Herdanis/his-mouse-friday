package daemon

import (
	"database/sql"
	"errors"
	"fmt"
)

type Registry struct {
	Store *Store
}

type Workspace struct {
	ID   int64
	Name string
}

type Project struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Path        string
}

// ProjectListItem is a (workspace, name, path) row for the project_list RPC.
type ProjectListItem struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}

var ErrNotFound = errors.New("not found")

// upsertReturningID runs an INSERT … ON CONFLICT and returns the row's id.
// modernc/sqlite returns a stale LastInsertId() on ON CONFLICT DO NOTHING/UPDATE,
// so when RowsAffected==0 we re-SELECT the existing row by the unique key.
func (r *Registry) upsertReturningID(res sql.Result, selectQuery string, args ...any) (int64, error) {
	id, _ := res.LastInsertId()
	if id != 0 {
		return id, nil
	}
	row := r.Store.db.QueryRow(selectQuery, args...)
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (r *Registry) AddWorkspace(name string) (Workspace, error) {
	res, err := r.Store.db.Exec(`INSERT INTO workspaces(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, name)
	if err != nil {
		return Workspace{}, err
	}
	id, err := r.upsertReturningID(res, `SELECT id FROM workspaces WHERE name=?`, name)
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, Name: name}, nil
}

func (r *Registry) getWorkspaceID(name string) (int64, error) {
	var id int64
	err := r.Store.db.QueryRow(`SELECT id FROM workspaces WHERE name=?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return id, err
}

func (r *Registry) AddProject(wsName, projName, path string) (Project, error) {
	wsID, err := r.getWorkspaceID(wsName)
	if err != nil {
		return Project{}, err
	}
	// Path may be claimed by only one (workspace, project). Re-adding the
	// same (workspace, name) is allowed (upsert); a different claim blocks.
	var conflictWS, conflictName string
	err = r.Store.db.QueryRow(
		`SELECT w.name, p.name FROM projects p JOIN workspaces w ON p.workspace_id=w.id
		 WHERE p.path=? AND NOT (p.workspace_id=? AND p.name=?)`,
		path, wsID, projName).Scan(&conflictWS, &conflictName)
	if err == nil {
		return Project{}, fmt.Errorf("path %q already registered under %s/%s; delete that registration first", path, conflictWS, conflictName)
	}
	if err != sql.ErrNoRows {
		return Project{}, err
	}
	res, err := r.Store.db.Exec(
		`INSERT INTO projects(workspace_id, name, path) VALUES(?,?,?)
		 ON CONFLICT(workspace_id, name) DO UPDATE SET path=excluded.path`,
		wsID, projName, path)
	if err != nil {
		return Project{}, err
	}
	id, err := r.upsertReturningID(res, `SELECT id FROM projects WHERE workspace_id=? AND name=?`, wsID, projName)
	if err != nil {
		return Project{}, err
	}
	return Project{ID: id, WorkspaceID: wsID, Name: projName, Path: path}, nil
}

func (r *Registry) ResolveByPath(path string) (Project, Workspace, error) {
	var p Project
	var wsName string
	err := r.Store.db.QueryRow(
		`SELECT p.id, p.workspace_id, p.name, p.path, w.name
		 FROM projects p JOIN workspaces w ON p.workspace_id=w.id
		 WHERE p.path=?`, path).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Path, &wsName)
	if err == sql.ErrNoRows {
		return Project{}, Workspace{}, ErrNotFound
	}
	if err != nil {
		return Project{}, Workspace{}, err
	}
	return p, Workspace{ID: p.WorkspaceID, Name: wsName}, nil
}

func (r *Registry) ListProjects(wsName string) ([]Project, error) {
	wsID, err := r.getWorkspaceID(wsName)
	if err != nil {
		return nil, err
	}
	rows, err := r.Store.db.Query(`SELECT id, workspace_id, name, path FROM projects WHERE workspace_id=?`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Path); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Registry) ListWorkspaces() ([]string, error) {
	rows, err := r.Store.db.Query(`SELECT name FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ListAllProjects returns every project across all workspaces, ordered.
func (r *Registry) ListAllProjects() ([]ProjectListItem, error) {
	rows, err := r.Store.db.Query(
		`SELECT w.name, p.name, p.path FROM projects p JOIN workspaces w ON p.workspace_id=w.id ORDER BY w.name, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectListItem
	for rows.Next() {
		var it ProjectListItem
		if err := rows.Scan(&it.Workspace, &it.Name, &it.Path); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (r *Registry) DeleteWorkspace(name string) error {
	tx, err := r.Store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var wsID int64
	err = tx.QueryRow(`SELECT id FROM workspaces WHERE name=?`, name).Scan(&wsID)
	if err != nil {
		return ErrNotFound
	}
	// Order respects FKs: messages→channels, sessions→projects, channels→workspaces, projects→workspaces.
	if _, err := tx.Exec(`DELETE FROM messages WHERE channel_id IN (SELECT id FROM channels WHERE workspace_id=?)`, wsID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)`, wsID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM channels WHERE workspace_id=?`, wsID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE workspace_id=?`, wsID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM workspaces WHERE id=?`, wsID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) DeleteProject(wsName, projName string) error {
	wsID, err := r.getWorkspaceID(wsName)
	if err != nil {
		return err
	}
	res, err := r.Store.db.Exec(`DELETE FROM projects WHERE workspace_id=? AND name=?`, wsID, projName)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
