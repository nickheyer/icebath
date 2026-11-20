package discord

import (
	"context"
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

var allowedMentionEnvVars = []string{"DISCORD_ALLOW_MENTION_EVERYONE", "DISCORD_ALLOW_MENTION_ROLES", "DISCORD_ALLOW_MENTION_USERS", "DISCORD_ALLOW_MENTION_REPLIED_USER"}

// Clears every allow mention env so only the defaults apply
func clearAllowedMentionEnv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range allowedMentionEnvVars {
		t.Setenv(name, "")
	}
}

func mentionAllows(am *discordgo.MessageAllowedMentions, kind discordgo.AllowedMentionType) bool {
	return slices.Contains(am.Parse, kind)
}

func TestSafeDefaultsBlockEveryoneAndRoles(t *testing.T) {
	clearAllowedMentionEnv(t)
	am, err := buildAllowedMentions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mentionAllows(am, discordgo.AllowedMentionTypeEveryone) {
		t.Fatal("default must NOT allow @everyone/@here pings")
	}
	if mentionAllows(am, discordgo.AllowedMentionTypeRoles) {
		t.Fatal("default must NOT allow role pings")
	}
	if !mentionAllows(am, discordgo.AllowedMentionTypeUsers) {
		t.Fatal("default must allow user pings so replies work")
	}
	if !am.RepliedUser {
		t.Fatal("default must allow reply-reference pings")
	}
}

func TestEnvVarOptsBackIntoEveryone(t *testing.T) {
	clearAllowedMentionEnv(t)
	t.Setenv("DISCORD_ALLOW_MENTION_EVERYONE", "true")
	am, err := buildAllowedMentions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !mentionAllows(am, discordgo.AllowedMentionTypeEveryone) || mentionAllows(am, discordgo.AllowedMentionTypeRoles) ||
		!mentionAllows(am, discordgo.AllowedMentionTypeUsers) || !am.RepliedUser {
		t.Fatalf("unexpected mentions %+v", am)
	}
}
