package discord

import (
	"context"
	"math/big"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shortest wait in seconds a button view may be given
const DiscordPromptTimeoutMin = 30

// Longest wait in seconds a button view may be given, the longest a timer takes safely
var DiscordPromptTimeoutMax = int(tools.MaxSafeTimeoutSeconds)

// Server whose roles also authorize direct messages for this adapter's profile, nil while roles apply only inside servers
func (a *DiscordAdapter) dmRoleAuthGuild(ctx context.Context) *big.Int {
	raw := pystr.Strip(settings.DiscordDmRoleAuthGuild.Get(a.sctx(ctx)))
	if !isDigitString(raw) {
		return nil
	}
	guildID, ok := pyint.Parse(raw)
	if !ok || guildID.Sign() <= 0 {
		return nil
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
	switch pystr.Lower(pystr.Strip(pyrepr.Str(rawToggle))) {
	case "true", "1", "yes":
	default:
		return false, map[string]struct{}{}
	}
	return true, gateway.CoerceIDList(configExtra["allow_admin_from"])
}
