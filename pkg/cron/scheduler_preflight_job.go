package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

var skillMissingFields = []struct{ key, format string }{
	{"missing_required_environment_variables", "env $%s"},
	{"missing_required_commands", "command '%s'"},
	{"missing_credential_files", "credential file %s"},
}

// Job and MCP server pairs already warned as reconnecting
var reconnectingWarned = struct {
	sync.Mutex
	keys map[[2]string]bool
}{keys: map[[2]string]bool{}}

// Seams tests replace the way pytest patches the resolver, skill view and MCP reconnect probe
var (
	preflightResolveRuntime = provider.ResolveRuntimeProvider
	preflightSkillView      = func(ctx context.Context, home string, cfg config.Tree, name string) string {
		return skills.NewScope(home, cfg).SkillView(ctx, name, "", "", true)
	}
	preflightResolveToolset  = func(ctx context.Context, name string) []string { return tools.Default.ResolveToolset(ctx, "", name) }
	preflightMCPReconnecting = mcp.ServerReconnecting
)

// Stripped cron.model_provider, empty when absent
func cronDefaultProvider(cfg config.Tree) string {
	cronCfg, ok := cfg["cron"].(map[string]any)
	if !ok || !pyTruthy(cronCfg["model_provider"]) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(cronCfg["model_provider"]))
}

// Missing provider credential reason, nil with a fallback chain, rate limiting or any other failure
func (s *Store) preflightCheckProviderKey(ctx context.Context, job *Job, cfg config.Tree) *string {
	if len(jobFallbackChain(job, cfg)) > 0 {
		return nil
	}
	requested := ""
	if job.Provider != nil && *job.Provider != "" {
		requested = *job.Provider
	} else {
		requested = cronDefaultProvider(cfg)
	}
	model := ""
	if job.Model != nil && *job.Model != "" {
		model = *job.Model
	} else {
		model = s.cronEnvSetting(ctx, "ICEBATH_MODEL", "")
	}
	baseURL := ""
	if job.BaseURL != nil {
		baseURL = *job.BaseURL
	}
	_, err := preflightResolveRuntime(ctx, s.home, requested, model, baseURL, "")
	var authErr *provider.AuthError
	if err == nil || !errors.As(err, &authErr) {
		return nil
	}
	if provider.IsRateLimitedAuthError(authErr) {
		return nil
	}
	msg := fmt.Sprintf("provider credential missing: %v %s. Set the provider API key in .env (or `icebath setup`) for that home, or pin a working provider via `icebath cron edit %s --provider <p>`.", authErr, s.credentialStoreScopeLabel(ctx), job.ID)
	return &msg
}

// Profile and home a credential lookup ran against
func (s *Store) credentialStoreScopeLabel(ctx context.Context) string {
	name := profiles.GetActiveProfileName(ctx)
	if name == "" {
		name = "default"
	}
	return fmt.Sprintf("[profile '%s', ICEBATH_HOME %s]", name, s.home)
}

// True when a primary profile route of this home names the platform
func (s *Store) deliveryPlatformRoutedFromPrimaryGateway(ctx context.Context, platform string) bool {
	key := strings.ToLower(platform)
	for _, route := range primaryProfileRoutesForCurrentHome(ctx) {
		if strings.ToLower(pyrepr.Str(route.Platform)) == key {
			return true
		}
	}
	return false
}

// Reason a delivery platform of either lane is unknown or unconnected, nil when deliverable
func (s *Store) preflightCheckDelivery(ctx context.Context, job *Job) *string {
	var deliverRaw any = "local"
	if job.Deliver != nil {
		deliverRaw = *job.Deliver
	}
	deliver := normalizeDeliverValue(deliverRaw)
	failureDeliver := normalizeDeliverValue(deliveryLaneValue(job, true))
	lanes := []string{deliver}
	if failureDeliver != deliver {
		lanes = append(lanes, failureDeliver)
	}
	var parts []string
	for _, lane := range lanes {
		for _, part := range strings.Split(lane, ",") {
			part = pystr.Strip(part)
			if part == "" || map[string]bool{"local": true, "origin": true, "all": true}[strings.ToLower(part)] {
				continue
			}
			if _, ok := parseBotChatDeliverToken(part); ok {
				continue
			}
			head, _, _ := strings.Cut(part, ":")
			parts = append(parts, pystr.Strip(head))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	var connected map[string]bool
	for _, platform := range parts {
		if !isKnownDeliveryPlatform(ctx, platform) {
			msg := fmt.Sprintf("delivery platform '%s' is not a known cron delivery target. Fix the job's `deliver` value or configure the platform's gateway credentials.", platform)
			return &msg
		}
		if connected == nil {
			cfg, err := loadGatewayConfig(ctx)
			if err != nil {
				s.log.Debug("preflight: gateway config unavailable — skipping delivery credential check")
				return nil
			}
			connected = map[string]bool{}
			for _, p := range cfg.GetConnectedPlatforms(ctx) {
				connected[string(p)] = true
			}
			for p := range s.relayFrontedConnectedPlatforms(ctx, connected) {
				connected[p] = true
			}
		}
		if !connected[strings.ToLower(platform)] && !s.deliveryPlatformRoutedFromPrimaryGateway(ctx, platform) {
			msg := fmt.Sprintf("delivery platform '%s' has no gateway credentials configured (not connected). Configure it via `icebath setup` or change the job's `deliver` target.", platform)
			return &msg
		}
	}
	return nil
}

// Reason an attached skill still needs setup, nil when every skill is ready or unreadable
func (s *Store) preflightCheckSkills(ctx context.Context, job *Job, cfg config.Tree) *string {
	for _, name := range jobSkillNames(job) {
		var payload map[string]any
		if json.Unmarshal([]byte(preflightSkillView(ctx, s.home, cfg, name)), &payload) != nil || payload == nil || !pyTruthy(payload["success"]) {
			continue
		}
		if !pyTruthy(payload["setup_needed"]) && payload["readiness_status"] != "setup_needed" {
			continue
		}
		var missing []string
		for _, f := range skillMissingFields {
			list, _ := payload[f.key].([]any)
			for _, item := range list {
				missing = append(missing, fmt.Sprintf(f.format, pyrepr.Str(item)))
			}
		}
		detail := strings.Join(missing, ", ")
		if detail == "" {
			detail = "required setup incomplete"
		}
		msg := fmt.Sprintf("attached skill '%s' is not ready: missing %s. Provide the missing prerequisites or detach the skill from this job.", name, detail)
		return &msg
	}
	return nil
}

// Reason enabled MCP toolsets resolved to zero tools, reconnecting servers warned once and let through
func (s *Store) emptyRequestedMCPToolsets(ctx context.Context, job *Job, cfg config.Tree) string {
	if len(job.EnabledToolsets) == 0 {
		return ""
	}
	enabled := tools.EnabledMCPServerNames(cfg, nil)
	var missing []string
	for _, name := range job.EnabledToolsets {
		if enabled[name] && len(preflightResolveToolset(ctx, name)) == 0 {
			missing = append(missing, name)
		}
	}
	var reconnecting []string
	for _, name := range missing {
		if preflightMCPReconnecting(ctx, name) {
			reconnecting = append(reconnecting, name)
		}
	}
	sort.Strings(reconnecting)
	jobID := jobIDOr(job, "?")
	reconnectingWarned.Lock()
	for key := range reconnectingWarned.keys {
		if key[0] == jobID && !containsString(reconnecting, key[1]) {
			delete(reconnectingWarned.keys, key)
		}
	}
	var unwarned []string
	for _, name := range reconnecting {
		if !reconnectingWarned.keys[[2]string{jobID, name}] {
			unwarned = append(unwarned, name)
		}
	}
	for _, name := range unwarned {
		reconnectingWarned.keys[[2]string{jobID, name}] = true
	}
	reconnectingWarned.Unlock()
	if len(unwarned) > 0 {
		s.log.Warn(fmt.Sprintf("Job '%s': MCP server(s) %s named in enabled_toolsets are reconnecting — running without their tools until they recover (a server parked on a permanent error blocks the job instead)", jobID, strings.Join(unwarned, ", ")))
	}
	var still []string
	for _, name := range missing {
		if !containsString(reconnecting, name) {
			still = append(still, name)
		}
	}
	if len(still) == 0 {
		return ""
	}
	sort.Strings(still)
	return fmt.Sprintf("MCP server(s) %s named in this job's enabled_toolsets resolved to zero tools for this profile (never connected for this profile, or connected for another profile only). If the server is only temporarily unreachable this clears by itself — the check re-runs on every dispatch and the job resumes once the server reconnects. If the name is wrong or belongs to another profile, fix the server or remove it from the job's toolsets.", strings.Join(still, ", "))
}

// First failing config check reason, a panicking check fails open
func (s *Store) preflightJobConfig(ctx context.Context, job *Job, cfg config.Tree) (string, error) {
	checks := []struct {
		name string
		run  func() *string
	}{
		{"provider_key", func() *string { return s.preflightCheckProviderKey(ctx, job, cfg) }},
		{"skills", func() *string { return s.preflightCheckSkills(ctx, job, cfg) }},
		{"delivery", func() *string { return s.preflightCheckDelivery(ctx, job) }},
	}
	for _, c := range checks {
		reason := func() (r *string) {
			defer func() {
				if recover() != nil {
					s.log.Debug(fmt.Sprintf("preflight check %s raised — failing open", c.name))
					r = nil
				}
			}()
			return c.run()
		}()
		if reason != nil && *reason != "" {
			return *reason, nil
		}
	}
	return "", nil
}
