package daemon

import (
	"database/sql"
	"errors"
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

var ErrNotFound = errors.New("not found")

func (r *Registry) AddWorkspace(name string) (Workspace, error) {
	res, err := r.Store.db.Exec(`INSERT INTO workspaces(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, name)
	if err != nil {
		return Workspace{}, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		row := r.Store.db.QueryRow(`SELECT id FROM workspaces WHERE name=?`, name)
		err = row.Scan(&id)
		if err != nil {
			return Workspace{}, err
		}
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
	res, err := r.Store.db.Exec(
		`INSERT INTO projects(workspace_id, name, path) VALUES(?,?,?)
		 ON CONFLICT(workspace_id, name) DO UPDATE SET path=excluded.path`,
		wsID, projName, path)
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		row := r.Store.db.QueryRow(`SELECT id FROM projects WHERE workspace_id=? AND name=?`, wsID, projName)
		err = row.Scan(&id)
		if err != nil {
			return Project{}, err
		}
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
