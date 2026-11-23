package discord

import (
	"context"
	"maps"
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

// Current role policy of the profile the request serves, read like ingress reads it
func (a *DiscordAdapter) nativeRolePolicy(ctx context.Context, source *gateway.SessionSource) (rolePolicy, error) {
	roles, err := a.getAllowedRoles(ctx)
	if err != nil {
		return rolePolicy{}, err
	}
	var p rolePolicy
	p.roles = slices.Sorted(maps.Keys(roles))
	if source.ChatType == "dm" {
		p.guildID = a.dmRoleAuthGuild(ctx)
		return p, nil
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
