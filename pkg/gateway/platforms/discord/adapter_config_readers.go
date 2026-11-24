package discord

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Shortest wait in seconds a button view may be given
const DiscordPromptTimeoutMin = 30

// Longest wait in seconds a button view may be given, the longest a timer takes safely
var DiscordPromptTimeoutMax = int(tools.MaxSafeTimeoutSeconds)

// Server whose roles also authorize direct messages for this adapter's profile, 0 while roles apply only inside servers
func (a *DiscordAdapter) dmRoleAuthGuild(ctx context.Context) int64 {
	raw := strings.TrimSpace(settings.DiscordDmRoleAuthGuild.Get(a.sctx(ctx)))
	if !isDigitString(raw) {
		return 0
	}
	guildID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || guildID <= 0 {
		return 0
	}
	return guildID
}

// Seconds a button view of this adapter's profile waits for an answer, kept between the shortest and the longest safe wait
func (a *DiscordAdapter) promptTimeoutSeconds(ctx context.Context) float64 {
	seconds := settings.ApprovalsDiscordPromptTimeout.Get(a.sctx(ctx)).Seconds()
	return min(max(seconds, DiscordPromptTimeoutMin), float64(DiscordPromptTimeoutMax))
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
	switch strings.ToLower(strings.TrimSpace(anyutil.String(rawToggle))) {
	case "true", "1", "yes":
	default:
		return false, map[string]struct{}{}
	}
	return true, gateway.CoerceIDList(configExtra["allow_admin_from"])
}
