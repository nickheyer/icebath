package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Bind host as text for logs
func (a *APIServerAdapter) hostText() string { return pyrepr.Str(a.host) }

// Bind host as the listener's optional host
func (a *APIServerAdapter) hostPtr() *string {
	if a.host == nil {
		return nil
	}
	h := pyrepr.Str(a.host)
	return &h
}

// Whether API_SERVER_KEY is present and strong enough to start the listener
func (a *APIServerAdapter) apiKeyPassesStartupGuard() bool {
	if a.apiKey == "" {
		apiLog().Error(fmt.Sprintf("[%s] Refusing to start: API_SERVER_KEY is required for the API server, "+
			"including loopback-only binds on %s.", a.Name(), a.hostText()))
		return false
	}
	if !provider.HasUsableSecret(a.apiKey, 16) {
		apiLog().Error(fmt.Sprintf("[%s] Refusing to start: API_SERVER_KEY is a "+
			"placeholder or too short (<16 chars). This endpoint "+
			"dispatches terminal-capable agent work — a guessable "+
			"key is remote code execution. Generate a strong secret "+
			"(e.g. `openssl rand -hex 32`) and set API_SERVER_KEY "+
			"before starting the API server on %s.", a.Name(), a.hostText()))
		return false
	}
	return true
}

// Application with the guard, profile prefix, CORS, body limit and security middlewares and every route mirrored under the profile prefix
func (a *APIServerAdapter) buildApp() (*gateway.WebApplication, error) {
	app := gateway.NewWebApplication(gateway.APIMaxRequestBytes)
	app.Middlewares = []gateway.WebMiddleware{gateway.APIConfigUnavailableMiddleware, requestStateMiddleware, a.profilePrefixMiddleware,
		corsMiddlewareFor(app), bodyLimitMiddleware, securityHeadersMiddleware}
	for _, route := range a.httpRouteTable() {
		if err := app.AddRoute(route.method, route.path, route.handler); err != nil {
			return nil, err
		}
		if err := app.AddRoute(route.method, "/p/{profile}"+route.path, route.handler); err != nil {
			return nil, err
		}
	}
	if err := app.AddRoute("*", "/p/{profile}/{tail:.*}", a.handleProfileIngress); err != nil {
		return nil, err
	}
	app.State["api_server_adapter"] = a
	if a.GatewayRunner != nil {
		app.State["gateway_runner"] = a.GatewayRunner
	}
	return app, nil
}

// Starts the HTTP listener, a rejected key or a held port being a non retryable fatal error
func (a *APIServerAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.sessionDBCacheMu.Lock()
	a.sessionDBCacheClosed = false
	a.sessionDBCacheMu.Unlock()
	if !a.apiKeyPassesStartupGuard() {
		a.SetFatalError(ctx, "api_server_key_invalid",
			"API_SERVER_KEY was rejected by the startup guard (missing, "+
				"placeholder/too short, or strength unverifiable — see the "+
				"error logged above). Generate a strong secret (e.g. "+
				"`openssl rand -hex 32`), set API_SERVER_KEY, then "+
				"`/platform resume api_server`.", false)
		return false, nil
	}
	ok, err := func() (ok bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return a.connectListener(ctx)
	}()
	if err != nil {
		apiLog().Error(fmt.Sprintf("[%s] Failed to start API server: %v", a.Name(), err))
		return false, nil
	}
	return ok, nil
}

// Builds the app, binds with retries while a predecessor releases the port and starts the heartbeat
func (a *APIServerAdapter) connectListener(ctx context.Context) (bool, error) {
	app, err := a.buildApp()
	if err != nil {
		return false, err
	}
	a.app = app
	runCtx := context.WithoutCancel(ctx)
	a.trackBackgroundTask(startAgentTask(runCtx, a.sweepOrphanedRuns))
	if gateway.IsNetworkAccessible(ctx, a.hostText()) {
		backend := "local"
		func() {
			defer func() { _ = recover() }()
			cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
			if err != nil {
				return
			}
			if terminal, ok := cfg["terminal"].(map[string]any); ok {
				if b, ok := terminal["backend"]; ok {
					backend = pyrepr.Str(b)
				}
			}
		}()
		if strings.ToLower(backend) == "local" {
			apiLog().Warn(fmt.Sprintf("[%s] API server is network-accessible (%s) AND the "+
				"terminal backend is 'local' (unsandboxed). Agent work "+
				"dispatched through this endpoint runs as the host user "+
				"with full terminal/file access. Strongly consider a "+
				"sandboxed backend (terminal.backend: docker) and "+
				"firewalling this port to trusted networks only.", a.Name(), a.hostText()))
		}
	}
	a.WirePluginHandlers(ctx, app)
	var bindErr error
	for attempt := 0; attempt < bindAttempts; attempt++ {
		runner, err := gateway.StartTCPSite(ctx, app, a.hostPtr(), a.port, a.Name(), gateway.AccessLog{})
		if err == nil {
			a.runner = runner
			bindErr = nil
			break
		}
		bindErr = err
		if !errors.Is(err, syscall.EADDRINUSE) || attempt == bindAttempts-1 {
			break
		}
		time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
	}
	if bindErr != nil {
		a.runner = nil
		if errors.Is(bindErr, syscall.EADDRINUSE) {
			a.SetFatalError(ctx, "api_server_port_in_use", fmt.Sprintf("Port %d already in use. Set "+
				"platforms.api_server.port in config.yaml to a "+
				"different value, then `/platform resume api_server`.", a.port), false)
		}
		apiLog().Error(fmt.Sprintf("[%s] Could not bind %s:%d: %v. Set a different port in "+
			"config.yaml: platforms.api_server.port", a.Name(), a.hostText(), a.port, bindErr))
		return false, nil
	}
	a.MarkConnected(ctx, gateway.ListenerBaseURL(a.hostPtr(), a.port))
	a.publishRuntimeStatus(ctx)
	a.trackBackgroundTask(startAgentTask(runCtx, a.heartbeatLoop))
	apiLog().Info(fmt.Sprintf("[%s] API server listening on http://%s:%d (model: %s)", a.Name(), a.hostText(), a.port, a.modelName))
	return true, nil
}

// Stops the listener and releases every owned store, memory manager and session handle
func (a *APIServerAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.responseStoreMu.Lock()
	stores := []*gateway.ResponseStore{}
	if a.responseStore != nil {
		stores = append(stores, a.responseStore)
	}
	for _, s := range a.responseStores {
		if s != nil {
			stores = append(stores, s)
		}
	}
	a.responseStores = map[string]*gateway.ResponseStore{}
	a.responseStoreMu.Unlock()
	for _, store := range stores {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					apiLog().Debug(fmt.Sprintf("Failed to close response store for %s", a.Name()), "error", rec)
				}
			}()
			store.Close(ctx)
		}()
	}
	func() {
		defer func() { _ = recover() }()
		a.memorySessions.CloseAll(ctx)
	}()
	var err error
	if a.runner != nil {
		err = a.runner.Cleanup(ctx)
		a.runner = nil
	}
	a.closeRunState(ctx)
	a.closeCachedSessionDBs()
	a.app = nil
	if err != nil {
		return err
	}
	apiLog().Info(fmt.Sprintf("[%s] API server stopped", a.Name()))
	return nil
}

// Delivery is the HTTP request and response cycle itself, never send
func (a *APIServerAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false, Error: "API server uses HTTP request/response, not send()"}, nil
}

// Clarify question routed to the authority run that asked it
func (a *APIServerAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	return sendClarify(ctx, a, chatID, clarifyID)
}

// Basic info about the API server
func (a *APIServerAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "API Server", "type": "api", "host": a.host, "port": int64(a.port)}, nil
}

// Bind address, key and model name of this listener for a wake self post
func (a *APIServerAdapter) SelfPostTarget() (string, int, string, string) {
	host := ""
	if pyrepr.Truthy(a.host) {
		host = a.hostText()
	}
	return host, a.port, a.apiKey, a.modelName
}
