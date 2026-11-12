package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch flags that pick the model route, a new session starting on the profile's default route
var gatewayRouteFlags = map[string]bool{"model": true, "provider": true, "api_key": true, "base_url": true}

// Adopts the profile default toolsets the gateway resolved for display and prints its unknown toolsets warning
func adoptGatewayToolsets(c *CLI, store *gatewaycli.GatewaySessionStore) {
	current := store.Current()
	var info map[string]any
	if current != nil {
		info, _ = pyjson.Plain(current.Vals["info"]).(map[string]any)
	}
	if listed, isList := info["enabled_toolsets"].([]any); c.enabledToolsets == nil && isList {
		names := make([]string, 0, len(listed))
		for _, name := range listed {
			names = append(names, fmt.Sprint(name))
		}
		c.enabledToolsets = names
	}
	if unknown, _ := info["unknown_toolsets"].([]any); len(unknown) > 0 {
		names := make([]string, len(unknown))
		for i, name := range unknown {
			names[i] = fmt.Sprint(name)
		}
		c.consolePrint("[bold red]" + i18n.T(c.ctx, "cli.startup.unknown_toolsets", "", map[string]any{"names": strings.Join(names, ", ")}) + "[/]")
	}
}

// This run's explicit launch flags as the gateway session.create policy
func gatewayLaunch(c *CLI) map[string]any {
	launch := map[string]any{"model": nil, "provider": nil, "api_key": nil, "base_url": nil, "toolsets": nil, "reasoning": nil,
		"max_turns": nil, "checkpoints": nil, "pass_session_id": nil, "ignore_rules": nil, "run_budget": nil, "verbose": nil,
		"skills": nil}
	if c.explicitModelOverride {
		launch["model"] = c.model
		if p := c.explicitProvider; p != "" && p != "auto" {
			launch["provider"] = p
		}
	}
	if c.explicitAPIKey != "" {
		launch["api_key"] = c.explicitAPIKey
	}
	if c.explicitBaseURL != "" {
		launch["base_url"] = c.explicitBaseURL
	}
	if c.explicitToolsets && c.enabledToolsets != nil {
		launch["toolsets"] = toAnyList(c.enabledToolsets)
	}
	if c.explicitReasoning != "" {
		launch["reasoning"] = c.explicitReasoning
	}
	if c.explicitMaxTurns != nil {
		launch["max_turns"] = *c.explicitMaxTurns
	}
	if c.explicitCheckpoints {
		launch["checkpoints"] = true
	}
	if c.passSessionID {
		launch["pass_session_id"] = true
	}
	if c.ignoreRules {
		launch["ignore_rules"] = true
	}
	if c.explicitRunBudget != nil {
		launch["run_budget"] = *c.explicitRunBudget
	}
	if c.verbose {
		launch["verbose"] = true
	}
	if skills := explicitLaunchSkills(c); skills != nil {
		launch["skills"] = toAnyList(skills)
	}
	return launch
}

// List of strings as a decoded JSON list
func toAnyList(names []string) []any {
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

// Skills names the gateway renders into the session once at creation, the auto loaded ones left to its agent
func explicitLaunchSkills(c *CLI) []string {
	requested := c.preloadSkillsRequested
	if len(requested) == 0 {
		return nil
	}
	auto := map[string]bool{}
	if !c.ignoreRules {
		for _, name := range resolveAutoLoadSkills(CLIConfig(c.ctx)) {
			auto[name] = true
		}
	}
	var out []string
	for _, name := range requested {
		if !auto[name] {
			out = append(out, name)
		}
	}
	return out
}

// Deduplicated skills.auto_load names of a config, empty when unset or malformed
func resolveAutoLoadSkills(userConfig map[string]any) []string {
	block, _ := userConfig["skills"].(map[string]any)
	autoLoad, isList := block["auto_load"].([]any)
	if !isList {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, entry := range autoLoad {
		name, isStr := entry.(string)
		name = strings.TrimSpace(name)
		if !isStr || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// First use shell hook consent on this terminal, which the gateway reads when the session opens
func recordShellHookConsent(c *CLI) {
	flags := gatewaycli.EnvLaunchFlags()
	_, safe := flags.Get("safe_mode")
	_, ignoreUser := flags.Get("ignore_user_config")
	if strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL")) != "" || safe || ignoreUser {
		return
	}
	cfg, err := config.Load(home.Dir(c.ctx), nil)
	if err == nil {
		_, err = plugins.RecordShellHookConsentFromConfig(c.ctx, map[string]any(cfg), false)
	}
	if err != nil {
		cliLogger.Warn(fmt.Sprintf("shell-hook consent check failed: %v", err))
	}
}

// Runs a gateway attach without this CLI's own active session lease, then claims it again
func (c *CLI) leaseHandedOver(held bool, body func() error) (err error) {
	if !held {
		return body()
	}
	c.releaseActiveSession()
	finished := false
	defer func() {
		if !finished {
			c.claimActiveSession("cli", true)
		}
	}()
	err = body()
	finished = true
	if err != nil {
		c.claimActiveSession("cli", true)
		return err
	}
	if !c.claimActiveSession("cli", true) {
		return errors.New("session_already_owned")
	}
	return nil
}

// Creates or resumes this CLI's gateway session before the banner and first turn and adopts its id
func (c *CLI) ensureGatewaySession() error {
	store := c.sessionDB
	if c.gatewaySessionOpen || store == nil {
		return nil
	}
	launch := gatewayLaunch(c)
	held := c.resumed && c.activeSessionLease != nil
	recordShellHookConsent(c)
	return c.leaseHandedOver(held, func() error {
		var typedAPIKey, typedToolsets, explicitIgnoreRules any
		if c.typedAPIKey != "" {
			typedAPIKey = c.typedAPIKey
		}
		if c.typedToolsets != nil {
			typedToolsets = toAnyList(c.typedToolsets)
		}
		if c.explicitIgnoreRules {
			explicitIgnoreRules = true
		}
		sid, err := store.OpenSession(c.ctx, gatewaycli.OpenSessionCLI{SessionID: c.sessionID, Resumed: c.resumed,
			TypedAPIKey: typedAPIKey, TypedToolsets: typedToolsets, ExplicitIgnoreRules: explicitIgnoreRules}, launch)
		if err != nil {
			return err
		}
		c.gatewaySessionOpen = true
		tools.GatewayPluginToolsets = func() ([]tools.ConfigurableToolset, map[string]bool, bool) { return gatewayPluginToolsets(c) }
		adoptGatewayToolsets(c, store)
		bindGatewaySession(c, store.Connection)
		if sid != "" && sid != c.sessionID {
			c.uiMu.Lock()
			c.sessionID = sid
			c.uiMu.Unlock()
			c.writeTerminalBreadcrumb()
		}
		return nil
	})
}

// Id for a new or branched session, a session the gateway created once this CLI's gateway session is open
func (c *CLI) newSessionID(now time.Time, branchOf, title string) (string, error) {
	store := c.sessionDB
	if store == nil || !c.gatewaySessionOpen {
		return newSessionID(now), nil
	}
	if branchOf != "" {
		child, err := store.Branch(c.ctx, branchOf, title)
		if err != nil {
			return "", err
		}
		if tools.IsSessionYoloEnabled(branchOf) {
			if err := store.SetSessionYolo(c.ctx, child, true); err != nil {
				return "", err
			}
			tools.EnableSessionYolo(child)
		}
		return child, nil
	}
	launch := map[string]any{}
	for key, value := range gatewayLaunch(c) {
		if !gatewayRouteFlags[key] {
			launch[key] = value
		}
	}
	sid, err := store.Create(c.ctx, launch, "", false)
	if err != nil {
		return "", err
	}
	c.uiMu.Lock()
	c.sessionID = sid
	c.uiMu.Unlock()
	return sid, nil
}

// Closes the modal answering a server request the gateway withdrew, as if cancelled locally
func (c *CLI) withdrawGatewayModal(method string) {
	c.uiMu.Lock()
	clarify, sudo, secret := c.clarifyState, c.sudoState, c.secretState
	c.uiMu.Unlock()
	switch {
	case method == "clarify" && clarify != nil:
		c.clarifyTeardown()
		select {
		case clarify.response <- nil:
		default:
		}
	case (method == "sudo" || method == "vault.unlock_prompt" || method == "vault.save_login" || method == "vault.code") && sudo != nil:
		select {
		case sudo.response <- "":
		default:
		}
	case method == "secret" && secret != nil:
		c.cancelSecretCapture()
	}
}

// Asks before a guarded model target the owner refused, once per warning the user already confirmed
func (c *CLI) confirmOwnerModelSwitch(refusal map[string]any) bool {
	message := strOrEmpty(refusal["confirm_message"])
	confirmed := c.confirmedSwitchWarning
	c.confirmedSwitchWarning = ""
	if message != "" && message == confirmed {
		return true
	}
	return c.askSwitchAnyway(confirmationTitle(refusal), message)
}
