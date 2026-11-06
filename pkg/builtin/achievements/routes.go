package achievements

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

//go:embed dashboard/manifest.json dashboard/dist
var dashboardFS embed.FS

func init() {
	assets, err := fs.Sub(dashboardFS, "dashboard")
	if err != nil {
		panic(err)
	}
	manifest, err := fs.ReadFile(assets, "manifest.json")
	if err != nil {
		panic(err)
	}
	plugins.RegisterBundledDashboard(plugins.BundledDashboard{Name: pluginName, Manifest: manifest, Assets: assets, API: Handler})
}

type route struct {
	methods []string
	serve   func(params map[string]string) (any, error)
}

// Serves every plugin API route below the plugin API prefix for the profile home of ctx
func Handler(ctx context.Context) http.Handler { return newEngine(ctx) }

// Route of a request path with its path parameters, nil when no route matches
func (e *engine) route(path string) (*route, map[string]string) {
	get := []string{"GET", "HEAD"}
	post := []string{"POST"}
	switch path {
	case "/achievements":
		return &route{get, func(map[string]string) (any, error) { return e.achievementsRoute() }}, nil
	case "/scan-status":
		return &route{get, func(map[string]string) (any, error) { return e.scanStatusPayload(0) }}, nil
	case "/recent-unlocks":
		return &route{get, func(map[string]string) (any, error) { return e.recentUnlocks() }}, nil
	case "/rescan":
		return &route{post, func(map[string]string) (any, error) { return e.rescan() }}, nil
	case "/reset-state":
		return &route{post, func(map[string]string) (any, error) { return e.resetState() }}, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[0] == "" && parts[1] == "sessions" && parts[2] != "" && parts[3] == "badges" {
		return &route{get, func(p map[string]string) (any, error) { return e.sessionBadges(p["session_id"]) }}, map[string]string{"session_id": parts[2]}
	}
	return nil, nil
}

// Dispatches a request like the FastAPI router, with 404, 405 and the trailing slash redirect
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if rt, params := e.route(path); rt != nil {
		allowed := false
		for _, m := range rt.methods {
			allowed = allowed || m == r.Method
		}
		if !allowed {
			w.Header().Set("Allow", strings.Join(rt.methods, ", "))
			writeResponse(w, http.StatusMethodNotAllowed, obj("detail", "Method Not Allowed"))
			return
		}
		body, err := rt.serve(params)
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("Internal Server Error"))
			return
		}
		writeResponse(w, http.StatusOK, body)
		return
	}
	if path != "/" && strings.HasSuffix(path, "/") {
		if rt, _ := e.route(strings.TrimSuffix(path, "/")); rt != nil {
			uri, query, hasQuery := strings.Cut(r.RequestURI, "?")
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			target := scheme + "://" + r.Host + strings.TrimSuffix(uri, "/")
			if hasQuery {
				target += "?" + query
			}
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
	}
	writeResponse(w, http.StatusNotFound, obj("detail", "Not Found"))
}

// Writes a JSON body the way the FastAPI JSONResponse renders it
func writeResponse(w http.ResponseWriter, code int, body any) {
	text, err := pyjson.Dumps(jsonSafe(body), pyjson.EnsureASCII(false), pyjson.AllowNaN(false), pyjson.Separators(",", ":"))
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Internal Server Error"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(text))
}

// Achievements payload with staleness and the scan status folded into scan_meta
func (e *engine) achievementsRoute() (any, error) {
	data, err := e.evaluateAll(false)
	if err != nil {
		return nil, err
	}
	payload := pyjson.NewObject()
	for _, k := range []string{"achievements", "unlocked_count", "discovered_count", "secret_count", "total_count", "error", "generated_at"} {
		if v, ok := data.Get(k); ok {
			payload.Set(k, v)
		}
	}
	stale, err := isSnapshotStale(data, e.now().Unix())
	if err != nil {
		return nil, err
	}
	payload.Set("is_stale", stale)
	status, err := e.scanStatusPayload(0)
	if err != nil {
		return nil, err
	}
	meta, _ := get(data, "scan_meta").(*pyjson.Object)
	payload.Set("scan_meta", merge(meta, obj("status", status)))
	return payload, nil
}

// Achievements of a payload, failing like a missing key or non list would
func achievementList(data *pyjson.Object, key string) ([]*pyjson.Object, error) {
	v, ok := data.Get(key)
	if !ok {
		return nil, fmt.Errorf("KeyError: '%s'", key)
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("'%T' achievements list", v)
	}
	out := make([]*pyjson.Object, 0, len(items))
	for _, it := range items {
		o, ok := it.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%T' object is not subscriptable", it)
		}
		out = append(out, o)
	}
	return out, nil
}

// Twenty most recent unlocked achievements, newest first
func (e *engine) recentUnlocks() (any, error) {
	data, err := e.evaluateAll(false)
	if err != nil {
		return nil, err
	}
	all, err := achievementList(data, "achievements")
	if err != nil {
		return nil, err
	}
	var unlocked []*pyjson.Object
	for _, a := range all {
		v, ok := a.Get("unlocked")
		if !ok {
			return nil, fmt.Errorf("KeyError: 'unlocked'")
		}
		if truthy(v) {
			unlocked = append(unlocked, a)
		}
	}
	sort.SliceStable(unlocked, func(i, j int) bool {
		return numKey(or(get(unlocked[i], "unlocked_at"), int64(0))) > numKey(or(get(unlocked[j], "unlocked_at"), int64(0)))
	})
	if len(unlocked) > 20 {
		unlocked = unlocked[:20]
	}
	return toAny(unlocked), nil
}

// Badges a single session earns on its own stats
func (e *engine) sessionBadges(sessionID string) (any, error) {
	data, err := e.evaluateAll(false)
	if err != nil {
		return nil, err
	}
	sessions, err := achievementList(data, "sessions")
	if err != nil {
		return nil, err
	}
	var session *pyjson.Object
	for _, s := range sessions {
		v, ok := s.Get("session_id")
		if !ok {
			return nil, fmt.Errorf("KeyError: 'session_id'")
		}
		if v == sessionID {
			session = s
			break
		}
	}
	if session == nil || session.Len() == 0 {
		return obj("session_id", sessionID, "badges", []any{}), nil
	}
	agg, err := aggregateStats([]*pyjson.Object{session})
	if err != nil {
		return nil, err
	}
	badges := []any{}
	for _, d := range e.catalog {
		r, err := evaluateDefinition(d, agg)
		if err != nil {
			return nil, err
		}
		if truthy(get(r, "unlocked")) {
			shown, err := displayAchievement(merge(d, r))
			if err != nil {
				return nil, err
			}
			badges = append(badges, shown)
		}
	}
	return obj("session_id", sessionID, "badges", badges), nil
}

// Synchronous rescan payload
func (e *engine) rescan() (any, error) {
	data, err := e.evaluateAll(true)
	if err != nil {
		return nil, err
	}
	return merge(obj("ok", true), data), nil
}

// Clears unlocks, the cache, the scan status and the snapshot and checkpoint files
func (e *engine) resetState() (any, error) {
	if err := e.saveState(obj("unlocks", pyjson.NewObject())); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.cache, e.cacheAt = nil, 0
	for _, k := range []string{"state", "started_at", "finished_at", "last_error", "last_duration_ms"} {
		var v any
		if k == "state" {
			v = "idle"
		}
		e.status.Set(k, v)
	}
	e.mu.Unlock()
	for _, name := range []string{snapshotFile, checkpointFile} {
		if path, err := e.dataFile(name); err == nil {
			_ = removeIfExists(path)
		}
	}
	return obj("ok", true), nil
}
