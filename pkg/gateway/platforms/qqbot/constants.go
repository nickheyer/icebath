package qqbot

import "os"

const (
	QQBotVersion             = "1.1.0"
	APIBase                  = "https://api.sgroup.qq.com"
	TokenURL                 = "https://bots.qq.com/app/getAppAccessToken"
	GatewayURLPath           = "/gateway"
	OnboardCreatePath        = "/lite/create_bind_task"
	OnboardPollPath          = "/lite/poll_bind_result"
	QRURLTemplate            = "https://q.qq.com/qqbot/openclaw/connect.html?task_id={task_id}&_wv=2&source=hermes"
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

// PortalHost is QQ_PORTAL_HOST read once at load, q.qq.com when unset
var PortalHost = func() string {
	if v, ok := os.LookupEnv("QQ_PORTAL_HOST"); ok {
		return v
	}
	return "q.qq.com"
}()

// ReconnectBackoff lists the reconnect delays in seconds
var ReconnectBackoff = []int{2, 5, 10, 30, 60}
