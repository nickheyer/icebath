package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Reasoning display toggles mapped to the attribute, value, headline key and follow up note key
var reasoningToggles = map[string]struct {
	attr     string
	value    bool
	headline string
	note     string
}{
	"show": {"show_reasoning", true, "headline_on", "note_show"}, "on": {"show_reasoning", true, "headline_on", "note_show"},
	"hide": {"show_reasoning", false, "headline_off", ""}, "off": {"show_reasoning", false, "headline_off", ""},
	"full": {"reasoning_full", true, "headline_full", "note_full"}, "all": {"reasoning_full", true, "headline_full", "note_full"},
	"clamp": {"reasoning_full", false, "headline_clamped", ""}, "collapse": {"reasoning_full", false, "headline_clamped", ""},
	"short": {"reasoning_full", false, "headline_clamped", ""},
}

// Fast argument mapped to the service tier and the persisted config value
var fastTiers = map[string][2]string{
	"fast": {"priority", "fast"}, "on": {"priority", "fast"}, "normal": {"", "normal"}, "off": {"", "normal"},
	"auto": {"auto", "auto"}, "cold": {"cold", "cold"}, "ultrafast": {"ultrafast", "ultrafast"}, "ultra": {"ultrafast", "ultrafast"},
}

// Provider, model and api mode the effort labels read, the live agent's mode before the configured runtime
func (c *CLI) reasoningRoute() (string, string, string) {
	apiMode := ""
	if c.agent != nil {
		apiMode = c.agent.APIMode()
	}
	if apiMode == "" {
		apiMode = provider.GetCurrentCodexRuntime(c.config)
	}
	return c.provider, c.model, apiMode
}

// Handles /reasoning, the effort level at session or global scope and the thinking display toggles
func (c *CLI) handleReasoningCommand(cmd string) {
	raw := commandArg(cmd, false)
	prov, model, apiMode := c.reasoningRoute()
	if raw == "" {
		rc := c.reasoningConfig
		var level string
		switch {
		case rc == nil:
			level = c.gatewayT("reasoning.level_default", nil)
		case rc["enabled"] == false:
			level = c.gatewayT("reasoning.level_disabled", nil)
		default:
			level = provider.EffortDisplayLabel(cliStr(mapValueOr(rc, "effort", "medium"), "medium"), prov, model, apiMode)
		}
		display := c.commandsT("reasoning.display_off", nil)
		if c.showReasoning {
			display = c.commandsT("reasoning.display_on", nil)
		}
		full := c.commandsT("reasoning.full_state_clamped", nil)
		if c.reasoningFull {
			full = c.commandsT("reasoning.full_state_full", nil)
		}
		cp(accentLine(c.commandsT("reasoning.effort_line", map[string]any{"level": level})),
			accentLine(c.commandsT("reasoning.display_line", map[string]any{"display": display, "full": full})),
			dimLine(c.commandsT("reasoning.usage", nil)))
		return
	}
	arg, explicitGlobal := splitScopeFlags(raw)
	if toggle, ok := reasoningToggles[arg]; ok {
		key := "display." + toggle.attr
		if toggle.attr == "show_reasoning" {
			c.showReasoning = toggle.value
			if c.agent != nil {
				c.agent.SetReasoningCallback(c.currentReasoningCallback())
			}
			key = "display.platforms.cli.show_reasoning"
		} else {
			c.reasoningFull = toggle.value
		}
		saveConfigValue(c.ctx, key, toggle.value)
		cp(accentLine(c.commandsT("reasoning.display_saved", map[string]any{"headline": c.commandsT("reasoning."+toggle.headline, nil)})))
		if toggle.note != "" {
			cp(dimLine("  " + c.commandsT("reasoning."+toggle.note, nil)))
		}
		if toggle.attr == "reasoning_full" && toggle.value && !c.showReasoning {
			cp(dimLine("  " + c.commandsT("reasoning.display_off_note", nil)))
		}
		return
	}
	parsed := parseReasoningConfig(arg)
	if parsed == nil {
		cp(dimLine(c.commandsT("shared.unknown_argument", map[string]any{"arg": arg})), dimLine(c.commandsT("reasoning.valid_levels", nil)),
			dimLine(c.commandsT("reasoning.valid_display", nil)), dimLine(c.commandsT("reasoning.valid_scope", nil)))
		return
	}
	c.reasoningConfig = parsed
	c.reasoningUserSet = true
	retireAgent(c)
	saved := explicitGlobal && saveConfigValue(c.ctx, "agent.reasoning_effort", arg)
	if saved {
		agentCfg, ok := c.config["agent"].(map[string]any)
		if !ok {
			agentCfg = map[string]any{}
			c.config["agent"] = agentCfg
		}
		agentCfg["reasoning_effort"] = arg
	}
	cp(accentLine(c.commandsT("reasoning.effort_set", map[string]any{"effort": provider.EffortDisplayLabel(arg, prov, model, apiMode),
		"scope": c.scopeOutcome(explicitGlobal, saved)})))
}

// Handles /fast, the priority processing tier at session scope or saved with --global
func (c *CLI) handleFastCommand(cmd string) {
	if !c.fastCommandAvailable() {
		cp("  " + c.commandsT("fast.not_supported", nil))
		return
	}
	model := c.model
	if c.agent != nil && c.agent.Model() != "" {
		model = c.agent.Model()
	}
	feature := c.commandsT("fast.feature_openai", nil)
	if provider.IsAnthropicFastModeModel(model) {
		feature = c.commandsT("fast.feature_anthropic", nil)
	}
	raw := commandArg(cmd, false)
	usage := dimLine(c.commandsT("fast.usage", nil))
	if raw == "" || pystr.Lower(raw) == "status" {
		cp(accentLine(c.commandsT("fast.status", map[string]any{"feature": feature, "status": provider.ServiceTierWord(c.serviceTier)})), usage)
		return
	}
	arg, explicitGlobal := splitScopeFlags(raw)
	tier, ok := fastTiers[arg]
	if !ok {
		cp(dimLine(c.commandsT("shared.unknown_argument", map[string]any{"arg": arg})), usage)
		return
	}
	if tier[0] == "ultrafast" {
		offered := func() (ok bool) {
			defer func() {
				if recover() != nil {
					ok = false
				}
			}()
			got, err := provider.UltrafastOfferedFor(provider.Route{Provider: c.provider, BaseURL: c.baseURL, APIKey: c.apiKey}, model)
			return err == nil && got
		}()
		if !offered {
			shown := model
			if shown == "" {
				shown = "?"
			}
			cp(dimLine(c.commandsT("fast.ultrafast_not_supported", map[string]any{"model": shown})), usage)
			return
		}
		feature = c.commandsT("fast.feature_ultrafast", nil)
	}
	c.serviceTier = tier[0]
	retireAgent(c)
	saved := explicitGlobal && saveConfigValue(c.ctx, "agent.service_tier", tier[1])
	cp(accentLine(c.commandsT("fast.set_to", map[string]any{"feature": feature, "value": strings.ToUpper(tier[1]),
		"scope": c.scopeOutcome(explicitGlobal, saved)})))
}

// Handles /debug, uploading the debug report and logs, nous for the internal service and local for no upload
func (c *CLI) handleDebugCommand(cmdOriginal string) {
	words := map[string]bool{}
	fields := strings.Fields(cmdOriginal)
	for _, w := range fields[min(1, len(fields)):] {
		words[pystr.Lower(w)] = true
	}
	local := words["local"]
	ns := map[string]any{"debug_command": "share", "lines": 200, "expire": 7, "local": local, "nous": words["nous"] && !local, "yes": true}
	if _, err := selfupdate.RunDebug(c.ctx, home.Dir(c.ctx), ns, c.console.Out); err != nil {
		panic(err)
	}
}

// Handles /codex-runtime, showing or switching the codex app server runtime opt in
func (c *CLI) handleCodexRuntime(cmdOriginal string) {
	parts := pystr.SplitWhitespace(cmdOriginal, 1)
	arg := ""
	if len(parts) > 1 {
		arg = pystr.Strip(parts[1])
	}
	newValue, errs := provider.CodexRuntimeParseArgs(arg)
	if len(errs) > 0 {
		for _, e := range errs {
			cprint("❌ " + e)
		}
		return
	}
	h := home.Dir(c.ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		cprint(c.t("cli.model.could_not_load_config", map[string]any{"error": err.Error()}))
		return
	}
	var persist func(map[string]any) error
	if newValue != nil {
		persist = func(updated map[string]any) error { return config.SaveConfig(c.ctx, h, updated, config.SaveOptions{}) }
	}
	result, err := provider.ApplyCodexRuntime(c.ctx, map[string]any(cfg), newValue, persist)
	if err != nil {
		panic(err)
	}
	prefix := "✗"
	if result.Success {
		prefix = "✓"
	}
	for _, line := range pystr.Splitlines(result.Message, false) {
		if strings.HasPrefix(line, "openai_runtime") {
			cprint("  " + prefix + " " + line)
		} else {
			cprint("    " + line)
		}
	}
	if result.Success && result.RequiresNewSession {
		cprint("    " + c.t("cli.model.tip_reset_new_session", nil))
	}
}

// Handles /moa, one prompt through the default mixture of agents preset with the prior model restored after
func (c *CLI) cmdMoa(cmdOriginal string) {
	payload := slashArgs(cmdOriginal)
	if payload == "" {
		cprint("  " + provider.MoAUsage())
		return
	}
	preset := cliStr(provider.NormalizeMoAConfig(c.config["moa"])["default_preset"], "")
	c.pendingMoaRestoreModel = map[string]string{"requested_provider": c.requestedProvider, "provider": c.provider, "model": c.model,
		"api_key": c.apiKey, "base_url": c.baseURL, "api_mode": c.apiMode}
	c.requestedProvider = "moa"
	c.provider = "moa"
	c.model = preset
	c.apiKey = "moa-virtual-provider"
	c.baseURL = "moa://local"
	c.apiMode = "chat_completions"
	retireAgent(c)
	c.pendingMoaDisableAfterTurn = true
	c.pendingAgentSeed = payload
	cprint("  " + c.t("cli.model.moa_one_shot_queued", map[string]any{"preset": preset}))
}
