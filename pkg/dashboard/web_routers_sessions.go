package dashboard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const sessionNotFound = "Session not found"

const sessionImportMaxBytes = 25 * 1024 * 1024

const activeWindowS = 300

// Request body of a session import read in chunks, 413 once it passes the import cap
func readSessionImportBody(ctx context.Context, req *Request) ([]byte, error) {
	var body []byte
	err := req.Stream(ctx, func(chunk []byte) error {
		if len(body)+len(chunk) > sessionImportMaxBytes {
			return NewHTTPException(413, "Session import payload is too large")
		}
		body = append(body, chunk...)
		return nil
	})
	return body, err
}

// Stripped non empty items of a comma separated query value
func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if s := pystr.Strip(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Whether a session row is open and active inside the activity window
func sessionRowIsActive(row *pyjson.Object, now float64) (bool, error) {
	if ended, _ := row.Get("ended_at"); ended != nil {
		return false, nil
	}
	var last any = int64(0)
	if v, ok := row.Get("last_active"); ok {
		last = v
	} else if v, ok := row.Get("started_at"); ok {
		last = v
	}
	f, err := pyint.Float(last)
	if err != nil {
		return false, err
	}
	return now-f < activeWindowS, nil
}

// Profile pointer of an optional query value, nil for none
func optionalProfile(profile string) *string {
	if profile == "" {
		return nil
	}
	return &profile
}

// Opens the profile's session store, runs fn on it and always closes it, writes under the owner lock
func withSessionDB[T any](ctx context.Context, profile string, readOnly bool, fn func(db *state.DB) (T, error)) (T, error) {
	run := func(ctx context.Context) (T, error) {
		var zero T
		db, err := openSessionDBForProfile(ctx, optionalProfile(profile), readOnly)
		if err != nil {
			return zero, err
		}
		defer db.Close()
		return fn(db)
	}
	if readOnly {
		return run(ctx)
	}
	return withSessionMaintenance(ctx, profile, run)
}

// Profile name rows are stamped with, the requested one or else the serving process's own
func servingProfile(ctx context.Context, profile string) (string, error) {
	if profile != "" {
		name, _, err := cronProfileHome(ctx, profile)
		return name, err
	}
	return cronDefaultProfile(ctx), nil
}

// Store surface the session id resolver reads
type sessionIDResolver interface {
	ResolveSessionID(idOrPrefix string) (string, error)
	Path() string
}

// Full id of a session id or prefix, a corrupt store answered 503 rather than a misleading 404
func resolveSessionIDOrStatus(db sessionIDResolver, sessionID string) (string, error) {
	sid, err := db.ResolveSessionID(sessionID)
	if err == nil {
		return sid, nil
	}
	if class, ok := state.SQLiteClassOf(err); ok && class != state.SQLiteInterfaceError {
		if !state.IsMalformedDBError(err) {
			return "", err
		}
		routersLog().Error(fmt.Sprintf("state.db is corrupt while resolving session %s: %s", sessionID, err.Error()))
		return "", &causedHTTPException{NewHTTPException(503, "Session store is corrupt (database disk image is malformed). "+
			"Sessions cannot be read until it is repaired — run `icebath doctor` for diagnosis."), err}
	}
	var replaced *state.StateDBReplacedError
	if errors.As(err, &replaced) {
		return "", corruptStoreAsStatus(db.Path(), err)
	}
	return "", err
}

// Sessions page with total, active flags, owning profile and the storage state of the store
func getSessions(ctx context.Context, limit, offset, minMessages int64, archived, order, source, sources, excludeSources,
	cwdPrefix string, full bool, profile string) (any, error) {
	if archived != "exclude" && archived != "only" && archived != "include" {
		return nil, NewHTTPException(400, "archived must be one of: exclude, only, include")
	}
	if order != "created" && order != "recent" {
		return nil, NewHTTPException(400, "order must be one of: created, recent")
	}
	profileName := ""
	if profile != "" {
		name, _, err := cronProfileHome(ctx, profile)
		if err != nil {
			return nil, err
		}
		profileName = name
	}
	result, err := listSessionsPage(ctx, limit, offset, minMessages, archived, order, source, sources, excludeSources, cwdPrefix,
		full, profile, profileName)
	if err == nil {
		return result, nil
	}
	if _, ok := asHTTPException(err); ok {
		return nil, err
	}
	class, isSQLite := state.SQLiteClassOf(err)
	if isSQLite && class == state.SQLiteOperationalError {
		routersLog().Error("GET /api/sessions failed", "exc_info", err)
		if state.IsTransientSQLiteError(err) {
			return nil, &causedHTTPException{NewHTTPException(503, "Session store is busy (disk I/O or lock). Retry; the list was not cleared."), err}
		}
		return nil, &causedHTTPException{NewHTTPException(500, "Internal server error"), err}
	}
	if isSQLite && class != state.SQLiteInterfaceError {
		dbPath, perr := sessionDBPathForProfile(ctx, optionalProfile(profile))
		if perr != nil {
			return nil, perr
		}
		if !(state.NoteStorageError(dbPath, err) || state.IsMalformedDBError(err)) {
			routersLog().Error("GET /api/sessions failed", "exc_info", err)
			return nil, &causedHTTPException{NewHTTPException(500, "Internal server error"), err}
		}
		routersLog().Error(fmt.Sprintf("GET /api/sessions: state.db at %s is corrupt: %s", dbPath, err.Error()))
		return nil, &causedHTTPException{NewHTTPException(503, pyjson.ObjectOf("error", corruptStoreDetail.code,
			"message", corruptStoreDetail.message)), err}
	}
	var replaced *state.StateDBReplacedError
	if errors.As(err, &replaced) {
		dbPath, perr := sessionDBPathForProfile(ctx, optionalProfile(profile))
		if perr != nil {
			return nil, perr
		}
		return nil, corruptStoreAsStatus(dbPath, err)
	}
	routersLog().Error("GET /api/sessions failed", "exc_info", err)
	return nil, NewHTTPException(500, "Internal server error")
}

// Reads one page of the sessions list and its total under the requested scope
func listSessionsPage(ctx context.Context, limit, offset, minMessages int64, archived, order, source, sources, excludeSources,
	cwdPrefix string, full bool, profile, profileName string) (*pyjson.Object, error) {
	db, err := openSessionDBForProfile(ctx, optionalProfile(profile), true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	minMessageCount := int(max(0, minMessages))
	archivedOnly, includeArchived := archived == "only", archived == "include"
	sourceList := splitCSV(sources)
	includeSubagents, excludeList := subagentListingScope(ctx, filepath.Dir(db.Path()), source, sourceList, splitCSV(excludeSources))
	pageLimit := int(limit)
	sessions, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Source: source, Sources: sourceList,
		ExcludeSources: excludeList, CwdPrefix: cwdPrefix, MinMessageCount: minMessageCount, IncludeArchived: includeArchived,
		ArchivedOnly: archivedOnly, IncludeSubagents: includeSubagents, Limit: &pageLimit, Offset: int(offset),
		OrderByLastActive: order == "recent", CompactRows: !full, IncludePinned: true})
	if err != nil {
		return nil, err
	}
	total, err := db.SessionCount(ctx, state.SessionCountOptions{Source: source, Sources: sourceList, ExcludeSources: excludeList,
		CwdPrefix: cwdPrefix, MinMessageCount: minMessageCount, IncludeArchived: includeArchived, ArchivedOnly: archivedOnly,
		IncludeSubagents: includeSubagents, ExcludeChildren: true})
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	rowProfile := profileName
	if rowProfile == "" {
		rowProfile = cronDefaultProfile(ctx)
	}
	for _, s := range sessions {
		active, err := sessionRowIsActive(s, now)
		if err != nil {
			return nil, err
		}
		s.Set("is_active", active)
		s.Set("profile", rowProfile)
		s.Set("is_default_profile", rowProfile == "default")
		archivedFlag, _ := s.Get("archived")
		s.Set("archived", pyTruthy(archivedFlag))
		pinned, _ := s.Get("pinned")
		s.Set("pinned", pyTruthy(pinned))
	}
	if !full {
		stripSessionListRows(sessions)
	}
	storage := pyjson.NewObject()
	if state.StorageState(db.Path()) == state.StorageCorrupt {
		storage.Set(rowProfile, state.StorageCorrupt)
	}
	rows := make([]any, len(sessions))
	for i, s := range sessions {
		rows[i] = s
	}
	return pyjson.ObjectOf("sessions", rows, "total", total, "limit", limit, "offset", offset, "storage", storage), nil
}

// Whether a child row continues its parent across a compression boundary
func isCompressionEdge(child, parent *pyjson.Object) (bool, error) {
	endReason, _ := parent.Get("end_reason")
	parentEndedAt, _ := parent.Get("ended_at")
	startedAt, _ := child.Get("started_at")
	if endReason != "compression" || parentEndedAt == nil || startedAt == nil {
		return false, nil
	}
	started, err := pyint.Float(startedAt)
	if err != nil {
		return false, err
	}
	ended, err := pyint.Float(parentEndedAt)
	if err != nil {
		return false, err
	}
	return started >= ended, nil
}

// Search tokens, quoted phrases kept whole
var searchTokenRE = regexp.MustCompile(`"[^"]*"|\S+`)

// Session search by id first, then message content, then titles, deduped by compression lineage
func searchSessions(ctx context.Context, q string, limit int64, profile, source, sources, excludeSources string) (any, error) {
	if q == "" || pystr.Strip(q) == "" {
		return pyjson.ObjectOf("results", []any{}), nil
	}
	detail := "Search failed"
	result, err := func() (any, error) {
		rowProfile, err := servingProfile(ctx, profile)
		if err != nil {
			return nil, err
		}
		return withSessionDB(ctx, profile, true, func(db *state.DB) (any, error) {
			return searchSessionLineages(ctx, db, q, limit, profile, rowProfile, source, sources, excludeSources)
		})
	}()
	if err != nil {
		return nil, httpFailure(err, "GET /api/sessions/search failed", 500, "", &detail)
	}
	return result, nil
}

// Lineage deduped search results of one store
func searchSessionLineages(ctx context.Context, db *state.DB, q string, limit int64, profile, rowProfile, source, sources,
	excludeSources string) (any, error) {
	if limit == 0 {
		limit = 20
	}
	safeLimit := int(max(1, min(limit, 100)))
	sourceList := splitCSV(sources)
	includeSources := sourceList
	if source != "" {
		includeSources = []string{source}
	}
	excludeList := splitCSV(excludeSources)
	now := float64(time.Now().UnixNano()) / 1e9
	getSession := func(sid string) *pyjson.Object {
		row, err := db.GetSession(ctx, sid)
		if err != nil {
			return nil
		}
		return row
	}
	rootCache := map[string]string{}
	compressionRoot := func(sessionID string) (string, error) {
		var chain []string
		cur, root := sessionID, sessionID
		inChain := map[string]bool{}
		for cur != "" && !inChain[cur] {
			if cached, ok := rootCache[cur]; ok {
				root = cached
				break
			}
			chain = append(chain, cur)
			inChain[cur] = true
			s := getSession(cur)
			parent := ""
			if s != nil {
				p, _ := s.Get("parent_session_id")
				parent, _ = p.(string)
			}
			var parentSession *pyjson.Object
			if parent != "" {
				parentSession = getSession(parent)
			}
			if parentSession == nil {
				root = cur
				break
			}
			edge, err := isCompressionEdge(s, parentSession)
			if err != nil {
				return "", err
			}
			if !edge {
				root = cur
				break
			}
			cur = parent
		}
		for _, node := range chain {
			rootCache[node] = root
		}
		return root, nil
	}
	tipCache := map[string]string{}
	lineageTip := func(sessionID string) string {
		if _, ok := tipCache[sessionID]; !ok {
			tip, err := db.GetCompressionTip(ctx, sessionID)
			if err != nil || tip == "" {
				tip = sessionID
			}
			tipCache[sessionID] = tip
		}
		return tipCache[sessionID]
	}
	seen := pyjson.NewObject()
	addLineageResult := func(rawSID string, hit *pyjson.Object) error {
		if rawSID == "" {
			return nil
		}
		root, err := compressionRoot(rawSID)
		if err != nil {
			return err
		}
		if _, dup := seen.Get(root); dup || len(seen.Keys) >= safeLimit {
			return nil
		}
		payload := hit.Copy()
		sid := lineageTip(rawSID)
		payload.Set("session_id", sid)
		payload.Set("lineage_root", root)
		payload.Set("profile", rowProfile)
		payload.Set("is_default_profile", rowProfile == "default")
		row, rerr := db.GetSessionRichRow(ctx, sid, false)
		if rerr != nil {
			row = nil
		}
		if row != nil && len(row.Keys) > 0 {
			get := func(key string) any { v, _ := row.Get(key); return v }
			orZero := func(key string) any {
				if v := get(key); pyTruthy(v) {
					return v
				}
				return int64(0)
			}
			lastActive := get("last_active")
			if !pyTruthy(lastActive) {
				lastActive = get("started_at")
			}
			id := get("id")
			if !pyTruthy(id) {
				id = sid
			}
			lastActiveNum := any(int64(0))
			if pyTruthy(lastActive) {
				lastActiveNum = lastActive
			}
			lastF, err := pyint.Float(lastActiveNum)
			if err != nil {
				return err
			}
			owner, err := servingProfile(ctx, profile)
			if err != nil {
				return err
			}
			payload.Set("id", id)
			payload.Set("source", get("source"))
			payload.Set("model", get("model"))
			payload.Set("title", get("title"))
			payload.Set("started_at", get("started_at"))
			payload.Set("ended_at", get("ended_at"))
			payload.Set("last_active", lastActive)
			payload.Set("is_active", get("ended_at") == nil && now-lastF < 300)
			payload.Set("message_count", orZero("message_count"))
			payload.Set("tool_call_count", orZero("tool_call_count"))
			payload.Set("input_tokens", orZero("input_tokens"))
			payload.Set("output_tokens", orZero("output_tokens"))
			payload.Set("preview", get("preview"))
			payload.Set("parent_session_id", get("parent_session_id"))
			payload.Set("profile", owner)
			payload.Set("archived", pyTruthy(get("archived")))
		} else {
			payload.Set("id", sid)
		}
		seen.Set(root, payload)
		return nil
	}
	hitPayload := func(row *pyjson.Object, snippet string, role, sessionStarted any) *pyjson.Object {
		src, _ := row.Get("source")
		model, _ := row.Get("model")
		lastActive, _ := row.Get("last_active")
		return pyjson.ObjectOf("snippet", snippet, "role", role, "source", src, "model", model, "session_started", sessionStarted,
			"last_active", lastActive)
	}
	includeArchived := true
	idRows, err := db.SearchSessionsByID(ctx, q, state.SearchSessionsByIDOptions{Limit: &safeLimit, IncludeArchived: &includeArchived,
		Source: source, Sources: sourceList, ExcludeSources: excludeList})
	if err != nil {
		return nil, err
	}
	for _, row := range idRows {
		id, _ := row.Get("id")
		sid, _ := id.(string)
		preview, _ := row.Get("preview")
		snippet := pystr.Strip(pyStrOr(preview))
		if snippet == "" {
			snippet = "Session ID: " + pyStrOr(id)
		}
		started, _ := row.Get("started_at")
		if err := addLineageResult(sid, hitPayload(row, snippet, nil, started)); err != nil {
			return nil, err
		}
	}
	var tokens []string
	for _, tok := range searchTokenRE.FindAllString(pystr.Strip(q), -1) {
		if strings.HasPrefix(tok, `"`) || strings.HasSuffix(tok, "*") {
			tokens = append(tokens, tok)
		} else {
			tokens = append(tokens, tok+"*")
		}
	}
	opts := state.NewSearchMessagesOptions()
	opts.SourceFilter = includeSources
	opts.ExcludeSources = excludeList
	opts.Limit = max(safeLimit*5, 50)
	opts.Fields = []string{"session_id", "role", "snippet", "source", "model", "session_started"}
	matches, err := db.SearchMessages(ctx, strings.Join(tokens, " "), opts)
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		if len(seen.Keys) >= safeLimit {
			break
		}
		sid, _ := m.Get("session_id")
		snippet, _ := m.Get("snippet")
		role, _ := m.Get("role")
		started, _ := m.Get("session_started")
		if err := addLineageResult(pyStrOr(sid), hitPayload(m, pyStrOr(snippet), role, started)); err != nil {
			return nil, err
		}
	}
	if len(seen.Keys) < safeLimit {
		titleLimit := safeLimit
		titleRows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{SearchQuery: pystr.Strip(q), IncludeArchived: true,
			OrderByLastActive: true, Source: source, Sources: sourceList, ExcludeSources: excludeList, Limit: &titleLimit})
		if err != nil {
			head := q
			if r := []rune(q); len(r) > 200 {
				head = string(r[:200])
			}
			routersLog().Debug(fmt.Sprintf("Title-match supplement skipped for %s", pyrepr.Repr(head)))
			titleRows = nil
		}
		for _, row := range titleRows {
			if len(seen.Keys) >= safeLimit {
				break
			}
			id, _ := row.Get("id")
			sid := pyStrOr(id)
			if !pyTruthy(id) {
				continue
			}
			preview, _ := row.Get("preview")
			snippet := pystr.Strip(pyStrOr(preview))
			if snippet == "" {
				snippet = "Session title matched: " + pystr.Strip(q)
			}
			started, _ := row.Get("started_at")
			if err := addLineageResult(sid, hitPayload(row, snippet, nil, started)); err != nil {
				return nil, err
			}
		}
	}
	results := make([]any, 0, len(seen.Keys))
	for _, key := range seen.Keys {
		v, _ := seen.Get(key)
		results = append(results, v)
	}
	return pyjson.ObjectOf("results", results), nil
}

// String of a row value, empty for a missing or non string value
func pyStrOr(v any) string {
	s, _ := v.(string)
	return s
}

// List router of the sessions page, mounted before the profiles sessions router
func sessionsListRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/sessions", &Endpoint{
		Name: "get_sessions",
		Doc: `List sessions.

    ` + "``order=recent``" + ` sorts by latest activity across the compression chain, so
    a long-running chat stays on page one after it auto-compresses onto a fresh
    id.  Rows omit ` + "``system_prompt`` / ``model_config``" + ` unless ` + "``full=1``" + `.`,
		Params: []Param{QueryParam("limit", IntRange(Bound(0), Bound(100)), int64(20)),
			QueryParam("offset", IntRange(Bound(0), nil), int64(0)), QueryParam("min_messages", Int, int64(0)),
			QueryParam("archived", Str, "exclude"), QueryParam("order", Str, "created"),
			QueryParam("source", Optional(Str), nil), QueryParam("sources", Optional(Str), nil),
			QueryParam("exclude_sources", Optional(Str), nil), QueryParam("cwd_prefix", Optional(Str), nil),
			QueryParam("full", Bool, false), QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessions(ctx, c.Int("limit"), c.Int("offset"), c.Int("min_messages"), c.Str("archived"), c.Str("order"),
				c.Str("source"), c.Str("sources"), c.Str("exclude_sources"), c.Str("cwd_prefix"), c.Bool("full"), c.Str("profile"))
		},
	})
	return r
}

// Search router of the sessions page, mounted right after the profiles sessions router
func sessionsSearchRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/sessions/search", &Endpoint{
		Name: "search_sessions",
		Doc: `Search sessions by ID (first) plus FTS5 message content.

    Results are deduped by compression lineage, not raw ` + "``session_id``" + `:
    auto-compression rotates a chat onto a fresh id and leaves the old segment
    in the FTS index.  Branches also use ` + "``parent_session_id``" + ` but are real
    alternate conversations — they are NOT collapsed into the parent.`,
		Params: []Param{QueryParam("q", Str, ""), QueryParam("limit", Int, int64(20)), QueryParam("profile", Optional(Str), nil),
			QueryParam("source", Optional(Str), nil), QueryParam("sources", Optional(Str), nil),
			QueryParam("exclude_sources", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return searchSessions(ctx, c.Str("q"), c.Int("limit"), c.Str("profile"), c.Str("source"), c.Str("sources"),
				c.Str("exclude_sources"))
		},
	})
	return r
}
