package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Active personality name of a config, the personality owner assigned at startup
var ActivePersonalityName func(cfg map[string]any) string

// Tool detail modes of the display config
var detailModes = map[string]bool{"hidden": true, "collapsed": true, "expanded": true}

// Thinking display modes of the display config
var thinkingModes = map[string]bool{"collapsed": true, "truncated": true, "full": true}

// Error codes of the config getters whose failure is a JSON RPC error
var configGetErr = map[string]int{"provider": 5013, "approval_mode": 5001, "approvals.mode": 5001}

var configRegistry = &handlerRegistry{}

func init() {
	projectsHandler("projects.discover_repos", projectsDiscoverRepos)
	projectsHandler("projects.record_repos", projectsRecordRepos)
	projectsHandler("projects.tree", projectsTree)
	projectsHandler("projects.project_sessions", projectsProjectSessions)
	configRegistry.scopedMethod("config.get", configGet)
	configRegistry.install()
}

// Registers a profile scoped projects RPC whose failure answers 5061
func projectsHandler(name string, fn func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)) {
	configRegistry.scopedMethod(name, func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				resp = rpcErr(rid, projectsErr, panicMessage(r), nil)
			}
		}()
		out, err := fn(ctx, rid, params)
		if err != nil {
			return rpcErr(rid, projectsErr, err.Error(), nil)
		}
		return out
	})
}

// Clears cached scan rows when the discovery policy changed
func reconcileRepoDiscovery(ctx context.Context, pdb *tools.ProjectsDB, policy map[string]any, policyKey string) error {
	_, err := pdb.ReconcileDiscoveredReposPolicy(policyKey, repoDiscoveryPolicyIsDefault(ctx, policy))
	return err
}

// Scanned and session derived repos for the desktop overview with the discovery policy
func projectsDiscoverRepos(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer release()
	policy := repoDiscoveryPolicy(ctx, nil)
	pdb, err := openProjectsDB(ctx)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	if err := reconcileRepoDiscovery(ctx, pdb, policy, repoDiscoveryPolicyKey(policy)); err != nil {
		return nil, err
	}
	if truthy(params["scan"]) && truthy(policy["enabled"]) {
		scanDiscoveredReposRemote(ctx, pdb, policy)
	}
	repos, err := discoverReposPayload(ctx, db, pdb, true, truthy(policy["enabled"]))
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"repos": anyList(repos), "discovery_policy": policy}), nil
}

// Persists the repo roots of a client side scan when its policy matches and answers the merged list
func projectsRecordRepos(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	policy := repoDiscoveryPolicy(ctx, nil)
	policyKey := repoDiscoveryPolicyKey(policy)
	var accepted bool
	if incoming, isDict := params["discovery_policy"].(map[string]any); isDict {
		accepted = repoDiscoveryPolicyKey(repoDiscoveryPolicy(ctx, incoming)) == policyKey
	} else {
		accepted = repoDiscoveryPolicyIsDefault(ctx, policy)
	}
	accepted = truthy(policy["enabled"]) && accepted
	var pairs []tools.DiscoveredRepo
	if raw := params["repos"]; truthy(raw) {
		items, isList := raw.([]any)
		if !isList {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
		}
		for _, item := range items {
			switch v := item.(type) {
			case string:
				pairs = append(pairs, tools.DiscoveredRepo{Root: v})
			case map[string]any:
				if truthy(v["root"]) {
					label := ""
					if truthy(v["label"]) {
						label = pyStrOf(v["label"])
					}
					pairs = append(pairs, tools.DiscoveredRepo{Root: pyStrOf(v["root"]), Label: label})
				}
			}
		}
	}
	err := func() error {
		pdb, err := openProjectsDB(ctx)
		if err != nil {
			return err
		}
		defer pdb.Close()
		if err := reconcileRepoDiscovery(ctx, pdb, policy, policyKey); err != nil {
			return err
		}
		if accepted {
			_, err = pdb.RecordDiscoveredRepos(pairs, true, &policyKey)
			return err
		}
		if !truthy(policy["enabled"]) {
			return pdb.ClearDiscoveredRepos(&policyKey)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer release()
	repos, err := discoverReposPayload(ctx, db, nil, true, truthy(policy["enabled"]))
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"repos": anyList(repos), "accepted": accepted, "discovery_policy": policy}), nil
}

// Project tree with every session row stamped with the response profile
func stampedProjectTree(ctx context.Context, db *state.DB, params map[string]any, o projectTreeBuild) (map[string]any, any, error) {
	tree, active, err := buildProjectTree(ctx, db, o)
	if err != nil {
		return nil, nil, err
	}
	stampProfile(dictList(tree["projects"]), responseProfileName(ctx, strOr(params["profile"])))
	return tree, active, nil
}

// int of a param value or its default when falsy, raising like int() on a bad value
func intParamOr(v any, def int) int {
	if !truthy(v) {
		return def
	}
	n, err := pyint.Int64(v)
	if err != nil {
		panic(err)
	}
	return int(n)
}

// Project overview with counts and previews plus the session ids any project claims
func projectsTree(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if db == nil {
		return ok(rid, map[string]any{"projects": []any{}, "active_id": nil, "scoped_session_ids": []any{}}), nil
	}
	tree, active, err := stampedProjectTree(ctx, db, params, projectTreeBuild{previewLimit: intParamOr(params["preview_limit"], 3),
		sessionLimit: intParamOr(params["session_limit"], 2000), includeDiscovered: true})
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"projects": tree["projects"], "active_id": active, "scoped_session_ids": tree["scoped_session_ids"]}), nil
}

// Fully hydrated lanes of one project from the same grouping as the overview
func projectsProjectSessions(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	projectID := strOr(params["project_id"])
	if projectID == "" {
		return rpcErr(rid, projectArgErr, "project_id required", nil), nil
	}
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if db == nil {
		return ok(rid, map[string]any{"project": nil}), nil
	}
	tree, _, err := stampedProjectTree(ctx, db, params, projectTreeBuild{hydrate: true,
		sessionLimit: intParamOr(params["session_limit"], 5000)})
	if err != nil {
		return nil, err
	}
	var found any
	for _, p := range dictList(tree["projects"]) {
		if p["id"] == projectID {
			found = p
			break
		}
	}
	return ok(rid, map[string]any{"project": found}), nil
}

// display section of the config, empty when unset
func displayRaw(ctx context.Context) map[string]any {
	display, _ := loadCfg(ctx)["display"].(map[string]any)
	if display == nil {
		return map[string]any{}
	}
	return display
}

// Normalized display word, unknown values reading back as the default
func displayWord(ctx context.Context, key, def string, allowed map[string]bool) string {
	raw, present := displayRaw(ctx)[key]
	if !present {
		raw = def
	}
	word := pystr.Lower(pystr.Strip(strOr(raw)))
	if allowed[word] {
		return word
	}
	return def
}

// Current model, its normalized provider and every listed provider
func cfgGetProvider(ctx context.Context, params map[string]any) map[string]any {
	model := resolveModel(ctx)
	providerName := "unknown"
	if head, _, found := strings.Cut(model, "/"); found {
		providerName = provider.NormalizeProvider(head)
	}
	providers := []any{}
	for _, p := range provider.ListAvailableProviders(ctx) {
		aliases := make([]any, len(p.Aliases))
		for i, alias := range p.Aliases {
			aliases[i] = alias
		}
		providers = append(providers, map[string]any{"id": p.ID, "label": p.Label, "aliases": aliases, "authenticated": p.Authenticated})
	}
	return map[string]any{"model": model, "provider": providerName, "providers": providers}
}

// Explicit or configured project cwd with its git branch
func cfgGetProject(ctx context.Context, params map[string]any) map[string]any {
	raw := params["cwd"]
	if !truthy(raw) {
		terminal, _ := loadCfg(ctx)["terminal"].(map[string]any)
		raw = terminal["cwd"]
	}
	cwd, err := completionCwd(ctx, map[string]any{"cwd": pystr.Strip(strOr(raw)), "cwd_explicit": truthy(params["cwd"]), "profile": params["profile"]})
	if err != nil {
		panic(err)
	}
	return map[string]any{"cwd": cwd, "branch": tools.GitProbeBranch(cwd)}
}

// Effective reasoning effort of the session or config with whether reasoning shows
func cfgGetReasoning(ctx context.Context, params map[string]any) map[string]any {
	cfg := loadCfg(ctx)
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	var reasoning any
	if session != nil {
		reasoning = session.Get("create_reasoning_override")
		if _, isDict := reasoning.(map[string]any); !isDict {
			reasoning = nil
			if a := sessionAgent(session); a != nil {
				if rc := agent.SessionRuntimeView(a).ReasoningConfig(); rc != nil {
					reasoning = rc
				}
			}
		}
	}
	effort := ""
	if rc, isDict := reasoning.(map[string]any); isDict {
		if rc["enabled"] != false {
			effort = "medium"
			if truthy(rc["effort"]) {
				effort = pyStrOf(rc["effort"])
			}
		} else {
			effort = "none"
		}
	} else {
		agentCfg, _ := cfg["agent"].(map[string]any)
		rawEffort, present := agentCfg["reasoning_effort"]
		if !present {
			rawEffort = ""
		}
		if effortDict, isDict := rawEffort.(map[string]any); isDict {
			parsed := config.ParseReasoningEffort(effortDict)
			if parsed["enabled"] == false {
				rawEffort = false
			} else {
				rawEffort = parsed["effort"]
			}
		}
		switch {
		case rawEffort == false:
			effort = "none"
		case truthy(rawEffort):
			effort = pyStrOf(rawEffort)
		default:
			effort = "medium"
		}
	}
	display, _ := cfg["display"].(map[string]any)
	show, present := display["show_reasoning"]
	if !present {
		show = true
	}
	shown := "hide"
	if truthy(show) {
		shown = "show"
	}
	return map[string]any{"value": effort, "display": shown}
}

// Fast tier of the session pin or live agent, else the configured tier
func cfgGetFast(ctx context.Context, params map[string]any) map[string]any {
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	tier, known := "", false
	if a := sessionAgent(session); session != nil && a != nil {
		tier = a.ServiceTier()
		known = tier != ""
	} else if session != nil {
		if raw, present := session.Lookup("create_service_tier_override"); present && raw != nil {
			tier, known = strOr(raw), true
		}
	}
	if !known {
		tier = loadServiceTier(ctx)
	}
	value := map[string]string{"priority": "fast", "ultrafast": "ultrafast"}[tier]
	if value == "" {
		value = "normal"
	}
	return map[string]any{"value": value}
}

// Thinking display mode, the legacy details mode deciding when it is unset
func cfgGetThinkingMode(ctx context.Context, params map[string]any) map[string]any {
	raw := displayWord(ctx, "thinking_mode", "", thinkingModes)
	if raw == "" {
		raw = "collapsed"
		if displayWord(ctx, "details_mode", "collapsed", detailModes) == "expanded" {
			raw = "full"
		}
	}
	return map[string]any{"value": raw}
}

// Launch config version token with the MCP section hash, zero when the version cannot be read
func cfgGetMtime(ctx context.Context, params map[string]any) (out map[string]any) {
	defer func() {
		if recover() != nil {
			out = map[string]any{"mtime": 0}
		}
	}()
	version, err := config.ConfigVersionOrNone(ctx, filepath.Join(importedHome(), "config.yaml"))
	if err != nil {
		return map[string]any{"mtime": 0}
	}
	return map[string]any{"mtime": config.VersionToken(version), "mcp_rev": computeMCPRev(ctx)}
}

// Getter of each config.get key returning its result payload
var configGetters = map[string]func(ctx context.Context, params map[string]any) map[string]any{
	"provider": cfgGetProvider,
	"profile": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"home": importedHome(), "display": home.Display(home.Dir(ctx))}
	},
	"project": cfgGetProject,
	"full": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"config": loadCfg(ctx)}
	},
	"prompt": func(ctx context.Context, params map[string]any) map[string]any {
		prompt, present := loadCfg(ctx)["custom_prompt"]
		if !present {
			prompt = ""
		}
		return map[string]any{"prompt": prompt}
	},
	"skin": func(ctx context.Context, params map[string]any) map[string]any {
		skin, present := displayRaw(ctx)["skin"]
		if !present {
			skin = "default"
		}
		return map[string]any{"value": skin}
	},
	"indicator": func(ctx context.Context, params map[string]any) map[string]any {
		allowed := map[string]bool{}
		for _, style := range home.IndicatorStyles {
			allowed[style] = true
		}
		return map[string]any{"value": displayWord(ctx, "tui_status_indicator", home.DefaultIndicatorStyle, allowed)}
	},
	"personality": func(ctx context.Context, params map[string]any) map[string]any {
		name := ActivePersonalityName(loadCfg(ctx))
		if name == "" {
			name = "none"
		}
		return map[string]any{"value": name}
	},
	"reasoning": cfgGetReasoning,
	"fast":      cfgGetFast,
	"daybreak": func(ctx context.Context, params map[string]any) map[string]any {
		agentCfg, _ := loadCfg(ctx)["agent"].(map[string]any)
		return map[string]any{"value": agentCfg["daybreak"] == true}
	},
	"busy": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": loadBusyInputMode(ctx)}
	},
	"approval_mode": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": loadApprovalMode(ctx)}
	},
	"approvals.mode": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": loadApprovalMode(ctx)}
	},
	"details_mode": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": displayWord(ctx, "details_mode", "collapsed", detailModes)}
	},
	"thinking_mode": cfgGetThinkingMode,
	"density": func(ctx context.Context, params map[string]any) map[string]any {
		value := "off"
		if truthy(displayRaw(ctx)["tui_compact"]) {
			value = "on"
		}
		return map[string]any{"value": value}
	},
	"theme": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": displayWord(ctx, "tui_theme", "auto", map[string]bool{"auto": true, "light": true, "dark": true})}
	},
	"statusbar": func(ctx context.Context, params map[string]any) map[string]any {
		raw, present := displayCfg(ctx)["tui_statusbar"]
		if !present {
			raw = "top"
		}
		return map[string]any{"value": coerceStatusbar(raw)}
	},
	"focus": func(ctx context.Context, params map[string]any) map[string]any {
		value := "off"
		if truthy(displayCfg(ctx)["focus_view"]) {
			value = "on"
		}
		return map[string]any{"value": value, "tool_progress": loadToolProgressMode(ctx)}
	},
	"mouse": func(ctx context.Context, params map[string]any) map[string]any {
		return map[string]any{"value": displayMouseTracking(loadCfg(ctx)["display"])}
	},
	"mtime": cfgGetMtime,
}

// Reads one config key, a failing provider or approval getter answering its own error code
func configGet(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
	key, present := params["key"]
	if !present {
		key = ""
	}
	name, _ := key.(string)
	getter := configGetters[name]
	if getter == nil {
		return rpcErr(rid, 4002, fmt.Sprintf("unknown config key: %s", pyStrOf(key)), nil)
	}
	if code, mapped := configGetErr[name]; mapped {
		defer func() {
			if r := recover(); r != nil {
				resp = rpcErr(rid, code, panicMessage(r), nil)
			}
		}()
	}
	return ok(rid, getter(ctx, params))
}
