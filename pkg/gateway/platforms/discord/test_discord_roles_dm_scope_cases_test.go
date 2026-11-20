package discord

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Adapter whose config names guild 222222 for DM role checks
func dmScopeAdapter(t *testing.T, users, roles []string) *DiscordAdapter {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("DISCORD_ALLOWED_CHANNELS", "")
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("discord:\n  dm_role_auth_guild: 222222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(t, nil)
	a.allowedUserIDs = newStrSet(users...)
	a.allowedRoleIDs = newStrSet(roles...)
	return a
}

// Caches a guild holding one member with the given roles
func addGuildMember(t *testing.T, s *discordgo.Session, guildID, memberID string, roles ...string) {
	if err := s.State.MemberAdd(&discordgo.Member{GuildID: guildID, User: &discordgo.User{ID: memberID}, Roles: roles}); err != nil {
		t.Fatal(err)
	}
}

func TestDmRejectsRoleHeldInOtherGuild(t *testing.T) {
	a := dmScopeAdapter(t, nil, []string{"5555"})
	s := attachClient(t, a, "1", []string{"111111", "222222"})
	addGuildMember(t, s, "111111", "42", "5555")
	if a.isAllowedUser(context.Background(), "42", nil, "", true, nil) {
		t.Fatal("role from another guild authorized a DM")
	}
}

func TestGuildMessageRejectsAuthorRolesFromDifferentGuild(t *testing.T) {
	a := dmScopeAdapter(t, nil, []string{"5555"})
	attachClient(t, a, "1", []string{"999", "222222"})
	foreign := &authorRoles{roles: []string{"5555"}, guildID: "999"}
	if a.isAllowedUser(context.Background(), "42", foreign, "222222", false, nil) {
		t.Fatal("foreign guild roles trusted")
	}
}

func TestUserIDAllowlistWorksInGuild(t *testing.T) {
	a := dmScopeAdapter(t, []string{"42"}, nil)
	if !a.isAllowedUser(context.Background(), "42", nil, "111", false, nil) {
		t.Fatal("allowlisted id rejected")
	}
}

func TestChannelAllowlistAuthorizesWithMatchingChannelContext(t *testing.T) {
	a := dmScopeAdapter(t, nil, nil)
	t.Setenv("DISCORD_ALLOWED_CHANNELS", "999")
	attachClient(t, a, "1", []string{"111111"})
	if !a.isAllowedUser(context.Background(), "42", nil, "111111", false, newStrSet("999")) {
		t.Fatal("channel context rejected")
	}
}
