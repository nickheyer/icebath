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
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Active personality name of a config, the personality owner assigned at startup
var ActivePersonalityName func(cfg map[string]any) string

// Tool detail modes of the display config
var detailModes = map[string]bool{"hidden": true, "collapsed": true, "expanded": true}

// Thinking display modes of the display config
var thinkingModes = map[string]bool{"collapsed": true, "truncated": true, "full": true}

// Error codes of the config getters whose failure is a JSON RPC error
var configGetErr = map[string]int{"provider": 5013, "approval_mode": 5001, "approvals.mode": 5001}

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

// Answer carrying one word
func wordAnswer(word string) tuigateway.ConfigGetResult {
	return tuigateway.ConfigGetResult{Value: tuigateway.TextValue(word)}
}

// Current model, its normalized provider and every listed provider
func cfgGetProvider(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
	model := resolveModel(ctx)
	providerName := "unknown"
	if head, _, found := strings.Cut(model, "/"); found {
		providerName = provider.NormalizeProvider(head)
	}
	providers := []tuigateway.ConfigProviderRef{}
	for _, p := range provider.ListAvailableProviders(ctx) {
		providers = append(providers, tuigateway.ConfigProviderRef{ID: p.ID, Label: p.Label, Aliases: p.Aliases, Authenticated: p.Authenticated})
	}
	return tuigateway.ConfigGetResult{Model: model, Provider: providerName, Providers: providers}
}

// Explicit or configured project cwd with its git branch
func cfgGetProject(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
	raw := params["cwd"]
	if !truthy(raw) {
		raw = settings.WorkingFolder.Get(ctx)
	}
	cwd, err := completionCwd(ctx, map[string]any{"cwd": pystr.Strip(strOr(raw)), "cwd_explicit": truthy(params["cwd"]), "profile": params["profile"]})
	if err != nil {
		panic(err)
	}
	return tuigateway.ConfigGetResult{Cwd: cwd, Branch: tools.GitProbeBranch(cwd)}
}

// Effective reasoning effort of the session or config with whether reasoning shows
func cfgGetReasoning(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
	session := tuihost.SessionRecord(strOr(params["session_id"]))
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
	} else if effort = settings.ReasoningEffort.Get(ctx); effort == "" {
		effort = "medium"
	}
	shown := "hide"
	if sessionShowReasoning(ctx, strOr(params["session_id"])) {
		shown = "show"
	}
	return tuigateway.ConfigGetResult{Value: tuigateway.TextValue(effort), Display: shown}
}

// Fast tier of the session pin or live agent, else the configured tier
func cfgGetFast(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
	session := tuihost.SessionRecord(strOr(params["session_id"]))
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
		tier = loadServiceTier(sessionSettings(ctx, strOr(params["session_id"])))
	}
	value := map[string]string{"priority": "fast", "ultrafast": "ultrafast"}[tier]
	if value == "" {
		value = "normal"
	}
	return wordAnswer(value)
}

// Launch config version token with the MCP section hash, zero when the version cannot be read
func cfgGetMtime(ctx context.Context, params map[string]any) (out tuigateway.ConfigGetResult) {
	unread := tuigateway.ConfigGetResult{Mtime: new(float64)}
	defer func() {
		if recover() != nil {
			out = unread
		}
	}()
	version, err := config.ConfigVersionOrNone(ctx, filepath.Join(importedHome(), "config.yaml"))
	if err != nil {
		return unread
	}
	token := config.VersionToken(version)
	return tuigateway.ConfigGetResult{Mtime: &token, McpRev: computeMCPRev(ctx)}
}

// Text of a config value, the default when it is not text
func textOr(v any, def string) string {
	if text, isText := v.(string); isText {
		return text
	}
	return def
}

// Getter of each config.get key returning its result payload
var configGetters = map[string]func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult{
	"provider": cfgGetProvider,
	"profile": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return tuigateway.ConfigGetResult{Home: importedHome(), Display: home.Display(home.Dir(ctx))}
	},
	"project": cfgGetProject,
	"full": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		client := tuigateway.ClientConfigOf(sessionSettings(ctx, strOr(params["session_id"])))
		return tuigateway.ConfigGetResult{Config: &client}
	},
	"prompt": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		prompt := textOr(loadCfg(ctx)["custom_prompt"], "")
		return tuigateway.ConfigGetResult{Prompt: &prompt}
	},
	"skin": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(textOr(displayRaw(ctx)["skin"], "default"))
	},
	"indicator": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		allowed := map[string]bool{}
		for _, style := range home.IndicatorStyles {
			allowed[style] = true
		}
		return wordAnswer(displayWord(ctx, "tui_status_indicator", home.DefaultIndicatorStyle, allowed))
	},
	"personality": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		name := ActivePersonalityName(loadCfg(ctx))
		if name == "" {
			name = "none"
		}
		return wordAnswer(name)
	},
	"reasoning": cfgGetReasoning,
	"fast":      cfgGetFast,
	"daybreak": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return tuigateway.ConfigGetResult{Value: tuigateway.FlagValue(settings.Daybreak.Get(ctx))}
	},
	"busy": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(loadBusyInputMode(sessionSettings(ctx, strOr(params["session_id"]))))
	},
	"approval_mode": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(loadApprovalMode(ctx))
	},
	"approvals.mode": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(loadApprovalMode(ctx))
	},
	"details_mode": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(displayWord(ctx, "details_mode", "collapsed", detailModes))
	},
	"density": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		value := "off"
		if truthy(displayRaw(ctx)["tui_compact"]) {
			value = "on"
		}
		return wordAnswer(value)
	},
	"theme": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(displayWord(ctx, "tui_theme", "auto", map[string]bool{"auto": true, "light": true, "dark": true}))
	},
	"statusbar": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		raw, present := displayCfg(ctx)["tui_statusbar"]
		if !present {
			raw = "top"
		}
		return wordAnswer(coerceStatusbar(raw))
	},
	"mouse": func(ctx context.Context, params map[string]any) tuigateway.ConfigGetResult {
		return wordAnswer(settings.Mouse.Get(ctx))
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
	return ok(rid, tuigateway.Plain(getter(ctx, params)))
}

// str of a param value
func pyStrOf(v any) string {
	if text, isText := v.(string); isText {
		return text
	}
	return pyrepr.Str(v)
}
