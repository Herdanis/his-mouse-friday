package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Registry struct {
	Store *Store
}

type Project struct {
	ID   int64
	Name string
	Path string
}

// ProjectListItem is a (name, path) row for the project_list RPC.
type ProjectListItem struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

var ErrNotFound = errors.New("not found")

// AddProject registers a project by bare name. Identity is UNIQUE(name): a
// duplicate name is an error naming the conflict, and a path claimed by a
// different project is blocked too.
func (r *Registry) AddProject(name, path string) (Project, error) {
	var conflict string
	err := r.Store.db.QueryRow(
		`SELECT name FROM projects WHERE path=? AND name<>?`, path, name).Scan(&conflict)
	if err == nil {
		return Project{}, fmt.Errorf("path %q already registered under %s; delete that registration first", path, conflict)
	}
	if err != sql.ErrNoRows {
		return Project{}, err
	}
	res, err := r.Store.db.Exec(`INSERT INTO projects(name, path) VALUES(?,?)`, name, path)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: projects.name") {
			return Project{}, fmt.Errorf("project %q already registered; delete it first or pick another name", name)
		}
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return Project{ID: id, Name: name, Path: path}, nil
}

func (r *Registry) FindProject(name string) (Project, error) {
	var p Project
	err := r.Store.db.QueryRow(
		`SELECT id, name, path FROM projects WHERE name=?`, name).
		Scan(&p.ID, &p.Name, &p.Path)
	return p, err
}

// ResolveProject resolves a bare project name. UNIQUE(name) keeps matches to
// one; the 2+ check is a defensive guard that names the candidates.
func (r *Registry) ResolveProject(name string) (Project, error) {
	projs, err := r.lookupByName(name)
	if err != nil {
		return Project{}, err
	}
	switch len(projs) {
	case 0:
		return Project{}, ErrNotFound
	case 1:
		return projs[0], nil
	default:
		var names []string
		for _, p := range projs {
			names = append(names, p.Name)
		}
		return Project{}, fmt.Errorf("ambiguous %q: matches %s", name, strings.Join(names, ", "))
	}
}

func (r *Registry) lookupByName(name string) ([]Project, error) {
	rows, err := r.Store.db.Query(`SELECT id, name, path FROM projects WHERE name=?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Registry) ResolveByPath(path string) (Project, error) {
	var p Project
	err := r.Store.db.QueryRow(
		`SELECT id, name, path FROM projects WHERE path=?`, path).
		Scan(&p.ID, &p.Name, &p.Path)
	if err == sql.ErrNoRows {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

func (r *Registry) ListProjects() ([]Project, error) {
	rows, err := r.Store.db.Query(`SELECT id, name, path FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Registry) DeleteProject(name string) error {
	tx, err := r.Store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// sessions.project_id references projects(id); clear them first.
	if _, err := tx.Exec(`DELETE FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE name=?)`, name); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM projects WHERE name=?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
