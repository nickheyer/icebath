package qqbot

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func applyQQEnv(t *testing.T) *gateway.PlatformConfig {
	t.Helper()
	cfg := &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
	for _, step := range envOverrides {
		if err := step.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	return cfg.Platforms[gateway.PlatformQQBot]
}

func clearQQEnv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"QQ_APP_ID", "QQ_CLIENT_SECRET", "QQ_ALLOWED_USERS", "QQ_GROUP_ALLOWED_USERS",
		"QQBOT_HOME_CHANNEL", "QQBOT_HOME_CHANNEL_NAME", "QQBOT_HOME_CHANNEL_THREAD_ID", "QQ_HOME_CHANNEL",
		"QQ_HOME_CHANNEL_NAME", "QQ_HOME_CHANNEL_THREAD_ID"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQQEnvCredentialsEnableAndFillExtra(t *testing.T) {
	clearQQEnv(t)
	if pc := applyQQEnv(t); pc != nil {
		t.Fatalf("no credential created qqbot: %+v", pc)
	}
	t.Setenv("QQ_APP_ID", "app")
	if pc := applyQQEnv(t); pc == nil || !pc.Enabled || pc.Extra["app_id"] != "app" || pc.Extra["client_secret"] != nil {
		t.Fatalf("either credential of the one group enables qqbot: %+v", pc)
	}
	t.Setenv("QQ_CLIENT_SECRET", "sec")
	t.Setenv("QQ_ALLOWED_USERS", " u1,u2 ")
	t.Setenv("QQ_GROUP_ALLOWED_USERS", " g1 ")
	t.Setenv("QQBOT_HOME_CHANNEL", " chat ")
	pc := applyQQEnv(t)
	if pc == nil || !pc.Enabled || pc.Extra["app_id"] != "app" || pc.Extra["client_secret"] != "sec" ||
		pc.Extra["allow_from"] != "u1,u2" || pc.Extra["group_allow_from"] != "g1" {
		t.Fatalf("qqbot config %+v", pc)
	}
	h := pc.HomeChannel
	if h == nil || h.Platform != gateway.PlatformQQBot || h.ChatID != "chat" || h.Name != "Home" || h.ThreadID != nil {
		t.Fatalf("home %+v", h)
	}
}

func TestQQEnvDeprecatedHomeNamesWarnAndResolve(t *testing.T) {
	clearQQEnv(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	t.Setenv("QQ_APP_ID", "app")
	t.Setenv("QQ_CLIENT_SECRET", "sec")
	t.Setenv("QQ_HOME_CHANNEL", " old ")
	t.Setenv("QQ_HOME_CHANNEL_NAME", "Old Name")
	t.Setenv("QQ_HOME_CHANNEL_THREAD_ID", "th")
	h := applyQQEnv(t).HomeChannel
	if h == nil || h.ChatID != "old" || h.Name != "Old Name" || h.ThreadID == nil || *h.ThreadID != "th" {
		t.Fatalf("home %+v", h)
	}
	if !strings.Contains(logs.String(), "QQ_HOME_CHANNEL is deprecated; rename to QQBOT_HOME_CHANNEL") {
		t.Fatalf("no deprecation warning in %q", logs.String())
	}
	t.Setenv("QQBOT_HOME_CHANNEL_NAME", "New Name")
	t.Setenv("QQBOT_HOME_CHANNEL_THREAD_ID", "nt")
	if h = applyQQEnv(t).HomeChannel; h.Name != "New Name" || *h.ThreadID != "nt" {
		t.Fatalf("renamed names lost %+v", h)
	}
	logs.Reset()
	t.Setenv("QQBOT_HOME_CHANNEL", "new")
	if h = applyQQEnv(t).HomeChannel; h.ChatID != "new" || logs.Len() != 0 {
		t.Fatalf("home %+v logs %q", h, logs.String())
	}
}

func TestQQAuthzAndEnableCredentialRows(t *testing.T) {
	if len(envEnableCredentials) != 2 || envEnableCredentials[0] != "QQ_APP_ID" || envEnableCredentials[1] != "QQ_CLIENT_SECRET" {
		t.Fatalf("enable credentials %v", envEnableCredentials)
	}
	if authz["allowed_users_env"] != "QQ_ALLOWED_USERS" || authz["allow_all_env"] != "QQ_ALLOW_ALL_USERS" ||
		authz["group_allowed_chats_env"] != "QQ_GROUP_ALLOWED_USERS" || authz["allowlist_suppresses_pairing"] != true {
		t.Fatalf("authz %v", authz)
	}
}
