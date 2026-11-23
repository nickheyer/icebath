package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

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
		for _, name := range resolveAutoLoadSkills(c.ctx) {
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

// Deduplicated names of the skills every session loads
func resolveAutoLoadSkills(ctx context.Context) []string {
	seen := map[string]bool{}
	var names []string
	for _, name := range settings.SkillsAutoLoad.Strings(ctx) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
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
		pkgLog().Warn(fmt.Sprintf("shell-hook consent check failed: %v", err))
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
		sid, err := store.OpenSession(c.ctx, c.gatewayOpenRequest(), launch)
		if err != nil {
			return err
		}
		c.gatewaySessionOpen = true
		tools.GatewayPluginToolsets = func() ([]tools.ConfigurableToolset, map[string]bool, bool) { return gatewayPluginToolsets(c) }
		adoptGatewayToolsets(c, store)
		if sid != "" && sid != c.sessionID {
			c.uiMu.Lock()
			c.sessionID = sid
			c.uiMu.Unlock()
			c.writeTerminalBreadcrumb()
		}
		return nil
	})
}

// Attributes of this CLI the store reads when it opens the gateway session
func (c *CLI) gatewayOpenRequest() gatewaycli.OpenSessionCLI {
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
	return gatewaycli.OpenSessionCLI{SessionID: c.sessionID, Resumed: c.resumed,
		TypedAPIKey: typedAPIKey, TypedToolsets: typedToolsets, ExplicitIgnoreRules: explicitIgnoreRules}
}
