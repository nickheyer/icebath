package kanban

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"

	"golang.org/x/text/cases"
)

// Task id of t_ and four random hex bytes
func newTaskID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "t_" + hex.EncodeToString(b)
}

// host:pid identifying this claimer
func claimerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// host: prefix shared by every claim lock issued from this host
func hostPrefix() string {
	host, _, _ := strings.Cut(claimerID(), ":")
	return host + ":"
}

// Stripped model and provider overrides, a provider without a model being a ValueError
func validateModelOverride(model, provider *string) (*string, *string, error) {
	strip := func(s *string) *string {
		if s == nil {
			return nil
		}
		v := strings.TrimSpace(*s)
		if v == "" {
			return nil
		}
		return &v
	}
	m, p := strip(model), strip(provider)
	if p != nil && m == nil {
		return nil, nil, &ValueError{"provider_override requires a model_override"}
	}
	return m, p, nil
}

// Profile normalized assignee, nil staying nil
func canonicalAssignee(assignee *string) (*string, error) {
	if assignee == nil {
		return nil, nil
	}
	name, err := profiles.NormalizeProfileName(*assignee)
	if err != nil {
		return nil, err
	}
	return &name, nil
}

// Project link create_task stores, the project, the repo a worktree path still derives from and the workspace kind
type projectLink struct {
	ProjectID     *string
	Project       *tools.Project
	ProjectRepo   *string
	WorkspaceKind string
}

// Anchors a project linked task to its primary repo as a worktree, recovering the project from a source task when needed
func resolveProjectLink(ctx context.Context, conn state.SQLiteConn, projectID, sourceTaskID *string, workspaceKind string, workspacePath *string) (projectLink, error) {
	none := projectLink{WorkspaceKind: workspaceKind}
	id := ""
	if projectID != nil {
		id = strings.TrimSpace(*projectID)
	}
	if id == "" {
		return none, nil
	}
	project := lookupProject(ctx, id)
	var repo *string
	if project == nil && sourceTaskID != nil && *sourceTaskID != "" {
		var err error
		project, repo, err = projectFromSourceTask(ctx, conn, id, *sourceTaskID)
		if err != nil {
			return none, err
		}
		if project != nil && workspaceKind == "scratch" {
			workspaceKind = "worktree"
		}
	}
	if project == nil {
		return projectLink{WorkspaceKind: workspaceKind}, nil
	}
	primary := project.PrimaryPath != nil && *project.PrimaryPath != ""
	if workspaceKind == "scratch" && primary {
		workspaceKind = "worktree"
	}
	if workspaceKind == "worktree" && workspacePath == nil && primary {
		p := *project.PrimaryPath
		repo = &p
	}
	pid := project.ID
	return projectLink{ProjectID: &pid, Project: project, ProjectRepo: repo, WorkspaceKind: workspaceKind}, nil
}

// Project from the active profile projects database, nil on any failure
func lookupProject(ctx context.Context, id string) (project *tools.Project) {
	defer func() {
		if recover() != nil {
			project = nil
		}
	}()
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		env = &tools.Env{Home: home.Dir(ctx)}
	}
	db, err := tools.OpenProjectsDB(tools.WithEnvHome(ctx, env), env)
	if err != nil {
		return nil
	}
	defer db.Close()
	p, err := db.GetProject(id)
	if err != nil {
		return nil
	}
	return p
}

// Project and repo recovered from a canonical repo/.worktrees/id project worktree task on this board
func projectFromSourceTask(ctx context.Context, conn state.SQLiteConn, projectID, sourceTaskID string) (*tools.Project, *string, error) {
	src, err := getTask(ctx, conn, sourceTaskID)
	if err != nil {
		return nil, nil, err
	}
	if src == nil || src.ProjectID == nil || *src.ProjectID != projectID || src.WorkspaceKind != "worktree" ||
		src.WorkspacePath == nil || *src.WorkspacePath == "" {
		return nil, nil, nil
	}
	sourcePath := filepath.Clean(*src.WorkspacePath)
	if !filepath.IsAbs(sourcePath) || pathutil.Name(sourcePath) != src.ID || pathutil.Name(filepath.Dir(filepath.Clean(sourcePath))) != ".worktrees" {
		return nil, nil, nil
	}
	slug := ""
	found := false
	if src.BranchName != nil && *src.BranchName != "" {
		prefix, leaf, sep := strings.Cut(*src.BranchName, "/")
		if sep && (leaf == src.ID || strings.HasPrefix(leaf, src.ID+"-")) {
			if s, err := tools.NormalizeProjectSlug(prefix); err == nil {
				slug, found = s, true
			}
		}
	}
	if !found {
		if s, err := tools.NormalizeProjectSlug(projectID); err == nil {
			slug = s
		}
	}
	if slug == "" {
		return nil, nil, nil
	}
	repo := filepath.Dir(filepath.Clean(filepath.Dir(filepath.Clean(sourcePath))))
	primary := repo
	return &tools.Project{ID: projectID, Slug: slug, Name: slug, CreatedAt: 0, PrimaryPath: &primary}, &repo, nil
}

// Stripped deduplicated skills, nil for no list, refusing commas and toolset names all at once
func normalizeTaskSkills(skills []string) ([]string, error) {
	if skills == nil {
		return nil, nil
	}
	cleaned := []string{}
	seen := map[string]bool{}
	var typos []string
	known := knownToolsetNames()
	for _, s := range skills {
		name := strings.TrimSpace(s)
		if name == "" {
			continue
		}
		if strings.Contains(name, ",") {
			return nil, &ValueError{fmt.Sprintf("skill name cannot contain comma: %s "+
				"(pass a list of separate names instead of a comma-joined string)", errchain.Show(name))}
		}
		if known[cases.Fold().String(name)] {
			typos = append(typos, name)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		cleaned = append(cleaned, name)
	}
	if len(typos) > 0 {
		quoted := make([]string, len(typos))
		for i, n := range typos {
			quoted[i] = errchain.Show(n)
		}
		noun := "are toolset names"
		if len(typos) == 1 {
			noun = "is a toolset name"
		}
		return nil, &ValueError{fmt.Sprintf("%s %s, not skill name(s). "+
			"Put toolsets in the assignee profile's `toolsets:` config "+
			"instead of per-task skills. Skills are named skill bundles "+
			"(e.g. `blogwatcher`, `github-code-review`); toolsets are runtime "+
			"capabilities (e.g. `web`, `browser`, `terminal`).", strings.Join(quoted, ", "), noun)}
	}
	return cleaned, nil
}

// Integer of a SQLite cell, a ValueError for text that is no integer literal, text beyond int64 saturated at its bounds
func cellIntOf(v any) (int64, error) {
	switch x := v.(type) {
	case nil:
		return 0, &RuntimeError{"expected a whole number, got nothing"}
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case []byte:
		return cellIntOf(string(x))
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, &ValueError{fmt.Sprintf("%s is not a whole number", x)}
		}
		return n, nil
	}
	return 0, &RuntimeError{fmt.Sprintf("expected a whole number, got %s", errchain.Kind(v))}
}
