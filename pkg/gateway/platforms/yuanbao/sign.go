package yuanbao

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	defaultWSGatewayURL       = "wss://bot-wss.yuanbao.tencent.com/wss/connection"
	defaultBotAPIDomain       = "https://bot.yuanbao.tencent.com"
	heartbeatIntervalSeconds  = 30.0
	connectTimeoutSeconds     = 15.0
	authTimeoutSeconds        = 10.0
	defaultSendTimeout        = 30.0
	wsCloseTimeoutS           = 1.0
	heartbeatTimeoutThreshold = 2
	replyHeartbeatIntervalS   = 2.0
	replyHeartbeatTimeoutS    = 30.0
	slowResponseTimeoutS      = 120.0

	cronWrapperHeaderPrefix = "Cronjob Response: "
	cronWrapperDivider      = "\n-------------\n\n"
	cronWrapperFooterPrefix = "\n\nTo stop or manage this job, send me a new message (e.g. \"stop reminder "

	textElemType = "TIMTextElem"

	observedMediaBackfillLookback          = 50
	observedMediaBackfillMaxResolvePerTurn = 12
	defaultResolveConcurrency              = 6
	minResolveConcurrency                  = 1
	maxResolveConcurrency                  = 12

	maxTextChunk     = 4000
	mediaMaxSizeMB   = 50
	dmMaxChars       = 10000
	memberCacheTTLS  = 300.0
	debounceWindow   = 1.5
	chatDictMaxSize  = 1000
	msgContentCacheN = 200
)

// Reconnect attempts before the adapter gives up, a package var the way hermes tests patch it
var maxReconnectAttempts = 100

// Close codes of permanent errors the adapter never reconnects after
var noReconnectCloseCodes = map[int]bool{4012: true, 4013: true, 4014: true, 4018: true, 4019: true, 4021: true}

var (
	ybResRefRE     = pyre.MustCompile(`\[(image|voice|video|file(?::[^|\]]*)?)\|ybres:([A-Za-z0-9_\-]+)\]`)
	ybLocalMediaRE = pyre.MustCompile(`\[(\w+):[^\]]*?(/[^\]]+?)\s*\]`)
	indicatorRE    = pyre.MustCompile(`\s*\(\d+/\d+\)$`)
	atUserRE       = pyre.MustCompile(`(?m)(?:(?<=\s)|(?<=^))@(\S+?)(?=\s|$)`)
)

// Media kinds whose anchors resolve to local files for the model
var resolvableMediaKinds = map[string]bool{"image": true, "file": true, "video": true}

// Wall clock, monotonic clock and sleep of the adapter, swappable like module globals
var (
	processStart = time.Now()
	wallClock    = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	monotonic    = func() float64 { return time.Since(processStart).Seconds() }
	sleep        = func(ctx context.Context, seconds float64) error {
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// Python exception carried as a Go error with its class name
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

func runtimeError(msg string) error { return &RuntimeError{Msg: msg} }

func valueError(msg string) error { return &pyErr{"ValueError", msg} }

// Whether the error is the TimeoutError of an awaited future, an elapsed wait or a timed out opening handshake
func isTimeout(err error) bool {
	var pe *pyErr
	if errors.As(err, &pe) {
		return pe.kind == "TimeoutError"
	}
	var open *pywebsockets.OpenTimeoutError
	return errors.As(err, &open) || errors.Is(err, context.DeadlineExceeded)
}

// Whether the error is a ValueError or one of its subclasses
func isValueError(err error) bool {
	name := pyrepr.ExceptionName(err)
	var pe *pyErr
	var ie *pyint.Error
	var be *pybase64.Error
	switch {
	case errors.As(err, &pe):
		name = pe.kind
	case errors.As(err, &ie):
		name = ie.Kind
	case errors.As(err, &be), errors.Is(err, pybase64.ErrNonASCII):
		name = "ValueError"
	}
	for _, class := range pyrepr.ExceptionMROFrom(name, nil) {
		if class == "ValueError" {
			return true
		}
	}
	return false
}

// str of an exception as a percent s log argument renders it
func errText(err error) string { return pyrepr.ExceptionMessage(err) }

// str of an exception, its class name when the message is empty
func errTextOrName(err error) string {
	if msg := errText(err); msg != "" {
		return msg
	}
	return pyrepr.ExceptionName(err)
}

// sys.platform of the host
func operationSystem() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Base version of the install, empty when the version record cannot be read
func baseVersion() (string, error) {
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return "", err
	}
	return info.BaseVersion, nil
}

// Patience notice pushed after a long agent silence, localized
func slowResponseMessage(ctx context.Context) string {
	return i18n.T(ctx, "platform.yuanbao.slow_response_notice", "", nil)
}

// Value under key or the default when the key is absent
func dictGetOr(d map[string]any, key string, def any) any {
	if v, ok := d[key]; ok {
		return v
	}
	return def
}

// str of a falsy defaulted value, like str(value or "")
func strOrEmpty(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Stripped str of a falsy defaulted value, like str(value or "").strip()
func stripOrEmpty(v any) string { return pystr.Strip(strOrEmpty(v)) }

// Text of a value expected to be a string, other values through str and None as empty
func textValue(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return pyrepr.Str(v)
}

// Whether a decoded value equals an int the way Python compares numbers
func equalsInt(v any, n int64) bool { return v != nil && pyjson.Equal(v, n) }

// A TIMTextElem message body entry
func textElem(text string) *pyjson.Object {
	return pyjson.ObjectOf("msg_type", textElemType, "msg_content", pyjson.ObjectOf("text", text))
}

// One ybres anchor of a transcript message
type ybresRef struct{ rid, kind, filename string }

// Resource id, kind and file name of each resolvable anchor match
func iterYbresRefs(matches []*regexp2.Match) []ybresRef {
	var refs []ybresRef
	for _, m := range matches {
		kind, filename := pystrPartition(m.GroupByNumber(1).String(), ":")
		if kind = pystr.Strip(kind); resolvableMediaKinds[kind] {
			refs = append(refs, ybresRef{m.GroupByNumber(2).String(), kind, pystr.Strip(filename)})
		}
	}
	return refs
}

// Head and tail of text around the first separator, the whole text and an empty tail when it is absent
func pystrPartition(text, sep string) (string, string) {
	head, tail, _ := strings.Cut(text, sep)
	return head, tail
}

// Every match of the pattern in the text in order
func findAll(re *regexp2.Regexp, text string) []*regexp2.Match {
	var out []*regexp2.Match
	for m, _ := re.FindStringMatch(text); m != nil; m, _ = re.FindNextMatch(m) {
		out = append(out, m)
	}
	return out
}

// Sign token acquisition, caching, signing and retry shared by the whole process
var signManager = struct {
	sync.Mutex
	cache map[string]map[string]any
	locks map[string]*sync.Mutex
}{cache: map[string]map[string]any{}, locks: map[string]*sync.Mutex{}}

const (
	signTokenPath          = "/api/v5/robotLogic/sign-token"
	signRetryableCode      = 10099
	signMaxRetries         = 3
	signRetryDelayS        = 1.0
	signCacheRefreshMargin = 60
	signHTTPTimeout        = 10 * time.Second
)

// HTTP client of a sign token request, a seam tests replace
var signHTTPClient = func() *http.Client { return pyhttpx.PlainClient(false) }

// Refresh lock of an app key, created on demand
func signRefreshLock(appKey string) *sync.Mutex {
	signManager.Lock()
	defer signManager.Unlock()
	lock, ok := signManager.locks[appKey]
	if !ok {
		lock = &sync.Mutex{}
		signManager.locks[appKey] = lock
	}
	return lock
}

// HMAC-SHA256 of nonce, timestamp, app key and app secret under the app secret, as hex
func computeSignature(nonce, timestamp, appKey, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(nonce + timestamp + appKey + appSecret))
	return hex.EncodeToString(mac.Sum(nil))
}

// Beijing time ISO 8601 timestamp without milliseconds
func buildTimestamp() string {
	return time.Now().In(time.FixedZone("", 8*3600)).Format("2006-01-02T15:04:05") + "+08:00"
}

// Float of a decoded number, zero for any other value
func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// Whether a cache entry has more than the refresh margin left
func signCacheValid(entry map[string]any) bool {
	return numberOf(entry["expire_ts"])-wallClock() > signCacheRefreshMargin
}

// Drops every refresh lock so none outlives a disconnect
func signClearLocks() {
	signManager.Lock()
	signManager.locks = map[string]*sync.Mutex{}
	signManager.Unlock()
}

// Drops expired token cache entries and returns how many were dropped
func signPurgeExpired() int {
	now := wallClock()
	signManager.Lock()
	defer signManager.Unlock()
	purged := 0
	for key, entry := range signManager.cache {
		if now-numberOf(dictGetOr(entry, "expire_ts", int64(0))) > 0 {
			delete(signManager.cache, key)
			purged++
		}
	}
	return purged
}

// Posts the sign token request, retrying the retryable code up to the retry cap
func signFetch(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (map[string]any, error) {
	rawURL := pystr.RstripChars(apiDomain, "/") + signTokenPath
	client := signHTTPClient()
	defer client.CloseIdleConnections()
	for attempt := 0; attempt <= signMaxRetries; attempt++ {
		nonceBytes := make([]byte, 16)
		_, _ = rand.Read(nonceBytes)
		nonce, timestamp := hex.EncodeToString(nonceBytes), buildTimestamp()
		body, err := pyjson.Dumps(pyjson.ObjectOf("app_key", appKey, "nonce", nonce,
			"signature", computeSignature(nonce, timestamp, appKey, appSecret), "timestamp", timestamp),
			pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
		if err != nil {
			return nil, err
		}
		version, err := baseVersion()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader([]byte(body)))
		if err != nil {
			return nil, err
		}
		for name, value := range map[string]string{"Content-Type": "application/json", "X-AppVersion": version,
			"X-OperationSystem": operationSystem(), "X-Instance-Id": strconv.Itoa(IcebathInstanceID), "X-Bot-Version": version} {
			req.Header.Set(name, value)
		}
		if routeEnv != "" {
			req.Header.Set("X-Route-Env", routeEnv)
		}
		retry := ""
		if attempt > 0 {
			retry = fmt.Sprintf(" (retry %d/%d)", attempt, signMaxRetries)
		}
		slog.Info(fmt.Sprintf("Sign token request: url=%s%s", rawURL, retry))
		resp, err := pyhttpx.Do(client, req, signHTTPTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, requestError(err)
		}
		if resp.StatusCode != 200 {
			return nil, runtimeError(fmt.Sprintf("Sign token API returned %d: %s", resp.StatusCode, pystr.Slice(resp.Text(), 0, 200)))
		}
		decoded, err := resp.JSON()
		if err != nil {
			return nil, valueError("Sign token response parse error: " + errText(err))
		}
		result, ok := asDict(decoded)
		if !ok {
			return nil, &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		code := result["code"]
		if equalsInt(code, 0) {
			data, isDict := asDict(result["data"])
			if !isDict {
				return nil, valueError("Sign token response missing 'data' field: " + pyrepr.Str(decoded))
			}
			slog.Info(fmt.Sprintf("Sign token success: bot_id=%s", pyrepr.Str(data["bot_id"])))
			return data, nil
		}
		if !equalsInt(code, signRetryableCode) || attempt >= signMaxRetries {
			return nil, runtimeError(fmt.Sprintf("Sign token error: code=%s, msg=%s", pyrepr.Str(code), pyrepr.Str(dictGetOr(result, "msg", ""))))
		}
		slog.Warn(fmt.Sprintf("Sign token retryable: code=%s, retrying in %ss (attempt=%d/%d)", pyrepr.Str(code), pyrepr.Float(signRetryDelayS),
			attempt+1, signMaxRetries))
		if err := sleep(ctx, signRetryDelayS); err != nil {
			return nil, err
		}
	}
	return nil, runtimeError("Sign token failed: max retries exceeded")
}

// Fetches a sign token into the cache with its expiry
func signFetchIntoCache(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) error {
	data, err := signFetch(ctx, appKey, appSecret, apiDomain, routeEnv)
	if err != nil {
		return err
	}
	duration := dictGetOr(data, "duration", int64(0))
	lifetime := 3600.0
	if seconds := numberOf(duration); seconds > 0 {
		lifetime = seconds
	}
	signManager.Lock()
	signManager.cache[appKey] = map[string]any{"token": dictGetOr(data, "token", ""), "bot_id": dictGetOr(data, "bot_id", ""),
		"duration": duration, "product": dictGetOr(data, "product", ""), "source": dictGetOr(data, "source", ""),
		"expire_ts": wallClock() + lifetime}
	signManager.Unlock()
	return nil
}

// Copy of the cached token entry of an app key, nil when none is cached
func signCached(appKey string) map[string]any {
	signManager.Lock()
	defer signManager.Unlock()
	entry, ok := signManager.cache[appKey]
	if !ok {
		return nil
	}
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		out[k] = v
	}
	return out
}

// Websocket auth token served from the cache while it is valid, else fetched under the refresh lock
func signGetToken(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (map[string]any, error) {
	signPurgeExpired()
	if cached := signCached(appKey); len(cached) > 0 && signCacheValid(cached) {
		slog.Info(fmt.Sprintf("Using cached token (%ds remaining)", int64(numberOf(cached["expire_ts"])-wallClock())))
		return cached, nil
	}
	lock := signRefreshLock(appKey)
	lock.Lock()
	defer lock.Unlock()
	if cached := signCached(appKey); len(cached) > 0 && signCacheValid(cached) {
		return cached, nil
	}
	if err := signFetchIntoCache(ctx, appKey, appSecret, apiDomain, routeEnv); err != nil {
		return nil, err
	}
	return signCached(appKey), nil
}

// Clears the cached token of an app key and signs again
func signForceRefresh(ctx context.Context, appKey, appSecret, apiDomain, routeEnv string) (map[string]any, error) {
	slog.Warn(fmt.Sprintf("[force-refresh] Clearing cache and re-signing token: app_key=****%s", pystr.Slice(appKey, -4, pystr.Len(appKey))))
	lock := signRefreshLock(appKey)
	lock.Lock()
	defer lock.Unlock()
	signManager.Lock()
	delete(signManager.cache, appKey)
	signManager.Unlock()
	if err := signFetchIntoCache(ctx, appKey, appSecret, apiDomain, routeEnv); err != nil {
		return nil, err
	}
	return signCached(appKey), nil
}
