package discord

import (
	"context"
	"maps"
	"os"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/tools"
)

var gateIsolationVars = []string{"DISCORD_ALLOWED_USERS", "DISCORD_ALLOWED_ROLES", "DISCORD_ALLOWED_CHANNELS", "DISCORD_IGNORED_CHANNELS",
	"DISCORD_NO_THREAD_CHANNELS", "DISCORD_FREE_RESPONSE_CHANNELS", "DISCORD_ALLOW_ALL_USERS", "DISCORD_ALLOW_BOTS",
	"GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS", "DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS"}

// Adapter with every gate env cleared and the given extra config
func gateAdapter(t *testing.T, extra map[string]any) *DiscordAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, v := range gateIsolationVars {
		t.Setenv(v, "")
	}
	return newTestAdapter(t, extra)
}

// Installs a gate env snapshot holding the given values and blanks for the rest
func snapshot(a *DiscordAdapter, values map[string]string) {
	snap := map[string]string{}
	for _, k := range gateEnvKeys {
		snap[k] = values[k]
	}
	a.gateEnvSnapshot = snap
}

func setOf(items ...string) strSet { return newStrSet(items...) }

func TestTwoAdapterChannelIsolationSnapshotsIsolateAllowedChannels(t *testing.T) {
	a, b := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_ALLOWED_CHANNELS": "111"})
	snapshot(b, map[string]string{"DISCORD_ALLOWED_CHANNELS": "222"})
	ctx := context.Background()
	if !maps.Equal(a.getAllowedChannels(ctx), setOf("111")) || !maps.Equal(b.getAllowedChannels(ctx), setOf("222")) {
		t.Fatal("channel gates leaked")
	}
}

func TestTwoAdapterChannelIsolationOrderIndependent(t *testing.T) {
	b := gateAdapter(t, nil)
	snapshot(b, map[string]string{"DISCORD_ALLOWED_CHANNELS": "222"})
	a := gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_ALLOWED_CHANNELS": "111"})
	ctx := context.Background()
	if !a.discordChannelIDsAllowed(ctx, setOf("111")) || a.discordChannelIDsAllowed(ctx, setOf("222")) ||
		!b.discordChannelIDsAllowed(ctx, setOf("222")) || b.discordChannelIDsAllowed(ctx, setOf("111")) {
		t.Fatal("order changed the winner")
	}
}

func TestTwoAdapterChannelIsolationExtrasIsolateAllowedChannelsWithoutSnapshot(t *testing.T) {
	a := gateAdapter(t, map[string]any{"allowed_channels": "111"})
	b := gateAdapter(t, map[string]any{"allowed_channels": "222"})
	ctx := context.Background()
	if !maps.Equal(a.getAllowedChannels(ctx), setOf("111")) || !maps.Equal(b.getAllowedChannels(ctx), setOf("222")) {
		t.Fatal("extras leaked")
	}
}

func TestTwoAdapterChannelIsolationProcessEnvDoesNotLeakIntoSnapshottedAdapter(t *testing.T) {
	b := gateAdapter(t, map[string]any{"allowed_channels": "222"})
	t.Setenv("DISCORD_ALLOWED_CHANNELS", "999")
	snapshot(b, map[string]string{"DISCORD_ALLOWED_CHANNELS": "222"})
	if !maps.Equal(b.getAllowedChannels(context.Background()), setOf("222")) {
		t.Fatal("process env leaked")
	}
}

func TestTwoAdapterChannelIsolationIgnoredChannelsIsolated(t *testing.T) {
	a, b := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_IGNORED_CHANNELS": "311"})
	snapshot(b, map[string]string{"DISCORD_IGNORED_CHANNELS": "322"})
	ctx := context.Background()
	if !maps.Equal(a.getIgnoredChannels(ctx), setOf("311")) || !maps.Equal(b.getIgnoredChannels(ctx), setOf("322")) {
		t.Fatal("ignored channels leaked")
	}
}

func TestTwoAdapterUserRoleIsolationAllowedUsersIsolated(t *testing.T) {
	a, b := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_ALLOWED_USERS": "1001,<@1002>"})
	snapshot(b, map[string]string{"DISCORD_ALLOWED_USERS": "2001"})
	ctx := context.Background()
	if !maps.Equal(a.getAllowedUsers(ctx), setOf("1001", "1002")) || !maps.Equal(b.getAllowedUsers(ctx), setOf("2001")) {
		t.Fatal("users leaked")
	}
}

func TestTwoAdapterUserRoleIsolationAllowedRolesIsolated(t *testing.T) {
	a, b := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_ALLOWED_ROLES": "31,32"})
	snapshot(b, map[string]string{"DISCORD_ALLOWED_ROLES": "41"})
	ctx := context.Background()
	ra, _ := a.getAllowedRoles(ctx)
	rb, _ := b.getAllowedRoles(ctx)
	if !maps.Equal(ra, setOf("31", "32")) || !maps.Equal(rb, setOf("41")) {
		t.Fatal("roles leaked")
	}
}

func TestTwoAdapterUserRoleIsolationIsAllowedUserEnforcesOwnList(t *testing.T) {
	a, b := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(a, map[string]string{"DISCORD_ALLOWED_USERS": "1001"})
	snapshot(b, map[string]string{"DISCORD_ALLOWED_USERS": "2001"})
	ctx := context.Background()
	a.allowedUserIDs = a.getAllowedUsers(ctx)
	b.allowedUserIDs = b.getAllowedUsers(ctx)
	if !a.isAllowedUser(ctx, "1001", nil, "", false, nil) || a.isAllowedUser(ctx, "2001", nil, "", false, nil) ||
		!b.isAllowedUser(ctx, "2001", nil, "", false, nil) || b.isAllowedUser(ctx, "1001", nil, "", false, nil) {
		t.Fatal("allowlists leaked")
	}
}

func TestAllowAllFlagIsolationDiscordAllowAllIsolated(t *testing.T) {
	open, closed := gateAdapter(t, nil), gateAdapter(t, nil)
	snapshot(open, map[string]string{"DISCORD_ALLOW_ALL_USERS": "true"})
	snapshot(closed, map[string]string{})
	ctx := context.Background()
	if !open.isAllowedUser(ctx, "555", nil, "", false, nil) || closed.isAllowedUser(ctx, "555", nil, "", false, nil) {
		t.Fatal("allow all leaked")
	}
}

func TestAllowAllFlagIsolationEnvAllowAllDoesNotOpenSnapshottedAdapter(t *testing.T) {
	b := gateAdapter(t, nil)
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	snapshot(b, map[string]string{})
	ctx := context.Background()
	if b.discordAllowAllUsers(ctx) || b.isAllowedUser(ctx, "555", nil, "", false, nil) {
		t.Fatal("env opened snapshotted adapter")
	}
}

func TestAllowAllFlagIsolationGatewayAllowAllIsolated(t *testing.T) {
	b := gateAdapter(t, nil)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	snapshot(b, map[string]string{})
	if b.gatewayAllowAllUsers(context.Background()) {
		t.Fatal("gateway allow all leaked")
	}
}

// Adapter allowlisting username teknium whose cached guild holds that member
func tekniumAdapter(t *testing.T) *DiscordAdapter {
	a := gateAdapter(t, nil)
	a.allowedUserIDs = setOf("teknium")
	attachClient(t, a, "1", nil)
	if err := a.client.State.GuildAdd(&discordgo.Guild{ID: "1", Name: "g", MemberCount: 1, Members: []*discordgo.Member{
		{User: &discordgo.User{ID: "12345", Username: "teknium", GlobalName: "teknium", Discriminator: "0"}}}}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUsernameResolutionEnvWriteNoEnvWriteWhenMultiplexActive(t *testing.T) {
	a := tekniumAdapter(t)
	t.Setenv("DISCORD_ALLOWED_USERS", "999")
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
	snapshot(a, map[string]string{"DISCORD_ALLOWED_USERS": "teknium"})
	a.resolveAllowedUsernames(context.Background())
	if !maps.Equal(a.allowedUserIDs, setOf("12345")) || a.gateEnvSnapshot["DISCORD_ALLOWED_USERS"] != "12345" || os.Getenv("DISCORD_ALLOWED_USERS") != "999" {
		t.Fatalf("allowed %v env %s", a.allowedUserIDs, os.Getenv("DISCORD_ALLOWED_USERS"))
	}
}

func TestUsernameResolutionEnvWriteEnvWritePreservedSingleProfile(t *testing.T) {
	a := tekniumAdapter(t)
	t.Setenv("DISCORD_ALLOWED_USERS", "teknium")
	tools.SetMultiplexActive(false)
	a.resolveAllowedUsernames(context.Background())
	if os.Getenv("DISCORD_ALLOWED_USERS") != "12345" {
		t.Fatalf("env %s", os.Getenv("DISCORD_ALLOWED_USERS"))
	}
}
