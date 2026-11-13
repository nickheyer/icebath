package connections

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Rows one per profile session page read may ask for
const profileSessionPageCap = 500

var sidebarSlices = []string{"recents", "cron", "messaging"}

// FetchProfilePage reads a path from the backend of one profile
type FetchProfilePage func(profile, path string) (any, error)

type profileRead struct {
	profile string
	data    any
	err     string
	failed  bool
}

func readEachProfile(profiles []string, read func(profile string) (any, error)) []profileRead {
	out := make([]profileRead, len(profiles))
	parallel(len(profiles), func(i int) {
		data, err := read(profiles[i])
		if err != nil {
			out[i] = profileRead{profile: profiles[i], err: errorMessage(err), failed: true}
			return
		}
		out[i] = profileRead{profile: profiles[i], data: data}
	})
	return out
}

func scanError(profile, message string) map[string]any {
	return map[string]any{"profile": profile, "error": message}
}

// Number of row.last_active or row.started_at, the recency sessions sort by
func recencyOf(row any) float64 {
	obj := jsObject(row)
	v := obj["last_active"]
	if !jsTruthy(v) {
		v = obj["started_at"]
	}
	return ecmaToNumber(orZero(v))
}

// Sort of a copy by descending key, stable like Array.prototype.sort
func sortedByDescending(rows []any, key func(any) float64) []any {
	out := append([]any{}, rows...)
	sort.SliceStable(out, func(i, j int) bool { return key(out[j])-key(out[i]) < 0 })
	return out
}

func sortedWindow(rows []any, key func(any) float64, offset, limit float64) []any {
	return MergeProfileSessionWindow(sortedByDescending(rows, key), int(offset), int(limit))
}

// JavaScript strict equality of two decoded JSON values, objects by reference
func jsStrictEqual(a, b any) bool { return rowIdentity(a) == rowIdentity(b) }

// Reads one profile session list in capped pages until the wanted rows or the archived rows are in
func readProfileSessionRows(pathname string, urlParams *searchParams, profile string, want float64, fetchPage FetchProfilePage) (any, error) {
	var first any
	rows := []any{}
	seen := map[string]bool{}
	errs := []any{}
	storage := map[string]any{}
	archivedOnly := urlParams.getString("archived") == "only" && urlParams.hasName("archived")
	archivedRows := 0.0
	for offset := 0.0; ; {
		params := urlParams.copyParams()
		remaining := want - offset
		if archivedOnly {
			remaining = want - archivedRows
		}
		limit := math.Min(profileSessionPageCap, math.Max(0, remaining))
		params.set("limit", jsNumber(limit))
		params.set("offset", jsNumber(offset))
		params.set("profile", profile)
		page, err := fetchPage(profile, pathname+"?"+params.encoded())
		if err != nil {
			return nil, err
		}
		pageRows := rowsOf(page)
		if first == nil {
			first = page
		}
		for _, e := range errorsOf(page) {
			dup := false
			for _, seenErr := range errs {
				if jsStrictEqual(jsObject(seenErr)["profile"], jsObject(e)["profile"]) && jsStrictEqual(jsObject(seenErr)["error"], jsObject(e)["error"]) {
					dup = true
					break
				}
			}
			if !dup {
				errs = append(errs, e)
			}
		}
		if s := jsObject(page)["storage"]; jsTruthy(s) && jsIsObject(s) {
			jsAssign(storage, s)
		}
		newRows := 0
		for _, row := range pageRows {
			id, hasID := profileSessionID(row)
			if hasID && seen[id] {
				continue
			}
			rows = append(rows, row)
			newRows++
			if isArchived(row) {
				archivedRows++
			}
			if hasID {
				seen[id] = true
			}
		}
		offset += limit
		total, hasTotal := nonNegativeNumber(jsObject(page)["total"])
		var done bool
		if archivedOnly {
			bound := want
			if hasTotal {
				bound = total
			}
			done = newRows == 0 || archivedRows >= math.Min(want, bound)
		} else {
			done = offset >= want || float64(len(pageRows)) < limit
		}
		if done {
			out := jsSpreadOf(first)
			out["sessions"] = rows
			out["errors"] = errs
			if jsTruthy(jsObject(first)["storage"]) || len(storage) > 0 {
				out["storage"] = storage
			}
			return out, nil
		}
	}
}

func allProfilesSessionList(pathname string, params *searchParams, profiles []string, fetchPage FetchProfilePage) map[string]any {
	limitParam := 20.0
	if _, ok := params.get("limit"); ok {
		limitParam = params.number("limit")
	}
	limit := math.Max(0, orNumber(limitParam, 0))
	offset := math.Max(0, orNumber(params.number("offset"), 0))
	created := params.getString("order") == "created" && params.hasName("order")
	reads := readEachProfile(profiles, func(profile string) (any, error) {
		return readProfileSessionRows(pathname, params, profile, limit+offset, fetchPage)
	})
	rows := []any{}
	totals := map[string]any{}
	errs := []any{}
	storage := map[string]any{}
	for _, read := range reads {
		if read.failed {
			errs = append(errs, scanError(read.profile, read.err))
			continue
		}
		rows = append(rows, rowsOf(read.data)...)
		jsAssign(totals, jsObject(read.data)["profile_totals"])
		jsAssign(storage, jsObject(read.data)["storage"])
		errs = append(errs, errorsOf(read.data)...)
	}
	key := recencyOf
	if created {
		key = func(row any) float64 { return ecmaToNumber(orZero(jsObject(row)["started_at"])) }
	}
	sum := 0.0
	for _, k := range sortedKeysJS(totals) {
		sum += orNumber(ecmaToNumber(totals[k]), 0)
	}
	return map[string]any{"sessions": sortedWindow(rows, key, offset, limit), "total": sum, "profile_totals": totals,
		"limit": limit, "offset": offset, "errors": errs, "storage": storage}
}

// Object.entries of a decoded JSON value
func jsEntriesOf(v any) [][2]any {
	switch t := v.(type) {
	case map[string]any:
		out := [][2]any{}
		for _, k := range sortedKeysJS(t) {
			out = append(out, [2]any{k, t[k]})
		}
		return out
	case []any:
		out := [][2]any{}
		for i, e := range t {
			out = append(out, [2]any{jsNumber(float64(i)), e})
		}
		return out
	case string:
		out := [][2]any{}
		for k, c := range jsSpreadOf(t) {
			out = append(out, [2]any{k, c})
		}
		sort.Slice(out, func(i, j int) bool {
			return ecmaStringToNumber(out[i][0].(string)) < ecmaStringToNumber(out[j][0].(string))
		})
		return out
	}
	return nil
}

func allProfilesSidebar(pathname string, params *searchParams, profiles []string, fetchPage FetchProfilePage) map[string]any {
	capOf := func(key string, fallback float64) float64 {
		return math.Min(math.Max(orNumber(params.number(key), fallback), 1), profileSessionPageCap)
	}
	caps := map[string]float64{"recents": capOf("recents_limit", 20), "cron": capOf("cron_limit", 50), "messaging": capOf("messaging_limit", 100)}
	reads := readEachProfile(profiles, func(profile string) (any, error) {
		p := params.copyParams()
		p.set("recents_profile", profile)
		if p.hasName("profile") {
			p.set("profile", profile)
		}
		return fetchPage(profile, pathname+"?"+p.encoded())
	})
	rows := map[string][]any{"recents": {}, "cron": {}, "messaging": {}}
	truncated := map[string]any{}
	usage := map[string]any{}
	errs := []any{}
	storage := map[string]any{}
	failed := []any{}
	messagingTotal := 0.0
	contributed := false
	for _, read := range reads {
		if read.failed {
			errs = append(errs, scanError(read.profile, read.err))
			failed = append(failed, scanError(read.profile, read.err))
			continue
		}
		data := jsObject(read.data)
		jsAssign(storage, data["storage"])
		errs = append(errs, errorsOf(read.data)...)
		recents := jsObject(data["recents"])
		if jsTruthy(recents["failed"]) {
			if e := errorsOf(data["recents"]); e != nil {
				failed = append(failed, e...)
			} else {
				failed = append(failed, scanError(read.profile, ""))
			}
			continue
		}
		for _, entry := range jsEntriesOf(data["profiles_failed"]) {
			message := ""
			if v := jsObject(entry[1])["error"]; v != nil {
				message = jsString(v)
			}
			failed = append(failed, scanError(entry[0].(string), message))
		}
		for _, key := range sidebarSlices {
			rows[key] = append(rows[key], rowsOf(data[key])...)
			if len(rowsOf(data[key])) > 0 {
				contributed = true
			}
		}
		jsAssign(truncated, recents["profiles_truncated"])
		jsAssign(usage, recents["profiles_usage"])
		messagingTotal += orNumber(ecmaToNumber(jsObject(data["messaging"])["total"]), float64(len(rowsOf(data["messaging"]))))
	}
	live := []string{}
	for _, profile := range profiles {
		if _, held := storage[profile]; !held {
			live = append(live, profile)
		}
	}
	failedFor := func(profile string) bool {
		for _, e := range failed {
			if jsObject(e)["profile"] == profile {
				return true
			}
		}
		return false
	}
	everyFailed := true
	for _, profile := range live {
		if !failedFor(profile) {
			everyFailed = false
			break
		}
	}
	copies := func() []any {
		out := make([]any, len(failed))
		for i, e := range failed {
			out[i] = jsSpreadOf(e)
		}
		return out
	}
	if len(live) > 0 && !contributed && everyFailed {
		failedSlice := func(extra map[string]any) map[string]any {
			out := map[string]any{"failed": true, "retry": true, "errors": copies()}
			for k, v := range extra {
				out[k] = v
			}
			return out
		}
		return map[string]any{
			"recents":   failedSlice(map[string]any{"profiles_truncated": map[string]any{}, "profiles_usage": map[string]any{}}),
			"cron":      failedSlice(nil),
			"messaging": failedSlice(map[string]any{"total": 0.0}),
			"errors":    errs,
			"storage":   storage,
		}
	}
	body := map[string]any{
		"recents": map[string]any{"sessions": sortedWindow(rows["recents"], recencyOf, 0, caps["recents"]),
			"profiles_truncated": truncated, "profiles_usage": usage},
		"cron":      map[string]any{"sessions": sortedWindow(rows["cron"], recencyOf, 0, caps["cron"])},
		"messaging": map[string]any{"sessions": sortedWindow(rows["messaging"], recencyOf, 0, caps["messaging"]), "total": messagingTotal},
		"errors":    errs,
		"storage":   storage,
	}
	profilesFailed := map[string]any{}
	for _, e := range failed {
		obj := jsObject(e)
		if !jsTruthy(obj["profile"]) {
			continue
		}
		message := obj["error"]
		if !jsTruthy(message) {
			message = ""
		}
		profilesFailed[jsString(obj["profile"])] = map[string]any{"failed": true, "retry": true, "error": message}
	}
	if len(profilesFailed) > 0 {
		body["profiles_failed"] = profilesFailed
		for _, key := range sidebarSlices {
			slice := body[key].(map[string]any)
			slice["profiles_failed"] = profilesFailed
			slice["errors"] = copies()
		}
	}
	return body
}

// FetchAllProfilesSessionsPerProfile assembles the unified session list or sidebar from one read per profile
func FetchAllProfilesSessionsPerProfile(path string, profiles []string, fetchPage FetchProfilePage) (any, error) {
	pathname, params, _, ok := parsePathParams(path, "http://desktop.local")
	if !ok {
		return nil, errors.New("Invalid URL")
	}
	if strings.TrimRight(pathname, "/") == "/api/profiles/sessions/sidebar" {
		return allProfilesSidebar(pathname, params, profiles, fetchPage), nil
	}
	return allProfilesSessionList(pathname, params, profiles, fetchPage), nil
}

// IsAllProfilesAggregateRequest reports the project tree, pull request scan and all profile cron reads a native grant splits
func IsAllProfilesAggregateRequest(method, path string) bool {
	if path == "" {
		return false
	}
	pathname, params, _, ok := parsePathParams(path, "http://desktop.local")
	if !ok {
		return false
	}
	pathname = strings.TrimRight(pathname, "/")
	verb := strings.ToUpper(orGetMethod(method))
	return (verb == "GET" && pathname == "/api/profiles/projects/tree") ||
		(verb == "POST" && pathname == "/api/profiles/sessions/pull-requests") ||
		(verb == "GET" && pathname == "/api/cron/jobs" && strings.ToLower(jsTrim(orDefault(params.getString("profile"), "all"))) == "all")
}

// Key a JavaScript Map holds a property under, an absent property distinct from null
type undefinedValue struct{}

func mapKeyOf(obj map[string]any, field string) any {
	v, ok := obj[field]
	if !ok {
		return undefinedValue{}
	}
	return rowIdentity(v)
}

// Insertion ordered Map of decoded JSON values
type orderedMap struct {
	keys []any
	vals map[any]any
}

func newOrderedMap() *orderedMap { return &orderedMap{vals: map[any]any{}} }

func (m *orderedMap) set(k, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *orderedMap) values() []any {
	out := make([]any, len(m.keys))
	for i, k := range m.keys {
		out[i] = m.vals[k]
	}
	return out
}

// Values a for of loop visits, strings by code point, an error for anything else JavaScript cannot iterate
func jsIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, notIterable(jsTypeName(v))
}

func jsTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case bool:
		return fmt.Sprint(t)
	}
	return jsString(v)
}

// value ?? [] spread into a fresh array
func spreadOrEmpty(v any) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	return jsIterate(v)
}

// JavaScript a + b of two decoded JSON values
func jsAdd(a, b any) any {
	_, as := a.(string)
	_, bs := b.(string)
	if as || bs || jsIsObject(a) || jsIsObject(b) {
		return jsPrimitiveString(a) + jsPrimitiveString(b)
	}
	return ecmaToNumber(a) + ecmaToNumber(b)
}

// ToString of ToPrimitive of a decoded JSON value
func jsPrimitiveString(v any) string {
	switch v.(type) {
	case []any:
		return ecmaJoinElement(v)
	case nil:
		return "null"
	}
	return jsString(v)
}

// Settable object of a merge target, the strict mode TypeError of a primitive
func jsTarget(v any, key string) (map[string]any, error) {
	if obj, ok := v.(map[string]any); ok {
		return obj, nil
	}
	if v == nil {
		return nil, fmt.Errorf("Cannot set properties of null (setting '%s')", key)
	}
	if _, ok := v.([]any); ok {
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("Cannot create property '%s' on %s '%s'", key, jsPrimitiveKind(v), jsString(v))
}

func jsPrimitiveKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	return "number"
}

func mergeTreeByID(into *orderedMap, entries any, childKey string) error {
	list, err := jsIterate(entries)
	if err != nil {
		return err
	}
	for _, entry := range list {
		if entry == nil {
			return fmt.Errorf("Cannot read properties of null (reading 'id')")
		}
		key := mapKeyOf(jsObject(entry), "id")
		existingValue, ok := into.vals[key]
		if !ok || !jsTruthy(existingValue) {
			into.set(key, entry)
			continue
		}
		existing, err := jsTarget(existingValue, childKey)
		if err != nil {
			return err
		}
		entryObj := jsObject(entry)
		if childKey == "sessions" {
			a, err := spreadOrEmpty(existing["sessions"])
			if err != nil {
				return err
			}
			b, err := spreadOrEmpty(entryObj["sessions"])
			if err != nil {
				return err
			}
			existing["sessions"] = append(append([]any{}, a...), b...)
		} else {
			children := newOrderedMap()
			current, err := spreadOrEmpty(existing[childKey])
			if err != nil {
				return err
			}
			for _, child := range current {
				if child == nil {
					return fmt.Errorf("Cannot read properties of null (reading 'id')")
				}
				children.set(mapKeyOf(jsObject(child), "id"), child)
			}
			incoming := entryObj[childKey]
			if incoming == nil {
				incoming = []any{}
			}
			if err := mergeTreeByID(children, incoming, "sessions"); err != nil {
				return err
			}
			existing[childKey] = children.values()
		}
		if _, has := existing["sessionCount"]; has {
			existing["sessionCount"] = jsAdd(orZero(existing["sessionCount"]), orZero(entryObj["sessionCount"]))
		}
	}
	return nil
}

func mergeProjectInto(existingValue, project any, previewLimit float64) error {
	existing, err := jsTarget(existingValue, "repos")
	if err != nil {
		return err
	}
	projectObj := jsObject(project)
	repos := newOrderedMap()
	current, err := spreadOrEmpty(existing["repos"])
	if err != nil {
		return err
	}
	for _, repo := range current {
		if repo == nil {
			return fmt.Errorf("Cannot read properties of null (reading 'id')")
		}
		repos.set(mapKeyOf(jsObject(repo), "id"), repo)
	}
	incoming := projectObj["repos"]
	if incoming == nil {
		incoming = []any{}
	}
	if err := mergeTreeByID(repos, incoming, "groups"); err != nil {
		return err
	}
	existing["repos"] = repos.values()
	for _, total := range []string{"sessionCount", "totalTokens", "totalCostUsd"} {
		existing[total] = jsAdd(orZero(existing[total]), orZero(projectObj[total]))
	}
	existing["lastActive"] = math.Max(ecmaToNumber(orZero(existing["lastActive"])), ecmaToNumber(orZero(projectObj["lastActive"])))
	a, err := spreadOrEmpty(existing["previewSessions"])
	if err != nil {
		return err
	}
	b, err := spreadOrEmpty(projectObj["previewSessions"])
	if err != nil {
		return err
	}
	preview := sortedByDescending(append(append([]any{}, a...), b...), recencyOf)
	existing["previewSessions"] = jsSlice(preview, 0, int(previewLimit))
	return nil
}

func mergeProfileProjectTrees(reads []profileRead, previewLimit float64) (map[string]any, error) {
	merged := newOrderedMap()
	scopedSessionIDs := []any{}
	errs := []any{}
	for _, read := range reads {
		if read.failed {
			errs = append(errs, scanError(read.profile, read.err))
			continue
		}
		data := jsObject(read.data)
		if ids, ok := data["scoped_session_ids"].([]any); ok {
			scopedSessionIDs = append(scopedSessionIDs, ids...)
		}
		errs = append(errs, errorsOf(read.data)...)
		projects, _ := data["projects"].([]any)
		for _, incoming := range projects {
			project := incoming
			if project == nil {
				return nil, errors.New("Cannot read properties of null (reading 'mergeKey')")
			}
			p := jsObject(project)
			keyValue := p["mergeKey"]
			if keyValue == nil {
				keyValue = p["path"]
			}
			if keyValue == nil {
				keyValue = p["id"]
			}
			key := "undefined"
			if keyValue != nil {
				key = jsPrimitiveString(keyValue)
			} else if _, present := p["id"]; present {
				key = "null"
			}
			existing, ok := merged.vals[key]
			if !ok || !jsTruthy(existing) {
				merged.set(key, project)
				continue
			}
			if jsTruthy(jsObject(existing)["isAuto"]) && !jsTruthy(p["isAuto"]) {
				existing, project = project, existing
				merged.set(key, existing)
			}
			if err := mergeProjectInto(existing, project, previewLimit); err != nil {
				return nil, err
			}
		}
	}
	projects := sortedByDescending(merged.values(), func(row any) float64 { return ecmaToNumber(orZero(jsObject(row)["lastActive"])) })
	return map[string]any{"projects": projects, "active_id": nil, "scoped_session_ids": scopedSessionIDs, "errors": errs}, nil
}

func mergeProfilePullRequests(reads []profileRead) map[string]any {
	pullRequests := map[string]any{}
	scanned := []any{}
	anyFailed := false
	for _, read := range reads {
		if read.failed {
			anyFailed = true
			continue
		}
		data := jsObject(read.data)
		jsAssign(pullRequests, data["pull_requests"])
		if s, ok := data["scanned"].([]any); ok {
			scanned = s
		}
	}
	if anyFailed {
		keys := []any{}
		for _, k := range sortedKeysJS(pullRequests) {
			keys = append(keys, k)
		}
		return map[string]any{"pull_requests": pullRequests, "scanned": keys}
	}
	return map[string]any{"pull_requests": pullRequests, "scanned": scanned}
}

func mergeProfileCronJobs(reads []profileRead) []any {
	byID := newOrderedMap()
	unkeyed := []any{}
	for _, read := range reads {
		jobs, ok := read.data.([]any)
		if read.failed || !ok {
			continue
		}
		for _, job := range jobs {
			obj, isObj := job.(map[string]any)
			if !jsIsObject(job) {
				continue
			}
			if !isObj {
				obj = map[string]any{}
			}
			id := obj["id"]
			if !jsTruthy(id) {
				id = obj["job_id"]
			}
			if !jsTruthy(id) || id == "unknown" {
				unkeyed = append(unkeyed, job)
				continue
			}
			key := rowIdentity(id)
			existing, has := byID.vals[key]
			if !has || (jsTruthy(obj["is_default_profile"]) && !jsTruthy(jsObject(existing)["is_default_profile"])) {
				byID.set(key, job)
			}
		}
	}
	return append(byID.values(), unkeyed...)
}

// FetchAllProfilesAggregatePerProfile assembles a project tree, pull request scan or cron list from one read per profile
func FetchAllProfilesAggregatePerProfile(path string, profiles []string, fetchProfile FetchProfilePage) (any, error) {
	pathname, params, _, ok := parsePathParams(path, "http://desktop.local")
	if !ok {
		return nil, errors.New("Invalid URL")
	}
	reads := readEachProfile(profiles, func(profile string) (any, error) {
		p := params.copyParams()
		p.set("profile", profile)
		return fetchProfile(profile, pathname+"?"+p.encoded())
	})
	anyOK := false
	parts := make([]string, len(reads))
	for i, read := range reads {
		if read.failed {
			parts[i] = read.profile + ": " + read.err
		} else {
			anyOK = true
		}
	}
	if !anyOK {
		return nil, errors.New(orDefault(strings.Join(parts, "; "), "No profiles to read"))
	}
	switch strings.TrimRight(pathname, "/") {
	case "/api/profiles/projects/tree":
		previewParam := 3.0
		if _, ok := params.get("preview_limit"); ok {
			previewParam = params.number("preview_limit")
		}
		return mergeProfileProjectTrees(reads, math.Max(0, orNumber(previewParam, 0)))
	case "/api/cron/jobs":
		return mergeProfileCronJobs(reads), nil
	}
	return mergeProfilePullRequests(reads), nil
}

// PrimaryAggregateConnectionID names the connection the primary descriptor belongs to, local for an untagged local one
func PrimaryAggregateConnectionID(resolve func() (any, error)) string {
	descriptor, err := resolve()
	if err != nil {
		return ""
	}
	obj := jsObject(descriptor)
	if id := jsTrim(jsOrString(obj["connectionId"])); id != "" {
		return id
	}
	if jsTruthy(descriptor) && orDefault(jsOrString(obj["mode"]), "local") == "local" {
		return "local"
	}
	return ""
}

// TagPrimaryAggregateRows labels primary aggregate rows with their owner for a window pinned elsewhere
func TagPrimaryAggregateRows(data any, resolve func() (any, error), pinnedConnectionID string) {
	if owner := PrimaryAggregateConnectionID(resolve); owner != "" {
		TagPrimarySessionRows(data, owner, pinnedConnectionID)
	}
}

var notFoundAPIError = regexp.MustCompile(`(?:^|[\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}])404\b`)

// IsNotFoundAPIError reports an api failure whose message carries a 404 status
func IsNotFoundAPIError(err error) bool { return notFoundAPIError.MatchString(err.Error()) }
