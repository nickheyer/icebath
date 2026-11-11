package tuiserver

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Repo and worktree roots of a cwd, nil when not in git or unprobeable
type projectResolve func(cwd string) map[string]any

var (
	kanbanDirRE      = regexp.MustCompile(`^(.*[/\\]\.worktrees)[/\\]t_[0-9a-f]+[/\\]?\n?$`)
	wtSuffixRE       = regexp.MustCompile(`^(.+)-wt-(.+)\n?$`)
	windowsPathRE    = regexp.MustCompile(`^[A-Za-z]:[/\\]`)
	lastSegmentRE    = regexp.MustCompile(`[^/\\]+$`)
	pathSeparatorsRE = regexp.MustCompile(`[/\\]`)
	trunkBranches    = map[string]bool{"main": true, "master": true, "trunk": true, "develop": true}
)

const (
	defaultBranchLabel = "main"
	noProjectID        = "__no_project__"
	noProjectLabel     = "Home"
	maxSiblingProbes   = 4
)

// Every list member that is a dict, in order
func dictList(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if dict, isDict := row.(map[string]any); isDict {
				out = append(out, dict)
			}
		}
		return out
	}
	return nil
}

// Python float of a row value, raising the way float() does on a bad value
func treeFloat(v any) float64 {
	f, err := pyFloat(v)
	if err != nil {
		panic(err)
	}
	return f
}

// Stamps every preview and lane session row with the request scope profile
func stampProfile(projects []map[string]any, profile string) {
	for _, project := range projects {
		var lanes []map[string]any
		for _, repo := range dictList(project["repos"]) {
			lanes = append(lanes, dictList(repo["groups"])...)
		}
		sessions := dictList(project["previewSessions"])
		for _, group := range lanes {
			sessions = append(sessions, dictList(group["sessions"])...)
		}
		for _, session := range sessions {
			session["profile"] = profile
		}
	}
}

// Main checkout lane id shared with the desktop
func branchLaneID(repoRoot, branch string) string {
	return repoRoot + "::branch::" + pystr.Strip(branch)
}

// Kanban aggregate lane id of a repo
func kanbanLaneID(repoRoot string) string { return repoRoot + "::kanban" }

// Non empty path segments split on either separator after trailing separators go
func pathSegments(path string) []string {
	var out []string
	for _, s := range pathSeparatorsRE.Split(pystr.RstripChars(path, `/\`), -1) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Whether a path is drive lettered, UNC or backslash rooted
func isWindowsTreePath(path string) bool {
	value := pystr.Strip(path)
	return windowsPathRE.MatchString(value) || strings.HasPrefix(value, `\`) || strings.HasPrefix(value, "//")
}

// NFC segments, casefolded for Windows paths so identity compares across spellings
func comparisonSegments(path string) []string {
	segs := pathSegments(path)
	windows := isWindowsTreePath(path)
	for i, segment := range segs {
		segs[i] = norm.NFC.String(segment)
		if windows {
			segs[i] = pystr.Casefold(segs[i])
		}
	}
	return segs
}

// Separator and trailing slash agnostic comparison key of a path
func treePathKey(path string) string { return strings.Join(comparisonSegments(path), "/") }

// Lane id with only its path portion canonicalized, branch labels byte preserved
func treeLaneKey(pathOrLane string) string {
	for _, marker := range []string{"::branch::", "::kanban"} {
		if root, suffix, found := strings.Cut(pathOrLane, marker); found {
			return treePathKey(root) + marker + suffix
		}
	}
	return treePathKey(pathOrLane)
}

// Last path segment, empty for a root or empty path
func treeBaseName(path string) string {
	segs := pathSegments(path)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// The repo worktrees dir of a kanban task worktree path, empty otherwise
func kanbanWorktreeDir(path string) string {
	if m := kanbanDirRE.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// Path with its last segment swapped for name, the parent dir when name is empty
func withBaseName(path, name string) string {
	swapped := lastSegmentRE.ReplaceAllLiteralString(pystr.RstripChars(path, `/\`), name)
	if name != "" {
		return swapped
	}
	return pystr.RstripChars(swapped, `/\`)
}

// Stripped string field of a row, empty for None
func treeField(row map[string]any, key string) string {
	value := row[key]
	if !truthy(value) {
		return ""
	}
	text, isText := value.(string)
	if !isText {
		panic(fmt.Errorf("'%T' object has no attribute 'strip'", value))
	}
	return pystr.Strip(text)
}

// Recency of a session, last active else started else zero
func sessionTime(session map[string]any) float64 {
	if v := session["last_active"]; truthy(v) {
		return treeFloat(v)
	}
	if v := session["started_at"]; truthy(v) {
		return treeFloat(v)
	}
	return 0
}

// Most recent session time of a list, zero when empty
func treeLastActive(sessions []map[string]any) float64 {
	best, seen := 0.0, false
	for _, s := range sessions {
		if t := sessionTime(s); !seen || t > best {
			best, seen = t, true
		}
	}
	return best
}

// Repo and lane placement of a session
func treePlacement(repoRoot, laneKey, laneLabel, lanePath string, isMain, isKanban, isGit bool) map[string]any {
	label := treeBaseName(repoRoot)
	if label == "" {
		label = repoRoot
	}
	return map[string]any{"repo_key": repoRoot, "repo_label": label, "lane_key": laneKey, "lane_label": laneLabel,
		"lane_path": lanePath, "is_main": isMain, "is_kanban": isKanban, "is_git": isGit}
}

// Trunk lane placement, an unrecorded branch folding into the default lane
func trunkPlacement(repoRoot, branch string) map[string]any {
	b := pystr.Strip(branch)
	if b == "" {
		b = defaultBranchLabel
	}
	return treePlacement(repoRoot, branchLaneID(repoRoot, b), b, repoRoot, true, false, true)
}

// Kanban aggregate lane placement
func kanbanPlacement(repoRoot, kanbanDir string) map[string]any {
	return treePlacement(repoRoot, kanbanLaneID(repoRoot), "kanban", kanbanDir, false, true, true)
}

// Parent repo root of a deleted dash suffixed worktree found by bounded sibling probes, else empty
func probeSiblingWorktree(cwd string, resolve projectResolve) string {
	probes := 0
	path := pystr.RstripChars(cwd, `/\`)
	for path != "" && probes < maxSiblingProbes {
		parts := strings.Split(treeBaseName(path), "-")
		for i := len(parts) - 1; i > 0; i-- {
			if probes >= maxSiblingProbes {
				break
			}
			probes++
			info := resolve(withBaseName(path, strings.Join(parts[:i], "-")))
			if info != nil && truthy(info["repo_root"]) {
				return pystr.Strip(strOr(info["repo_root"]))
			}
		}
		path = withBaseName(path, "")
	}
	return ""
}

// Path only placement when neither a git probe nor a persisted root is known
func placeByHeuristic(path string) map[string]any {
	base := treeBaseName(path)
	if base == "" {
		return nil
	}
	if kanbanDir := kanbanWorktreeDir(path); kanbanDir != "" {
		return kanbanPlacement(withBaseName(kanbanDir, ""), kanbanDir)
	}
	if m := wtSuffixRE.FindStringSubmatch(base); m != nil {
		return treePlacement(withBaseName(path, m[1]), path, m[2], path, false, false, true)
	}
	return treePlacement(path, branchLaneID(path, defaultBranchLabel), base, path, true, false, false)
}

// Placement of a cwd from the live probe, else the persisted root, else a sibling probe or the path
func placeCwd(cwd, branch string, resolve projectResolve, persistedRoot string) map[string]any {
	var info map[string]any
	if resolve != nil {
		info = resolve(cwd)
	}
	if info != nil && truthy(info["repo_root"]) && truthy(info["worktree_root"]) {
		repoRoot, worktreeRoot := strOr(info["repo_root"]), strOr(info["worktree_root"])
		if treePathKey(worktreeRoot) == treePathKey(repoRoot) || truthy(info["is_main"]) {
			return trunkPlacement(repoRoot, branch)
		}
		if kanbanDir := kanbanWorktreeDir(worktreeRoot); kanbanDir != "" {
			return kanbanPlacement(repoRoot, kanbanDir)
		}
		label := treeBaseName(worktreeRoot)
		if label == "" {
			label = worktreeRoot
		}
		return treePlacement(repoRoot, worktreeRoot, label, worktreeRoot, false, false, true)
	}
	if persistedRoot != "" {
		if kanbanDir := kanbanWorktreeDir(cwd); kanbanDir != "" {
			return kanbanPlacement(persistedRoot, kanbanDir)
		}
		return trunkPlacement(persistedRoot, branch)
	}
	siblingRoot := ""
	if resolve != nil {
		siblingRoot = probeSiblingWorktree(cwd, resolve)
	}
	if siblingRoot != "" {
		return trunkPlacement(siblingRoot, branch)
	}
	return placeByHeuristic(cwd)
}

// Placement of a session row anchored on its cwd else its persisted root, nil when it has neither
func placeSession(session map[string]any, resolve projectResolve) map[string]any {
	root := treeField(session, "git_repo_root")
	anchor := treeField(session, "cwd")
	if anchor == "" {
		anchor = root
	}
	if anchor == "" {
		return nil
	}
	return placeCwd(anchor, treeField(session, "git_branch"), resolve, root)
}

// Common repo root a session belongs to, folding linked worktrees
func sessionRepoRoot(session map[string]any, resolve projectResolve) string {
	if cwd := treeField(session, "cwd"); cwd != "" && resolve != nil {
		if info := resolve(cwd); info != nil && truthy(info["repo_root"]) {
			return strOr(info["repo_root"])
		}
	}
	return treeField(session, "git_repo_root")
}

// Sort position of a lane, trunk first, kanban last, then recency, then label
type laneOrder struct {
	trunk, kanban int
	recency       float64
	label         string
}

// Sort key of a lane read from its flags, sessions and label
func laneSortKey(group map[string]any) laneOrder {
	label := pystr.Lower(strOr(group["label"]))
	key := laneOrder{trunk: 1, recency: -treeLastActive(dictList(group["sessions"])), label: label}
	if truthy(group["isMain"]) && trunkBranches[label] {
		key.trunk = 0
	}
	if truthy(group["isKanban"]) {
		key.kanban = 1
	}
	return key
}

// Tuple comparison of two lane sort keys
func (a laneOrder) compare(b laneOrder) int {
	switch {
	case a.trunk != b.trunk:
		return a.trunk - b.trunk
	case a.kanban != b.kanban:
		return a.kanban - b.kanban
	case a.recency < b.recency:
		return -1
	case a.recency > b.recency:
		return 1
	}
	return strings.Compare(a.label, b.label)
}

// Grows colliding basename labels into path prefixed labels in place
func disambiguateLabels(items []map[string]any) {
	var order []string
	byLabel := map[string][]map[string]any{}
	for _, item := range items {
		label := strOr(item["label"])
		if _, seen := byLabel[label]; !seen {
			order = append(order, label)
		}
		byLabel[label] = append(byLabel[label], item)
	}
	for _, label := range order {
		var pathed []map[string]any
		for _, g := range byLabel[label] {
			if truthy(g["path"]) {
				pathed = append(pathed, g)
			}
		}
		if len(pathed) < 2 {
			continue
		}
		parents := make([][]string, len(pathed))
		deepest := 0
		for i, g := range pathed {
			segs := pathSegments(strOr(g["path"]))
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			parents[i] = segs
			deepest = max(deepest, len(segs))
		}
		for depth := 1; depth <= deepest; depth++ {
			distinct := map[string]bool{}
			for i, g := range pathed {
				prefix := strings.Join(parents[i][max(0, len(parents[i])-depth):], "/")
				base := treeBaseName(strOr(g["path"]))
				if base == "" {
					base = strOr(g["path"])
				}
				if prefix != "" {
					g["label"] = prefix + "/" + base
				} else {
					g["label"] = base
				}
				distinct[strOr(g["label"])] = true
			}
			if len(distinct) == len(pathed) {
				break
			}
		}
	}
}

// Repo node with no lanes yet
func repoNode(root, label string) map[string]any {
	return map[string]any{"id": root, "label": label, "path": root, "groups": []any{}, "sessionCount": 0}
}

// Most recent first, ties keeping their order
func sortByRecency(sessions []map[string]any) {
	slices.SortStableFunc(sessions, func(a, b map[string]any) int {
		ta, tb := sessionTime(a), sessionTime(b)
		switch {
		case ta > tb:
			return -1
		case ta < tb:
			return 1
		}
		return 0
	})
}

// List value of dict rows for a wire field
func anyList(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Repo to lane to sessions subtree of a set of sessions
func buildRepos(sessions []map[string]any, resolve projectResolve, hydrate bool) []map[string]any {
	type lane struct {
		group     map[string]any
		sessions  []map[string]any
		placement map[string]any
	}
	var laneOrder []string
	lanes := map[string]*lane{}
	for _, session := range sessions {
		placement := placeSession(session, resolve)
		if placement == nil {
			continue
		}
		identity := treeLaneKey(strOr(placement["lane_key"]))
		if _, seen := lanes[identity]; !seen {
			group := map[string]any{"id": placement["lane_key"], "label": placement["lane_label"], "path": placement["lane_path"],
				"isMain": placement["is_main"], "isKanban": placement["is_kanban"], "isGit": placement["is_git"]}
			lanes[identity] = &lane{group: group, placement: placement}
			laneOrder = append(laneOrder, identity)
		}
		lanes[identity].sessions = append(lanes[identity].sessions, session)
	}
	var repoOrder []string
	repos := map[string]map[string]any{}
	repoGroups := map[string][]map[string]any{}
	for _, identity := range laneOrder {
		l := lanes[identity]
		sortByRecency(l.sessions)
		l.group["sessions"] = anyList(l.sessions)
		repoKey := strOr(l.placement["repo_key"])
		key := treePathKey(repoKey)
		if _, seen := repos[key]; !seen {
			repos[key] = repoNode(repoKey, strOr(l.placement["repo_label"]))
			repoOrder = append(repoOrder, key)
		}
		repoGroups[key] = append(repoGroups[key], l.group)
		repos[key]["sessionCount"] = repos[key]["sessionCount"].(int) + len(l.sessions)
	}
	repoList := make([]map[string]any, 0, len(repoOrder))
	for _, key := range repoOrder {
		repo, groups := repos[key], repoGroups[key]
		slices.SortStableFunc(groups, func(a, b map[string]any) int { return laneSortKey(a).compare(laneSortKey(b)) })
		disambiguateLabels(groups)
		if !hydrate {
			for _, group := range groups {
				group["sessions"] = []any{}
			}
		}
		repo["groups"] = anyList(groups)
		repoList = append(repoList, repo)
	}
	disambiguateLabels(repoList)
	return repoList
}

// Repos with every declared project folder present even with no sessions
func seedFolderRepos(repos []map[string]any, folders []map[string]any, resolve projectResolve) []map[string]any {
	seen := map[string]bool{}
	for _, repo := range repos {
		for _, v := range []any{repo["id"], repo["path"]} {
			if truthy(v) {
				seen[treePathKey(strOr(v))] = true
			}
		}
	}
	seeded := slices.Clone(repos)
	for _, folder := range folders {
		raw := treeField(folder, "path")
		if raw == "" {
			continue
		}
		var info map[string]any
		if resolve != nil {
			info = resolve(raw)
		}
		root := strOr(info["repo_root"])
		if root == "" {
			root = pystr.RstripChars(raw, `/\`)
		}
		rootKey := treePathKey(root)
		if rootKey == "" || seen[rootKey] {
			continue
		}
		label := treeBaseName(root)
		if label == "" {
			label = root
		}
		seeded = append(seeded, repoNode(root, label))
		seen[rootKey] = true
	}
	if len(seeded) != len(repos) {
		disambiguateLabels(seeded)
	}
	return seeded
}

// Owning project of a folder path with the depth of the matched folder
type folderHit struct {
	project map[string]any
	depth   int
}

// Normalized folder path to its owning project, matched by walking cwd ancestors
type folderIndex map[string]folderHit

// Index of every active project folder, the deepest folder winning and ties keeping scan order
func newFolderIndex(projects []map[string]any) folderIndex {
	index := folderIndex{}
	for _, project := range projects {
		for _, folder := range dictList(project["folders"]) {
			segs := comparisonSegments(strOr(folder["path"]))
			key := strings.Join(segs, "/")
			depth := -1
			if hit, present := index[key]; present {
				depth = hit.depth
			}
			if len(segs) > 0 && len(segs) > depth {
				index[key] = folderHit{project: project, depth: len(segs)}
			}
		}
	}
	return index
}

// Owning project of a target by its longest ancestor folder, with that folder depth
func (index folderIndex) match(target string) folderHit {
	segs := comparisonSegments(target)
	for end := len(segs); end > 0; end-- {
		if hit, present := index[strings.Join(segs[:end], "/")]; present {
			return hit
		}
	}
	return folderHit{depth: -1}
}

// Explicit project owning a session by cwd or repo root, the longer folder match winning
func projectForSession(session map[string]any, index folderIndex, resolve projectResolve) map[string]any {
	cwd := treeField(session, "cwd")
	repoRoot := sessionRepoRoot(session, resolve)
	var candidates []string
	for _, t := range []string{cwd, repoRoot} {
		if t != "" && !slices.Contains(candidates, t) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	best := index.match(candidates[0])
	for _, t := range candidates[1:] {
		if hit := index.match(t); hit.depth > best.depth {
			best = hit
		}
	}
	return best.project
}

// Python sum of two numeric row values, ints staying ints
func treeAdd(a, b any) any {
	af, aFloat := a.(float64)
	bf, bFloat := b.(float64)
	if aFloat || bFloat {
		if !aFloat {
			af = treeFloat(a)
		}
		if !bFloat {
			bf = treeFloat(b)
		}
		return af + bf
	}
	return intOr(a) + intOr(b)
}

// Row field value, zero when falsy
func treeOrZero(v any) any {
	if truthy(v) {
		return v
	}
	return int64(0)
}

// Project node wire shape with its token and cost totals and claimed session ids
func projectNode(pid, label string, path any, repos []map[string]any, sessionCount int, lastActive float64,
	previews []map[string]any, rows []map[string]any, flags map[string]any) map[string]any {
	var totalTokens any = int64(0)
	var totalCost any = int64(0)
	ids := []any{}
	for _, s := range rows {
		totalTokens = treeAdd(totalTokens, treeAdd(treeOrZero(s["input_tokens"]), treeOrZero(s["output_tokens"])))
		cost := s["actual_cost_usd"]
		if !truthy(cost) {
			cost = s["estimated_cost_usd"]
		}
		totalCost = treeAdd(totalCost, treeFloat(treeOrZero(cost)))
		if truthy(s["id"]) {
			ids = append(ids, s["id"])
		}
	}
	node := map[string]any{"id": pid, "label": label, "path": path, "color": nil, "icon": nil, "isAuto": false,
		"isNoProject": false, "sessionCount": sessionCount, "lastActive": lastActive, "totalTokens": totalTokens,
		"totalCostUsd": totalCost, "repos": anyList(repos), "previewSessions": anyList(previews), "sessionIds": ids}
	for key, value := range flags {
		node[key] = value
	}
	return node
}

// Auto project root bucket of a leftover session
type autoBucket struct {
	root     string
	sessions []map[string]any
}

// Groups leftover sessions by auto project root, the rest going to the Home bucket
func autoBuckets(unowned []map[string]any, resolve projectResolve, junk, junkCwd, exists func(string) bool) ([]string, map[string]*autoBucket, []map[string]any) {
	var order []string
	byAutoRoot := map[string]*autoBucket{}
	var homeless []map[string]any
	for _, session := range unowned {
		root := sessionRepoRoot(session, resolve)
		if root != "" {
			if junk(root) || !exists(root) {
				root = ""
			}
		} else if cwd := treeField(session, "cwd"); cwd != "" && !junkCwd(cwd) {
			if placement := placeSession(session, resolve); placement != nil && exists(strOr(placement["repo_key"])) {
				root = strOr(placement["repo_key"])
			}
		}
		key := ""
		if root != "" {
			key = treePathKey(root)
		}
		if key == "" {
			homeless = append(homeless, session)
			continue
		}
		if _, seen := byAutoRoot[key]; !seen {
			byAutoRoot[key] = &autoBucket{root: root}
			order = append(order, key)
		}
		byAutoRoot[key].sessions = append(byAutoRoot[key].sessions, session)
	}
	return order, byAutoRoot, homeless
}

// Synthetic Home bucket whose single lane carries every unplaced row
func homeProject(homeless []map[string]any, hydrate bool, previews []map[string]any) map[string]any {
	laneSessions := []any{}
	if hydrate {
		laneSessions = anyList(homeless)
	}
	lane := map[string]any{"id": noProjectID, "label": noProjectLabel, "path": nil, "isMain": false, "isKanban": false,
		"sessions": laneSessions}
	homeRepo := map[string]any{"id": noProjectID, "label": noProjectLabel, "path": nil, "groups": []any{lane},
		"sessionCount": len(homeless)}
	return projectNode(noProjectID, noProjectLabel, nil, []map[string]any{homeRepo}, len(homeless), treeLastActive(homeless),
		previews, homeless, map[string]any{"isNoProject": true})
}

// Options of one project tree build
type treeOptions struct {
	previewLimit int
	hydrate      bool
	isJunkRoot   func(string) bool
	isJunkCwd    func(string) bool
	exists       func(string) bool
}

// Authoritative project tree of explicit, auto, discovered and Home projects with the scoped session ids
func buildTree(projects, sessions, discoveredRepos []map[string]any, resolve projectResolve, o treeOptions) map[string]any {
	var activeProjects []map[string]any
	for _, p := range projects {
		if !truthy(p["archived"]) {
			activeProjects = append(activeProjects, p)
		}
	}
	junk, junkCwd, exists := o.isJunkRoot, o.isJunkCwd, o.exists
	if junk == nil {
		junk = func(string) bool { return false }
	}
	if junkCwd == nil {
		junkCwd = func(string) bool { return false }
	}
	if exists == nil {
		exists = func(string) bool { return true }
	}
	index := newFolderIndex(activeProjects)
	byProject := map[string][]map[string]any{}
	var unowned []map[string]any
	for _, session := range sessions {
		if owner := projectForSession(session, index, resolve); owner != nil {
			id := strOr(owner["id"])
			byProject[id] = append(byProject[id], session)
		} else {
			unowned = append(unowned, session)
		}
	}
	scopedIDs := []any{}
	var result []map[string]any
	previews := func(projectSessions []map[string]any) []map[string]any {
		if o.previewLimit <= 0 {
			return nil
		}
		sorted := slices.Clone(projectSessions)
		sortByRecency(sorted)
		return sorted[:min(o.previewLimit, len(sorted))]
	}
	scope := func(projectSessions []map[string]any) {
		for _, s := range projectSessions {
			if truthy(s["id"]) {
				scopedIDs = append(scopedIDs, s["id"])
			}
		}
	}
	for _, project := range activeProjects {
		id := strOr(project["id"])
		psessions := byProject[id]
		scope(psessions)
		repos := seedFolderRepos(buildRepos(psessions, resolve, o.hydrate), dictList(project["folders"]), resolve)
		label := strOr(project["name"])
		if label == "" {
			label = id
		}
		result = append(result, projectNode(id, label, project["primary_path"], repos, len(psessions), treeLastActive(psessions),
			previews(psessions), psessions, map[string]any{"color": project["color"], "icon": project["icon"]}))
	}
	order, byAutoRoot, homeless := autoBuckets(unowned, resolve, junk, junkCwd, exists)
	seen := map[string]bool{}
	for _, key := range order {
		bucket := byAutoRoot[key]
		autoKey := treePathKey(bucket.root)
		repos := buildRepos(bucket.sessions, resolve, o.hydrate)
		var node map[string]any
		for _, r := range repos {
			idOrPath := r["id"]
			if !truthy(idOrPath) {
				idOrPath = r["path"]
			}
			if treePathKey(strOr(idOrPath)) == autoKey {
				node = r
				break
			}
		}
		if node == nil {
			homeless = append(homeless, bucket.sessions...)
			continue
		}
		seen[autoKey] = true
		scope(bucket.sessions)
		label := treeBaseName(bucket.root)
		if label == "" {
			label = bucket.root
		}
		result = append(result, projectNode(bucket.root, label, bucket.root, repos, node["sessionCount"].(int),
			treeLastActive(bucket.sessions), previews(bucket.sessions), bucket.sessions, map[string]any{"isAuto": true}))
	}
	for _, repo := range discoveredRepos {
		rawRoot := treeField(repo, "root")
		if rawRoot == "" {
			continue
		}
		var info map[string]any
		if resolve != nil {
			info = resolve(rawRoot)
		}
		root := strOr(info["repo_root"])
		if root == "" {
			root = rawRoot
		}
		rootKey := treePathKey(root)
		if seen[rootKey] || junk(root) || index.match(root).project != nil {
			continue
		}
		seen[rootKey] = true
		label := strOr(repo["label"])
		if label == "" {
			label = treeBaseName(root)
		}
		if label == "" {
			label = root
		}
		lastActive := 0.0
		if truthy(repo["last_active"]) {
			lastActive = treeFloat(repo["last_active"])
		}
		result = append(result, projectNode(root, label, root, []map[string]any{repoNode(root, label)}, int(intOr(repo["sessions"])),
			lastActive, nil, nil, map[string]any{"isAuto": true}))
	}
	var autos []map[string]any
	for _, p := range result {
		if truthy(p["isAuto"]) {
			autos = append(autos, p)
		}
	}
	disambiguateLabels(autos)
	if len(homeless) > 0 {
		sortByRecency(homeless)
		scope(homeless)
		result = append([]map[string]any{homeProject(homeless, o.hydrate, previews(homeless))}, result...)
	}
	return map[string]any{"projects": anyList(result), "scoped_session_ids": scopedIDs}
}
