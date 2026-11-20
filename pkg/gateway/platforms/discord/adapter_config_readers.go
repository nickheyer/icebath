package discord

import (
	"context"
	"errors"
	"math/big"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	DiscordPromptTimeoutDefault = 300
	DiscordPromptTimeoutMin     = 30
)

var DiscordPromptTimeoutMax = int(tools.MaxSafeTimeoutSeconds)

// Raw config.yaml of the home in effect, replaced by tests that patch read_raw_config
var readRawConfig = func(ctx context.Context) (map[string]any, error) {
	return config.ReadRawConfig(ctx, home.Dir(ctx))
}

// Section of the raw config, ok false where Python's dict access would raise
func rawSection(ctx context.Context, name string) (map[string]any, bool) {
	cfg, err := readRawConfig(ctx)
	if err != nil {
		return nil, false
	}
	section, present := cfg[name]
	if !present || !pyrepr.Truthy(section) {
		return map[string]any{}, true
	}
	m, ok := section.(map[string]any)
	return m, ok
}

// Python int of a config value, ok false for the TypeError and ValueError the readers catch
func configInt(raw any) (*big.Int, bool, error) {
	n, err := pyint.Of(raw)
	if err == nil {
		return n, true, nil
	}
	var pe *pyint.Error
	if errors.As(err, &pe) && pe.Kind == "OverflowError" {
		return nil, false, errors.New("OverflowError: " + pe.Msg)
	}
	return nil, false, nil
}

// Guild opted in to DM role based auth from discord.dm_role_auth_guild, nil for the secure default
func ReadDMRoleAuthGuild(ctx context.Context) (*big.Int, error) {
	section, ok := rawSection(ctx, "discord")
	if !ok {
		return nil, nil
	}
	raw := section["dm_role_auth_guild"]
	if raw == nil || raw == "" {
		return nil, nil
	}
	guildID, ok, err := configInt(raw)
	if err != nil || !ok {
		return nil, err
	}
	if guildID.Sign() <= 0 {
		return nil, nil
	}
	return guildID, nil
}

// Button view timeout from approvals.discord_prompt_timeout clamped to the min and safe max
func ReadDiscordPromptTimeout(ctx context.Context) (int, error) {
	section, ok := rawSection(ctx, "approvals")
	if !ok {
		return DiscordPromptTimeoutDefault, nil
	}
	raw := section["discord_prompt_timeout"]
	if raw == nil || raw == "" {
		return DiscordPromptTimeoutDefault, nil
	}
	seconds, ok, err := configInt(raw)
	if err != nil {
		return 0, err
	}
	if !ok {
		return DiscordPromptTimeoutDefault, nil
	}
	if seconds.Cmp(big.NewInt(DiscordPromptTimeoutMin)) < 0 {
		return DiscordPromptTimeoutMin, nil
	}
	if seconds.Cmp(big.NewInt(int64(DiscordPromptTimeoutMax))) > 0 {
		return DiscordPromptTimeoutMax, nil
	}
	return int(seconds.Int64()), nil
}

// Whether exec approval buttons are admin only and the admin ids allowed to click, empty when gated with none
func ResolveExecApprovalAdminGate(configExtra map[string]any) (bool, map[string]struct{}) {
	if configExtra == nil {
		configExtra = map[string]any{}
	}
	rawToggle, ok := configExtra["require_admin_for_exec_approval"]
	if !ok {
		rawToggle = false
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(rawToggle))) {
	case "true", "1", "yes":
	default:
		return false, map[string]struct{}{}
	}
	return true, gateway.CoerceIDList(configExtra["allow_admin_from"])
}
