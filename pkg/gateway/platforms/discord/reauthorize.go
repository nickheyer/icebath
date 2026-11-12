package discord

import (
	"context"
	"math/big"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Role allowlist and guild a recovered grant is checked against
type rolePolicy struct {
	roles   []string
	guildID *big.Int
}

func (p rolePolicy) equal(o rolePolicy) bool {
	if !slices.Equal(p.roles, o.roles) || (p.guildID == nil) != (o.guildID == nil) {
		return false
	}
	return p.guildID == nil || p.guildID.Cmp(o.guildID) == 0
}

// Current transport scoped role policy resolved like ingress from the config the loader builds now
func (a *DiscordAdapter) nativeRolePolicy(ctx context.Context, source *gateway.SessionSource) (rolePolicy, error) {
	gcfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		return rolePolicy{}, err
	}
	current := gcfg.Platforms[gateway.PlatformDiscord]
	if current == nil {
		current = gateway.NewPlatformConfig()
	}
	seen := map[string]bool{}
	var p rolePolicy
	for item := range gateCSVSet(a.gateRaw(ctx, "allowed_roles", "DISCORD_ALLOWED_ROLES", current)) {
		item = pystr.Strip(item)
		if !isDigitString(item) {
			continue
		}
		if n, ok := pyint.Parse(item); ok && !seen[n.String()] {
			seen[n.String()] = true
			p.roles = append(p.roles, n.String())
		}
	}
	slices.Sort(p.roles)
	if source.ChatType == "dm" {
		p.guildID, err = ReadDMRoleAuthGuild(ctx)
		return p, err
	}
	if source.ScopeID != nil {
		if n, ok := pyint.Parse(pystr.Strip(*source.ScopeID)); ok {
			p.guildID = n
		} else {
			p.guildID = big.NewInt(-1)
		}
	}
	return p, nil
}

// Rechecks a role granted session against the member's live roles under the current role policy
func (a *DiscordAdapter) ReauthorizeNativeRoles(ctx context.Context, source *gateway.SessionSource) (any, error) {
	policy, err := a.nativeRolePolicy(ctx, source)
	if err != nil {
		return nil, err
	}
	client := a.liveClient()
	if len(policy.roles) == 0 || policy.guildID == nil || client == nil {
		return false, nil
	}
	if policy.guildID.Sign() < 0 || source.UserID == nil {
		return false, nil
	}
	uid, ok := pyint.Parse(pystr.Strip(*source.UserID))
	if !ok {
		return false, nil
	}
	guild := a.stateGuild(policy.guildID.String())
	if guild == nil {
		return false, nil
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	member, err := client.GuildMember(guild.ID, uid.String(), discordgo.WithContext(fctx))
	cancel()
	if err != nil {
		return false, nil
	}
	recheck, err := a.nativeRolePolicy(gateway.ProfileRuntimeScope(ctx, home.Dir(ctx), true), source)
	if err != nil {
		return nil, err
	}
	if a.liveClient() != client || !recheck.equal(policy) {
		return false, nil
	}
	if member.User == nil || member.User.ID != uid.String() || member.GuildID != policy.guildID.String() {
		return false, nil
	}
	for _, role := range member.Roles {
		if n, ok := pyint.Parse(role); ok && slices.Contains(policy.roles, n.String()) {
			return true, nil
		}
	}
	return false, nil
}
