package connections

import (
	"errors"
	"math"
	"time"
)

// Milliseconds one remote profile session read may take before it counts as unavailable
const RemoteProfileSessionBudgetMs = 10000

// RemoteProfileQueryScope is the remote profile a desktop profile reads sessions as, a non default alias winning
func RemoteProfileQueryScope(profile, remoteProfileAlias string) string {
	if configured := jsTrim(remoteProfileAlias); configured != "" && configured != "default" {
		return configured
	}
	return jsTrim(profile)
}

// TagRemoteSessionRows claims unowned rows for the scope and marks the scope rows as not default
func TagRemoteSessionRows(rows []any, scope string) []any {
	for _, row := range rows {
		session, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if p, isString := session["profile"].(string); !isString || jsTrim(p) == "" {
			session["profile"] = scope
		}
		if session["profile"] == scope {
			session["is_default_profile"] = false
		}
	}
	return rows
}

// PathWithRemoteOwnerScope sets the profile query of a path to the owner scope
func PathWithRemoteOwnerScope(path, scope string) (string, error) {
	scoped := jsTrim(scope)
	if scoped == "" {
		return path, nil
	}
	pathname, params, hash, ok := parsePathParams(path, "http://hermes.local")
	if !ok {
		return "", errors.New("Invalid URL")
	}
	params.set("profile", scoped)
	return pathname + params.search() + hash, nil
}

// RemoteOwnerRoutePath rebuilds a session path with its query scoped to the owner profile
func RemoteOwnerRoutePath(pathname string, params *searchParams, ownerScope string) (string, error) {
	p := params.copyParams()
	p.deleteName("profile")
	if query := p.encoded(); query != "" {
		return PathWithRemoteOwnerScope(pathname+"?"+query, ownerScope)
	}
	return PathWithRemoteOwnerScope(pathname, ownerScope)
}

// FetchRemoteProfileSessions reads a remote profile session list in pages, retrying unscoped when the scope is refused
func FetchRemoteProfileSessions(profile string, params *searchParams, fetch FetchJSONForProfile, remoteProfileAlias string) (any, error) {
	base := params.copyParams()
	scope := RemoteProfileQueryScope(profile, remoteProfileAlias)
	fetchPage := func(pageParams *searchParams) (any, error) {
		pageParams.deleteName("profile")
		if scope != "" {
			pageParams.set("profile", scope)
			page, err := fetch(profile, "/api/sessions?"+pageParams.encoded())
			if err == nil {
				return page, nil
			}
			if !IsRemoteProfileScopeError(err) {
				return nil, err
			}
			pageParams.deleteName("profile")
		}
		return fetch(profile, "/api/sessions?"+pageParams.encoded())
	}
	requestedLimit := base.number("limit")
	requestedOffset := ecmaStringToNumber(orDefault(base.getString("offset"), "0"))
	if !needsSessionPaging(requestedLimit, requestedOffset) {
		return fetchPage(base)
	}
	sessions := []any{}
	backfilled := []any{}
	seenIDs := map[string]bool{}
	backfilledIDs := map[string]bool{}
	var firstPage any
	pageOffset := requestedOffset
	targetOffset := requestedOffset + requestedLimit
	for pageOffset < targetOffset {
		pageParams := base.copyParams()
		pageLimit := math.Min(remoteSessionPageLimit, targetOffset-pageOffset)
		pageParams.set("limit", jsNumber(pageLimit))
		pageParams.set("offset", jsNumber(pageOffset))
		page, err := fetchPage(pageParams)
		if err != nil {
			return nil, err
		}
		if firstPage == nil {
			firstPage = page
		}
		if page == nil {
			return nil, errors.New("Cannot read properties of null (reading 'total')")
		}
		total, hasTotal := nonNegativeNumber(jsObject(page)["total"])
		pageRows := rowsOf(page)
		windowed := math.Min(pageLimit, float64(len(pageRows)))
		if hasTotal {
			windowed = math.Min(pageLimit, math.Max(0, total-pageOffset))
		}
		for _, row := range jsSlice(pageRows, 0, int(windowed)) {
			id, hasID := sessionID(row)
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
			id, hasID := sessionID(row)
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
		if id, hasID := sessionID(row); !hasID || backfilledIDs[id] {
			sessions = append(sessions, row)
		}
	}
	out := jsSpreadOf(firstPage)
	out["sessions"] = sessions
	if total, ok := nonNegativeNumber(jsObject(firstPage)["total"]); ok {
		out["total"] = total
	} else {
		out["total"] = float64(len(sessions))
	}
	out["limit"] = requestedLimit
	out["offset"] = requestedOffset
	return out, nil
}

// RemoteProfileSessionsOutcome is the list or error one remote profile answered within its budget
type RemoteProfileSessionsOutcome struct {
	Profile string
	List    any
	Error   string
	Failed  bool
}

// SettleRemoteProfileSessions reads every remote profile at once, each bounded by the budget
func SettleRemoteProfileSessions(remoteProfiles []string, fetchRemote func(profile string) (any, error), budgetMs float64) []RemoteProfileSessionsOutcome {
	out := make([]RemoteProfileSessionsOutcome, len(remoteProfiles))
	parallel(len(remoteProfiles), func(i int) {
		profile := remoteProfiles[i]
		type result struct {
			list any
			err  error
		}
		done := make(chan result, 1)
		go func() {
			list, err := fetchRemote(profile)
			done <- result{list, err}
		}()
		select {
		case r := <-done:
			if r.err != nil {
				out[i] = RemoteProfileSessionsOutcome{Profile: profile, Error: errorMessage(r.err), Failed: true}
				return
			}
			out[i] = RemoteProfileSessionsOutcome{Profile: profile, List: r.list}
		case <-time.After(time.Duration(budgetMs * float64(time.Millisecond))):
			out[i] = RemoteProfileSessionsOutcome{Profile: profile, Failed: true,
				Error: `remote profile "` + profile + `" did not respond within ` + jsNumber(budgetMs) + "ms"}
		}
	})
	return out
}

// ApplyRemoteProfileSessionOutcomes merges answered remote lists and turns dead remotes into scan errors
func ApplyRemoteProfileSessionOutcomes(outcomes []RemoteProfileSessionsOutcome, merged *[]any, profileTotals map[string]any, total float64) (float64, []SessionScanError) {
	errs := []SessionScanError{}
	for _, o := range outcomes {
		if o.Failed || !jsTruthy(o.List) {
			delete(profileTotals, o.Profile)
			errs = append(errs, SessionScanError{Profile: o.Profile, Error: orDefault(o.Error, "unavailable")})
			continue
		}
		rows := rowsOf(o.List)
		*merged = append(*merged, rows...)
		count := orNumber(ecmaToNumber(jsObject(o.List)["total"]), float64(len(rows)))
		profileTotals[o.Profile] = count
		total += count
	}
	return total, errs
}

func archivedOnlyWindow(merged []any, offset, limit int) []any {
	archived := []any{}
	for _, row := range merged {
		if isArchived(row) {
			archived = append(archived, row)
		}
	}
	window := MergeProfileSessionWindow(archived, offset, limit)
	ids := map[string]bool{}
	for _, row := range window {
		if id, ok := profileSessionID(row); ok {
			ids[id] = true
		}
	}
	for _, row := range merged {
		id, hasID := profileSessionID(row)
		if !isArchived(row) && isPinned(row) && !(hasID && ids[id]) {
			if hasID {
				ids[id] = true
			}
			window = append(window, row)
		}
	}
	return window
}

// ComposeUnifiedSessionResponse windows the merged rows over the base response with totals and scan errors
func ComposeUnifiedSessionResponse(base any, merged []any, offset, limit, total float64, profileTotals map[string]any, extraErrors []SessionScanError, archivedOnly bool) map[string]any {
	scanErrors := []any{}
	if errs, ok := jsObject(base)["errors"].([]any); ok {
		scanErrors = append(scanErrors, errs...)
	}
	scanErrors = append(scanErrors, scanErrorValues(extraErrors)...)
	out := jsSpreadOf(base)
	if archivedOnly {
		out["sessions"] = archivedOnlyWindow(merged, int(offset), int(limit))
	} else {
		out["sessions"] = MergeProfileSessionWindow(merged, int(offset), int(limit))
	}
	out["total"] = total
	out["profile_totals"] = profileTotals
	out["limit"] = limit
	out["offset"] = offset
	if len(scanErrors) > 0 {
		out["errors"] = scanErrors
	}
	return out
}

// FindRemoteOwnerProfileForSession names the first remote profile whose recent sessions hold the id or its lineage root
func FindRemoteOwnerProfileForSession(id string, remoteProfiles []string, listForProfile func(profile string, params *searchParams) (any, error)) string {
	if id == "" || len(remoteProfiles) == 0 {
		return ""
	}
	params := newSearchParams()
	params.set("limit", "200")
	params.set("offset", "0")
	matches := make([]string, len(remoteProfiles))
	parallel(len(remoteProfiles), func(i int) {
		list, err := listForProfile(remoteProfiles[i], params)
		if err != nil {
			return
		}
		rows, _ := jsObject(list)["sessions"].([]any)
		for _, row := range rows {
			obj := jsObject(row)
			if obj["id"] == id || obj["_lineage_root_id"] == id {
				matches[i] = remoteProfiles[i]
				return
			}
		}
	})
	for _, m := range matches {
		if m != "" {
			return m
		}
	}
	return ""
}
