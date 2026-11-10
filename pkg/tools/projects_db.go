package tools

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	_ "modernc.org/sqlite"
)

// ProjectsDBPath is the per profile projects database under the home
func ProjectsDBPath(h string) string { return filepath.Join(h, "projects.db") }

const projectsSchemaSQL = `
CREATE TABLE IF NOT EXISTS projects (
    id            TEXT PRIMARY KEY,
    slug          TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    description   TEXT,
    icon          TEXT,
    color         TEXT,
    board_slug    TEXT,
    primary_path  TEXT,
    created_at    INTEGER NOT NULL,
    archived      INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS project_folders (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    label       TEXT,
    is_primary  INTEGER NOT NULL DEFAULT 0,
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (project_id, path)
);
CREATE INDEX IF NOT EXISTS idx_project_folders_path ON project_folders(path);
CREATE TABLE IF NOT EXISTS project_meta (
    key    TEXT PRIMARY KEY,
    value  TEXT
);
CREATE TABLE IF NOT EXISTS discovered_repos (
    root          TEXT PRIMARY KEY,
    label         TEXT,
    last_seen     INTEGER NOT NULL
);
`

var (
	projectSlugRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]{0,63}$`)
	projectBranchSafeRe = regexp.MustCompile(`[^a-z0-9._-]+`)
	projectSlugifyRe    = regexp.MustCompile(`[^a-z0-9]+`)
	projectsInitialized = map[string]bool{}
	projectsInitMu      sync.Mutex
)

const (
	projectActiveMetaKey          = "active_id"
	projectDiscoveryPolicyMetaKey = "repo_discovery_policy"
)

var projectOptionalColumns = []string{"board_slug", "primary_path", "icon", "color"}

// ProjectFolder is one folder attached to a project
type ProjectFolder struct {
	Path      string
	Label     *string
	IsPrimary bool
	AddedAt   int64
}

// JSON row of the folder
func (f ProjectFolder) AsMap() map[string]any {
	return map[string]any{"path": f.Path, "label": nilOrString(f.Label), "is_primary": f.IsPrimary, "added_at": f.AddedAt}
}

// Project is one named workspace with its folders
type Project struct {
	ID          string
	Slug        string
	Name        string
	CreatedAt   int64
	Description *string
	Icon        *string
	Color       *string
	BoardSlug   *string
	PrimaryPath *string
	Archived    bool
	Folders     []ProjectFolder
}

// JSON row of the project with its folders
func (p *Project) AsMap() map[string]any {
	folders := make([]any, len(p.Folders))
	for i, f := range p.Folders {
		folders[i] = f.AsMap()
	}
	return map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Name, "description": nilOrString(p.Description), "icon": nilOrString(p.Icon),
		"color": nilOrString(p.Color), "board_slug": nilOrString(p.BoardSlug), "primary_path": nilOrString(p.PrimaryPath),
		"archived": p.Archived, "created_at": p.CreatedAt, "folders": folders}
}

// PrimaryPathOf is the explicit primary path, else the primary folder, else the first folder
func (p *Project) PrimaryPathOf() string {
	if p.PrimaryPath != nil && *p.PrimaryPath != "" {
		return *p.PrimaryPath
	}
	for _, f := range p.Folders {
		if f.IsPrimary {
			return f.Path
		}
	}
	if len(p.Folders) > 0 {
		return p.Folders[0].Path
	}
	return ""
}

// JSON value of an optional string
func nilOrString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Slug candidate derived from a human name
func projectSlugify(name string) string {
	s := strings.Trim(projectSlugifyRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-_")
	if len(s) > 64 {
		s = s[:64]
	}
	s = strings.Trim(s, "-_")
	if s == "" {
		return "project"
	}
	return s
}

// NormalizeProjectSlug lowercases and validates a slug, empty for empty input
func NormalizeProjectSlug(slug string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(slug))
	if s == "" {
		return "", nil
	}
	if !projectSlugRe.MatchString(s) {
		return "", fmt.Errorf("invalid project slug %s: must be 1-64 chars, lowercase alphanumerics / hyphens / underscores, not starting with '-' or '_'", pyRepr(slug))
	}
	return s, nil
}

// Absolute, user expanded, separator normalized path with no trailing separator
func projectNormalizePath(path string) string {
	p := expandTilde(strings.TrimSpace(path))
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	trimmed := strings.TrimRight(p, `/\`)
	if trimmed == "" {
		return p
	}
	return trimmed
}

// ProjectsDB is an open per profile projects database
type ProjectsDB struct {
	db   *sql.DB
	path string
}

// Configured database.journal_mode, wal unless delete is set
func projectsJournalMode(env *Env) string {
	doc, err := readRawConfig(env)
	if err != nil {
		return "wal"
	}
	if database, ok := doc["database"].(map[string]any); ok {
		if raw, ok := database["journal_mode"].(string); ok {
			if mode := strings.ToLower(strings.TrimSpace(raw)); mode == "delete" {
				return mode
			}
		}
	}
	return "wal"
}

// OpenProjectsDB opens and initializes the projects database of the env home
func OpenProjectsDB(ctx context.Context, env *Env) (*ProjectsDB, error) {
	return OpenProjectsDBAt(WithEnvHome(ctx, env), ProjectsDBPath(env.Home), projectsJournalMode(env))
}

// OpenProjectsDBAt opens the database at a path with WAL or DELETE journaling, creating the parent and the schema
func OpenProjectsDBAt(ctx context.Context, path, journalMode string) (*ProjectsDB, error) {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return nil, err
	}
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(" + journalMode + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	state.TrackConnection(path)
	p := &ProjectsDB{db: db, path: path}
	if err := p.initialize(resolved); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Runs the idempotent schema and additive column migrations once per path per process
func (p *ProjectsDB) initialize(resolved string) error {
	projectsInitMu.Lock()
	defer projectsInitMu.Unlock()
	if projectsInitialized[resolved] {
		return nil
	}
	if _, err := p.db.Exec(projectsSchemaSQL); err != nil {
		return err
	}
	rows, err := p.db.Query("PRAGMA table_info(projects)")
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	for _, col := range projectOptionalColumns {
		if cols[col] {
			continue
		}
		if _, err := p.db.Exec(fmt.Sprintf("ALTER TABLE projects ADD COLUMN %s TEXT", col)); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	projectsInitialized[resolved] = true
	return nil
}

// Close releases the connection
func (p *ProjectsDB) Close() error {
	state.UntrackConnection(p.path)
	return p.db.Close()
}

// sqlExecutor is what both a connection and the pool offer to queries
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Runs fn inside an IMMEDIATE write transaction on one connection, rolling back on error
func (p *ProjectsDB) writeTxn(fn func(x sqlExecutor) error) error {
	ctx := context.Background()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// Unix seconds now
func projectNow() int64 { return time.Now().Unix() }

// Materializes one projects row together with its folders
func (p *ProjectsDB) loadProject(x sqlExecutor, id string) (*Project, error) {
	ctx := context.Background()
	row := x.QueryRowContext(ctx, "SELECT id, slug, name, created_at, archived, description, icon, color, board_slug, primary_path FROM projects WHERE id = ?", id)
	proj := &Project{}
	var archived int
	if err := row.Scan(&proj.ID, &proj.Slug, &proj.Name, &proj.CreatedAt, &archived, &proj.Description, &proj.Icon, &proj.Color, &proj.BoardSlug, &proj.PrimaryPath); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	proj.Archived = archived != 0
	rows, err := x.QueryContext(ctx, "SELECT path, label, is_primary, added_at FROM project_folders WHERE project_id = ? ORDER BY is_primary DESC, added_at ASC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f ProjectFolder
		var primary int
		if err := rows.Scan(&f.Path, &f.Label, &primary, &f.AddedAt); err != nil {
			return nil, err
		}
		f.IsPrimary = primary != 0
		proj.Folders = append(proj.Folders, f)
	}
	return proj, rows.Err()
}

// The candidate or candidate-2, -3 and so on when taken
func projectUniqueSlug(x sqlExecutor, candidate string) (string, error) {
	n, slug := 1, candidate
	for {
		var one int
		err := x.QueryRowContext(context.Background(), "SELECT 1 FROM projects WHERE slug = ?", slug).Scan(&one)
		if err == sql.ErrNoRows {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
		n++
		suffix := fmt.Sprintf("-%d", n)
		base := candidate
		if len(base) > 64-len(suffix) {
			base = base[:64-len(suffix)]
		}
		slug = strings.TrimRight(base, "-_") + suffix
	}
}

// Comparison key for primary path dedup, case folded on Windows
func projectPrimaryPathKey(path string) string {
	key := projectNormalizePath(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(strings.ReplaceAll(key, "/", `\`))
	}
	return key
}

// FindByPrimaryPath is the oldest project whose primary path matches, nil when none
func (p *ProjectsDB) FindByPrimaryPath(path string, includeArchived bool) (*Project, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	key := projectPrimaryPathKey(path)
	projects, err := p.ListProjects(includeArchived)
	if err != nil {
		return nil, err
	}
	for _, proj := range projects {
		if primary := proj.PrimaryPathOf(); primary != "" && projectPrimaryPathKey(primary) == key {
			return proj, nil
		}
	}
	return nil, nil
}

// CreateProjectOptions are the optional fields of a new project
type CreateProjectOptions struct {
	Slug               string
	Folders            []string
	PrimaryPath        string
	Description        *string
	Icon               *string
	Color              *string
	BoardSlug          string
	AllowDuplicatePath bool
}

// CreateProject inserts a project and returns its id, the primary path joining the folder set and marked primary
func (p *ProjectsDB) CreateProject(name string, opts CreateProjectOptions) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("project name must not be empty")
	}
	slugCandidate := projectSlugify(name)
	if opts.Slug != "" {
		s, err := NormalizeProjectSlug(opts.Slug)
		if err != nil {
			return "", err
		}
		slugCandidate = s
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	pid := "p_" + hex.EncodeToString(raw[:])
	now := projectNow()
	var folderPaths []string
	seen := map[string]bool{}
	for _, f := range opts.Folders {
		norm := projectNormalizePath(f)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		folderPaths = append(folderPaths, norm)
	}
	primary := ""
	if opts.PrimaryPath != "" {
		primary = projectNormalizePath(opts.PrimaryPath)
	}
	if primary != "" && !seen[primary] {
		folderPaths = append([]string{primary}, folderPaths...)
	}
	if primary == "" && len(folderPaths) > 0 {
		primary = folderPaths[0]
	}
	if primary != "" && !opts.AllowDuplicatePath {
		existing, err := p.FindByPrimaryPath(primary, false)
		if err != nil {
			return "", err
		}
		if existing != nil {
			return "", fmt.Errorf("folder already belongs to project '%s' (%s); switch to it instead of creating a duplicate", existing.Slug, existing.ID)
		}
	}
	var boardSlug *string
	if opts.BoardSlug != "" {
		s, err := NormalizeProjectSlug(opts.BoardSlug)
		if err != nil {
			return "", err
		}
		boardSlug = &s
	}
	var primaryValue *string
	if primary != "" {
		primaryValue = &primary
	}
	err := p.writeTxn(func(x sqlExecutor) error {
		slug, err := projectUniqueSlug(x, slugCandidate)
		if err != nil {
			return err
		}
		ctx := context.Background()
		if _, err := x.ExecContext(ctx, "INSERT INTO projects (id, slug, name, description, icon, color, board_slug,  primary_path, created_at, archived) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)",
			pid, slug, name, opts.Description, opts.Icon, opts.Color, boardSlug, primaryValue, now); err != nil {
			return err
		}
		for _, path := range folderPaths {
			isPrimary := 0
			if path == primary {
				isPrimary = 1
			}
			if _, err := x.ExecContext(ctx, "INSERT INTO project_folders (project_id, path, label, is_primary, added_at) VALUES (?, ?, ?, ?, ?)", pid, path, nil, isPrimary, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return pid, nil
}

// ListProjects returns projects oldest first, archived ones only when asked
func (p *ProjectsDB) ListProjects(includeArchived bool) ([]*Project, error) {
	q := "SELECT id FROM projects"
	if !includeArchived {
		q += " WHERE archived = 0"
	}
	q += " ORDER BY created_at ASC"
	rows, err := p.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]*Project, 0, len(ids))
	for _, id := range ids {
		proj, err := p.loadProject(p.db, id)
		if err != nil {
			return nil, err
		}
		if proj != nil {
			out = append(out, proj)
		}
	}
	return out, nil
}

// GetProject looks a project up by id first, then by slug
func (p *ProjectsDB) GetProject(idOrSlug string) (*Project, error) {
	return p.getProject(p.db, idOrSlug)
}

// Id then slug lookup on an executor
func (p *ProjectsDB) getProject(x sqlExecutor, idOrSlug string) (*Project, error) {
	ctx := context.Background()
	var id string
	err := x.QueryRowContext(ctx, "SELECT id FROM projects WHERE id = ?", idOrSlug).Scan(&id)
	if err == sql.ErrNoRows {
		err = x.QueryRowContext(ctx, "SELECT id FROM projects WHERE slug = ?", strings.ToLower(idOrSlug)).Scan(&id)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p.loadProject(x, id)
}

// UpdateProjectFields are the patchable top level fields, nil leaves a field untouched and empty clears icon, color and board slug
type UpdateProjectFields struct {
	Name        *string
	Description *string
	Icon        *string
	Color       *string
	BoardSlug   *string
}

// UpdateProject patches the provided fields and reports whether a row changed
func (p *ProjectsDB) UpdateProject(projectID string, f UpdateProjectFields) (bool, error) {
	var sets []string
	var params []any
	if f.Name != nil {
		name := strings.TrimSpace(*f.Name)
		if name == "" {
			return false, fmt.Errorf("project name must not be empty")
		}
		sets, params = append(sets, "name = ?"), append(params, name)
	}
	if f.Description != nil {
		sets, params = append(sets, "description = ?"), append(params, *f.Description)
	}
	if f.Icon != nil {
		sets, params = append(sets, "icon = ?"), append(params, emptyAsNull(*f.Icon))
	}
	if f.Color != nil {
		sets, params = append(sets, "color = ?"), append(params, emptyAsNull(*f.Color))
	}
	if f.BoardSlug != nil {
		slug := ""
		if strings.TrimSpace(*f.BoardSlug) != "" {
			s, err := NormalizeProjectSlug(*f.BoardSlug)
			if err != nil {
				return false, err
			}
			slug = s
		}
		sets, params = append(sets, "board_slug = ?"), append(params, emptyAsNull(slug))
	}
	if len(sets) == 0 {
		return false, nil
	}
	params = append(params, projectID)
	n, err := p.execRowcount("UPDATE projects SET "+strings.Join(sets, ", ")+" WHERE id = ?", params...)
	return n > 0, err
}

// NULL for an empty string
func emptyAsNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Runs one write statement in its own transaction and returns the affected row count
func (p *ProjectsDB) execRowcount(query string, params ...any) (int64, error) {
	var n int64
	err := p.writeTxn(func(x sqlExecutor) error {
		res, err := x.ExecContext(context.Background(), query, params...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// AddFolder attaches a folder and returns its normalized path, the first folder or an explicit primary becoming primary
func (p *ProjectsDB) AddFolder(projectID, path string, label *string, isPrimary bool) (string, error) {
	norm := projectNormalizePath(path)
	if norm == "" {
		return "", fmt.Errorf("folder path must not be empty")
	}
	proj, err := p.GetProject(projectID)
	if err != nil {
		return "", err
	}
	if proj == nil {
		return "", fmt.Errorf("no such project: %s", projectID)
	}
	err = p.writeTxn(func(x sqlExecutor) error {
		ctx := context.Background()
		if _, err := x.ExecContext(ctx, "INSERT OR IGNORE INTO project_folders (project_id, path, label, is_primary, added_at) VALUES (?, ?, ?, 0, ?)", projectID, norm, label, projectNow()); err != nil {
			return err
		}
		if label != nil {
			if _, err := x.ExecContext(ctx, "UPDATE project_folders SET label = ? WHERE project_id = ? AND path = ?", *label, projectID, norm); err != nil {
				return err
			}
		}
		var one int
		hasPrimary := x.QueryRowContext(ctx, "SELECT 1 FROM project_folders WHERE project_id = ? AND is_primary = 1", projectID).Scan(&one) == nil
		if isPrimary || !hasPrimary {
			return projectSetPrimaryLocked(x, projectID, norm)
		}
		return nil
	})
	return norm, err
}

// RemoveFolder detaches a folder and repoints the primary when it was primary
func (p *ProjectsDB) RemoveFolder(projectID, path string) (bool, error) {
	norm := projectNormalizePath(path)
	var removed int64
	err := p.writeTxn(func(x sqlExecutor) error {
		ctx := context.Background()
		var wasPrimary int
		found := x.QueryRowContext(ctx, "SELECT is_primary FROM project_folders WHERE project_id = ? AND path = ?", projectID, norm).Scan(&wasPrimary) == nil
		res, err := x.ExecContext(ctx, "DELETE FROM project_folders WHERE project_id = ? AND path = ?", projectID, norm)
		if err != nil {
			return err
		}
		removed, _ = res.RowsAffected()
		if found && wasPrimary != 0 {
			var next string
			if err := x.QueryRowContext(ctx, "SELECT path FROM project_folders WHERE project_id = ? ORDER BY added_at ASC LIMIT 1", projectID).Scan(&next); err == nil && next != "" {
				return projectSetPrimaryLocked(x, projectID, next)
			}
			_, err = x.ExecContext(ctx, "UPDATE projects SET primary_path = NULL WHERE id = ?", projectID)
			return err
		}
		return nil
	})
	return removed > 0, err
}

// Sets the primary folder inside a write transaction
func projectSetPrimaryLocked(x sqlExecutor, projectID, path string) error {
	ctx := context.Background()
	if _, err := x.ExecContext(ctx, "UPDATE project_folders SET is_primary = 0 WHERE project_id = ?", projectID); err != nil {
		return err
	}
	if _, err := x.ExecContext(ctx, "UPDATE project_folders SET is_primary = 1 WHERE project_id = ? AND path = ?", projectID, path); err != nil {
		return err
	}
	_, err := x.ExecContext(ctx, "UPDATE projects SET primary_path = ? WHERE id = ?", path, projectID)
	return err
}

// SetPrimary marks an attached folder primary, false when the folder is not attached
func (p *ProjectsDB) SetPrimary(projectID, path string) (bool, error) {
	norm := projectNormalizePath(path)
	ok := false
	err := p.writeTxn(func(x sqlExecutor) error {
		var one int
		if x.QueryRowContext(context.Background(), "SELECT 1 FROM project_folders WHERE project_id = ? AND path = ?", projectID, norm).Scan(&one) != nil {
			return nil
		}
		ok = true
		return projectSetPrimaryLocked(x, projectID, norm)
	})
	return ok, err
}

// ArchiveProject hides a project from the default listing
func (p *ProjectsDB) ArchiveProject(projectID string) (bool, error) {
	n, err := p.execRowcount("UPDATE projects SET archived = 1 WHERE id = ?", projectID)
	return n > 0, err
}

// RestoreProject unarchives a project
func (p *ProjectsDB) RestoreProject(projectID string) (bool, error) {
	n, err := p.execRowcount("UPDATE projects SET archived = 0 WHERE id = ?", projectID)
	return n > 0, err
}

// DeleteProject hard deletes a project and its folders
func (p *ProjectsDB) DeleteProject(projectID string) (bool, error) {
	n, err := p.execRowcount("DELETE FROM projects WHERE id = ?", projectID)
	return n > 0, err
}

// Upserts a project_meta row inside a write transaction
func projectUpsertMetaLocked(x sqlExecutor, key, value string) error {
	_, err := x.ExecContext(context.Background(), "INSERT INTO project_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// Meta value, nil when absent
func (p *ProjectsDB) getMeta(key string) (*string, error) {
	var value *string
	err := p.db.QueryRowContext(context.Background(), "SELECT value FROM project_meta WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return value, err
}

// SetActive sets or clears the active project pointer
func (p *ProjectsDB) SetActive(projectID *string) error {
	return p.writeTxn(func(x sqlExecutor) error {
		if projectID == nil {
			_, err := x.ExecContext(context.Background(), "DELETE FROM project_meta WHERE key = ?", projectActiveMetaKey)
			return err
		}
		return projectUpsertMetaLocked(x, projectActiveMetaKey, *projectID)
	})
}

// GetActiveID is the active project pointer, empty when unset
func (p *ProjectsDB) GetActiveID() (string, error) {
	v, err := p.getMeta(projectActiveMetaKey)
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

// GetDiscoveryPolicyKey is the recorded repo discovery policy, nil when unset
func (p *ProjectsDB) GetDiscoveryPolicyKey() (*string, error) {
	return p.getMeta(projectDiscoveryPolicyMetaKey)
}

// Optionally wipes the scan cache then records the policy key inside a write transaction
func projectClearReposLocked(x sqlExecutor, clear bool, policyKey *string) error {
	if clear {
		if _, err := x.ExecContext(context.Background(), "DELETE FROM discovered_repos"); err != nil {
			return err
		}
	}
	if policyKey != nil {
		return projectUpsertMetaLocked(x, projectDiscoveryPolicyMetaKey, *policyKey)
	}
	return nil
}

// ReconcileDiscoveredReposPolicy clears cached scan rows when the policy changes and reports whether it did
func (p *ProjectsDB) ReconcileDiscoveredReposPolicy(policyKey string, preserveUnversioned bool) (bool, error) {
	current, err := p.GetDiscoveryPolicyKey()
	if err != nil {
		return false, err
	}
	if current != nil && *current == policyKey {
		return false, nil
	}
	cleared := current != nil || !preserveUnversioned
	err = p.writeTxn(func(x sqlExecutor) error { return projectClearReposLocked(x, cleared, &policyKey) })
	return cleared, err
}

// ClearDiscoveredRepos wipes the scan cache and records the policy key when given
func (p *ProjectsDB) ClearDiscoveredRepos(policyKey *string) error {
	return p.writeTxn(func(x sqlExecutor) error { return projectClearReposLocked(x, true, policyKey) })
}

// DiscoveredRepo is one cached repo root with its label
type DiscoveredRepo struct {
	Root     string
	Label    string
	LastSeen int64
}

// RecordDiscoveredRepos persists scanned repo roots, replace deleting stale rows first, and returns the row count
func (p *ProjectsDB) RecordDiscoveredRepos(repos []DiscoveredRepo, replace bool, policyKey *string) (int, error) {
	now := projectNow()
	var rows []DiscoveredRepo
	for _, r := range repos {
		norm := projectNormalizePath(r.Root)
		if norm == "" {
			continue
		}
		label := r.Label
		if label == "" {
			label = filepath.Base(norm)
			if label == "" || label == "." || label == string(filepath.Separator) {
				label = norm
			}
		}
		rows = append(rows, DiscoveredRepo{Root: norm, Label: label, LastSeen: now})
	}
	err := p.writeTxn(func(x sqlExecutor) error {
		ctx := context.Background()
		if replace {
			if _, err := x.ExecContext(ctx, "DELETE FROM discovered_repos"); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if _, err := x.ExecContext(ctx, "INSERT INTO discovered_repos (root, label, last_seen) VALUES (?, ?, ?) ON CONFLICT(root) DO UPDATE SET label = excluded.label, last_seen = excluded.last_seen", r.Root, r.Label, r.LastSeen); err != nil {
				return err
			}
		}
		return projectClearReposLocked(x, false, policyKey)
	})
	return len(rows), err
}

// ListDiscoveredRepos returns cached repo roots most recently seen first
func (p *ProjectsDB) ListDiscoveredRepos() ([]DiscoveredRepo, error) {
	rows, err := p.db.QueryContext(context.Background(), "SELECT root, label, last_seen FROM discovered_repos ORDER BY last_seen DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoveredRepo
	for rows.Next() {
		var r DiscoveredRepo
		var label *string
		if err := rows.Scan(&r.Root, &label, &r.LastSeen); err != nil {
			return nil, err
		}
		if label != nil {
			r.Label = *label
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProjectForPath returns the innermost project whose folder equals or contains the path
func (p *ProjectsDB) ProjectForPath(path string, includeArchived bool) (*Project, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	target := projectNormalizePath(path)
	q := "SELECT pf.project_id AS pid, pf.path AS folder FROM project_folders pf JOIN projects p ON p.id = pf.project_id"
	if !includeArchived {
		q += " WHERE p.archived = 0"
	}
	rows, err := p.db.QueryContext(context.Background(), q)
	if err != nil {
		return nil, err
	}
	bestID, bestLen := "", -1
	for rows.Next() {
		var pid, folder string
		if err := rows.Scan(&pid, &folder); err != nil {
			rows.Close()
			return nil, err
		}
		stem := strings.TrimRight(folder, `/\`)
		owns := target == folder || strings.HasPrefix(target, stem+string(os.PathSeparator)) || strings.HasPrefix(target, stem+"/")
		if owns && len(folder) > bestLen {
			bestID, bestLen = pid, len(folder)
		}
	}
	rows.Close()
	if bestID == "" {
		return nil, nil
	}
	return p.GetProject(bestID)
}

// BranchNameFor is the deterministic <project-slug>/<task-id>[-<title-slug>] branch name for a project linked task
func BranchNameFor(project *Project, taskID, title string) string {
	slug := project.Slug
	if slug == "" {
		slug = projectSlugify(project.Name)
	}
	base := slug + "/" + taskID
	if title == "" {
		return base
	}
	tslug := strings.Trim(projectBranchSafeRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), "-"), "-")
	if len(tslug) > 40 {
		tslug = tslug[:40]
	}
	tslug = strings.Trim(tslug, "-")
	if tslug == "" {
		return base
	}
	return base + "-" + tslug
}
