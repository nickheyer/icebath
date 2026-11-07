package gateway

import (
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

const (
	defaultPlatformKeepaliveExpiry = 2.0
	defaultPlatformMaxKeepalive    = 10
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

// Env integer when set, parseable and positive, else the default
func positiveEnvInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, ok := pyint.Parse(raw)
	if !ok || n.Sign() <= 0 || !n.IsInt64() || n.Int64() > math.MaxInt {
		return def
	}
	return int(n.Int64())
}

// Env float when set, parseable and positive, else the default
func positiveEnvFloat(name string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := pyint.ParseFloat(raw)
	if err != nil || !(v > 0) {
		return def
	}
	return v
}

// Tighter keepalive limits for persistent platform adapter clients with their env overrides
func PlatformHTTPXLimits() HTTPXLimits {
	keep := positiveEnvInt("ICEBATH_GATEWAY_HTTPX_MAX_KEEPALIVE", defaultPlatformMaxKeepalive)
	expiry := positiveEnvFloat("ICEBATH_GATEWAY_HTTPX_KEEPALIVE_EXPIRY", defaultPlatformKeepaliveExpiry)
	return HTTPXLimits{MaxKeepaliveConnections: &keep, KeepaliveExpiry: &expiry}
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
