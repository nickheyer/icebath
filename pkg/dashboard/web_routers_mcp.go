package dashboard

import (
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	mcpDashboardOAuthTTL       = 15 * 60
	maxPendingMCPOAuthFlows    = 8
	mcpAuthorizationURLTimeout = 30 * time.Second
)

// Dashboard MCP OAuth flows by id
var (
	mcpOAuthFlowsMu sync.Mutex
	mcpOAuthFlows   = map[string]*mcp.DashboardOAuthFlow{}
)

var mcpInstallSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Drops flows older than the dashboard OAuth TTL
func gcMCPOAuthFlows() {
	cutoff := time.Now().Add(-mcpDashboardOAuthTTL * time.Second)
	mcpOAuthFlowsMu.Lock()
	defer mcpOAuthFlowsMu.Unlock()
	for id, flow := range mcpOAuthFlows {
		if flow.CreatedAt.Before(cutoff) {
			delete(mcpOAuthFlows, id)
		}
	}
}

// Externally reachable OAuth callback URL of a dashboard flow for the server
func mcpOAuthCallbackURL(ctx context.Context, req *Request, serverName string) string {
	suffix := "/api/mcp/oauth/callback/" + pyQuoteAll(serverName)
	if publicURL := gateway.ResolvePublicURL(ctx, listenerStateOf(req.App().State)); publicURL != "" {
		return publicURL + suffix
	}
	base := req.BaseURL()
	path := PrefixFromRequest(&req.HTTPConnection) + suffix
	empty := ""
	return base.Replace(nil, nil, &path, &empty).String()
}

// Percent encoding of every byte outside the unreserved set, quote with safe empty
func pyQuoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Unique per entry mcp install action name registered with its own log file
func mcpInstallActionName(name string) string {
	slug := strings.Trim(mcpInstallSlugRe.ReplaceAllString(pystr.Lower(name), "-"), "-")
	slug = pystr.Slice(slug, 0, 48)
	if slug == "" {
		slug = "server"
	}
	digest := sha1.Sum([]byte(name))
	action := fmt.Sprintf("mcp-install-%s-%s", slug, hex.EncodeToString(digest[:])[:8])
	actionMu.Lock()
	if _, held := actionLogFiles[action]; !held {
		actionLogFiles[action] = "action-" + action + ".log"
	}
	actionMu.Unlock()
	return action
}

// Config and plugin servers of the home in ctx with their owning plugins
func mcpServersWithSources(ctx context.Context) (map[string]map[string]any, map[string]*string, error) {
	configServers, err := mcp.GetMCPServers(ctx)
	if err != nil {
		return nil, nil, err
	}
	servers, plugins := mcp.ServerConfigsWithSources(ctx, configServers)
	return servers, plugins, nil
}

// 409 for a server a plugin provides
func mcpPluginOwned(name string, plugins map[string]*string) error {
	if plugin := plugins[name]; plugin != nil && *plugin != "" {
		return NewHTTPException(409, fmt.Sprintf("Server '%s' is provided by plugin '%s' and cannot be modified", name, *plugin))
	}
	return nil
}

// Flow status fields in their wire order
func mcpFlowSnapshot(flow *mcp.DashboardOAuthFlow) *pyjson.Object {
	snap := flow.Snapshot()
	return pyjson.ObjectOf("flow_id", snap["flow_id"], "server_name", snap["server_name"], "status", snap["status"],
		"authorization_url", snap["authorization_url"], "error", snap["error"])
}

// Resolved home of ctx as the flow records it
func mcpFlowHome(ctx context.Context) string {
	expanded := pypath.Expanduser(home.Dir(ctx))
	if resolved, err := pypath.Resolve(expanded); err == nil {
		return resolved
	}
	return expanded
}

// Catalog entry as the dashboard catalog row with its install and enable state
func catalogEntryJSON(entry *mcp.CatalogEntry, installed, enabled bool) *pyjson.Object {
	requiredEnv := []any{}
	for _, e := range entry.Auth.Env {
		requiredEnv = append(requiredEnv, pyjson.ObjectOf("name", e.Name, "prompt", e.Prompt, "required", e.Required))
	}
	var installURL, installRef any
	bootstrap := []any{}
	if entry.Install != nil {
		installURL, installRef = entry.Install.URL, entry.Install.Ref
		for _, b := range entry.Install.Bootstrap {
			bootstrap = append(bootstrap, b)
		}
	}
	var defaultEnabled any
	if entry.Tools.DefaultEnabled != nil {
		defaultEnabled = stringsAny(entry.Tools.DefaultEnabled)
	}
	var suggest any
	if entry.Suggest != nil {
		suggest = pyjson.ObjectOf(
			"keywords", stringsAny(entry.Suggest.Keywords),
			"hosts", stringsAny(entry.Suggest.Hosts),
			"applications", stringsAny(entry.Suggest.Applications),
			"examples", stringsAny(entry.Suggest.Examples),
			"requires_app", entry.Suggest.RequiresApp,
		)
	}
	authType := entry.Auth.Type
	if authType == "" {
		authType = "none"
	}
	return pyjson.ObjectOf(
		"name", entry.Name,
		"description", entry.Description,
		"connector_slug", entry.ConnectorSlug,
		"source", entry.Source,
		"transport", entry.Transport.Type,
		"auth_type", authType,
		"required_env", requiredEnv,
		"command", entry.Transport.Command,
		"args", stringsAny(entry.Transport.Args),
		"url", entry.Transport.URL,
		"install_url", installURL,
		"install_ref", installRef,
		"bootstrap", bootstrap,
		"default_enabled", defaultEnabled,
		"post_install", entry.PostInstall,
		"suggest", suggest,
		"needs_install", entry.Install != nil,
		"installed", installed,
		"enabled", enabled,
	)
}

// Starts a dashboard MCP OAuth flow and returns its snapshot once the authorization URL is known
func authMCPServer(ctx context.Context, c *Call) (any, error) {
	name := c.Str("name")
	profile := profileQuery(c)
	if err := requireToken(c.Request); err != nil {
		return nil, err
	}
	gcMCPOAuthFlows()
	processHome := mcpFlowHome(ctx)
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	servers, plugins, err := mcpServersWithSources(sctx)
	if err != nil {
		return nil, err
	}
	flowHome := mcpFlowHome(sctx)
	server, ok := servers[name]
	if !ok {
		return nil, NewHTTPException(404, fmt.Sprintf("Server '%s' not found", name))
	}
	if err := mcpPluginOwned(name, plugins); err != nil {
		return nil, err
	}
	cfg := make(map[string]any, len(server)+1)
	for k, v := range server {
		cfg[k] = v
	}
	if !pyrepr.Truthy(cfg["url"]) {
		return nil, NewHTTPException(400, "stdio servers authenticate via env keys, not OAuth")
	}
	if pyrepr.Truthy(cfg["headers"]) && cfg["auth"] != "oauth" {
		return nil, NewHTTPException(400, "This server uses header/API-key auth, not OAuth")
	}
	cfg["auth"] = "oauth"
	redirect := ""
	if oauthCfg, ok := cfg["oauth"].(map[string]any); ok {
		if r, ok := oauthCfg["redirect_uri"].(string); ok {
			redirect = r
		}
	}
	if redirect == "" {
		redirect = mcpOAuthCallbackURL(ctx, c.Request, name)
	}
	flowID := tokenURLSafe(24)
	flow := mcp.NewDashboardOAuthFlow(flowID, name, profile, flowHome, redirect, flowHome == processHome)
	mcpOAuthFlowsMu.Lock()
	var live []*mcp.DashboardOAuthFlow
	for _, f := range mcpOAuthFlows {
		if !f.WorkerDone() {
			live = append(live, f)
		}
	}
	if len(live) >= maxPendingMCPOAuthFlows {
		mcpOAuthFlowsMu.Unlock()
		return nil, NewHTTPException(429, "Too many MCP OAuth flows are already in progress")
	}
	for _, f := range live {
		if f.ServerName == name && f.HomeDir == flowHome {
			mcpOAuthFlowsMu.Unlock()
			return nil, NewHTTPException(409, fmt.Sprintf("MCP OAuth for '%s' is already in progress", name))
		}
	}
	mcpOAuthFlows[flowID] = flow
	mcpOAuthFlowsMu.Unlock()
	workerCtx := context.WithoutCancel(ctx)
	go mcp.RunDashboardMCPOAuth(workerCtx, &tools.Env{Home: flowHome, TerminalScope: tools.GetTerminalScope(workerCtx)}, flow, cfg)
	if _, err := flow.WaitForAuthorizationURL(ctx, mcpAuthorizationURLTimeout); err != nil {
		flow.MarkError(mcp.ExceptionMessage(err), false)
	}
	return mcpFlowSnapshot(flow), nil
}

// Answers the OAuth provider redirect of the flow whose state matches
func mcpOAuthCallback(ctx context.Context, c *Call) (any, error) {
	serverName := c.Str("server_name")
	gcMCPOAuthFlows()
	code, _ := c.Value("code").(string)
	state, hasState := c.Value("state").(string)
	errText, _ := c.Value("error").(string)
	iss, _ := c.Value("iss").(string)
	mcpOAuthFlowsMu.Lock()
	var candidates []*mcp.DashboardOAuthFlow
	for _, f := range mcpOAuthFlows {
		if f.ServerName == serverName && f.Status() == "authorization_required" {
			candidates = append(candidates, f)
		}
	}
	mcpOAuthFlowsMu.Unlock()
	var flow *mcp.DashboardOAuthFlow
	for _, f := range candidates {
		expected := f.ExpectedState()
		if expected != "" && hasState && subtle.ConstantTimeCompare([]byte(expected), []byte(state)) == 1 {
			flow = f
			break
		}
	}
	if flow == nil {
		return HTMLResponse("<h1>OAuth flow expired</h1><p>Return to Hermes and try again.</p>", 404), nil
	}
	if err := flow.DeliverCallback(code, state, errText, iss); err != nil {
		status := 400
		if strings.Contains(err.Error(), "already received") {
			status = 409
		}
		return HTMLResponse("<h1>OAuth callback rejected</h1><p>The callback was invalid or already used.</p>", status), nil
	}
	if errText != "" {
		return HTMLResponse("<h1>Authorization failed</h1><p>Return to Hermes for details.</p>", 400), nil
	}
	return HTMLResponse("<h1>Authorization received</h1><p>You can close this tab and return to Hermes.</p>", 200), nil
}

// Installs a catalog MCP, secrets to .env first and git bootstrap entries as a background action
func installMCPCatalogEntry(ctx context.Context, c *Call) (any, error) {
	body := c.Object("body")
	name := pystr.Strip(optText(body.GetOr("name", nil)))
	entry, err := mcp.GetEntry(name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, NewHTTPException(404, fmt.Sprintf("No catalog entry '%s'", name))
	}
	env, _ := body.GetOr("env", nil).(*pyjson.Object)
	if env == nil {
		env = pyjson.NewObject()
	}
	declared := map[string]bool{}
	for _, spec := range entry.Auth.Env {
		declared[spec.Name] = true
	}
	var undeclared []string
	for _, k := range env.Keys {
		if !declared[k] {
			undeclared = append(undeclared, k)
		}
	}
	if len(undeclared) > 0 {
		slices.Sort(undeclared)
		return nil, NewHTTPException(400, fmt.Sprintf("Catalog entry '%s' does not declare environment variable(s): %s", name, strings.Join(undeclared, ", ")))
	}
	for _, k := range env.Keys {
		if err := config.ValidateEnvNameForWrite(k); err != nil {
			return nil, NewHTTPException(400, err.Error())
		}
	}
	effectiveProfile := bodyOrQueryProfile(body, c)
	if env.Len() > 0 {
		if _, err := scopedToThread(ctx, effectiveProfile, func(sctx context.Context) (any, error) {
			for _, spec := range entry.Auth.Env {
				value, _ := env.GetOr(spec.Name, nil).(string)
				if spec.Secret && value != "" {
					if err := config.SaveEnvValue(home.Dir(sctx), spec.Name, value); err != nil {
						return nil, err
					}
				}
			}
			return nil, nil
		}); err != nil {
			return nil, err
		}
	}
	if entry.Install != nil {
		action := mcpInstallActionName(name)
		args, err := profileCLIArgs(effectiveProfile)
		if err == nil {
			_, err = spawnHermesAction(ctx, append(args, "mcp", "install", name), action, nil)
		}
		if err != nil {
			if _, ok := asHTTPException(err); ok {
				return nil, err
			}
			return nil, NewHTTPException(500, fmt.Sprintf("Install failed: %s", pyrepr.ExceptionMessage(err)))
		}
		return pyjson.ObjectOf("ok", true, "name", name, "background", true, "action", action), nil
	}
	var preloaded map[string]string
	if env.Len() > 0 {
		preloaded = map[string]string{}
		for _, k := range env.Keys {
			preloaded[k], _ = env.Vals[k].(string)
		}
	}
	enable := body.GetOr("enable", true) == true
	if _, err := scopedToThread(ctx, effectiveProfile, func(sctx context.Context) (any, error) {
		return nil, mcp.InstallEntry(sctx, &tools.Env{Home: home.Dir(sctx), TerminalScope: tools.GetTerminalScope(sctx)}, mcp.HeadlessUI(), entry, enable, preloaded)
	}); err != nil {
		if _, ok := asHTTPException(err); ok {
			return nil, err
		}
		webServerLog().Error("install_mcp_catalog_entry failed", "exc", err)
		return nil, NewHTTPException(400, pyrepr.ExceptionMessage(err))
	}
	return pyjson.ObjectOf("ok", true, "name", name, "background", false), nil
}

// Router of the MCP server, OAuth flow and catalog routes
func mcpRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	namePath := PathParam("name", Str)
	r.Get("/api/mcp/servers", &Endpoint{
		Name:   "list_mcp_servers",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			sctx, _, err := configProfileScope(ctx, profileQuery(c))
			if err != nil {
				return nil, err
			}
			servers, plugins, err := mcpServersWithSources(sctx)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(servers))
			for n := range servers {
				names = append(names, n)
			}
			slices.Sort(names)
			rows := make([]any, 0, len(names))
			for _, n := range names {
				rows = append(rows, mcpServerSummary(n, servers[n], plugins[n]))
			}
			return pyjson.ObjectOf("servers", rows), nil
		},
	})
	r.Post("/api/mcp/servers", &Endpoint{
		Name:   "add_mcp_server",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", mcpServerCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			name, serverConfig, bearerToken, err := normalizeMCPServerCreate(body)
			if err != nil {
				return nil, NewHTTPException(400, err.Error())
			}
			profile := bodyOrQueryProfile(body, c)
			err = func() error {
				saved, err := func() (bool, error) {
					wctx, release, err := configWriteScope(ctx, profile)
					if err != nil {
						return false, err
					}
					defer release()
					servers, plugins, err := mcpServersWithSources(wctx)
					if err != nil {
						return false, err
					}
					if err := mcpPluginOwned(name, plugins); err != nil {
						return false, err
					}
					if _, exists := servers[name]; exists {
						return false, NewHTTPException(409, fmt.Sprintf("Server '%s' already exists", name))
					}
					if bearerToken != nil {
						headers, err := mcp.SaveBearerAuthToken(wctx, name, *bearerToken)
						if err != nil {
							return false, err
						}
						serverConfig["headers"] = headers
					}
					return mcp.SaveServer(wctx, mcp.HeadlessUI(), name, serverConfig)
				}()
				if err != nil {
					return err
				}
				source := "local"
				if pyrepr.Truthy(serverConfig["url"]) {
					source = "url"
				}
				outcome, failureClass := "success", ""
				if !saved {
					outcome, failureClass = "failed", "config_rejected"
				}
				if _, err := scopedToThread(ctx, profile, func(sctx context.Context) (any, error) {
					mcp.RecordMCPInstall(sctx, source, "", outcome, failureClass, nil)
					return nil, nil
				}); err != nil {
					return err
				}
				if !saved {
					return NewHTTPException(400, fmt.Sprintf("Server '%s' rejected: suspicious command/args configuration", name))
				}
				return nil
			}()
			if err != nil {
				if _, ok := asHTTPException(err); ok {
					return nil, err
				}
				webServerLog().Error("POST /api/mcp/servers failed", "exc", err)
				return nil, NewHTTPException(400, pyrepr.ExceptionMessage(err))
			}
			return mcpServerSummary(name, serverConfig, nil), nil
		},
	})
	r.Put("/api/mcp/servers", &Endpoint{
		Name:   "replace_mcp_servers",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", mcpServersReplaceModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			wctx, release, err := configWriteScope(ctx, bodyOrQueryProfile(body, c))
			if err != nil {
				return nil, err
			}
			ok, issues, err := func() (bool, []string, error) {
				defer release()
				_, plugins := mcp.ServerConfigsWithSources(wctx, map[string]any{})
				incoming, _ := body.GetOr("servers", nil).(*pyjson.Object)
				replacement := map[string]any{}
				if incoming != nil {
					for _, n := range incoming.Keys {
						if plugin := plugins[n]; plugin != nil && *plugin != "" {
							return false, []string{fmt.Sprintf("Server '%s' is provided by plugin '%s' and cannot be modified", n, *plugin)}, nil
						}
						replacement[n] = configMapOf(incoming.Vals[n])
					}
				}
				return mcp.ReplaceServers(wctx, replacement)
			}()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, NewHTTPException(400, strings.Join(issues, "; "))
			}
			return pyjson.ObjectOf("ok", true), nil
		},
	})
	r.Delete("/api/mcp/servers/{name}", &Endpoint{
		Name:   "remove_mcp_server",
		Params: []Param{namePath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			wctx, release, err := configWriteScope(ctx, profileQuery(c))
			if err != nil {
				return nil, err
			}
			removed, err := func() (bool, error) {
				defer release()
				_, plugins, err := mcpServersWithSources(wctx)
				if err != nil {
					return false, err
				}
				if err := mcpPluginOwned(name, plugins); err != nil {
					return false, err
				}
				return mcp.RemoveServer(wctx, name)
			}()
			if err != nil {
				return nil, err
			}
			if !removed {
				return nil, NewHTTPException(404, fmt.Sprintf("Server '%s' not found", name))
			}
			return pyjson.ObjectOf("ok", true), nil
		},
	})
	r.Post("/api/mcp/servers/{name}/test", &Endpoint{
		Name:   "test_mcp_server",
		Params: []Param{namePath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			sctx, _, err := configProfileScope(ctx, profileQuery(c))
			if err != nil {
				return nil, err
			}
			servers, _, err := mcpServersWithSources(sctx)
			if err != nil {
				return nil, err
			}
			cfg, ok := servers[name]
			if !ok {
				return nil, NewHTTPException(404, fmt.Sprintf("Server '%s' not found", name))
			}
			details := &mcp.ProbeDetails{}
			summaries, err := mcp.ProbeSingleServerDetails(sctx, &tools.Env{Home: home.Dir(sctx), TerminalScope: tools.GetTerminalScope(sctx)}, name, cfg, details)
			if err != nil {
				return pyjson.ObjectOf("ok", false, "error", mcp.RedactProbeText(pyrepr.ExceptionMessage(err)), "tools", []any{}), nil
			}
			if cfg["auth"] == "oauth" && !mcp.OAuthTokensPresent(sctx, name) {
				return pyjson.ObjectOf("ok", false, "error", "OAuth authentication required — no token found.", "tools", []any{}), nil
			}
			rows := make([]any, 0, len(summaries))
			for _, s := range summaries {
				row := pyjson.ObjectOf("name", s.Name, "description", s.Description)
				if n, ok := details.SchemaChars[s.Name]; ok {
					row.Set("schema_chars", int64(n))
				}
				rows = append(rows, row)
			}
			prompts, resources := int64(0), int64(0)
			if details.Prompts != nil {
				prompts = int64(*details.Prompts)
			}
			if details.Resources != nil {
				resources = int64(*details.Resources)
			}
			return pyjson.ObjectOf("ok", true, "tools", rows, "prompts", prompts, "resources", resources), nil
		},
	})
	r.Post("/api/mcp/servers/{name}/auth", &Endpoint{Name: "auth_mcp_server", Params: []Param{namePath, optProfile}, Handle: authMCPServer})
	r.Get("/api/mcp/oauth/flows/{flow_id}", &Endpoint{
		Name:   "mcp_oauth_flow_status",
		Params: []Param{PathParam("flow_id", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			gcMCPOAuthFlows()
			mcpOAuthFlowsMu.Lock()
			flow := mcpOAuthFlows[c.Str("flow_id")]
			mcpOAuthFlowsMu.Unlock()
			if flow == nil {
				return nil, NewHTTPException(404, "OAuth flow not found or expired")
			}
			snapshot := mcpFlowSnapshot(flow)
			var toolRows any
			if t := flow.ToolsSnapshot(); t != nil {
				rows := make([]any, len(t))
				for i, row := range t {
					rows[i] = pyjson.ObjectOf("name", row["name"], "description", row["description"])
				}
				toolRows = rows
			}
			snapshot.Set("tools", toolRows)
			return snapshot, nil
		},
	})
	r.Delete("/api/mcp/oauth/flows/{flow_id}", &Endpoint{
		Name:   "cancel_mcp_oauth_flow",
		Params: []Param{PathParam("flow_id", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			mcpOAuthFlowsMu.Lock()
			flow := mcpOAuthFlows[c.Str("flow_id")]
			mcpOAuthFlowsMu.Unlock()
			if flow == nil {
				return pyjson.ObjectOf("ok", true, "status", "expired"), nil
			}
			flow.MarkError("Cancelled by user", true)
			return pyjson.ObjectOf("ok", true, "status", flow.Snapshot()["status"]), nil
		},
	})
	r.Get("/api/mcp/oauth/callback/{server_name:path}", &Endpoint{
		Name: "mcp_oauth_callback",
		Params: []Param{PathParam("server_name", Str), QueryParam("code", Optional(Str), nil), QueryParam("state", Optional(Str), nil),
			QueryParam("error", Optional(Str), nil), QueryParam("iss", Optional(Str), nil)},
		Handle: mcpOAuthCallback,
	})
	r.Put("/api/mcp/servers/{name}/enabled", &Endpoint{
		Name:   "set_mcp_server_enabled",
		Params: []Param{namePath, optProfile},
		Body:   []BodyParam{Body("body", mcpEnabledToggleModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := c.Str("name")
			body := c.Object("body")
			enabled := pyrepr.Truthy(body.GetOr("enabled", false))
			wctx, release, err := configWriteScope(ctx, bodyOrQueryProfile(body, c))
			if err != nil {
				return nil, err
			}
			defer release()
			cfg, err := webLoadConfig(wctx)
			if err != nil {
				return nil, err
			}
			rawServers, _ := cfg["mcp_servers"].(map[string]any)
			if rawServers == nil {
				rawServers = map[string]any{}
			}
			_, plugins := mcp.ServerConfigsWithSources(wctx, rawServers)
			if err := mcpPluginOwned(name, plugins); err != nil {
				return nil, err
			}
			servers, isMap := cfg["mcp_servers"].(map[string]any)
			entry, present := servers[name]
			if !isMap || !present {
				return nil, NewHTTPException(404, fmt.Sprintf("Server '%s' not found", name))
			}
			entryMap, ok := entry.(map[string]any)
			if !ok {
				return nil, NewHTTPException(400, "Malformed server config")
			}
			entryMap["enabled"] = enabled
			if err := webSaveConfig(wctx, cfg, config.SaveOptions{}); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "name", name, "enabled", enabled), nil
		},
	})
	r.Get("/api/mcp/catalog", &Endpoint{
		Name:   "list_mcp_catalog",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			entries := []any{}
			_, err := scopedToThread(ctx, profileQuery(c), func(sctx context.Context) (any, error) {
				catalog, err := mcp.ListCatalog()
				if err != nil {
					return nil, err
				}
				for _, entry := range catalog {
					installed, err := mcp.IsInstalled(sctx, entry.Name)
					if err != nil {
						return nil, err
					}
					enabled, err := mcp.IsEnabled(sctx, entry.Name)
					if err != nil {
						return nil, err
					}
					entries = append(entries, catalogEntryJSON(entry, installed, enabled))
				}
				return nil, nil
			})
			if err != nil {
				if _, ok := asHTTPException(err); ok {
					return nil, err
				}
				webServerLog().Error("list_mcp_catalog failed", "exc", err)
				entries = []any{}
			}
			diagnostics := []any{}
			for _, d := range mcp.CatalogDiagnostics() {
				diagnostics = append(diagnostics, pyjson.ObjectOf("name", d.Name, "kind", d.Kind, "message", d.Message))
			}
			return pyjson.ObjectOf("entries", entries, "diagnostics", diagnostics), nil
		},
	})
	r.Post("/api/mcp/catalog/install", &Endpoint{
		Name:   "install_mcp_catalog_entry",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", mcpCatalogInstallModel)},
		Handle: installMCPCatalogEntry,
	})
	return r
}
