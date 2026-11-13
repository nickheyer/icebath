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

// Cast of the env value when set, parseable and positive, else the default
func positiveEnv[T int | float64](name string, def T, cast func(string) (T, bool)) T {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	val, ok := cast(raw)
	if !ok || !(val > 0) {
		return def
	}
	return val
}

// Python int of an env text that fits the platform int
func envInt(raw string) (int, bool) {
	n, ok := pyint.Parse(raw)
	if !ok || !n.IsInt64() || n.Int64() > math.MaxInt || n.Int64() < math.MinInt {
		return 0, false
	}
	return int(n.Int64()), true
}

// Python float of an env text
func envFloat(raw string) (float64, bool) {
	v, err := pyint.ParseFloat(raw)
	return v, err == nil
}

// Tighter keepalive limits for persistent platform adapter clients with their env overrides
func PlatformHTTPXLimits() HTTPXLimits {
	keep := positiveEnv("ICEBATH_GATEWAY_HTTPX_MAX_KEEPALIVE", defaultPlatformMaxKeepalive, envInt)
	expiry := positiveEnv("ICEBATH_GATEWAY_HTTPX_KEEPALIVE_EXPIRY", defaultPlatformKeepaliveExpiry, envFloat)
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
