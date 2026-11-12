package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Guild click by the user carrying the given roles
func componentInteraction(userID string, roles ...string) *discordgo.Interaction {
	return &discordgo.Interaction{ChannelID: "555", GuildID: "999", Member: &discordgo.Member{User: &discordgo.User{ID: userID}, Roles: roles}}
}

// Adapter with every component gate env cleared
func componentAdapter(t *testing.T) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, v := range []string{"DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS", "DISCORD_ALLOWED_USERS"} {
		t.Setenv(v, "")
	}
	return newTestAdapter(t, nil)
}

func TestComponentCheckExplicitAllowAllPasses(t *testing.T) {
	for _, env := range [][2]string{{"DISCORD_ALLOW_ALL_USERS", "true"}, {"GATEWAY_ALLOW_ALL_USERS", "yes"}} {
		a := componentAdapter(t)
		t.Setenv(env[0], env[1])
		if !a.componentCheckAuth(context.Background(), componentInteraction("11111"), strSet{}, strSet{}, nil) {
			t.Fatalf("%s=%s rejected", env[0], env[1])
		}
	}
}

func TestSnapshotUserIsConfirmedWithTheLiveCheck(t *testing.T) {
	yes, no := true, false
	for _, snap := range []string{"11111", "*"} {
		for _, c := range []struct {
			verdict  *bool
			expected bool
		}{{nil, true}, {&yes, true}, {&no, false}} {
			a := componentAdapter(t)
			var seen []string
			live := func(i *discordgo.Interaction) *bool {
				seen = append(seen, interactionUser(i).ID)
				return c.verdict
			}
			if got := a.componentCheckAuth(context.Background(), componentInteraction("11111"), newStrSet(snap), strSet{}, live); got != c.expected || len(seen) != 1 || seen[0] != "11111" {
				t.Fatalf("snapshot %s verdict %v got %v seen %v", snap, c.verdict, got, seen)
			}
		}
	}
}
