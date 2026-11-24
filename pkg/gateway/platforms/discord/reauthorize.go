package discord

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Role allowlist and guild a recovered grant is checked against
type rolePolicy struct {
	roles []string
	// Guild the grant is checked in, 0 when there is none and negative when the source names one that is no id
	guildID int64
}

func (p rolePolicy) equal(o rolePolicy) bool {
	return slices.Equal(p.roles, o.roles) && p.guildID == o.guildID
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
		if n, err := strconv.ParseInt(strings.TrimSpace(*source.ScopeID), 10, 64); err == nil {
			p.guildID = n
		} else {
			p.guildID = -1
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
	if len(policy.roles) == 0 || policy.guildID <= 0 || client == nil || source.UserID == nil {
		return false, nil
	}
	guildID := strconv.FormatInt(policy.guildID, 10)
	uid, err := strconv.ParseInt(strings.TrimSpace(*source.UserID), 10, 64)
	if err != nil {
		return false, nil
	}
	guild := a.stateGuild(guildID)
	if guild == nil {
		return false, nil
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	member, err := client.GuildMember(guild.ID, strconv.FormatInt(uid, 10), discordgo.WithContext(fctx))
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
	if member.User == nil || member.User.ID != strconv.FormatInt(uid, 10) || member.GuildID != guildID {
		return false, nil
	}
	for _, role := range member.Roles {
		if n, err := strconv.ParseInt(strings.TrimSpace(role), 10, 64); err == nil && slices.Contains(policy.roles, strconv.FormatInt(n, 10)) {
			return true, nil
		}
	}
	return false, nil
}
