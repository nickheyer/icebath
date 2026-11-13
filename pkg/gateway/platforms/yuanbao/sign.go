package yuanbao

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	signTokenPath        = "/api/v5/robotLogic/sign-token"
	signRetryableCode    = "10099"
	signMaxRetries       = 3
	signRetryDelay       = time.Second
	signRefreshMargin    = 60 * time.Second
	signHTTPTimeout      = 10 * time.Second
	signDefaultLifetimeS = 3600
)

// Websocket credentials a sign token request returns
type signToken struct {
	Token, BotID, Source string
	ExpireAt             time.Time
}

// Process wide token cache shared by every adapter, one refresh at a time per app key
var signCache = struct {
	mu      sync.Mutex
	entries map[string]signToken
	locks   map[string]*sync.Mutex
}{entries: map[string]signToken{}, locks: map[string]*sync.Mutex{}}

func refreshLock(appKey string) *sync.Mutex {
	signCache.mu.Lock()
	defer signCache.mu.Unlock()
	l := signCache.locks[appKey]
	if l == nil {
		l = &sync.Mutex{}
		signCache.locks[appKey] = l
	}
	return l
}

// Cached token still valid past the refresh margin, expired entries purged first
func cachedToken(appKey string) (signToken, bool) {
	signCache.mu.Lock()
	defer signCache.mu.Unlock()
	now := time.Now()
	for k, v := range signCache.entries {
		if !v.ExpireAt.After(now) {
			delete(signCache.entries, k)
		}
	}
	t, ok := signCache.entries[appKey]
	return t, ok && t.ExpireAt.Sub(now) > signRefreshMargin
}

// Token from the cache while valid, else freshly signed
func getSignToken(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (signToken, error) {
	if t, ok := cachedToken(appKey); ok {
		slog.Info(fmt.Sprintf("[Yuanbao] Using cached token (%ds remaining)", int(time.Until(t.ExpireAt).Seconds())))
		return t, nil
	}
	l := refreshLock(appKey)
	l.Lock()
	defer l.Unlock()
	if t, ok := cachedToken(appKey); ok {
		return t, nil
	}
	return fetchIntoCache(ctx, appKey, appSecret, apiDomain, routeEnv)
}

// Fresh token after dropping the cached one
func forceRefreshSignToken(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (signToken, error) {
	slog.Warn(fmt.Sprintf("[Yuanbao] Clearing cached token and re-signing: app_key=****%s", lastN(appKey, 4)))
	l := refreshLock(appKey)
	l.Lock()
	defer l.Unlock()
	signCache.mu.Lock()
	delete(signCache.entries, appKey)
	signCache.mu.Unlock()
	return fetchIntoCache(ctx, appKey, appSecret, apiDomain, routeEnv)
}

// First n runes of s
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Last n bytes of s
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func fetchIntoCache(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (signToken, error) {
	data, err := fetchSignToken(ctx, appKey, appSecret, apiDomain, routeEnv)
	if err != nil {
		return signToken{}, err
	}
	lifetime := jsonInt(data["duration"])
	if lifetime <= 0 {
		lifetime = signDefaultLifetimeS
	}
	t := signToken{Token: jsonText(data["token"]), BotID: jsonText(data["bot_id"]), Source: jsonText(data["source"]),
		ExpireAt: time.Now().Add(time.Duration(lifetime) * time.Second)}
	signCache.mu.Lock()
	signCache.entries[appKey] = t
	signCache.mu.Unlock()
	return t, nil
}

// Hex HMAC-SHA256 keyed by the secret over nonce, timestamp, app key and secret
func signSignature(nonce, timestamp, appKey, appSecret string) string {
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write([]byte(nonce + timestamp + appKey + appSecret))
	return hex.EncodeToString(m.Sum(nil))
}

// Beijing time ISO 8601 timestamp without fractional seconds
func beijingTimestamp() string {
	return time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02T15:04:05") + "+08:00"
}

// Base version of this build for the app and bot version headers
func baseVersion() string {
	info, err := updaterecord.GetVersionInfo()
	if err != nil || info.BaseVersion == "" {
		return "unknown"
	}
	return info.BaseVersion
}

// Operating system name the gateway expects, win32 for Windows
func operatingSystem() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Signed sign token request retrying the retryable code up to three times
func fetchSignToken(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (map[string]any, error) {
	endpoint := strings.TrimRight(apiDomain, "/") + signTokenPath
	client := gateway.TrustEnvHTTPClient(ctx, signHTTPTimeout)
	version := baseVersion()
	for attempt := 0; ; attempt++ {
		nonce, timestamp := randomHex(16), beijingTimestamp()
		payload, err := json.Marshal(struct {
			AppKey    string `json:"app_key"`
			Nonce     string `json:"nonce"`
			Signature string `json:"signature"`
			Timestamp string `json:"timestamp"`
		}{appKey, nonce, signSignature(nonce, timestamp, appKey, appSecret), timestamp})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AppVersion", version)
		req.Header.Set("X-OperationSystem", operatingSystem())
		req.Header.Set("X-Instance-Id", instanceID)
		req.Header.Set("X-Bot-Version", version)
		if routeEnv != "" {
			req.Header.Set("X-Route-Env", routeEnv)
		}
		retryNote := ""
		if attempt > 0 {
			retryNote = fmt.Sprintf(" (retry %d/%d)", attempt, signMaxRetries)
		}
		slog.Info(fmt.Sprintf("[Yuanbao] Sign token request: url=%s%s", endpoint, retryNote))
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("sign token API returned %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
		}
		result, err := decodeJSONObject(body)
		if err != nil {
			return nil, fmt.Errorf("sign token response parse error: %w", err)
		}
		code := jsonText(result["code"])
		if code == "0" {
			data, ok := result["data"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("sign token response has no data object")
			}
			slog.Info(fmt.Sprintf("[Yuanbao] Sign token success: bot_id=%s", jsonText(data["bot_id"])))
			return data, nil
		}
		if code != signRetryableCode || attempt >= signMaxRetries {
			return nil, fmt.Errorf("sign token error: code=%s, msg=%s", code, jsonText(result["msg"]))
		}
		slog.Warn(fmt.Sprintf("[Yuanbao] Sign token retryable: code=%s, retrying in %s (attempt=%d/%d)", code, signRetryDelay,
			attempt+1, signMaxRetries))
		select {
		case <-time.After(signRetryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
