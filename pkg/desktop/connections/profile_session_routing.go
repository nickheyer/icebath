package connections

import (
	"errors"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

// Pages of a remote session listing are read at most this many rows at a time
const remoteSessionPageLimit = 100

// FetchJSONForProfile reads a path from a profile backend, an empty profile meaning the primary
type FetchJSONForProfile func(profile string, path string) (any, error)

// SessionScanError is one profile a session scan could not read
type SessionScanError struct {
	Profile string `json:"profile"`
	Error   string `json:"error"`
}

func (e SessionScanError) value() map[string]any {
	return map[string]any{"profile": e.Profile, "error": e.Error}
}

func scanErrorValues(errs []SessionScanError) []any {
	out := make([]any, len(errs))
	for i, e := range errs {
		out[i] = e.value()
	}
	return out
}

// HTTP status of a failed request, NaN when it carries none
func httpStatusOf(err error) float64 {
	if code, ok := readStatusCode(err); ok {
		return float64(code)
	}
	return math.NaN()
}

// IsRemoteProfileScopeError reports a remote rejecting a profile scoped session query with 400 or 404
func IsRemoteProfileScopeError(err error) bool {
	status := httpStatusOf(err)
	return status == 400 || status == 404
}

// Search params of a path resolved against base, false when the path does not parse
func parsePathParams(path, base string) (pathname string, params *searchParams, hash string, ok bool) {
	u, err := parseURLRef(path, base)
	if err != nil {
		return "", nil, "", false
	}
	return u.Pathname(), parseSearchParams(u.Query()), u.Hash(), true
}

// URLSearchParams built from ordered pairs
func newSearchParams(pairs ...[2]string) *searchParams {
	return &searchParams{pairs: append([][2]string{}, pairs...)}
}

func (s *searchParams) copyParams() *searchParams { return newSearchParams(s.pairs...) }

func (s *searchParams) deleteName(name string) {
	out := s.pairs[:0:0]
	for _, p := range s.pairs {
		if p[0] != name {
			out = append(out, p)
		}
	}
	s.pairs = out
}

func (s *searchParams) hasName(name string) bool {
	_, ok := s.get(name)
	return ok
}

// URLSearchParams toString, the query without its question mark
func (s *searchParams) encoded() string { return strings.TrimPrefix(s.search(), "?") }

// Value of a param, empty when absent, the way a get followed by an or default reads it
func (s *searchParams) getString(name string) string {
	v, _ := s.get(name)
	return v
}

// Number of URLSearchParams.get, zero for an absent param as Number(null)
func (s *searchParams) number(name string) float64 {
	v, ok := s.get(name)
	if !ok {
		return 0
	}
	return ecmaStringToNumber(v)
}

// IsAllProfilesSessionListRequest reports a GET of the unified session list or sidebar across every profile
func IsAllProfilesSessionListRequest(method, path string) bool {
	if strings.ToUpper(orGetMethod(method)) != "GET" || path == "" {
		return false
	}
	pathname, params, _, ok := parsePathParams(path, "http://desktop.local")
	if !ok {
		return false
	}
	if pathname == "/api/profiles/sessions" {
		return jsTrim(orDefault(params.getString("profile"), "all")) == "all"
	}
	return pathname == "/api/profiles/sessions/sidebar" && jsTrim(orDefault(params.getString("recents_profile"), "all")) == "all"
}

func orGetMethod(method string) string { return orDefault(method, "GET") }

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

var remoteSessionRequestPath = regexp.MustCompile(`^/api/sessions/[^/]+(?:/(?:messages|messages/around|mutation-snapshot|timeline))?$`)

// IsRemoteSessionRequestPath reports a single session read a remote owner must answer
func IsRemoteSessionRequestPath(pathname string) bool {
	return remoteSessionRequestPath.MatchString(pathname)
}

// Whether a decoded JSON value is an object to JavaScript, arrays included
func jsIsObject(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// Sessions array of a listing, empty when absent or not an array
func rowsOf(data any) []any {
	obj, ok := data.(map[string]any)
	if !ok {
		return []any{}
	}
	if rows, ok := obj["sessions"].([]any); ok {
		return rows
	}
	return []any{}
}

func tagRowsWithConnection(rows []any, connectionID string) {
	for _, row := range rows {
		if session, ok := row.(map[string]any); ok {
			session["connection_id"] = connectionID
		}
	}
}

// TagPrimarySessionRows labels untagged primary rows with their owner when a window pins another connection
func TagPrimarySessionRows(data any, primaryConnectionID, pinnedConnectionID string) any {
	pinned := jsTrim(pinnedConnectionID)
	owner := orDefault(jsTrim(primaryConnectionID), "local")
	if pinned == "" || pinned == owner {
		return data
	}
	for _, row := range rowsOf(data) {
		if session, ok := row.(map[string]any); ok {
			if id, isString := session["connection_id"].(string); !isString || jsTrim(id) == "" {
				session["connection_id"] = owner
			}
		}
	}
	return data
}

var singleSessionPath = regexp.MustCompile(`^/api/sessions/[^/]+$`)

// TagRegistrySessionResponse stamps the registry connection on every session row a registry backend returned
func TagRegistrySessionResponse(path string, data any, connectionID string) any {
	if !jsIsObject(data) {
		return data
	}
	pathname, _, _ := strings.Cut(path, "?")
	pathname = strings.TrimRight(pathname, "/")
	switch {
	case pathname == "/api/sessions" || pathname == "/api/profiles/sessions":
		tagRowsWithConnection(rowsOf(data), connectionID)
	case pathname == "/api/profiles/sessions/sidebar":
		if response, ok := data.(map[string]any); ok {
			for _, key := range []string{"recents", "cron", "messaging"} {
				tagRowsWithConnection(rowsOf(response[key]), connectionID)
			}
		}
	case singleSessionPath.MatchString(pathname):
		if session, ok := data.(map[string]any); ok {
			session["connection_id"] = connectionID
		}
	}
	return data
}

func sessionID(row any) (string, bool) {
	obj, ok := row.(map[string]any)
	if !ok {
		return "", false
	}
	id, ok := obj["id"].(string)
	return id, ok && id != ""
}

// Integer at or above zero, false otherwise
func nonNegativeNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

func isPinned(row any) bool {
	obj, ok := row.(map[string]any)
	return ok && jsTruthy(obj["pinned"])
}

func isArchived(row any) bool {
	obj, ok := row.(map[string]any)
	return ok && jsTruthy(obj["archived"])
}

func profileSessionID(row any) (string, bool) {
	id, ok := sessionID(row)
	if !ok {
		return "", false
	}
	profile, _ := row.(map[string]any)["profile"].(string)
	return profile + "\x00" + id, true
}

type sliceIdentity struct {
	ptr uintptr
	n   int
}

// Key under which a JavaScript Set would hold the value, objects by reference and primitives by value
func rowIdentity(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return reflect.ValueOf(t).Pointer()
	case []any:
		return sliceIdentity{reflect.ValueOf(t).Pointer(), len(t)}
	}
	return v
}

func jsSlice(rows []any, start, end int) []any {
	if start > len(rows) {
		start = len(rows)
	}
	if end > len(rows) {
		end = len(rows)
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return append([]any{}, rows[start:end]...)
}

// MergeProfileSessionWindow is the page of rows plus every pinned row past it, once each
func MergeProfileSessionWindow(rows []any, offset, limit int) []any {
	window := jsSlice(rows, offset, offset+limit)
	seenRows := map[any]bool{}
	seenIDs := map[string]bool{}
	for _, row := range window {
		seenRows[rowIdentity(row)] = true
		if id, ok := profileSessionID(row); ok {
			seenIDs[id] = true
		}
	}
	for _, row := range jsSlice(rows, offset+limit, len(rows)) {
		if !isPinned(row) {
			continue
		}
		id, hasID := profileSessionID(row)
		if (hasID && seenIDs[id]) || (!hasID && seenRows[rowIdentity(row)]) {
			continue
		}
		if hasID {
			seenIDs[id] = true
		} else {
			seenRows[rowIdentity(row)] = true
		}
		window = append(window, row)
	}
	return window
}

// SidebarSessionSliceParams are the recents, cron and messaging queries of a sidebar read
type SidebarSessionSliceParams struct {
	Cron      *searchParams
	Messaging *searchParams
	Recents   *searchParams
}

// BuildSidebarSessionSliceParams splits a sidebar query into its three session list queries
func BuildSidebarSessionSliceParams(params *searchParams) SidebarSessionSliceParams {
	profile := orDefault(jsTrim(orDefault(params.getString("recents_profile"), "all")), "all")
	slice := func(limitKey, defaultLimit string, extra ...[2]string) *searchParams {
		out := newSearchParams([2]string{"limit", orDefault(params.getString(limitKey), defaultLimit)}, [2]string{"offset", "0"},
			[2]string{"min_messages", "1"}, [2]string{"archived", "exclude"}, [2]string{"order", "recent"})
		for _, e := range extra {
			out.set(e[0], e[1])
		}
		return out
	}
	recents := slice("recents_limit", "20", [2]string{"profile", profile})
	if v := params.getString("recents_exclude"); v != "" {
		recents.set("exclude_sources", v)
	}
	messaging := slice("messaging_limit", "100", [2]string{"profile", profile})
	if v := params.getString("messaging_exclude"); v != "" {
		messaging.set("exclude_sources", v)
	}
	return SidebarSessionSliceParams{Cron: slice("cron_limit", "50", [2]string{"profile", profile}, [2]string{"source", "cron"}), Messaging: messaging, Recents: recents}
}

// Errors array of a response, nil when absent or empty
func errorsOf(data any) []any {
	obj, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	if errs, ok := obj["errors"].([]any); ok && len(errs) > 0 {
		return errs
	}
	return nil
}

// Message of a thrown value as error.message or String(error) reads it
func errorMessage(err error) string { return err.Error() }

// FetchPrimaryProfileSessions reads the primary unified session list, a failure becoming an empty list with one error
func FetchPrimaryProfileSessions(params *searchParams, fetch FetchJSONForProfile, prefixForMerge bool) any {
	read := func() (any, error) {
		if prefixForMerge {
			pathname, urlParams, _, _ := parsePathParams("/api/profiles/sessions?"+params.encoded(), "http://desktop.local")
			limit := math.Max(1, orNumber(params.number("limit"), 20))
			offset := math.Max(0, orNumber(params.number("offset"), 0))
			profile := orDefault(jsTrim(orDefault(params.getString("profile"), "all")), "all")
			return readProfileSessionRows(pathname, urlParams, profile, limit+offset, func(_ string, path string) (any, error) {
				return fetch("", path)
			})
		}
		return fetch("", "/api/profiles/sessions?"+params.encoded())
	}
	data, err := read()
	if err != nil {
		profile := orDefault(jsTrim(params.getString("profile")), "all")
		return map[string]any{"sessions": []any{}, "total": 0.0, "profile_totals": map[string]any{},
			"errors": []any{SessionScanError{Profile: profile, Error: errorMessage(err)}.value()}}
	}
	return data
}

// Number || fallback, NaN and zero taking the fallback
func orNumber(n, fallback float64) float64 {
	if math.IsNaN(n) || n == 0 {
		return fallback
	}
	return n
}

// Truthy value or zero, the JavaScript value || 0
func orZero(v any) any {
	if jsTruthy(v) {
		return v
	}
	return 0.0
}

// AssembleSidebarSessionSlices joins three session lists into the sidebar response
func AssembleSidebarSessionSlices(recents, cron, messaging any) map[string]any {
	slice := func(data any) map[string]any {
		out := map[string]any{"sessions": rowsOf(data)}
		if errs := errorsOf(data); errs != nil {
			out["errors"] = errs
		}
		return out
	}
	recentsObj := jsObject(recents)
	recentsSlice := slice(recents)
	recentsSlice["total"] = orNumber(ecmaToNumber(recentsObj["total"]), 0)
	if totals := recentsObj["profile_totals"]; jsTruthy(totals) {
		recentsSlice["profile_totals"] = totals
	} else {
		recentsSlice["profile_totals"] = map[string]any{}
	}
	messagingSlice := slice(messaging)
	messagingSlice["total"] = orNumber(ecmaToNumber(jsObject(messaging)["total"]), float64(len(rowsOf(messaging))))
	return map[string]any{"recents": recentsSlice, "cron": slice(cron), "messaging": messagingSlice, "errors": []any{}}
}

// RegistrySessionBackend is one backend descriptor of a registry session source and the profile it answers for
type RegistrySessionBackend struct {
	Descriptor   any
	ProfileLabel *string
}

// RegistrySessionSource is a registered connection whose live backends join the unified session list
type RegistrySessionSource struct {
	ConnectionID   string
	Kind           string
	Backends       []RegistrySessionBackend
	SharedProfiles []string
	BackendID      string
}

// HasPinnedRegistrySessionSource reports whether the pinned connection and profile are covered by the sources
func HasPinnedRegistrySessionSource(connectionID, profile string, sources []RegistrySessionSource, baseCoversLocal bool) bool {
	required := jsTrim(connectionID)
	selected := jsTrim(profile)
	if required == "" || (required == "local" && baseCoversLocal) {
		return true
	}
	var source *RegistrySessionSource
	for i := range sources {
		if sources[i].ConnectionID == required {
			source = &sources[i]
			break
		}
	}
	if source == nil {
		return false
	}
	if selected == "" || selected == "all" || source.Kind == "" || source.Backends == nil {
		return true
	}
	if source.Kind != "ssh" && source.Kind != "local" {
		return true
	}
	for _, b := range source.Backends {
		label := "default"
		if b.ProfileLabel != nil && *b.ProfileLabel != "" {
			label = *b.ProfileLabel
		}
		if label == selected {
			return true
		}
	}
	for _, p := range source.SharedProfiles {
		if p == selected {
			return true
		}
	}
	return false
}

// ShouldIncludeLocalRegistrySessionSource reports a pinned read or a base that leaves local out
func ShouldIncludeLocalRegistrySessionSource(connectionID string, baseCoversLocal bool) bool {
	return jsTrim(connectionID) != "" || !baseCoversLocal
}

// Runs fn for every index at once and waits, the Promise.all of a map
func parallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// Object.assign of one source value onto target, strings and arrays assigning their indices
func jsAssign(target map[string]any, source any) {
	switch t := source.(type) {
	case map[string]any:
		for k, v := range t {
			target[k] = v
		}
	case []any:
		for i, v := range t {
			target[strconv.Itoa(i)] = v
		}
	case string:
		for i, u := range utf16.Encode([]rune(t)) {
			target[strconv.Itoa(i)] = string(utf16.Decode([]uint16{u}))
		}
	}
}

// Object spread of a value into a fresh object
func jsSpreadOf(v any) map[string]any {
	out := map[string]any{}
	jsAssign(out, v)
	return out
}

// Error of a for of loop over a value JavaScript cannot iterate
func notIterable(name string) error { return errors.New(name + " is not iterable") }
