package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Curator switches, schedule and last run of a profile, each read falling back to its default when it fails
func getCuratorStatus(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		state, err := memory.LoadCuratorState(sctx)
		if err != nil {
			state = pyjson.NewObject()
		}
		paused, err := memory.CuratorIsPaused(sctx)
		if err != nil {
			paused = false
		}
		return pyjson.ObjectOf(
			"enabled", memory.CuratorIsEnabled(sctx),
			"paused", paused,
			"interval_hours", int64(memory.CuratorIntervalHours(sctx)),
			"last_run_at", state.GetOr("last_run_at", nil),
			"min_idle_hours", memory.CuratorMinIdleHours(sctx),
			"stale_after_days", int64(memory.CuratorStaleAfterDays(sctx)),
			"archive_after_days", int64(memory.CuratorArchiveAfterDays(sctx)),
		), nil
	})
}

// Pauses or resumes the curator of the profile the request resolves to
func setCuratorPaused(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	paused := pyrepr.Truthy(body.GetOr("paused", nil))
	_, err := configScopedToThread(ctx, profile, func(sctx context.Context) (bool, error) {
		return true, memory.SetCuratorPaused(sctx, paused)
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "paused", paused), nil
}

// Spawns a background CLI action under a profile, any failure answered 500 with the prefix and the error text
func statusSpawnAction(ctx context.Context, argv []string, name, prefix, profile string) (*pyjson.Object, error) {
	proc, err := func() (actionProc, error) {
		args, err := profileCLIArgs(profile)
		if err != nil {
			return nil, err
		}
		return spawnHermesAction(ctx, append(args, argv...), name, nil)
	}()
	if err != nil {
		return nil, NewHTTPException(500, prefix+": "+pyExcStr(err))
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", name), nil
}

// Starts a curator review of the named profile in the background, refused unnamed while several profiles are served
func runCurator(ctx context.Context, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "POST /api/curator/run")
	if err != nil {
		return nil, err
	}
	return statusSpawnAction(ctx, []string{"curator", "run"}, "curator-run", "Failed to run curator", profile)
}

// Tool env of the home and terminal scope a request context resolves to
func statusToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Learning graph of the learned skills and memory chunks of a profile, 500 when the build fails
func getLearningGraph(ctx context.Context, profile string) (any, error) {
	graph, err := scopedToThread(ctx, profile, func(sctx context.Context) (map[string]any, error) {
		return memory.BuildLearningGraph(sctx, statusToolsEnv(sctx))
	})
	if err != nil {
		if h, ok := asHTTPException(err); ok {
			return nil, h
		}
		routersLog().Error("GET /api/learning/graph failed", "exc_info", err)
		return nil, NewHTTPException(500, "Failed to build learning graph")
	}
	return graph, nil
}

// Result of a learning mutation run inside the profile scope, a result that is not ok answered with status
func learningMutation(ctx context.Context, profile string, fn func(ctx context.Context, env *tools.Env) (map[string]any, error), status int, fallback string) (any, error) {
	res, err := scopedToThread(ctx, profile, func(sctx context.Context) (map[string]any, error) {
		return fn(sctx, statusToolsEnv(sctx))
	})
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(res["ok"]) {
		var detail any = fallback
		if message, present := res["message"]; present {
			detail = message
		}
		return nil, NewHTTPException(status, detail)
	}
	return res, nil
}

// Display state of one subscription feature
func featureState(feat provider.NousFeatureState) string {
	if feat.ManagedByNous {
		return "via Nous Portal"
	}
	if feat.Active {
		if feat.CurrentProvider != "" {
			return feat.CurrentProvider
		}
		return "active"
	}
	return "not configured"
}

// Nous Portal login state, endpoints, tier and feature routing of a profile without any token refresh
func getPortalStatus(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		env := bootstrapPoolEnv(sctx)
		auth := provider.GetNousAuthStatusLocal(env)
		if auth == nil {
			auth = map[string]any{}
		}
		features := []any{}
		for _, feat := range provider.GetNousSubscriptionFeatures(env, cfg, false).Items() {
			features = append(features, pyjson.ObjectOf("label", feat.Label, "state", featureState(feat)))
		}
		modelCfg, _ := cfg["model"].(map[string]any)
		providerName := ""
		if v := modelCfg["provider"]; pyrepr.Truthy(v) {
			providerName = pyrepr.Str(v)
		}
		return pyjson.ObjectOf(
			"logged_in", pyrepr.Truthy(auth["logged_in"]), "portal_url", auth["portal_base_url"],
			"inference_url", auth["inference_base_url"],
			"provider", providerName,
			"free_tier", pyrepr.Truthy(auth["free_tier"]), "account_tier", auth["account_tier"],
			"subscription_url", "https://portal.nousresearch.com/manage-subscription",
			"features", features), nil
	})
}

// Uploads a debug report with the logs of a profile and answers the paste URLs, 502 when the upload fails
func runDebugShare(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	lines, redact := int64(200), true
	if body != nil {
		lines, _ = body.GetOr("lines", int64(200)).(int64)
		redact = pyrepr.Truthy(body.GetOr("redact", true))
	}
	result, err := configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		return StatusCLI.BuildDebugShare(sctx, int(max(1, min(lines, 5000))), redact)
	})
	if err != nil {
		if h, ok := asHTTPException(err); ok {
			return nil, h
		}
		var pe *pm.PyError
		if errors.As(err, &pe) && pe.Class == "RuntimeError" {
			return nil, NewHTTPException(502, "Upload failed: "+StatusCLI.RedactDebugSupportText(err))
		}
		routersLog().Error("debug share failed", "exc_info", err)
		return nil, NewHTTPException(500, "Failed: "+StatusCLI.RedactDebugSupportText(err))
	}
	out := pyjson.ObjectOf("ok", true)
	for _, key := range []string{"urls", "failures", "redacted", "auto_delete_seconds"} {
		out.Set(key, result.GetOr(key, nil))
	}
	return out, nil
}

// Python slice of the last n items, the whole list for zero and the head dropped for a negative n
func lastItems(items []string, n int64) []string {
	size := int64(len(items))
	start := -n
	if start < 0 {
		start = max(size+start, 0)
	} else {
		start = min(start, size)
	}
	return items[start:]
}

// Tail of a known log file of a profile filtered by level, component and a case blind search text
func getLogs(ctx context.Context, file string, lines int64, level, component, search, profile string) (*pyjson.Object, error) {
	logName, ok := logging.LogFiles[file]
	if !ok || logName == "" {
		return nil, NewHTTPException(400, "Unknown log file: "+file)
	}
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	logPath := pypath.PathJoin(home.Dir(sctx), "logs", logName)
	if _, err := os.Stat(logPath); err != nil {
		return pyjson.ObjectOf("file", file, "lines", []any{}), nil
	}
	var minLevel *string
	if level != "" && pystr.Upper(level) != "ALL" {
		minLevel = &level
	}
	var prefixes []string
	hasComponent := false
	if component != "" && pystr.Lower(component) != "all" {
		if prefixes, hasComponent = logging.ComponentPrefixes[component]; !hasComponent {
			names := make([]string, 0, len(logging.ComponentPrefixes))
			for name := range logging.ComponentPrefixes {
				names = append(names, name)
			}
			slices.Sort(names)
			return nil, NewHTTPException(400, fmt.Sprintf("Unknown component: %s. Available: %s", component, strings.Join(names, ", ")))
		}
	}
	count := min(lines, 500)
	if search != "" {
		count = 2000
	}
	result, err := StatusCLI.ReadLogTail(logPath, int(count), minLevel != nil || len(prefixes) > 0 || search != "", minLevel, prefixes, hasComponent)
	if err != nil {
		return nil, err
	}
	if search != "" {
		needle := pystr.Lower(search)
		matched := make([]string, 0, len(result))
		for _, line := range result {
			if strings.Contains(pystr.Lower(line), needle) {
				matched = append(matched, line)
			}
		}
		result = lastItems(matched, min(lines, 500))
	}
	return pyjson.ObjectOf("file", file, "lines", stringsAny(result)), nil
}

// Router of the status probe, health and identity checks, system stats, curator, learning, portal and diagnostics
func statusRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/ssh/ownership", &Endpoint{
		Name:   "get_ssh_ownership",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getSSHOwnership(c.Request) },
	})
	r.Get("/api/health", &Endpoint{
		Name: "get_health",
		Doc: "Lightweight process liveness for desktop/backend readiness probes.\n\n" +
			"    ``commit`` is the code this process BOOTED from (``get_version_info`` is cached at\n" +
			"    ``web_server`` import): Desktop refuses to attach to a backend whose commit differs from\n" +
			"    its checkout, so a serve that outlived ``icebath update`` is never re-adopted.",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getHealth(c.Request) },
	})
	r.Get("/api/host/identity", &Endpoint{
		Name: "get_host_identity",
		Doc: "Prove to an attaching `icebath serve`/`dashboard` WHO owns this port.\n\n" +
			"    The host rendezvous record names a (pid, port) owner, but a record cannot say whether that\n" +
			"    owner still holds the port: a graceful-shutdown window or an unrelated listener that\n" +
			"    inherited the port both look identical on disk. The attaching side dials this endpoint with\n" +
			"    the owner's 0600 token and attaches only when pid+role match. ``servesSpa`` is false for\n" +
			"    headless ``serve``, so a `icebath dashboard` user is never routed to a backend with no UI.",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getHostIdentity(c.Request) },
	})
	r.Get("/api/health/idle", &Endpoint{
		Name:   "get_health_idle",
		Doc:    "Token-gated diagnostic snapshot; never a retirement permit. None means cannot prove idle.",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getHealthIdle(c.Request) },
	})
	r.Post("/api/health/retirement", &Endpoint{
		Name:   "post_health_retirement",
		Handle: func(ctx context.Context, c *Call) (any, error) { return postHealthRetirement(ctx, c.Request) },
	})
	r.Get("/api/status", &Endpoint{
		Name: "get_status",
		Doc: "Public machine-level liveness probe (``PUBLIC_API_PATHS``): version, gateway state,\n" +
			"    active session count and the auth-gate shape — no bodies, no session content, no secrets.\n\n" +
			"    ``?profile=`` (dashboard management switcher) uses the config-only contextvar scope, NOT\n" +
			"    _profile_scope: this handler awaits the remote health probe, and _profile_scope swaps\n" +
			"    process-global skills-module attributes a concurrent request would cross-restore.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getStatus(ctx, c.Request, profileQuery(c)) },
	})
	r.Get("/api/system/stats", &Endpoint{
		Name: "get_system_stats",
		Doc: "Host + process system stats for the System page (stdlib identity; psutil CPU/memory/\n" +
			"    disk/uptime when available). Non-sensitive: no env values, no paths beyond icebath home.",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getSystemStats(ctx) },
	})
	r.Get("/api/curator", &Endpoint{
		Name:   "get_curator_status",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getCuratorStatus(ctx, profileQuery(c)) },
	})
	r.Put("/api/curator/paused", &Endpoint{
		Name:   "set_curator_paused",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", curatorPauseModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return setCuratorPaused(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/curator/run", &Endpoint{
		Name: "run_curator",
		Doc: "Trigger a curator review now (backgrounded; tail via action status). The curator\n" +
			"    archives and rewrites skills, so an unnamed target is refused while this backend\n" +
			"    serves several profiles.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return runCurator(ctx, profileQuery(c)) },
	})
	r.Get("/api/learning/graph", &Endpoint{
		Name:   "get_learning_graph",
		Doc:    "Learning graph for the desktop panel: profile-scoped learned skills + memory chunks.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getLearningGraph(ctx, profileQuery(c)) },
	})
	r.Get("/api/learning/node", &Endpoint{
		Name:   "get_learning_node",
		Doc:    "Current content of a journey node (skill SKILL.md or memory chunk), for an edit prefill.",
		Params: []Param{RequiredQuery("id", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			id := c.Str("id")
			return learningMutation(ctx, profileQuery(c), func(sctx context.Context, env *tools.Env) (map[string]any, error) {
				return memory.NodeDetail(sctx, env, id)
			}, 404, "not found")
		},
	})
	r.Delete("/api/learning/node", &Endpoint{
		Name: "delete_learning_node",
		Doc: "Delete a journey node — skills are archived (restorable), memories removed.\n\n" +
			"    ``?profile=`` is honoured too: a shared-backend Desktop scopes this call by query only, and\n" +
			"    ignoring it archived the same-named skill of the launch profile instead.",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", learningNodeRefModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			id := bodyStr(body, "id")
			return learningMutation(ctx, bodyOrQueryProfile(body, c), func(sctx context.Context, env *tools.Env) (map[string]any, error) {
				return memory.DeleteNode(sctx, env, id)
			}, 400, "delete failed")
		},
	})
	r.Put("/api/learning/node", &Endpoint{
		Name:   "update_learning_node",
		Doc:    "Rewrite a journey node's content (SKILL.md or memory chunk); profile as for DELETE.",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", learningNodeEditModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			id, content := bodyStr(body, "id"), bodyStr(body, "content")
			return learningMutation(ctx, bodyOrQueryProfile(body, c), func(sctx context.Context, env *tools.Env) (map[string]any, error) {
				return memory.EditNode(sctx, env, id, content)
			}, 400, "edit failed")
		},
	})
	r.Get("/api/portal", &Endpoint{
		Name:   "get_portal_status",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getPortalStatus(ctx, profileQuery(c)) },
	})
	r.Post("/api/ops/prompt-size", &Endpoint{
		Name:   "run_prompt_size",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return statusSpawnAction(ctx, []string{"prompt-size"}, "prompt-size", "Failed", profileQuery(c))
		},
	})
	r.Post("/api/ops/dump", &Endpoint{
		Name:   "run_dump",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return statusSpawnAction(ctx, []string{"dump"}, "dump", "Failed", profileQuery(c))
		},
	})
	r.Post("/api/ops/config-migrate", &Endpoint{
		Name:   "run_config_migrate",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return statusSpawnAction(ctx, []string{"config", "migrate"}, "config-migrate", "Failed", profileQuery(c))
		},
	})
	r.Post("/api/ops/debug-share", &Endpoint{
		Name: "run_debug_share_endpoint",
		Doc: "Upload a redacted debug report + full logs and return the paste URLs. Synchronous,\n" +
			"    unlike the other diagnostics actions: the point is the shareable URLs, returned as a\n" +
			"    structured payload the dashboard renders as copyable links.",
		Params: []Param{optProfile},
		Body:   []BodyParam{{Name: "body", Type: debugShareRequestModel}},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return runDebugShare(ctx, c.Object("body"), profileQuery(c))
		},
	})
	return r
}

// Router of the log tail read
func statusLogsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/logs", &Endpoint{
		Name: "get_logs",
		Params: []Param{QueryParam("file", Str, "agent"), QueryParam("lines", Int, int64(100)), QueryParam("level", Optional(Str), nil),
			QueryParam("component", Optional(Str), nil), QueryParam("search", Optional(Str), nil), QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getLogs(ctx, c.Str("file"), c.Int("lines"), c.Str("level"), c.Str("component"), c.Str("search"), profileQuery(c))
		},
	})
	return r
}
