package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// JSON RPC error codes of a generic failure, an id resolving to nothing and an invalid argument
const (
	projectsErr    = 5061
	noProjectErr   = 5062
	projectArgErr  = 5063
	repoScanCap    = 500
	gitWarmWorkers = 8
)

// Raised inside a projects handler when the id param resolves to no project
var errNoProject = errors.New("no such project")

// Sources whose sessions are not user conversations in the project tree
var projectTreeExcludedSources = []string{"cron", "kanban", "oneshot"}

var projectsRegistry = &handlerRegistry{}

func init() {
	projectMutator("update", false, func(pdb *tools.ProjectsDB, id, _ string, params map[string]any) error {
		fields, err := projectUpdateFields(params)
		if err != nil {
			return err
		}
		_, err = pdb.UpdateProject(id, fields)
		return err
	})
	projectMutator("add_folder", true, func(pdb *tools.ProjectsDB, id, path string, params map[string]any) error {
		var label *string
		if raw, present := params["label"]; present && raw != nil {
			text := pyStrOf(raw)
			label = &text
		}
		_, err := pdb.AddFolder(id, path, label, truthy(params["is_primary"]))
		return err
	})
	projectMutator("remove_folder", true, func(pdb *tools.ProjectsDB, id, path string, _ map[string]any) error {
		_, err := pdb.RemoveFolder(id, path)
		return err
	})
	projectMutator("set_primary", true, func(pdb *tools.ProjectsDB, id, path string, _ map[string]any) error {
		_, err := pdb.SetPrimary(id, path)
		return err
	})
	projectsMethod("projects.list", projectsList)
	projectsMethod("projects.get", projectsGet)
	projectsMethod("projects.create", projectsCreate)
	projectsMethod("projects.archive", projectsArchive)
	projectsMethod("projects.delete", projectsDelete)
	projectsMethod("projects.set_active", projectsSetActive)
	projectsMethod("projects.for_cwd", projectsForCwd)
	projectsRegistry.install()
}

// Opens the projects database of the active profile home
func openProjectsDB(ctx context.Context) (*tools.ProjectsDB, error) {
	return tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
}

// Error response of a projects handler failure mapped to its code
func projectsFailure(rid any, err error) map[string]any {
	var valueErr *tools.ProjectsValueError
	switch {
	case errors.Is(err, errNoProject):
		return rpcErr(rid, noProjectErr, "no such project", nil)
	case errors.As(err, &valueErr):
		return rpcErr(rid, projectArgErr, valueErr.Msg, nil)
	}
	return rpcErr(rid, projectsErr, err.Error(), nil)
}

// Registers a profile scoped projects RPC with the database open and the shared error mapping
func projectsMethod(name string, fn func(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error)) {
	projectsRegistry.scopedMethod(name, func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				resp = rpcErr(rid, projectsErr, panicMessage(r), nil)
			}
		}()
		pdb, err := openProjectsDB(ctx)
		if err != nil {
			return projectsFailure(rid, err)
		}
		defer pdb.Close()
		out, err := fn(ctx, rid, params, pdb)
		if err != nil {
			return projectsFailure(rid, err)
		}
		return out
	})
}

// Every project with archived ones and the active project id
func projectsPayload(pdb *tools.ProjectsDB) (map[string]any, error) {
	projects, err := pdb.ListProjects(true)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, p.AsMap())
	}
	active, err := activeProjectID(pdb)
	if err != nil {
		return nil, err
	}
	return map[string]any{"projects": rows, "active_id": active}, nil
}

// Active project id, None when unset
func activeProjectID(pdb *tools.ProjectsDB) (any, error) {
	id, err := pdb.GetActiveID()
	if err != nil || id == "" {
		return nil, err
	}
	return id, nil
}

// The project the id param names, errNoProject when none
func requireProject(pdb *tools.ProjectsDB, params map[string]any) (*tools.Project, error) {
	proj, err := pdb.GetProject(strOr(params["id"]))
	if err != nil {
		return nil, err
	}
	if proj == nil {
		return nil, errNoProject
	}
	return proj, nil
}

// str of a param value
func pyStrOf(v any) string {
	if text, isText := v.(string); isText {
		return text
	}
	return pyrepr.Str(v)
}

// Optional string field of a param, nil when missing or None
func optionalParam(params map[string]any, key string) *string {
	raw, present := params[key]
	if !present || raw == nil {
		return nil
	}
	text := pyStrOf(raw)
	return &text
}

// Patchable fields of projects.update, a board slug that is not text failing like its strip call
func projectUpdateFields(params map[string]any) (tools.UpdateProjectFields, error) {
	fields := tools.UpdateProjectFields{Name: optionalParam(params, "name"), Description: optionalParam(params, "description"),
		Icon: optionalParam(params, "icon"), Color: optionalParam(params, "color")}
	if raw, present := params["board_slug"]; present && raw != nil {
		text, isText := raw.(string)
		if !isText {
			return fields, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
		}
		fields.BoardSlug = &text
	}
	return fields, nil
}

// Registers projects.<suffix> resolving the id param, applying the mutation and answering with the refreshed project
func projectMutator(suffix string, takesPath bool, apply func(pdb *tools.ProjectsDB, id, path string, params map[string]any) error) {
	projectsMethod("projects."+suffix, func(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
		proj, err := requireProject(pdb, params)
		if err != nil {
			return nil, err
		}
		path := ""
		if takesPath {
			path = strOr(params["path"])
		}
		if err := apply(pdb, proj.ID, path, params); err != nil {
			return nil, err
		}
		refreshed, err := pdb.GetProject(proj.ID)
		if err != nil {
			return nil, err
		}
		if refreshed == nil {
			panic(fmt.Errorf("'NoneType' object has no attribute 'to_dict'"))
		}
		return ok(rid, map[string]any{"project": refreshed.AsMap()}), nil
	})
}

// Every project with the active id
func projectsList(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	payload, err := projectsPayload(pdb)
	if err != nil {
		return nil, err
	}
	return ok(rid, payload), nil
}

// One project by id or slug
func projectsGet(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"project": proj.AsMap()}), nil
}

// Creates a project, optionally making it active, and records the feature use
func projectsCreate(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	var folders []string
	if raw := params["folders"]; truthy(raw) {
		list, isList := raw.([]any)
		if !isList {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
		}
		for _, f := range list {
			text, isText := f.(string)
			if !isText {
				return nil, fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyrepr.TypeName(f))
			}
			folders = append(folders, text)
		}
	}
	pid, err := pdb.CreateProject(strOr(params["name"]), tools.CreateProjectOptions{Slug: strOr(params["slug"]), Folders: folders,
		PrimaryPath: strOr(params["primary_path"]), Description: optionalParam(params, "description"),
		Icon: optionalParam(params, "icon"), Color: optionalParam(params, "color"), BoardSlug: strOr(params["board_slug"])})
	if err != nil {
		return nil, err
	}
	if truthy(params["use"]) {
		if err := pdb.SetActive(&pid); err != nil {
			return nil, err
		}
	}
	observability.RecordFeatureUsed(ctx, "projects", "")
	proj, err := pdb.GetProject(pid)
	if err != nil {
		return nil, err
	}
	var row any
	if proj != nil {
		row = proj.AsMap()
	}
	return ok(rid, map[string]any{"project": row}), nil
}

// Archives a project, or restores it when restore is set
func projectsArchive(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	if truthy(params["restore"]) {
		_, err = pdb.RestoreProject(proj.ID)
	} else {
		_, err = pdb.ArchiveProject(proj.ID)
	}
	if err != nil {
		return nil, err
	}
	return projectsList(ctx, rid, params, pdb)
}

// Hard deletes a project
func projectsDelete(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	if _, err := pdb.DeleteProject(proj.ID); err != nil {
		return nil, err
	}
	return projectsList(ctx, rid, params, pdb)
}

// Sets the active project, clearing it when no id is given
func projectsSetActive(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	var target *string
	if truthy(params["id"]) {
		proj, err := requireProject(pdb, params)
		if err != nil {
			return nil, err
		}
		target = &proj.ID
	}
	if err := pdb.SetActive(target); err != nil {
		return nil, err
	}
	active, err := activeProjectID(pdb)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"active_id": active}), nil
}

// Project owning the resolved cwd with that cwd and its git branch
func projectsForCwd(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	request := map[string]any{}
	if truthy(params["cwd"]) {
		request["cwd"] = pystr.Strip(strOr(params["cwd"]))
	}
	cwd, err := completionCwd(ctx, request)
	if err != nil {
		return nil, err
	}
	proj, err := pdb.ProjectForPath(cwd, false)
	if err != nil {
		return nil, err
	}
	var row any
	if proj != nil {
		row = proj.AsMap()
	}
	return ok(rid, map[string]any{"project": row, "cwd": cwd, "branch": tools.GitProbeBranch(cwd)}), nil
}

// Directories never treated as a workspace, root, the user home, its parent and both POSIX home roots
func nonWorkspaceDirs() map[string]bool {
	userHome := realpathLax(pypath.Expanduser("~"))
	out := map[string]bool{}
	for _, path := range []string{string(os.PathSeparator), userHome, pypath.Dirname(userHome), "/home", "/Users"} {
		if path != "" {
			out[pypath.Normcase(realpathLax(path))] = true
		}
	}
	return out
}

// os.path.realpath in its lax mode
func realpathLax(p string) string {
	resolved, err := pypath.Realpath(p, pypath.Lax)
	if err != nil {
		panic(err)
	}
	return resolved
}

// Whether a git root is never auto surfaced, a non workspace dir or anything under the icebath home
func isRepoJunk(ctx context.Context, root string) bool {
	if root == "" {
		return true
	}
	real := realpathLax(root)
	icebathHome := realpathLax(home.Dir(ctx))
	return nonWorkspaceDirs()[pypath.Normcase(real)] || real == icebathHome || strings.HasPrefix(real, icebathHome+string(os.PathSeparator))
}

// Whether a non git cwd stays in flat Recents, a non workspace dir or the icebath home itself
func isSessionCwdJunk(ctx context.Context, cwd string) bool {
	if cwd == "" {
		return true
	}
	real := pypath.Normcase(realpathLax(cwd))
	icebathHome := pypath.Normcase(realpathLax(home.Dir(ctx)))
	return nonWorkspaceDirs()[real] || real == icebathHome
}

// Effective profile local desktop repository scan policy, from raw when it is a dict else the config
func repoDiscoveryPolicy(ctx context.Context, raw any) map[string]any {
	defaults, _ := config.Defaults()["desktop"].(map[string]any)
	source, isDict := raw.(map[string]any)
	if !isDict {
		source, _ = loadCfg(ctx)["desktop"].(map[string]any)
		if source == nil {
			source = map[string]any{}
		}
	}
	get := func(short, long string) any {
		if v, present := source[short]; present {
			return v
		}
		if v, present := source[long]; present {
			return v
		}
		return defaults[long]
	}
	paths := func(short, long string) []any {
		values, isList := get(short, long).([]any)
		if !isList {
			fallback, _ := defaults[long].([]any)
			return slices.Clone(fallback)
		}
		out := []any{}
		for _, v := range values {
			if text, isText := v.(string); isText && pystr.Strip(text) != "" {
				out = append(out, pystr.Strip(text))
			}
		}
		return out
	}
	enabled := get("enabled", "repo_scan_enabled")
	if _, isBool := enabled.(bool); !isBool {
		enabled = defaults["repo_scan_enabled"]
	}
	return map[string]any{"enabled": enabled, "roots": paths("roots", "repo_scan_roots"),
		"exclude_paths": paths("exclude_paths", "repo_scan_exclude_paths")}
}

// Canonical JSON key of a discovery policy with its paths expanded, absolute and sorted
func repoDiscoveryPolicyKey(policy map[string]any) string {
	canonicalPaths := func(values any) []any {
		userHome := pypath.Expanduser("~")
		set := map[string]bool{}
		list, _ := values.([]any)
		for _, v := range list {
			abs, err := pypath.Abspath(pypath.Join(userHome, pypath.Expanduser(pyStrOf(v))))
			if err != nil {
				panic(err)
			}
			set[pypath.Normcase(abs)] = true
		}
		keys := make([]string, 0, len(set))
		for key := range set {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		out := make([]any, len(keys))
		for i, key := range keys {
			out[i] = key
		}
		return out
	}
	canonical := map[string]any{"enabled": truthy(policy["enabled"]), "roots": canonicalPaths(policy["roots"]),
		"exclude_paths": canonicalPaths(policy["exclude_paths"])}
	encoded, err := pyjson.Dumps(canonical, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	return encoded
}

// Whether a policy is the shipped default policy
func repoDiscoveryPolicyIsDefault(ctx context.Context, policy map[string]any) bool {
	defaults, _ := config.Defaults()["desktop"].(map[string]any)
	return repoDiscoveryPolicyKey(policy) == repoDiscoveryPolicyKey(repoDiscoveryPolicy(ctx, defaults))
}

// Directory entries in the order the filesystem lists them, split into directories and the rest
func scanDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		isDirEntry := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			st, statErr := os.Stat(filepath.Join(dir, entry.Name()))
			isDirEntry = statErr == nil && st.IsDir()
		}
		if isDirEntry {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs, nil
}

// Top down os.walk that skips unreadable directories and never follows directory symlinks
func walkTopDown(top string, visit func(dirpath string, dirnames []string) ([]string, bool)) bool {
	dirnames, err := scanDirNames(top)
	if err != nil {
		return true
	}
	keep, more := visit(top, dirnames)
	if !more {
		return false
	}
	for _, name := range keep {
		next := pypath.Join(top, name)
		if st, err := os.Lstat(next); err == nil && st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !walkTopDown(next, visit) {
			return false
		}
	}
	return true
}

// Scans the policy roots on this host into the discovery cache, true when every root was fully walked
func scanDiscoveredReposRemote(ctx context.Context, pdb *tools.ProjectsDB, policy map[string]any) bool {
	roots, _ := policy["roots"].([]any)
	excludes, _ := policy["exclude_paths"].([]any)
	var pairs []tools.DiscoveredRepo
	seen := map[string]bool{}
	authoritative := true
	isExcluded := func(path string) bool {
		for _, raw := range excludes {
			ex := pyStrOf(raw)
			if ex != "" && (path == ex || strings.HasPrefix(path, pystr.RstripChars(ex, `/\`)+string(os.PathSeparator))) {
				return true
			}
		}
		return false
	}
	for _, rawRoot := range roots {
		root := pyStrOf(rawRoot)
		if !isDir(root) {
			authoritative = false
			logger().Debug(fmt.Sprintf("discover_repos scan root missing, skipping: %s", root))
			continue
		}
		walkTopDown(root, func(dirpath string, dirnames []string) ([]string, bool) {
			var keep []string
			switch {
			case isExcluded(dirpath):
			case slices.Contains(dirnames, ".git"):
				if !seen[dirpath] {
					seen[dirpath] = true
					pairs = append(pairs, tools.DiscoveredRepo{Root: dirpath, Label: pypath.Basename(dirpath)})
				}
			default:
				for _, d := range dirnames {
					if !strings.HasPrefix(d, ".") && d != "node_modules" {
						keep = append(keep, d)
					}
				}
			}
			return keep, len(pairs) < repoScanCap
		})
		if len(pairs) >= repoScanCap {
			authoritative = false
			break
		}
	}
	if len(pairs) > 0 {
		key := repoDiscoveryPolicyKey(policy)
		if _, err := pdb.RecordDiscoveredRepos(pairs, authoritative, &key); err != nil {
			logger().Debug("discover_repos cache write failed", "error", err)
			authoritative = false
		}
	}
	return authoritative
}

// Discovered repos merged from the scan cache and session cwds, junk filtered, with session totals, most recent first
func discoverReposPayload(ctx context.Context, db *state.SessionDB, pdb *tools.ProjectsDB, backfill, includeCached bool) ([]map[string]any, error) {
	var order []string
	repos := map[string]map[string]any{}
	agg := func(root string) map[string]any {
		if _, present := repos[root]; !present {
			repos[root] = map[string]any{"root": root, "label": "", "sessions": int64(0), "last_active": 0.0}
			order = append(order, root)
		}
		return repos[root]
	}
	var cwdRows []*pyjson.Object
	if db != nil {
		rows, err := db.DistinctSessionCwds(ctx, false)
		if err != nil {
			return nil, err
		}
		cwdRows = rows
	}
	cwds := make([]string, 0, len(cwdRows))
	for _, row := range cwdRows {
		cwds = append(cwds, strOr(row.GetOr("cwd", nil)))
	}
	if err := tools.GitProbeWarmRoots(cwds, gitWarmWorkers); err != nil {
		return nil, err
	}
	cwdToRoot := map[string]string{}
	for i, row := range cwdRows {
		root := tools.GitProbeCommonRepoRoot(cwds[i])
		if root == "" {
			continue
		}
		cwdToRoot[cwds[i]] = root
		if isRepoJunk(ctx, root) {
			continue
		}
		entry := agg(root)
		entry["sessions"] = entry["sessions"].(int64) + intOr(row.GetOr("sessions", nil))
		lastActive := 0.0
		if raw := row.GetOr("last_active", nil); truthy(raw) {
			lastActive = treeFloat(raw)
		}
		entry["last_active"] = max(entry["last_active"].(float64), lastActive)
	}
	if backfill && db != nil && !db.IsReadOnly() {
		if err := db.BackfillRepoRoots(ctx, cwdToRoot); err != nil {
			logger().Debug("failed to backfill repo roots", "error", err)
		}
	}
	if includeCached {
		readCache := func(c *tools.ProjectsDB) {
			entries, err := c.ListDiscoveredRepos()
			if err != nil {
				logger().Debug("failed to read discovered repo cache", "error", err)
				return
			}
			for _, entry := range entries {
				if entry.Root != "" && !isRepoJunk(ctx, entry.Root) {
					row := agg(entry.Root)
					if entry.Label != "" {
						row["label"] = entry.Label
					}
				}
			}
		}
		if pdb != nil {
			readCache(pdb)
		} else if opened, err := openProjectsDB(ctx); err != nil {
			logger().Debug("failed to read discovered repo cache", "error", err)
		} else {
			readCache(opened)
			opened.Close()
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, root := range order {
		out = append(out, repos[root])
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		la, lb := a["last_active"].(float64), b["last_active"].(float64)
		switch {
		case la > lb:
			return -1
		case la < lb:
			return 1
		}
		return 0
	})
	for _, r := range out {
		if r["label"] == "" {
			root := r["root"].(string)
			label := pypath.Basename(pystr.RstripChars(root, `/\`))
			if label == "" {
				label = root
			}
			r["label"] = label
		}
	}
	return out, nil
}

// Session db row projected to the minimal shape the sidebar renders
func projectTreeRow(r *pyjson.Object) map[string]any {
	get := func(key string) any { return r.GetOr(key, nil) }
	row := map[string]any{}
	for _, key := range []string{"id", "_lineage_root_id", "_lineage_ids", "parent_session_id", "_reset_from", "_branched_from", "title", "preview"} {
		row[key] = get(key)
	}
	lastActive := get("last_active")
	if !truthy(lastActive) {
		lastActive = get("started_at")
	}
	row["started_at"] = treeOrZero(get("started_at"))
	row["ended_at"] = get("ended_at")
	row["last_active"] = treeOrZero(lastActive)
	row["source"] = get("source")
	row["archived"] = truthy(get("archived"))
	for _, key := range []string{"message_count", "tool_call_count", "input_tokens", "output_tokens"} {
		row[key] = treeOrZero(get(key))
	}
	for _, key := range []string{"actual_cost_usd", "estimated_cost_usd", "model"} {
		row[key] = get(key)
	}
	row["is_active"] = false
	for _, key := range []string{"cwd", "git_branch", "git_repo_root"} {
		row[key] = get(key)
	}
	return row
}

// Sessions, projects, discovered repos and the active id that a tree build reads
func projectTreeInputs(ctx context.Context, db *state.SessionDB, sessionLimit int, includeDiscovered bool) ([]map[string]any, []map[string]any, []map[string]any, any, error) {
	limit := sessionLimit
	rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, OrderByLastActive: true, MinMessageCount: 1,
		ExcludeSources: projectTreeExcludedSources, CompactRows: true})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sessions := make([]map[string]any, 0, len(rows))
	var cwds []string
	for _, r := range rows {
		row := projectTreeRow(r)
		sessions = append(sessions, row)
		if truthy(row["cwd"]) {
			cwds = append(cwds, pyStrOf(row["cwd"]))
		}
	}
	if err := tools.GitProbeWarmRoots(cwds, gitWarmWorkers); err != nil {
		return nil, nil, nil, nil, err
	}
	policy := repoDiscoveryPolicy(ctx, nil)
	policyKey := repoDiscoveryPolicyKey(policy)
	pdb, err := openProjectsDB(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer pdb.Close()
	if includeDiscovered {
		if _, err := pdb.ReconcileDiscoveredReposPolicy(policyKey, repoDiscoveryPolicyIsDefault(ctx, policy)); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	listed, err := pdb.ListProjects(false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	projects := make([]map[string]any, 0, len(listed))
	for _, p := range listed {
		projects = append(projects, p.AsMap())
	}
	active, err := activeProjectID(pdb)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var discovered []map[string]any
	if includeDiscovered {
		discovered, err = discoverReposPayload(ctx, db, pdb, false, truthy(policy["enabled"]))
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return sessions, projects, discovered, active, nil
}

// Options of one project tree RPC build
type projectTreeBuild struct {
	previewLimit      int
	hydrate           bool
	sessionLimit      int
	includeDiscovered bool
}

// Project tree of the session store with the active project id, directory existence memoized per build
func buildProjectTree(ctx context.Context, db *state.SessionDB, o projectTreeBuild) (map[string]any, any, error) {
	dirExists := map[string]bool{}
	sessions, projects, discovered, active, err := projectTreeInputs(ctx, db, o.sessionLimit, o.includeDiscovered)
	if err != nil {
		return nil, nil, err
	}
	var warm []string
	for _, p := range projects {
		for _, f := range dictList(p["folders"]) {
			warm = append(warm, strOr(f["path"]))
		}
	}
	for _, r := range discovered {
		warm = append(warm, strOr(r["root"]))
	}
	if err := tools.GitProbeWarmRoots(warm, gitWarmWorkers); err != nil {
		return nil, nil, err
	}
	tree := buildTree(projects, sessions, discovered, tools.GitProbeResolve, treeOptions{previewLimit: o.previewLimit, hydrate: o.hydrate,
		isJunkRoot: func(root string) bool { return isRepoJunk(ctx, root) },
		isJunkCwd:  func(cwd string) bool { return isSessionCwdJunk(ctx, cwd) },
		exists: func(path string) bool {
			hit, present := dirExists[path]
			if !present {
				hit = isDir(path)
				dirExists[path] = hit
			}
			return hit
		}})
	return tree, active, nil
}
