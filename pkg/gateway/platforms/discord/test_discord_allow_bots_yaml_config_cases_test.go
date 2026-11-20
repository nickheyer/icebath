package discord

import (
	"context"
	"testing"
)

func TestYamlAllowBotsIsSeededIntoExtra(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("DISCORD_ALLOW_BOTS", "")
	seeded, err := applyYAMLConfig(context.Background(), map[string]any{}, map[string]any{"allow_bots": "mentions"})
	if err != nil || seeded == nil || seeded["allow_bots"] != "mentions" {
		t.Fatalf("seeded %v err %v", seeded, err)
	}
}

func TestAdapterReadsAllowBotsFromExtraWithoutEnv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("DISCORD_ALLOW_BOTS", "")
	a := newTestAdapter(t, map[string]any{"allow_bots": "mentions"})
	snapshot(a, map[string]string{})
	if got := a.getAllowBots(context.Background()); got != "mentions" {
		t.Fatalf("allow bots %q", got)
	}
}
