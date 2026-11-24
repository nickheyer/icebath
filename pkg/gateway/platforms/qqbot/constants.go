package qqbot

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

const (
	QQBotVersion             = "1.1.0"
	APIBase                  = "https://api.sgroup.qq.com"
	SandboxAPIBase           = "https://sandbox.api.sgroup.qq.com"
	TokenURL                 = "https://bots.qq.com/app/getAppAccessToken"
	GatewayURLPath           = "/gateway"
	OnboardCreatePath        = "/lite/create_bind_task"
	OnboardPollPath          = "/lite/poll_bind_result"
	QRURLTemplate            = "https://q.qq.com/qqbot/openclaw/connect.html?task_id={task_id}&_wv=2&source=icebath"
	DefaultAPITimeout        = 30.0
	FileUploadTimeout        = 120.0
	ConnectTimeoutSeconds    = 20.0
	MaxReconnectAttempts     = 100
	RateLimitDelay           = 60
	QuickDisconnectThreshold = 5.0
	MaxQuickDisconnectCount  = 3
	OnboardPollInterval      = 2.0
	OnboardAPITimeout        = 10.0
	MaxMessageLength         = 4000
	DedupWindowSeconds       = 300
	DedupMaxSize             = 1000
	MsgTypeText              = 0
	MsgTypeMarkdown          = 2
	MsgTypeMedia             = 7
	MsgTypeInputNotify       = 6
	MediaTypeImage           = 1
	MediaTypeVideo           = 2
	MediaTypeVoice           = 3
	MediaTypeFile            = 4
)

// Host of the QQ Open Platform portal for the profile ctx serves, qqbot.portal_host
func portalHost(ctx context.Context) string {
	if host := strings.TrimSpace(settings.QqbotPortalHost.Get(gateway.SettingsContext(ctx))); host != "" {
		return host
	}
	return settings.QqbotPortalHost.Default
}

// Base address of the QQ API for the profile ctx serves, the sandbox servers when qqbot.sandbox is on
func apiBase(ctx context.Context) string {
	if settings.QqbotSandbox.Get(gateway.SettingsContext(ctx)) {
		return SandboxAPIBase
	}
	return APIBase
}

// ReconnectBackoff lists the reconnect delays in seconds
var ReconnectBackoff = []int{2, 5, 10, 30, 60}
