package connections

import (
	"math"
	"strings"
	"sync"
)

// GetJSONForDescriptor reads a path from a resolved backend descriptor
type GetJSONForDescriptor func(descriptor any, path string) (any, error)

type pagedSessionRows struct {
	rows  []any
	first any
}

// Whether a session query asks for a window past one remote page, read then in page sized steps
func needsSessionPaging(limit, offset float64) bool {
	return limit == math.Trunc(limit) && !math.IsInf(limit, 0) && limit > remoteSessionPageLimit &&
		offset == math.Trunc(offset) && !math.IsInf(offset, 0) && offset >= 0
}

// Reads a session listing in remote sized pages, nil when any page fails
func fetchSessionRowsInPages(basePath string, params *searchParams, getPage func(path string) (any, error)) *pagedSessionRows {
	requestedLimit := params.number("limit")
	requestedOffset := ecmaStringToNumber(orDefault(params.getString("offset"), "0"))
	if !needsSessionPaging(requestedLimit, requestedOffset) {
		page, err := getPage(basePath + "?" + params.encoded())
		if err != nil {
			return nil
		}
		return &pagedSessionRows{rows: rowsOf(page), first: page}
	}
	sessions := []any{}
	backfilled := []any{}
	seenIDs := map[string]bool{}
	backfilledIDs := map[string]bool{}
	var first any
	pageOffset := requestedOffset
	targetOffset := requestedOffset + requestedLimit
	for pageOffset < targetOffset {
		pageParams := params.copyParams()
		pageLimit := math.Min(remoteSessionPageLimit, targetOffset-pageOffset)
		pageParams.set("limit", jsNumber(pageLimit))
		pageParams.set("offset", jsNumber(pageOffset))
		page, err := getPage(basePath + "?" + pageParams.encoded())
		if err != nil {
			return nil
		}
		if first == nil {
			first = page
		}
		pageRows := rowsOf(page)
		total, hasTotal := nonNegativeNumber(jsObject(page)["total"])
		windowed := math.Min(pageLimit, float64(len(pageRows)))
		if hasTotal {
			windowed = math.Min(pageLimit, math.Max(0, total-pageOffset))
		}
		for _, row := range jsSlice(pageRows, 0, int(windowed)) {
			id, hasID := profileSessionID(row)
			if hasID && seenIDs[id] {
				continue
			}
			if hasID {
				seenIDs[id] = true
				delete(backfilledIDs, id)
			}
			sessions = append(sessions, row)
		}
		for _, row := range jsSlice(pageRows, int(windowed), len(pageRows)) {
			id, hasID := profileSessionID(row)
			if hasID && (seenIDs[id] || backfilledIDs[id]) {
				continue
			}
			if hasID {
				backfilledIDs[id] = true
			}
			backfilled = append(backfilled, row)
		}
		if hasTotal {
			targetOffset = math.Min(targetOffset, total)
		}
		pageOffset += pageLimit
	}
	for _, row := range backfilled {
		id, hasID := profileSessionID(row)
		if hasID && seenIDs[id] {
			continue
		}
		if hasID {
			seenIDs[id] = true
		}
		sessions = append(sessions, row)
	}
	return &pagedSessionRows{rows: sessions, first: first}
}

// Ordered string to number map, the Map of reported totals in insertion order
type orderedTotals struct {
	mu    sync.Mutex
	keys  []string
	value map[string]float64
}

func newOrderedTotals() *orderedTotals { return &orderedTotals{value: map[string]float64{}} }

func (t *orderedTotals) set(key string, v float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.value[key]; !ok {
		t.keys = append(t.keys, key)
	}
	t.value[key] = v
}

// FetchRegistrySessionRows reads the session rows of every registry source, tagged with connection and profile
func FetchRegistrySessionRows(sources []RegistrySessionSource, params *searchParams, getJSON GetJSONForDescriptor, reported *orderedTotals) []any {
	var mu sync.Mutex
	rows := []any{}
	recordTotals := func(page any, connectionID string, profileLabel *string) {
		data, ok := page.(map[string]any)
		if reported == nil || !ok {
			return
		}
		if profileLabel == nil {
			if totals, ok := data["profile_totals"].(map[string]any); ok {
				for _, profile := range sortedKeysJS(totals) {
					if count, ok := nonNegativeNumber(totals[profile]); ok {
						reported.set(connectionID+"\x00"+profile, count)
					}
				}
			}
			return
		}
		if count, ok := nonNegativeNumber(data["total"]); ok {
			reported.set(connectionID+"\x00"+orDefault(*profileLabel, "default"), count)
		}
	}
	tag := func(sourceRows []any, connectionID string, profileLabel *string) {
		mu.Lock()
		defer mu.Unlock()
		for _, row := range sourceRows {
			if !jsIsObject(row) {
				continue
			}
			if session, ok := row.(map[string]any); ok {
				if profileLabel != nil {
					session["profile"] = *profileLabel
				} else if p, isString := session["profile"].(string); !isString || p == "" {
					session["profile"] = "default"
				}
				session["is_default_profile"] = false
				session["connection_id"] = connectionID
			}
			rows = append(rows, row)
		}
	}
	readEachBackend := func(source RegistrySessionSource) {
		parallel(len(source.Backends), func(i int) {
			b := source.Backends[i]
			p := params.copyParams()
			p.deleteName("profile")
			read := fetchSessionRowsInPages("/api/sessions", p, func(path string) (any, error) { return getJSON(b.Descriptor, path) })
			if read != nil {
				label := "default"
				if b.ProfileLabel != nil && *b.ProfileLabel != "" {
					label = *b.ProfileLabel
				}
				recordTotals(read.first, source.ConnectionID, &label)
				tag(read.rows, source.ConnectionID, &label)
			}
		})
	}
	parallel(len(sources), func(i int) {
		source := sources[i]
		if source.Kind == "local" {
			readEachBackend(source)
			return
		}
		if len(source.Backends) == 0 {
			return
		}
		shared := source.Backends[0]
		getShared := func(path string) (any, error) { return getJSON(shared.Descriptor, path) }
		p := params.copyParams()
		p.set("profile", "all")
		if aggregate := fetchSessionRowsInPages("/api/profiles/sessions", p, getShared); aggregate != nil {
			recordTotals(aggregate.first, source.ConnectionID, nil)
			tag(aggregate.rows, source.ConnectionID, nil)
			return
		}
		if source.Kind == "ssh" {
			readEachBackend(source)
			return
		}
		flat := params.copyParams()
		flat.deleteName("profile")
		if read := fetchSessionRowsInPages("/api/sessions", flat, getShared); read != nil {
			label := "default"
			recordTotals(read.first, source.ConnectionID, &label)
			tag(read.rows, source.ConnectionID, nil)
		}
	})
	return rows
}

// Object.keys order of a decoded object, integer like keys ascending ahead of the rest sorted
func sortedKeysJS(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortJSKeys(keys)
	return keys
}

// Profile of a row, default when it names none
func rowProfile(row any) string {
	if p, ok := jsObject(row)["profile"].(string); ok && p != "" {
		return p
	}
	return "default"
}

// value || 0 then + n of a profile total, a string total concatenating as JavaScript does
func jsTotalPlus(v any, n float64) any {
	base := orZero(v)
	if s, ok := base.(string); ok {
		return s + jsNumber(n)
	}
	return ecmaToNumber(base) + n
}

// SpliceRegistrySessionRows appends the registry rows the merged list lacks and counts each into its profile total
func SpliceRegistrySessionRows(merged *[]any, registryRows []any, profileTotals map[string]any) int {
	seen := map[string]bool{}
	for _, row := range *merged {
		if id, ok := profileSessionID(row); ok {
			seen[id] = true
		}
	}
	added := 0
	for _, row := range registryRows {
		id, hasID := profileSessionID(row)
		if hasID && seen[id] {
			continue
		}
		if hasID {
			seen[id] = true
		}
		*merged = append(*merged, row)
		added++
		profile := rowProfile(row)
		profileTotals[profile] = jsTotalPlus(profileTotals[profile], 1)
	}
	return added
}

// RegistryBackendIdentity names the install ids that tell one backend reached twice from two
type RegistryBackendIdentity struct {
	PrimaryBackendID       string
	UnknownPrimaryProfiles []string
	BackendIDs             map[string]string
}

// Ordered tally of rows by key, objects only
type tally struct {
	keys  []string
	count map[string]float64
}

func countRows(rows []any, keyOf func(any) string) tally {
	t := tally{count: map[string]float64{}}
	for _, row := range rows {
		if !jsIsObject(row) {
			continue
		}
		key := keyOf(row)
		if _, ok := t.count[key]; !ok {
			t.keys = append(t.keys, key)
		}
		t.count[key]++
	}
	return t
}

// ApplyRegistrySessionTotals sets each profile total to the union of the backends that hold it, the added count returned
func ApplyRegistrySessionTotals(fetchedRows, addedRows []any, reported *orderedTotals, profileTotals map[string]any, primaryRows []any, identity RegistryBackendIdentity) float64 {
	connectionOf := func(row any) string {
		id, _ := jsObject(row)["connection_id"].(string)
		return id
	}
	scopeOf := func(row any) string { return connectionOf(row) + "\x00" + rowProfile(row) }
	fetched := countRows(fetchedRows, scopeOf)
	addedPerProfile := countRows(addedRows, rowProfile)
	primaryVisible := countRows(primaryRows, rowProfile)
	primaryTotals := map[string]any{}
	for k, v := range profileTotals {
		primaryTotals[k] = v
	}
	for _, profile := range addedPerProfile.keys {
		primaryTotals[profile] = ecmaToNumber(orZero(primaryTotals[profile])) - addedPerProfile.count[profile]
	}
	type scopeTotal struct {
		backend string
		total   float64
	}
	var profileOrder []string
	scopes := map[string][]scopeTotal{}
	seenKeys := map[string]bool{}
	var keys []string
	for _, k := range append(append([]string{}, fetched.keys...), reported.keys...) {
		if !seenKeys[k] {
			seenKeys[k] = true
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		connection, profile, _ := strings.Cut(key, "\x00")
		total, ok := reported.value[key]
		if !ok {
			total = fetched.count[key]
		}
		if _, ok := scopes[profile]; !ok {
			profileOrder = append(profileOrder, profile)
		}
		scopes[profile] = append(scopes[profile], scopeTotal{backend: identity.BackendIDs[connection], total: total})
	}
	unknownPrimary := map[string]bool{}
	for _, p := range identity.UnknownPrimaryProfiles {
		unknownPrimary[p] = true
	}
	contribution := 0.0
	for _, profile := range profileOrder {
		primaryCount := math.Max(0, ecmaToNumber(orZero(primaryTotals[profile])))
		var knownOrder []string
		known := map[string]float64{}
		unknownMax := 0.0
		add := func(backend string, total float64) {
			if backend != "" {
				if _, ok := known[backend]; !ok {
					knownOrder = append(knownOrder, backend)
				}
				known[backend] = math.Max(ecmaToNumber(orZero(known[backend])), total)
			} else {
				unknownMax = math.Max(unknownMax, total)
			}
		}
		if primaryCount > 0 {
			backend := identity.PrimaryBackendID
			if unknownPrimary[profile] {
				backend = ""
			}
			add(backend, primaryCount)
		}
		for _, s := range scopes[profile] {
			add(s.backend, s.total)
		}
		knownSum := 0.0
		for _, b := range knownOrder {
			knownSum += known[b]
		}
		visible := primaryVisible.count[profile] + addedPerProfile.count[profile]
		union := math.Max(math.Max(primaryCount, knownSum), math.Max(unknownMax, visible))
		profileTotals[profile] = union
		contribution += union - primaryCount
	}
	return contribution
}

// MergeRegistrySessionRows splices every registry source into the merged list and returns what it added to the total
func MergeRegistrySessionRows(sources []RegistrySessionSource, params *searchParams, getJSON GetJSONForDescriptor, merged *[]any, profileTotals map[string]any, identity RegistryBackendIdentity) float64 {
	reported := newOrderedTotals()
	registryRows := FetchRegistrySessionRows(sources, params, getJSON, reported)
	before := len(*merged)
	SpliceRegistrySessionRows(merged, registryRows, profileTotals)
	backendIDs := map[string]string{}
	for _, source := range sources {
		if id := jsTrim(source.BackendID); id != "" {
			backendIDs[source.ConnectionID] = id
		}
	}
	identity.BackendIDs = backendIDs
	return ApplyRegistrySessionTotals(registryRows, jsSlice(*merged, before, len(*merged)), reported, profileTotals, jsSlice(*merged, 0, before), identity)
}
