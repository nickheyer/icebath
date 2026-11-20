package slack

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func applyEnv(t *testing.T, cfg *gateway.GatewayConfig) {
	t.Helper()
	for _, step := range envOverrides {
		if err := step.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func emptyConfig(t *testing.T) *gateway.GatewayConfig {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
}

func TestSlackEnvHomeKeepsProvenanceOnlyForSameChat(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("SLACK_HOME_CHANNEL", "C0123456789")
	applyEnv(t, cfg)
	pc := cfg.Platforms[gateway.PlatformSlack]
	if pc == nil || pc.Enabled || pc.HomeChannel.ChatID != "C0123456789" || pc.HomeChannel.Name != "" || pc.HomeChannel.ThreadID != nil {
		t.Fatalf("created entry %+v", pc)
	}
	uid, sid := "U1", "T1"
	pc.HomeChannel.UserID, pc.HomeChannel.ScopeID = &uid, &sid
	t.Setenv("SLACK_HOME_CHANNEL_THREAD_ID", "171.5")
	t.Setenv("SLACK_HOME_CHANNEL_NAME", "General")
	applyEnv(t, cfg)
	h := cfg.Platforms[gateway.PlatformSlack].HomeChannel
	if h.UserID == nil || *h.UserID != "U1" || *h.ScopeID != "T1" || *h.ThreadID != "171.5" || h.Name != "General" {
		t.Fatalf("same chat home %+v", h)
	}
	t.Setenv("SLACK_HOME_CHANNEL", "C9999999999")
	applyEnv(t, cfg)
	if h := cfg.Platforms[gateway.PlatformSlack].HomeChannel; h.UserID != nil || h.ScopeID != nil {
		t.Fatalf("changed chat home %+v", h)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb")
	applyEnv(t, cfg)
	if pc := cfg.Platforms[gateway.PlatformSlack]; !pc.Enabled || pc.Token != "xoxb" {
		t.Fatalf("token config %+v", pc)
	}
}
