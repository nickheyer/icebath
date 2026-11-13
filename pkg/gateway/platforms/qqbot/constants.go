package qqbot

import (
	"os"
	"time"
)

const (
	adapterVersion        = "1.1.0"
	apiBaseProduction     = "https://api.sgroup.qq.com"
	apiBaseSandbox        = "https://sandbox.api.sgroup.qq.com"
	tokenURL              = "https://bots.qq.com/app/getAppAccessToken"
	gatewayURLPath        = "/gateway"
	onboardCreatePath     = "/lite/create_bind_task"
	onboardPollPath       = "/lite/poll_bind_result"
	qrURLBase             = "https://q.qq.com/qqbot/openclaw/connect.html"
	defaultAPITimeout     = 30 * time.Second
	fileUploadTimeout     = 120 * time.Second
	connectTimeout        = 20 * time.Second
	maxReconnectAttempts  = 100
	rateLimitDelay        = 60 * time.Second
	quickDisconnectWindow = 5 * time.Second
	maxQuickDisconnects   = 3
	onboardPollInterval   = 2 * time.Second
	onboardAPITimeout     = 10 * time.Second
	maxMessageLength      = 4000
	dedupWindowSeconds    = 300
	dedupMaxSize          = 1000

	msgTypeText        = 0
	msgTypeMarkdown    = 2
	msgTypeInputNotify = 6
	msgTypeMedia       = 7

	mediaTypeImage = 1
	mediaTypeVideo = 2
	mediaTypeVoice = 3
	mediaTypeFile  = 4
)

// Delays between gateway reconnect attempts, the last one repeating
var reconnectBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

// QQ_PORTAL_HOST when set, else the q.qq.com portal
func portalHost() string {
	if v, ok := os.LookupEnv("QQ_PORTAL_HOST"); ok {
		return v
	}
	return "q.qq.com"
}
