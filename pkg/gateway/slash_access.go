package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var alwaysAllowedForUsers = map[string]struct{}{"help": {}, "whoami": {}}

var slashDMChatTypes = map[string]struct{}{"dm": {}, "direct": {}, "private": {}, "": {}}

var slashScopeKeys = map[string][2]string{
	"dm":    {"allow_admin_from", "user_allowed_commands"},
	"group": {"group_allow_admin_from", "group_user_allowed_commands"},
}

// Resolved slash command gating of one platform and scope
type SlashAccessPolicy struct {
	Enabled             bool
	AdminUserIDs        map[string]struct{}
	UserAllowedCommands map[string]struct{}
}

func slashUserText(userID any) (string, bool) {
	if p, ok := userID.(*string); ok {
		if p == nil {
			return "", false
		}
		userID = *p
	}
	if !pyrepr.Truthy(userID) {
		return "", false
	}
	return pyrepr.Str(userID), true
}

// True for everyone while gating is off, otherwise only for listed admin ids
func (p SlashAccessPolicy) IsAdmin(userID any) bool {
	if !p.Enabled {
		return true
	}
	id, ok := slashUserText(userID)
	if !ok {
		return false
	}
	_, admin := p.AdminUserIDs[id]
	return admin
}

// Admins run anything, other users the help and whoami floor plus the allowed command list
func (p SlashAccessPolicy) CanRun(userID any, canonicalCmd string) bool {
	if p.IsAdmin(userID) {
		return true
	}
	if canonicalCmd == "" {
		return false
	}
	if _, ok := alwaysAllowedForUsers[canonicalCmd]; ok {
		return true
	}
	_, ok := p.UserAllowedCommands[canonicalCmd]
	return ok
}

var disabledSlashPolicy = SlashAccessPolicy{Enabled: false, AdminUserIDs: map[string]struct{}{}, UserAllowedCommands: map[string]struct{}{}}

var failClosedSlashPolicy = SlashAccessPolicy{Enabled: true, AdminUserIDs: map[string]struct{}{}, UserAllowedCommands: map[string]struct{}{}}

// Stripped non empty strings of a YAML list, comma string or scalar with normalize applied to each
func coerceSlashList(raw any, normalize func(string) string) map[string]struct{} {
	out := map[string]struct{}{}
	var items []any
	switch v := raw.(type) {
	case nil:
		return out
	case []any:
		items = v
	case pyrepr.Tuple:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if pystr.Strip(s) != "" {
				items = append(items, s)
			}
		}
	default:
		items = []any{raw}
	}
	for _, it := range items {
		if s := normalize(pystr.Strip(pyrepr.Str(it))); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

// Admin or user id list as a set of strings
func coerceIDList(raw any) map[string]struct{} {
	return coerceSlashList(raw, func(s string) string { return s })
}

// Admin or user id list as a set of strings, exported for the adapters that read allow_admin_from
func CoerceIDList(raw any) map[string]struct{} { return coerceIDList(raw) }

// Command allowlist with leading slashes dropped and names lowercased like resolve_command stores them
func coerceCommandList(raw any) map[string]struct{} {
	return coerceSlashList(raw, func(s string) string { return pystr.Lower(strings.TrimLeft(s, "/")) })
}

// Policy of one scope from a platform extra dict, the DM command list falling back to the group list
func PolicyFromExtra(extra map[string]any, scope string) SlashAccessPolicy {
	keys, ok := slashScopeKeys[scope]
	if !ok {
		keys = slashScopeKeys["dm"]
	}
	adminIDs := coerceIDList(extra[keys[0]])
	cmds := coerceCommandList(extra[keys[1]])
	if scope == "dm" && len(cmds) == 0 {
		cmds = coerceCommandList(extra["group_user_allowed_commands"])
	}
	return SlashAccessPolicy{Enabled: len(adminIDs) > 0, AdminUserIDs: adminIDs, UserAllowedCommands: cmds}
}

// Slash gating policy for a source, a blank chat type resolving to whichever scope is gated
func PolicyForSource(gatewayConfig *GatewayConfig, source *SessionSource) SlashAccessPolicy {
	if gatewayConfig == nil || source == nil {
		return disabledSlashPolicy
	}
	extra := map[string]any{}
	if pc := gatewayConfig.Platforms[source.Platform]; pc != nil && pc.Extra != nil {
		extra = pc.Extra
	}
	if normalized := pystr.Lower(pystr.Strip(source.ChatType)); normalized != "" {
		scope := "group"
		if _, dm := slashDMChatTypes[normalized]; dm {
			scope = "dm"
		}
		return PolicyFromExtra(extra, scope)
	}
	dmPolicy, groupPolicy := PolicyFromExtra(extra, "dm"), PolicyFromExtra(extra, "group")
	if dmPolicy.Enabled && !groupPolicy.Enabled {
		return dmPolicy
	}
	return groupPolicy
}

// Runner state the profile aware slash policy reads
type SlashAccessRunner interface {
	RunnerConfig() *GatewayConfig
	PrimaryProfileName() string
	ProfileConfigs() map[string]*GatewayConfig
}

// Slash policy from the profile whose bot received the source, failing closed for an uncached served profile
func PolicyForRunnerSource(runner SlashAccessRunner, source *SessionSource) SlashAccessPolicy {
	var config *GatewayConfig
	if runner != nil {
		config = runner.RunnerConfig()
	}
	if config == nil || config.MultiplexProfiles == nil || !*config.MultiplexProfiles || source == nil {
		return PolicyForSource(config, source)
	}
	owner := ""
	if identity := IdentityOf(source); identity != nil {
		owner = identity.TransportProfile
	} else if source.Profile != nil {
		owner = *source.Profile
	}
	primary := runner.PrimaryProfileName()
	if primary == "" {
		primary = "default"
	}
	if owner != "" && owner != primary {
		config = runner.ProfileConfigs()[owner]
		if config == nil {
			return failClosedSlashPolicy
		}
	}
	return PolicyForSource(config, source)
}
