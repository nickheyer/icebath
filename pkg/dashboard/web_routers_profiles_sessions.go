package dashboard

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Current time as float seconds, the time.time the session heuristics compare against
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Unified read only session list across every profile store, rows tagged with their owning profile
func getProfilesSessions(ctx context.Context, c *Call) (any, error) {
	limit, offset := int(c.Int("limit")), int(c.Int("offset"))
	archived, order, profile := c.Str("archived"), c.Str("order"), c.Str("profile")
	full := c.Bool("full")
	if archived != "exclude" && archived != "only" && archived != "include" {
		return nil, NewHTTPException(400, "archived must be one of: exclude, only, include")
	}
	if order != "created" && order != "recent" {
		return nil, NewHTTPException(400, "order must be one of: created, recent")
	}
	var targets []profiles.ServedProfile
	if profile != "" && profile != "all" {
		name, dir, err := cronProfileHome(ctx, profile)
		if err != nil {
			return nil, err
		}
		targets = []profiles.ServedProfile{{Name: name, Home: dir}}
	} else {
		var err error
		if targets, err = profileTargets(ctx, "GET /api/profiles/sessions"); err != nil {
			return nil, err
		}
	}
	source := c.Str("source")
	sources := splitCSV(c.Str("sources"))
	excludeSources := splitCSV(c.Str("exclude_sources"))
	minMessages := int(max(0, c.Int("min_messages")))
	perProfile := limit + offset
	merged := []*pyjson.Object{}
	totals := pyjson.NewObject()
	totalSum := 0
	errs := []*pyjson.Object{}
	now := nowSeconds()
	for _, t := range targets {
		name, profileHome := t.Name, t.Home
		readProfileDB(ctx, name, profileHome, &errs, func(db *state.DB) (struct{}, error) {
			includeSubagents, exclude := state.SubagentListingScope(ctx, profileHome, source, sources, excludeSources)
			rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Source: source, Sources: sources,
				ExcludeSources: exclude, MinMessageCount: minMessages, IncludeArchived: archived == "include",
				ArchivedOnly: archived == "only", IncludeSubagents: includeSubagents, Limit: &perProfile, Offset: 0,
				OrderByLastActive: order == "recent", CompactRows: !full, IncludePinned: true})
			if err != nil {
				return struct{}{}, err
			}
			count, err := db.SessionCount(ctx, state.SessionCountOptions{Source: source, Sources: sources,
				ExcludeSources: exclude, MinMessageCount: minMessages, IncludeArchived: archived == "include",
				ArchivedOnly: archived == "only", IncludeSubagents: includeSubagents, ExcludeChildren: true})
			if err != nil {
				return struct{}{}, err
			}
			if prev, ok := totals.Get(name); ok {
				totalSum -= prev.(int)
			}
			totals.Set(name, count)
			totalSum += count
			tagged, err := tagRows(rows, name, now)
			if err != nil {
				return struct{}{}, err
			}
			merged = append(merged, tagged...)
			return struct{}{}, nil
		})
	}
	sortKey := "started_at"
	if order == "recent" {
		sortKey = "last_active"
	}
	sortRowsDesc(merged, func(s *pyjson.Object) float64 {
		return counterFloat(firstTruthy(func(k string) any { return s.GetOr(k, nil) }, sortKey, "started_at"))
	})
	var window []*pyjson.Object
	if archived == "only" {
		var archivedRows []*pyjson.Object
		for _, s := range merged {
			if pyrepr.Truthy(s.GetOr("archived", nil)) {
				archivedRows = append(archivedRows, s)
			}
		}
		window = pinnedWindow(archivedRows, offset, limit)
		for _, s := range merged {
			if pyrepr.Truthy(s.GetOr("pinned", nil)) && !pyrepr.Truthy(s.GetOr("archived", nil)) {
				window = append(window, s)
			}
		}
	} else {
		window = pinnedWindow(merged, offset, limit)
	}
	if !full {
		stripSessionListRows(window)
	}
	return pyjson.ObjectOf("sessions", rowList(window), "total", totalSum, "profile_totals", totals, "limit", limit,
		"offset", offset, "errors", rowList(errs), "storage", corruptProfileStores(targets)), nil
}

// Comma list items that are not blank, kept unstripped
func nonBlankCSV(value string) []string {
	var out []string
	for _, s := range strings.Split(value, ",") {
		if pystr.Strip(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// Session rows of a JSON list value
func objectRows(v any) []*pyjson.Object {
	list, _ := v.([]any)
	out := make([]*pyjson.Object, 0, len(list))
	for _, item := range list {
		if o, ok := item.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Batched recents, cron and messaging sidebar slices with one store open per profile
func getProfilesSessionsSidebar(ctx context.Context, args ...any) (*pyjson.Object, error) {
	recentsProfile, recentsLimit, recentsExclude := args[0].(string), args[1].(int64), args[2].(string)
	cronLimit, messagingLimit, messagingExclude := args[3].(int64), args[4].(int64), args[5].(string)
	targets, err := profileTargets(ctx, "GET /api/profiles/sessions/sidebar")
	if err != nil {
		return nil, err
	}
	recentsScope := recentsProfile
	if recentsScope == "" {
		recentsScope = "all"
	}
	if recentsScope = pystr.Strip(recentsScope); recentsScope == "" {
		recentsScope = "all"
	}
	recentsExcludeList := nonBlankCSV(recentsExclude)
	messagingExcludeList := nonBlankCSV(messagingExclude)
	sliceKeys := []string{"recents", "cron", "messaging"}
	clampCap := func(n int64) int { return int(min(max(n, 1), 500)) }
	caps := map[string]int{"recents": clampCap(recentsLimit), "cron": clampCap(cronLimit), "messaging": clampCap(messagingLimit)}
	rows := map[string][]*pyjson.Object{"recents": {}, "cron": {}, "messaging": {}}
	recentsTruncated := pyjson.NewObject()
	profileTotals := pyjson.NewObject()
	errs := []*pyjson.Object{}
	now := nowSeconds()

	slice := func(db *state.DB, key string, includeSubagents bool, recentsExclude []string) ([]any, error) {
		source, exclude := "", recentsExclude
		switch key {
		case "cron":
			source, exclude, includeSubagents = "cron", nil, false
		case "messaging":
			exclude, includeSubagents = messagingExcludeList, false
		}
		limit := caps[key]
		got, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Source: source, ExcludeSources: exclude,
			Limit: &limit, Offset: 0, MinMessageCount: 1, IncludeArchived: false, ArchivedOnly: false,
			OrderByLastActive: true, CompactRows: true, IncludePinned: true, IncludeSubagents: includeSubagents})
		return rowList(got), err
	}

	var scanned []profiles.ServedProfile
	contributed := map[string]bool{}
	for _, t := range targets {
		name, profileHome := t.Name, t.Home
		if recentsScope != "all" && name != recentsScope {
			continue
		}
		scanned = append(scanned, t)
		dbPath := filepath.Join(profileHome, "state.db")
		if !pathExistsFollow(dbPath) {
			continue
		}
		var scopeExclude []string
		if len(recentsExcludeList) > 0 {
			scopeExclude = recentsExcludeList
		}
		includeSubagents, recentsExcl := state.SubagentListingScope(ctx, profileHome, "", nil, scopeExclude)
		cacheKey := sidebarProfileKey{dbPath: dbPath, fingerprint: sidebarDBFingerprint(dbPath),
			rest: freezeArgs([]any{caps["recents"], recentsExcludeList, caps["cron"], caps["messaging"], messagingExcludeList, includeSubagents})}
		slices := sidebarProfileCacheGet(cacheKey)
		if slices == nil {
			got, ok := readProfileDB(ctx, name, profileHome, &errs, func(db *state.DB) (*pyjson.Object, error) {
				recents, err := slice(db, "recents", includeSubagents, recentsExcl)
				if err != nil {
					return nil, err
				}
				tokens, cost, err := db.UsageTotals(ctx, 1, false)
				if err != nil {
					return nil, err
				}
				cron, err := slice(db, "cron", false, nil)
				if err != nil {
					return nil, err
				}
				messaging, err := slice(db, "messaging", false, nil)
				if err != nil {
					return nil, err
				}
				built := pyjson.ObjectOf("recents", recents, "usage", pyjson.ObjectOf("tokens", tokens, "cost_usd", cost),
					"cron", cron, "messaging", messaging)
				sidebarProfileCachePut(cacheKey, built)
				return built, nil
			})
			if !ok {
				continue
			}
			slices = got
		}
		if sliceHasRows(slices) {
			contributed[name] = true
		}
		recentsTruncated.Set(name, len(objectRows(slices.GetOr("recents", nil))) >= caps["recents"])
		profileTotals.Set(name, slices.GetOr("usage", nil))
		for _, key := range sliceKeys {
			tagged, err := tagRows(objectRows(slices.GetOr(key, nil)), name, now)
			if err != nil {
				return nil, err
			}
			rows[key] = append(rows[key], tagged...)
		}
	}

	window := func(key string) []any {
		sortRowsDesc(rows[key], recency)
		win := pinnedWindow(rows[key], 0, caps[key])
		stripSessionListRows(win)
		return rowList(win)
	}

	storage := corruptProfileStores(scanned)
	retryable := retryableProfileErrors(errs, scanned)
	failedNames := map[string]bool{}
	for _, e := range retryable {
		if p, _ := e.GetOr("profile", nil).(string); p != "" {
			failedNames[p] = true
		}
	}
	var live []string
	for _, t := range scanned {
		if _, bad := storage.Get(t.Name); !bad {
			live = append(live, t.Name)
		}
	}
	scopedFailed := recentsScope != "all" && failedNames[recentsScope] && !contributed[recentsScope]
	allFailed := recentsScope == "all" && len(live) > 0 && len(contributed) == 0
	if allFailed {
		for _, name := range live {
			if !failedNames[name] {
				allFailed = false
				break
			}
		}
	}
	if scopedFailed || allFailed {
		return pyjson.ObjectOf(
			"recents", failedLoadSlice(retryable, "profiles_truncated", pyjson.NewObject(), "profiles_usage", pyjson.NewObject()),
			"cron", failedLoadSlice(retryable),
			"messaging", failedLoadSlice(retryable, "total", 0),
			"errors", rowList(errs), "storage", storage), nil
	}
	body := pyjson.ObjectOf(
		"recents", pyjson.ObjectOf("sessions", window("recents"), "profiles_truncated", recentsTruncated,
			"profiles_usage", profileTotals),
		"cron", pyjson.ObjectOf("sessions", window("cron")),
		"messaging", pyjson.ObjectOf("sessions", window("messaging"), "total", len(rows["messaging"])),
		"errors", rowList(errs), "storage", storage)
	if failed := profilesFailed(retryable); failed.Len() > 0 {
		body.Set("profiles_failed", failed)
		for _, key := range sliceKeys {
			part := body.Vals[key].(*pyjson.Object)
			part.Set("profiles_failed", failed)
			part.Set("errors", errorRowList(retryable))
		}
	}
	return body, nil
}

// Sidebar scan behind its single flight response cache
var profilesSidebarScan = newSidebarSingleflight(getProfilesSessionsSidebar)

// Maps of a JSON list value
func mapRows(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Insertion ordered nodes keyed by id
type orderedNodes struct {
	keys []any
	vals map[any]map[string]any
}

// Ordered nodes of a list keyed by each id
func nodesByID(entries []map[string]any) *orderedNodes {
	o := &orderedNodes{vals: map[any]map[string]any{}}
	for _, e := range entries {
		o.put(e["id"], e)
	}
	return o
}

// Sets a node, a new id appended to the order
func (o *orderedNodes) put(id any, node map[string]any) {
	if _, ok := o.vals[id]; !ok {
		o.keys = append(o.keys, id)
	}
	o.vals[id] = node
}

// Nodes in insertion order as a JSON list
func (o *orderedNodes) list() []any {
	out := make([]any, len(o.keys))
	for i, k := range o.keys {
		out[i] = o.vals[k]
	}
	return out
}

// Folds entries into the nodes by id, counts adding up and the first writer keeping every other field
func mergeByID(into *orderedNodes, entries []map[string]any, childKey string) {
	for _, entry := range entries {
		existing, ok := into.vals[entry["id"]]
		if !ok {
			into.put(entry["id"], entry)
			continue
		}
		if childKey == "sessions" {
			current, _ := existing["sessions"].([]any)
			extra, _ := entry["sessions"].([]any)
			existing["sessions"] = append(current, extra...)
		} else {
			children := nodesByID(mapRows(existing[childKey]))
			mergeByID(children, mapRows(entry[childKey]), "sessions")
			existing[childKey] = children.list()
		}
		if _, has := existing["sessionCount"]; has {
			existing["sessionCount"] = addCounter(orZeroValue(existing["sessionCount"]), orZeroValue(entry["sessionCount"]))
		}
	}
}

// Value or int zero for a falsy one
func orZeroValue(v any) any {
	if !pyrepr.Truthy(v) {
		return int64(0)
	}
	return v
}

// Python slice end of a list of n items cut at limit
func sliceEnd(n, limit int) int {
	if limit < 0 {
		return max(0, n+limit)
	}
	return min(n, limit)
}

// Folds one profile's projects into the shared tree keyed by folder, sessions stamped with their profile
func mergeProfileTree(merged *orderedNodes, projects []map[string]any, profile string, previewLimit int) {
	for _, project := range projects {
		var sessions []map[string]any
		for _, r := range mapRows(project["repos"]) {
			for _, lane := range mapRows(r["groups"]) {
				sessions = append(sessions, mapRows(lane["sessions"])...)
			}
		}
		sessions = append(sessions, mapRows(project["previewSessions"])...)
		for _, s := range sessions {
			s["profile"] = profile
			s["is_default_profile"] = profile == "default"
		}
		keySource, _ := project["path"].(string)
		if !pyrepr.Truthy(project["path"]) {
			keySource, _ = project["id"].(string)
		}
		key := tuiserver.TreePathKey(keySource)
		project["mergeKey"] = key
		existing, ok := merged.vals[key]
		if !ok {
			merged.put(key, project)
			continue
		}
		if pyrepr.Truthy(existing["isAuto"]) && !pyrepr.Truthy(project["isAuto"]) {
			existing, project = project, existing
			merged.put(key, existing)
		}
		repos := nodesByID(mapRows(existing["repos"]))
		mergeByID(repos, mapRows(project["repos"]), "groups")
		existing["repos"] = repos.list()
		for _, totalKey := range []string{"sessionCount", "totalTokens", "totalCostUsd"} {
			existing[totalKey] = addCounter(orZeroValue(existing[totalKey]), orZeroValue(project[totalKey]))
		}
		if counterFloat(orZeroValue(project["lastActive"])) > counterFloat(orZeroValue(existing["lastActive"])) {
			existing["lastActive"] = orZeroValue(project["lastActive"])
		} else {
			existing["lastActive"] = orZeroValue(existing["lastActive"])
		}
		previews := append(mapRows(existing["previewSessions"]), mapRows(project["previewSessions"])...)
		sort.SliceStable(previews, func(i, j int) bool { return recencyMap(previews[i]) > recencyMap(previews[j]) })
		previews = previews[:sliceEnd(len(previews), previewLimit)]
		out := make([]any, len(previews))
		for i, p := range previews {
			out[i] = p
		}
		existing["previewSessions"] = out
	}
}

// Project tree of every profile at once, discovery off, merged by folder identity
func getProfilesProjectsTree(ctx context.Context, args ...any) (*pyjson.Object, error) {
	previewLimit, sessionLimit, profile := int(args[0].(int64)), int(args[1].(int64)), args[2].(string)
	merged := &orderedNodes{vals: map[any]map[string]any{}}
	scopedSessionIDs := []any{}
	errs := []*pyjson.Object{}
	targets, err := scopedProfileTargets(ctx, "GET /api/profiles/projects/tree", profile)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		name, profileHome := t.Name, t.Home
		readProfileDB(ctx, name, profileHome, &errs, func(db *state.DB) (struct{}, error) {
			hctx := home.WithOverride(ctx, profileHome)
			tree, _, err := tuiserver.BuildProjectTree(hctx, db, previewLimit, sessionLimit, false, false)
			if err != nil {
				return struct{}{}, err
			}
			mergeProfileTree(merged, mapRows(tree["projects"]), name, previewLimit)
			switch ids := tree["scoped_session_ids"].(type) {
			case []any:
				scopedSessionIDs = append(scopedSessionIDs, ids...)
			case []string:
				for _, id := range ids {
					scopedSessionIDs = append(scopedSessionIDs, id)
				}
			}
			return struct{}{}, nil
		})
	}
	projects := make([]map[string]any, 0, len(merged.keys))
	for _, k := range merged.keys {
		projects = append(projects, merged.vals[k])
	}
	sort.SliceStable(projects, func(i, j int) bool {
		return counterFloat(orZeroValue(projects[i]["lastActive"])) > counterFloat(orZeroValue(projects[j]["lastActive"]))
	})
	out := make([]any, len(projects))
	for i, p := range projects {
		out[i] = p
	}
	return pyjson.ObjectOf("projects", out, "active_id", nil, "scoped_session_ids", scopedSessionIDs, "errors", rowList(errs)), nil
}

// Projects tree scan behind its single flight response cache
var profilesProjectsTreeScan = newSidebarSingleflight(getProfilesProjectsTree)

// Whole tool output that is a GitHub pull request url
var prURLRE = regexp.MustCompile(`^https://github\.com/[\p{L}\p{N}_.-]+/[\p{L}\p{N}_.-]+/pull/(\p{Nd}+)/?$`)

// Pull request number and url a tool result announces, false for anything else
func prURLFromToolOutput(content any) (any, string, bool) {
	text, ok := content.(string)
	if !ok {
		return nil, "", false
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, "", false
	}
	if !pyrepr.Truthy(parsed) {
		return nil, "", false
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, "", false
	}
	output, ok := obj.GetOr("output", nil).(string)
	if !ok {
		return nil, "", false
	}
	m := prURLRE.FindStringSubmatch(pystr.Strip(output))
	if m == nil {
		return nil, "", false
	}
	n, err := pyint.Of(m[1])
	if err != nil {
		return nil, "", false
	}
	if n.IsInt64() {
		return n.Int64(), m[0], true
	}
	return n, m[0], true
}

// Pull request each requested session opened, recovered from its own transcript across every profile
func postProfilesSessionsPullRequests(ctx context.Context, c *Call) (any, error) {
	body := c.Object("body")
	seen := map[string]bool{}
	wanted := []string{}
	ids, _ := body.GetOr("ids", nil).([]any)
	for _, raw := range ids {
		s, _ := raw.(string)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		wanted = append(wanted, s)
	}
	if len(wanted) > 2000 {
		wanted = wanted[:2000]
	}
	if len(wanted) == 0 {
		return pyjson.ObjectOf("pull_requests", pyjson.NewObject(), "scanned", []any{}), nil
	}
	found := pyjson.NewObject()
	targets, err := scopedProfileTargets(ctx, "POST /api/profiles/sessions/pull-requests", c.Str("profile"))
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		readProfileDB(ctx, t.Name, t.Home, nil, func(db *state.DB) (struct{}, error) {
			prs, err := db.FindPRURLMessages(ctx, wanted)
			if err != nil {
				return struct{}{}, err
			}
			for _, pr := range prs {
				if number, url, ok := prURLFromToolOutput(pr["content"]); ok {
					sid, _ := pr["session_id"].(string)
					found.Set(sid, pyjson.ObjectOf("number", number, "url", url))
				}
			}
			return struct{}{}, nil
		})
	}
	scanned := make([]any, len(wanted))
	for i, s := range wanted {
		scanned[i] = s
	}
	return pyjson.ObjectOf("pull_requests", found, "scanned", scanned), nil
}

// Router of the cross profile session reads, registered before the templated profile routes
func profilesSessionsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/profiles/sessions", &Endpoint{
		Name: "get_profiles_sessions",
		Params: []Param{QueryParam("limit", IntRange(Bound(0), Bound(500)), int64(20)),
			QueryParam("offset", IntRange(Bound(0), nil), int64(0)), QueryParam("min_messages", Int, int64(0)),
			QueryParam("archived", Str, "exclude"), QueryParam("order", Str, "recent"), QueryParam("profile", Str, "all"),
			QueryParam("source", optStr, nil), QueryParam("sources", optStr, nil), QueryParam("exclude_sources", optStr, nil),
			QueryParam("full", Bool, false)},
		Handle: getProfilesSessions,
	})
	r.Get("/api/profiles/sessions/sidebar", &Endpoint{
		Name: "get_profiles_sessions_sidebar",
		Params: []Param{QueryParam("recents_profile", Str, "all"), QueryParam("recents_limit", Int, int64(20)),
			QueryParam("recents_exclude", optStr, nil), QueryParam("cron_limit", Int, int64(50)),
			QueryParam("messaging_limit", Int, int64(100)), QueryParam("messaging_exclude", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return profilesSidebarScan.call(ctx, c.Str("recents_profile"), c.Int("recents_limit"), c.Str("recents_exclude"),
				c.Int("cron_limit"), c.Int("messaging_limit"), c.Str("messaging_exclude"))
		},
	})
	r.Get("/api/profiles/projects/tree", &Endpoint{
		Name: "get_profiles_projects_tree",
		Params: []Param{QueryParam("preview_limit", Int, int64(3)), QueryParam("session_limit", Int, int64(2000)),
			QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return profilesProjectsTreeScan.call(ctx, c.Int("preview_limit"), c.Int("session_limit"), c.Str("profile"))
		},
	})
	r.Post("/api/profiles/sessions/pull-requests", &Endpoint{
		Name:   "post_profiles_sessions_pull_requests",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Body:   []BodyParam{Body("body", sessionPrScanBodyModel)},
		Handle: postProfilesSessionsPullRequests,
	})
	return r
}
