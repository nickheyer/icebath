package dashboard

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var pluginAPIMountMu sync.Mutex

var pluginAPIMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"}

// Context resolving config as the launch profile with its home and secrets switched together
func launchProfileScope(ctx context.Context) (context.Context, error) {
	sctx, _, err := configProfileScope(home.WithOverride(ctx, ""), "")
	return sctx, err
}

// Home and secret scope of a plugin api request, 404 when the scope disabled it or this router is not its copy
func pluginRouteSecretScope(ctx context.Context, conn *HTTPConnection, profile string) (context.Context, error) {
	name := pluginAPIName(conn.Scope)
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return ctx, err
	}
	refused := !isCurrentProfile(profile) && pluginAPIRequestRefused(sctx, name)
	if !refused {
		if refused, err = inProcessRouterRefused(sctx, conn.App(), name); err != nil {
			return ctx, err
		}
	}
	if refused {
		if conn.Scope.Type == "websocket" {
			return ctx, &WebSocketException{Code: 1008}
		}
		return ctx, NewHTTPException(404, "Plugin not found")
	}
	return sctx, nil
}

// Plugin api route under its prefix with the api file it runs when it is the compiled copy here
type pluginAPIRoute struct {
	*APIRoute
	inProcess bool
	apiPath   string
}

// Forward route of a plugin api served by the runtime plugin of the requesting profile
func mountGuestPluginAPI(plugin *pyjson.Object) *pluginAPIRoute {
	name := pyrepr.Str(plugin.GetOr("name", nil))
	ep := &Endpoint{
		Name:   "forward",
		Params: []Param{PathParam("path", Str), QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			sctx, err := pluginRouteSecretScope(ctx, c.Conn(), c.Str("profile"))
			if err != nil {
				return nil, err
			}
			pluginDir := guestPluginForRequest(sctx, name)
			if pluginDir == "" {
				return MustJSONResponse(map[string]any{"detail": fmt.Sprintf("plugin %s is not enabled in this profile", pyrepr.Repr(name))}, 404), nil
			}
			req := c.Request
			query := req.URL().Query()
			headers := [][2]string{}
			for _, kv := range req.Headers().Items() {
				if strings.ToLower(kv[0]) != "host" {
					headers = append(headers, kv)
				}
			}
			body, err := req.Body(ctx)
			if err != nil {
				return nil, err
			}
			authenticated, err := admittedForSensitiveRoute(req)
			if err != nil {
				return nil, err
			}
			forward := plugins.DashboardRequest{Method: req.Method(), Path: "/" + c.Str("path"), Query: query, Headers: headers, Body: body, Authenticated: authenticated}
			return runHostedRequest(sctx, home.Dir(sctx), name, func(jctx context.Context, timeout time.Duration) (*plugins.DashboardResponse, error) {
				return hostDashboardRequest(jctx, pluginDir, forward, timeout)
			})
		},
	}
	route := &pluginAPIRoute{APIRoute: NewAPIRoute("/api/plugins/"+name+"/{path:path}", pluginAPIMethods, ep)}
	webServerLog().Info(fmt.Sprintf("Mounted plugin API routes served by the runtime plugin: /api/plugins/%s/", name))
	return route
}

// Plugin directory of the runtime plugin serving name in the active profile, empty when that profile has no enabled copy on disk
func guestPluginForRequest(ctx context.Context, name string) string {
	for _, plugin := range discoverDashboardPlugins(nil) {
		if n, ok := plugin.GetOr("name", nil).(string); !ok || n != name || !pyTruthy(plugin.GetOr("_api_file", nil)) {
			continue
		}
		if _, compiled := compiledDashboard(plugin); compiled {
			continue
		}
		if source := plugin.GetOr("source", nil); source != "user" && source != "bundled" {
			continue
		}
		if pluginAPIMountSkipReason(plugin, plugins.EnabledPluginSet(ctx), plugins.DisabledPluginSet(ctx)) != "" {
			return ""
		}
		if pluginDir := guestPluginDir(plugin); pluginDir != "" {
			return pluginDir
		}
	}
	return ""
}

// Plugin directory above a dashboard entry when it holds a runtime plugin, empty otherwise
func guestPluginDir(plugin *pyjson.Object) string {
	pluginDir := filepath.Dir(pyrepr.Str(plugin.GetOr("_dir", nil)))
	if plugins.GuestManifest(pluginDir) == nil {
		return ""
	}
	return pluginDir
}

// Route of a compiled in dashboard api handler bound to the request profile scope
func compiledPluginAPIRoute(name string, d plugins.BundledDashboard, apiPath string) *pluginAPIRoute {
	ep := &Endpoint{
		Name:   "plugin_api",
		Params: []Param{PathParam("path", Str), QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			sctx, err := pluginRouteSecretScope(ctx, c.Conn(), c.Str("profile"))
			if err != nil {
				return nil, err
			}
			return serveCompiledPluginAPI(sctx, d.API(sctx), c.Request, "/"+c.Str("path"))
		},
	}
	return &pluginAPIRoute{APIRoute: NewAPIRoute("/api/plugins/"+name+"/{path:path}", pluginAPIMethods, ep), inProcess: true, apiPath: apiPath}
}

// Buffering response writer a compiled plugin handler writes its answer into
type compiledPluginWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *compiledPluginWriter) Header() http.Header { return w.header }

func (w *compiledPluginWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *compiledPluginWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

// Answer of a compiled plugin handler to the request with the plugin prefix stripped from its path
func serveCompiledPluginAPI(ctx context.Context, handler http.Handler, req *Request, subPath string) (*Response, error) {
	body, err := req.Body(ctx)
	if err != nil {
		return nil, err
	}
	s := req.Scope
	uri := s.RawPath
	if uri == "" {
		uri = (&url.URL{Path: s.Path}).EscapedPath()
	}
	if s.QueryString != "" {
		uri += "?" + s.QueryString
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method(), uri, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.URL.Path, hreq.URL.RawPath = subPath, ""
	hreq.URL.RawQuery = s.QueryString
	hreq.RequestURI = uri
	hreq.Proto = "HTTP/" + s.HTTPVersion
	hreq.ContentLength = int64(len(body))
	for _, kv := range req.Headers().Items() {
		hreq.Header.Add(kv[0], kv[1])
	}
	hreq.Host = req.Headers().Get("host")
	if s.Client != nil {
		hreq.RemoteAddr = net.JoinHostPort(s.Client.Host, strconv.Itoa(s.Client.Port))
	}
	if s.Scheme == "https" {
		hreq.TLS = &tls.ConnectionState{}
	}
	w := &compiledPluginWriter{header: http.Header{}}
	handler.ServeHTTP(w, hreq)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	keys := make([]string, 0, len(w.header))
	for k := range w.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		for _, v := range w.header[k] {
			pairs = append(pairs, k, v)
		}
	}
	return NewResponse(w.body.Bytes(), w.status, "", pairs...), nil
}

// Route serving the plugin api when its trust gates pass, nil when it must not be mounted
func onePluginAPIRoute(app *FastAPI, plugin *pyjson.Object, enabled, disabled map[string]bool) (*pluginAPIRoute, error) {
	apiFileName, _ := plugin.GetOr("_api_file", nil).(string)
	if !pyTruthy(plugin.GetOr("_api_file", nil)) {
		return nil, nil
	}
	name := pyrepr.Str(plugin.GetOr("name", ""))
	if pluginAPIMounted(app, name) {
		return nil, nil
	}
	if skip := pluginAPIMountSkipReason(plugin, enabled, disabled); skip != "" {
		webServerLog().Debug(fmt.Sprintf("Plugin %s: skipping API mount (%s)", name, skip))
		return nil, nil
	}
	if plugin.GetOr("source", nil) == "project" {
		webServerLog().Warn(fmt.Sprintf("Plugin %s: ignoring backend api=%s (project plugins "+
			"do not serve dashboard routes; move the plugin to "+
			"~/.icebath/plugins/ if you trust it)", name, apiFileName))
		return nil, nil
	}
	dashboardDir := pyrepr.Str(plugin.GetOr("_dir", nil))
	apiPath := filepath.Join(dashboardDir, apiFileName)
	resolved, errA := pypath.Resolve(apiPath)
	root, errB := pypath.Resolve(dashboardDir)
	if _, inside := pypath.PathRelativeTo(resolved, root); errA != nil || errB != nil || !inside {
		webServerLog().Warn(fmt.Sprintf("Plugin %s: refusing an api path outside its "+
			"dashboard directory (%s)", name, apiPath))
		return nil, nil
	}
	if d, compiled := compiledDashboard(plugin); compiled {
		if d.API == nil {
			webServerLog().Warn(fmt.Sprintf("Plugin %s declares api=%s but file not found", name, apiFileName))
			return nil, nil
		}
		route := compiledPluginAPIRoute(name, d, resolved)
		webServerLog().Info(fmt.Sprintf("Mounted plugin API routes: /api/plugins/%s/", name))
		return route, nil
	}
	if guestPluginDir(plugin) == "" {
		webServerLog().Warn(fmt.Sprintf("Plugin %s declares api=%s but is neither compiled in nor a runtime plugin; %s",
			name, apiFileName, plugins.PluginFormsText))
		return nil, nil
	}
	return mountGuestPluginAPI(plugin), nil
}

// Mounts one plugin api on the app when its trust gates pass, true when routes were added
func mountOnePluginAPI(app *FastAPI, plugin *pyjson.Object, enabled, disabled map[string]bool) (bool, error) {
	route, err := onePluginAPIRoute(app, plugin, enabled, disabled)
	if err != nil || route == nil {
		return false, err
	}
	app.Router.Routes = append(app.Router.Routes, route)
	return true, nil
}

// Mounts the backend api of every discovered plugin its gates allow, then the hot mount fallback
func mountPluginAPIRoutes(app *FastAPI) {
	ctx := context.Background()
	enabled, disabled := plugins.EnabledPluginSet(ctx), plugins.DisabledPluginSet(ctx)
	for _, plugin := range getDashboardPlugins(ctx, false) {
		pluginAPIMountMu.Lock()
		_, err := mountOnePluginAPI(app, plugin, enabled, disabled)
		pluginAPIMountMu.Unlock()
		if err != nil {
			panic(err)
		}
	}
	registerPluginAPIFallback(app)
}

// Mounts a plugin installed or enabled after start by the launch profile decision, true when routes went live
func tryHotMountPluginAPI(ctx context.Context, app *FastAPI, name string) (bool, error) {
	if name == "" || strings.ContainsAny(name, "/\\{}") || name == ".." {
		return false, nil
	}
	_, secondary := secondaryProfileHome(ctx)
	own := secondaryPlugin(ctx, name)
	reqEnabled, reqDisabled := pluginEnableSetsHere(ctx)
	lctx, err := launchProfileScope(ctx)
	if err != nil {
		return false, err
	}
	pluginAPIMountMu.Lock()
	defer pluginAPIMountMu.Unlock()
	enabled, disabled := plugins.EnabledPluginSet(lctx), plugins.DisabledPluginSet(lctx)
	if pluginAPIMounted(app, name) {
		return false, nil
	}
	plugin := namedPlugin(getDashboardPlugins(lctx, true), name)
	if secondary {
		if own == nil || own.GetOr("source", nil) != "bundled" {
			return false, nil
		}
		launchPath, err := pluginAPIPath(plugin)
		if err != nil {
			return false, err
		}
		ownPath, err := pluginAPIPath(own)
		if err != nil {
			return false, err
		}
		if launchPath != ownPath {
			return false, nil
		}
		enabled, disabled = reqEnabled, reqDisabled
	}
	if plugin == nil {
		return false, nil
	}
	route, err := onePluginAPIRoute(app, plugin, enabled, disabled)
	if err != nil || route == nil {
		return false, err
	}
	if hot := hotMountOf(app); hot != nil {
		hot.mu.Lock()
		hot.routes = append(hot.routes, route)
		hot.mu.Unlock()
	} else {
		app.Router.Routes = append(app.Router.Routes, route)
	}
	return true, nil
}

// True when a route of the app or of its hot mount fallback already serves the plugin prefix
func pluginAPIMounted(app *FastAPI, name string) bool {
	prefix := "/api/plugins/" + name
	under := func(p string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	for _, r := range app.Router.Routes {
		if ar, ok := r.(*ASGIRoute); ok {
			if hot, ok := ar.Endpoint.(*pluginAPIHotMount); ok {
				for _, hr := range hot.mounted() {
					if under(hr.RoutePath()) {
						return true
					}
				}
				continue
			}
		}
		if under(r.RoutePath()) {
			return true
		}
	}
	return false
}

// Compiled in route serving the plugin prefix in this process, nil when none is mounted
func mountedInProcessRoute(app *FastAPI, name string) *pluginAPIRoute {
	prefix := "/api/plugins/" + name + "/"
	routes := []*pluginAPIRoute{}
	for _, r := range app.Router.Routes {
		if pr, ok := r.(*pluginAPIRoute); ok {
			routes = append(routes, pr)
		}
	}
	if hot := hotMountOf(app); hot != nil {
		routes = append(routes, hot.mounted()...)
	}
	for _, pr := range routes {
		if pr.inProcess && strings.HasPrefix(pr.Path, prefix) {
			return pr
		}
	}
	return nil
}

// Fallback for plugin api requests no mounted route matched, holding the routes mounted on first request
type pluginAPIHotMount struct {
	app    *FastAPI
	route  *ASGIRoute
	mu     sync.RWMutex
	routes []*pluginAPIRoute
}

// Routes hot mounted so far in mount order
func (h *pluginAPIHotMount) mounted() []*pluginAPIRoute {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]*pluginAPIRoute(nil), h.routes...)
}

// Hands the request to the first hot mounted route fully matching it, false when none does
func (h *pluginAPIHotMount) dispatch(ctx context.Context, s *Scope, receive Receive, send Send) (bool, error) {
	probe := s.Clone()
	probe.PathParams = map[string]any{}
	for _, r := range h.mounted() {
		if match, child := r.Matches(probe); match == MatchFull {
			child.apply(probe)
			return true, r.Handle(ctx, probe, receive, send)
		}
	}
	return false, nil
}

// Mounts an unmounted plugin on its first request and serves that same request, else answers 404
func (h *pluginAPIHotMount) serve(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if handled, err := h.dispatch(ctx, s, receive, send); handled {
		return err
	}
	name := pyrepr.Str(s.PathParams["name"])
	if _, ok := s.PathParams["name"]; !ok {
		name = ""
	}
	sctx, release, err := pluginRequestScope(ctx, s)
	if err != nil {
		return err
	}
	mounted, err := tryHotMountPluginAPI(sctx, h.app, name)
	release()
	if err != nil {
		return err
	}
	if mounted {
		webServerLog().Info(fmt.Sprintf("Hot-mounted plugin API routes on first request: /api/plugins/%s/", name))
		if handled, err := h.dispatch(ctx, s, receive, send); handled {
			return err
		}
	}
	return MustJSONResponse(map[string]any{"detail": "Not Found"}, 404).Serve(ctx, s, receive, send)
}

// Hot mount fallback registered on the app, nil before registration
func hotMountOf(app *FastAPI) *pluginAPIHotMount {
	for _, r := range app.Router.Routes {
		if ar, ok := r.(*ASGIRoute); ok {
			if hot, ok := ar.Endpoint.(*pluginAPIHotMount); ok {
				return hot
			}
		}
	}
	return nil
}

// Registers the hot mount fallback once after the startup plugin routers and before the SPA catch all
func registerPluginAPIFallback(app *FastAPI) {
	if hotMountOf(app) != nil {
		return
	}
	hot := &pluginAPIHotMount{app: app}
	hot.route = NewASGIRoute("/api/plugins/{name}/{path:path}", hot, hot.serve, pluginAPIMethods)
	app.Router.Routes = append(app.Router.Routes, hot.route)
}
