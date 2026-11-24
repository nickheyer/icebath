package gateway

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Keepalive pool bounds of an HTTP client, nil fields meaning unbounded as in httpx Limits
type HTTPXLimits struct {
	MaxConnections          *int
	MaxKeepaliveConnections *int
	KeepaliveExpiry         *float64
}

// The httpx default pool of 100 connections, 20 kept alive for five seconds
func DefaultHTTPXLimits() HTTPXLimits {
	maxConns, keep, expiry := 100, 20, 5.0
	return HTTPXLimits{MaxConnections: &maxConns, MaxKeepaliveConnections: &keep, KeepaliveExpiry: &expiry}
}

// Tighter keepalive limits for persistent chat app clients of the profile ctx serves
func PlatformHTTPXLimits(ctx context.Context) HTTPXLimits {
	sctx := SettingsContext(ctx)
	keep := settings.GatewayHTTPMaxKeepalive.Get(sctx)
	expiry := settings.GatewayHTTPKeepaliveExpiry.Get(sctx)
	if expiry <= 0 {
		expiry = settings.GatewayHTTPKeepaliveExpiry.Default
	}
	seconds := expiry.Seconds()
	return HTTPXLimits{MaxKeepaliveConnections: &keep, KeepaliveExpiry: &seconds}
}

// Applies the pool bounds to a transport, unbounded fields lifting the net/http per host defaults
func applyHTTPXLimits(t *http.Transport, l HTTPXLimits) {
	t.MaxConnsPerHost = 0
	if l.MaxConnections != nil {
		t.MaxConnsPerHost = *l.MaxConnections
	}
	t.MaxIdleConns, t.MaxIdleConnsPerHost = 0, math.MaxInt32
	if l.MaxKeepaliveConnections != nil {
		t.MaxIdleConns, t.MaxIdleConnsPerHost = *l.MaxKeepaliveConnections, *l.MaxKeepaliveConnections
	}
	t.IdleConnTimeout = 0
	if l.KeepaliveExpiry != nil && !math.IsInf(*l.KeepaliveExpiry, 1) {
		t.IdleConnTimeout = time.Duration(*l.KeepaliveExpiry * float64(time.Second))
	}
}
