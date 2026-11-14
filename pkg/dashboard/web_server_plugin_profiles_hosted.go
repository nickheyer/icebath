package dashboard

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var gatewayCredentialHeaders = map[string]bool{
	"authorization": true, "cookie": true, "host": true, "x-icebath-gateway-ticket": true, "x-icebath-session-token": true}

var gatewayQueryKeys = map[string]bool{
	"token": true, "ticket": true, "internal": true, "display_ticket": true, "attach": true, "profile": true}

const hostedRequestThreads = 4

// Seconds a forwarded request may take with its queueing before it answers 504
var hostedRequestTimeoutSecs = 60.0

var hostedExecutors = map[string]chan struct{}{}

// Plugin host asgi round trip of one dashboard plugin request, the host method tests replace with a fake
var hostASGIRequest = (*plugins.PluginHost).ASGIRequest

// Backend file the in process router of name runs, the mounted compiled copy else the launch profile entry
func inProcessAPIFile(ctx context.Context, app *FastAPI, name string) (string, error) {
	if pluginAPIMounted(app, name) {
		if r := mountedInProcessRoute(app, name); r != nil {
			return r.apiPath, nil
		}
		return "", nil
	}
	lctx, err := launchProfileScope(ctx)
	if err != nil {
		return "", err
	}
	return pluginAPIPath(namedPlugin(getDashboardPlugins(lctx, false), name))
}

// True when a secondary profile entry may use this process router, only its compiled bundled copy that router runs
func runsInProcess(ctx context.Context, app *FastAPI, plugin *pyjson.Object) (bool, error) {
	if plugin == nil || plugin.GetOr("source", nil) != "bundled" {
		return false, nil
	}
	if _, compiled := compiledDashboard(plugin); !compiled {
		return false, nil
	}
	here, err := inProcessAPIFile(ctx, app, pyrepr.Str(plugin.GetOr("name", nil)))
	if err != nil {
		return false, err
	}
	own, err := pluginAPIPath(plugin)
	if err != nil {
		return false, err
	}
	return here == own, nil
}

// Secondary profile entry of name its plugin host serves, nil in the launch scope or for the compiled copy
func secondaryHostedPlugin(ctx context.Context, app *FastAPI, name string) (*pyjson.Object, error) {
	plugin := secondaryPlugin(ctx, name)
	if plugin == nil {
		return nil, nil
	}
	inProcess, err := runsInProcess(ctx, app, plugin)
	if err != nil || inProcess {
		return nil, err
	}
	return plugin, nil
}

// True when this process router of name must not serve a secondary profile scope
func inProcessRouterRefused(ctx context.Context, app *FastAPI, name string) (bool, error) {
	if _, ok := secondaryProfileHome(ctx); !ok {
		return false, nil
	}
	inProcess, err := runsInProcess(ctx, app, secondaryPlugin(ctx, name))
	return !inProcess, err
}

// Normal token guard decision for a sensitive plugin route, false on its 401 and any other refusal raised
func admittedForSensitiveRoute(req *Request) (bool, error) {
	err := requireToken(req)
	if err == nil {
		return true, nil
	}
	var he *HTTPException
	if errors.As(err, &he) && he.StatusCode == 401 {
		return false, nil
	}
	return false, err
}

// Response of a buffered plugin host asgi answer, every repeated header kept
func hostedResponse(result map[string]any) (*Response, error) {
	body, ok := result["body"].([]byte)
	if !ok {
		return nil, &TypeError{Msg: fmt.Sprintf("plugin host answer body is %T, not bytes", result["body"])}
	}
	status, err := hostedInt(result["status"])
	if err != nil {
		return nil, err
	}
	resp := NewResponse(body, status, "")
	headers, ok := result["headers"].([]any)
	if !ok {
		return nil, &TypeError{Msg: fmt.Sprintf("plugin host answer headers are %T, not a list", result["headers"])}
	}
	for _, h := range headers {
		pair, ok := h.([]any)
		if !ok {
			return nil, &TypeError{Msg: fmt.Sprintf("cannot unpack non-iterable %s object", pyTypeName(h))}
		}
		if len(pair) < 2 {
			return nil, &TypeError{Msg: fmt.Sprintf("not enough values to unpack (expected 2, got %d)", len(pair))}
		}
		if len(pair) > 2 {
			return nil, &TypeError{Msg: "too many values to unpack (expected 2)"}
		}
		key, kok := pair[0].(string)
		value, vok := pair[1].(string)
		if !kok || !vok {
			return nil, &TypeError{Msg: "plugin host answer header is not a pair of strings"}
		}
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		resp.Header().Append(key, value)
	}
	return resp, nil
}

// Integer of a decoded wire status the way int() reads it
func hostedInt(v any) (int, error) {
	switch n := v.(type) {
	case int64:
		return int(n), nil
	case int:
		return n, nil
	case float64:
		return int(n), nil
	case *big.Int:
		return int(n.Int64()), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &TypeError{Msg: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))}
}

// Request slots of the profile at homeDir, its own pool so a slow plugin queues behind its own requests only
func hostedExecutor(homeDir string) chan struct{} {
	key := home.Key(homeDir)
	profilePluginsMu.Lock()
	defer profilePluginsMu.Unlock()
	slots, ok := hostedExecutors[key]
	if !ok {
		slots = make(chan struct{}, hostedRequestThreads)
		hostedExecutors[key] = slots
	}
	return slots
}

// True for the TimeoutError of a host call that got no answer in time
func isHostTimeout(err error) bool {
	var be *plugins.HostBuiltinError
	return errors.As(err, &be) && be.Type == "TimeoutError"
}

// Runs one plugin host asgi round trip on the profile own slots, its answer or 504 after the hosted timeout
func runHostedRequest(ctx context.Context, homeDir, name string, call func(ctx context.Context, timeout time.Duration) (map[string]any, error)) (ASGIResponse, error) {
	timeout := hostedRequestTimeoutSecs
	limit := time.Duration(timeout * float64(time.Second))
	deadline := time.Now().Add(limit)
	slots := hostedExecutor(homeDir)
	type outcome struct {
		result map[string]any
		err    error
	}
	done := make(chan outcome, 1)
	jobCtx := context.WithoutCancel(ctx)
	go func() {
		slots <- struct{}{}
		defer func() { <-slots }()
		r, err := call(jobCtx, max(time.Until(deadline), 10*time.Millisecond))
		done <- outcome{r, err}
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case o := <-done:
		if o.err == nil {
			return hostedResponse(o.result)
		}
		if !isHostTimeout(o.err) {
			return nil, o.err
		}
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	secs, err := pyrepr.Format(timeout, "g")
	if err != nil {
		return nil, err
	}
	webServerLog().Warn(fmt.Sprintf("Plugin %s: its plugin host did not answer within %ss", name, secs))
	return MustJSONResponse(map[string]any{"detail": fmt.Sprintf("plugin %s did not answer within %ss", pyrepr.Repr(name), secs)}, 504), nil
}

// Serves a secondary profile plugin api request through its own plugin host, nil when the in process router answers
func forwardSecondaryPluginRequest(ctx context.Context, req *Request, name string) (ASGIResponse, error) {
	plugin, err := secondaryHostedPlugin(ctx, req.App(), name)
	if err != nil {
		return nil, err
	}
	homeDir, secondary := secondaryProfileHome(ctx)
	if plugin == nil || !secondary {
		return nil, nil
	}
	apiFile, _ := plugin.GetOr("_api_file", nil).(string)
	if !pyTruthy(plugin.GetOr("_api_file", nil)) {
		return MustJSONResponse(map[string]any{"detail": "Not Found"}, 404), nil
	}
	rest := strings.SplitN(getRoutePath(req.Scope), "/", 5)
	path := "/"
	if len(rest) > 4 {
		path += rest[4]
	}
	pairs := [][2]string{}
	for _, kv := range pyurl.ParseQSL(req.URL().Query(), true) {
		if !gatewayQueryKeys[kv[0]] {
			pairs = append(pairs, kv)
		}
	}
	query := pyurl.URLEncode(pairs)
	headers := [][2]string{}
	for _, kv := range req.Headers().Items() {
		if !gatewayCredentialHeaders[strings.ToLower(kv[0])] {
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
	dir := pyrepr.Str(plugin.GetOr("_dir", nil))
	method := req.Method()
	call := func(jctx context.Context, timeout time.Duration) (map[string]any, error) {
		host, err := plugins.GetPluginManager(jctx).HostForPluginDir(filepath.Dir(dir))
		if err != nil {
			return nil, err
		}
		return hostASGIRequest(host, jctx, name, dir, apiFile, method, path, query, headers, body, timeout, authenticated)
	}
	resp, err := runHostedRequest(ctx, homeDir, name, call)
	if err != nil {
		var cancelled *plugins.UserInterrupt
		if errors.As(err, &cancelled) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		webServerLog().Warn(fmt.Sprintf("Plugin %s: its profile's plugin host could not serve %s: %s", name, path, pyExcStr(err)))
		return MustJSONResponse(map[string]any{"detail": fmt.Sprintf("plugin %s could not run in this profile's plugin host", pyrepr.Repr(name))}, 502), nil
	}
	return resp, nil
}

// Answers a secondary profile socket to a plugin its host serves, false when the in process router takes it
func refuseSecondaryPluginSocket(ctx context.Context, scope *Scope, receive Receive, send Send, name string) (bool, error) {
	plugin, err := secondaryHostedPlugin(ctx, scope.App, name)
	if err != nil || plugin == nil {
		return false, err
	}
	socket := NewWebSocket(scope, receive, send)
	admitted := pyTruthy(scope.State["native_http_principal"])
	if !admitted {
		if admitted, err = wsAuthOK(ctx, socket, false, nil); err != nil {
			return true, err
		}
	}
	if !admitted {
		return true, send(ctx, Message{Type: "websocket.close", Code: 1008})
	}
	detail := fmt.Sprintf("plugin %s runs in this profile's plugin host, which does not carry websockets yet", pyrepr.Repr(name))
	homeDir, _ := secondaryProfileHome(ctx)
	webServerLog().Info(fmt.Sprintf("Plugin %s: refused a websocket for %s: %s", name, homeDir, detail))
	if _, ok := scope.Extensions["websocket.http.response"]; ok {
		return true, socket.SendDenialResponse(ctx, MustJSONResponse(map[string]any{"detail": detail}, 501))
	}
	reason := []rune(detail)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	return true, send(ctx, Message{Type: "websocket.close", Code: 1008, Reason: string(reason)})
}
