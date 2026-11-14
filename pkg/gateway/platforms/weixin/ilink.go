package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	weixinCopyLineWidth = 120

	ilinkBaseURL          = "https://ilinkai.weixin.qq.com"
	weixinCDNBaseURL      = "https://novac2c.cdn.weixin.qq.com/c2c"
	ilinkAppID            = "bot"
	channelVersion        = "2.2.0"
	ilinkAppClientVersion = (2 << 16) | (2 << 8) | 0

	epGetUpdates   = "ilink/bot/getupdates"
	epSendMessage  = "ilink/bot/sendmessage"
	epSendTyping   = "ilink/bot/sendtyping"
	epGetConfig    = "ilink/bot/getconfig"
	epGetUploadURL = "ilink/bot/getuploadurl"
	epGetBotQR     = "ilink/bot/get_bot_qrcode"
	epGetQRStatus  = "ilink/bot/get_qrcode_status"

	longPollTimeoutMS = 35_000
	apiTimeoutMS      = 15_000
	configTimeoutMS   = 10_000
	qrTimeoutMS       = 35_000

	maxConsecutiveFailures = 3
	retryDelaySeconds      = 2
	backoffDelaySeconds    = 30

	sessionExpiredErrcode   = -14
	rateLimitErrcode        = -2
	messageDedupTTLSeconds  = 300
	messageDedupDefaultSize = 2000

	mediaImage, mediaVideo, mediaFile, mediaVoice               = 1, 2, 3, 4
	itemText, itemImage, itemVoice, itemFile, itemVideo         = 1, 2, 3, 4, 5
	msgTypeBot, msgStateFinish                                  = 2, 2
	typingStart, typingStop                                     = 1, 2
	maxMessageLength                                            = 2000
	weixinSplitThreshold                                        = 1800
	sessionExpiredPauseSeconds, uploadTimeoutSeconds, qrPollGap = 600.0, 120.0, 1.0
)

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

func runtimeError(msg string) error { return &pyErr{"RuntimeError", msg} }

func valueError(msg string) error { return &pyErr{"ValueError", msg} }

// Whether the error is the TimeoutError of an awaited request
func isTimeout(err error) bool {
	var pe *pyErr
	return errors.As(err, &pe) && pe.kind == "TimeoutError"
}

// str of an exception as a percent s log argument renders it
func errText(err error) string { return pyrepr.ExceptionMessage(err) }

// Dict view of a decoded value, an empty object for a falsy value and the AttributeError of any other
func objOrEmpty(v any) (*pyjson.Object, error) {
	if !pyrepr.Truthy(v) {
		return pyjson.NewObject(), nil
	}
	if o, ok := pyjson.AsObject(v); ok {
		return o, nil
	}
	return nil, &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
}

// str of a falsy defaulted value, like str(value or "")
func strOf(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Stripped str of a falsy defaulted value, like str(value or "").strip()
func textOf(v any) string { return pystr.Strip(strOf(v)) }

// Whether a decoded value equals an int the way Python compares numbers
func equalsInt(v any, n int64) bool { return v != nil && pyjson.Equal(v, n) }

// Whether a ret or errcode value is zero or absent, the in {0, None} test
func zeroOrNone(v any) bool { return v == nil || pyjson.Equal(v, int64(0)) }

// Scoped WEIXIN env of the key first, else the extra value, stripped
func extraOrSecret(ctx context.Context, extra map[string]any, key, def string) (string, error) {
	v, err := gateway.ExtraOrSecret(ctx, extra, key, "WEIXIN_"+pystr.Upper(key), def, true)
	if err != nil {
		return "", err
	}
	return pystr.Strip(pyrepr.Str(v)), nil
}

// Whether a minus two response is a stale session rather than a real rate limit
func isStaleSessionRet(ret, errcode, errmsg any) bool {
	if !equalsInt(ret, rateLimitErrcode) && !equalsInt(errcode, rateLimitErrcode) {
		return false
	}
	msg := ""
	if s, ok := errmsg.(string); ok {
		msg = pystr.Lower(s)
	}
	return msg == "unknown error" || msg == "prepare failed"
}

// Whether a response reports an expired session or its stale session variant
func isSessionExpired(resp *pyjson.Object, ret, errcode any) bool {
	return equalsInt(ret, sessionExpiredErrcode) || equalsInt(errcode, sessionExpiredErrcode) ||
		isStaleSessionRet(ret, errcode, pyrepr.Or(resp.Vals["errmsg"], resp.Vals["msg"]))
}

// Error of a stale session that survives the tokenless resend, the peer must message the bot first
func sessionNotReadyError(ret, errcode, errmsg any) error {
	return runtimeError(fmt.Sprintf("iLink sendmessage session not ready: ret=%s errcode=%s errmsg=%s — the user must send the bot a message first (or re-pair)",
		pyrepr.Str(ret), pyrepr.Str(errcode), pyrepr.Str(pyrepr.Or(errmsg, "unknown error"))))
}

// One HTTP session with its closed flag
type ilinkSession struct {
	client *http.Client
	mu     sync.Mutex
	closed bool
}

// Session trusting proxy env vars when the gateway does, idle connections reaped after two seconds
func newSession(ctx context.Context) *ilinkSession {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if gateway.GatewayTrustEnv(ctx) {
		transport.Proxy = http.ProxyFromEnvironment
	}
	transport.IdleConnTimeout = 2 * time.Second
	return &ilinkSession{client: &http.Client{Transport: transport}}
}

func (s *ilinkSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Closes the session and its pooled connections
func (s *ilinkSession) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.client.CloseIdleConnections()
}

// Weixin needs no optional dependency in this build
func checkWeixinRequirements() bool { return true }

// First characters of an id for log lines, a question mark for a blank one
func safeID(value any) string {
	raw := textOf(value)
	if raw == "" {
		return "?"
	}
	return pystr.Slice(raw, 0, 8)
}

// Data padded to the AES block size the PKCS7 way
func pkcs7Pad(data []byte) []byte {
	padLen := aes.BlockSize - len(data)%aes.BlockSize
	return append(append([]byte{}, data...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
}

// AES-128-ECB encryption of the PKCS7 padded plaintext
func aes128ECBEncrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, valueError(fmt.Sprintf("Invalid key size (%d) for AES.", len(key)*8))
	}
	padded := pkcs7Pad(plaintext)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return out, nil
}

// AES-128-ECB decryption with well formed PKCS7 padding stripped, the raw blocks otherwise
func aes128ECBDecrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, valueError(fmt.Sprintf("Invalid key size (%d) for AES.", len(key)*8))
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, valueError("The length of the provided data is not a multiple of the block length.")
	}
	padded := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(padded[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	if len(padded) == 0 {
		return padded, nil
	}
	padLen := int(padded[len(padded)-1])
	if 1 <= padLen && padLen <= 16 && bytes.HasSuffix(padded, bytes.Repeat([]byte{byte(padLen)}, padLen)) {
		return padded[:len(padded)-padLen], nil
	}
	return padded, nil
}

// One HTTP header kept under its exact name
type header struct{ name, value string }

// iLink request headers of a JSON body with a random uin and the bearer token when one is set
func ilinkHeaders(token, body string) []header {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	uin := base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(raw[:])), 10)))
	headers := []header{{"Content-Type", "application/json"}, {"AuthorizationType", "ilink_bot_token"},
		{"Content-Length", strconv.Itoa(len(body))}, {"X-WECHAT-UIN", uin},
		{"iLink-App-Id", ilinkAppID}, {"iLink-App-ClientVersion", strconv.Itoa(ilinkAppClientVersion)}}
	if token != "" {
		headers = append(headers, header{"Authorization", "Bearer " + token})
	}
	return headers
}

// Accounts directory under the home, created when missing
func accountDir(home string) (string, error) {
	path := filepath.Join(home, "weixin", "accounts")
	if err := os.MkdirAll(path, 0o777); err != nil {
		return "", err
	}
	return path, nil
}

// Decoded JSON of a file tolerating a byte order mark, nil when it is missing or unreadable
func readJSON(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	return data
}

// Persists the credentials of an account with owner only permissions
func saveWeixinAccount(ctx context.Context, home, accountID, token, baseURL, userID string) error {
	dir, err := accountDir(home)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, accountID+".json")
	savedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := config.AtomicJSONWrite(ctx, path, pyjson.ObjectOf("token", token, "base_url", baseURL, "user_id", userID, "saved_at", savedAt)); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Persisted credentials of an account, nil when none were saved
func loadWeixinAccount(home, accountID string) (*pyjson.Object, error) {
	dir, err := accountDir(home)
	if err != nil {
		return nil, err
	}
	account, _ := readJSON(filepath.Join(dir, accountID+".json")).(*pyjson.Object)
	return account, nil
}

// Disk backed context token cache keyed by account and peer
type contextTokenStore struct {
	root      string
	mu        sync.Mutex
	cache     map[string]string
	order     []string
	persistMu sync.Mutex
}

func newContextTokenStore(home string) (*contextTokenStore, error) {
	root, err := accountDir(home)
	if err != nil {
		return nil, err
	}
	return &contextTokenStore{root: root, cache: map[string]string{}}, nil
}

func tokenKey(accountID, userID string) string { return accountID + ":" + userID }

// Stores a token under its key keeping first insertion order, caller holds the lock
func (s *contextTokenStore) put(key, token string) {
	if _, ok := s.cache[key]; !ok {
		s.order = append(s.order, key)
	}
	s.cache[key] = token
}

// Loads the persisted tokens of an account into the cache
func (s *contextTokenStore) restore(accountID string) error {
	path := filepath.Join(s.root, accountID+".context-tokens.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var data any
	if err == nil {
		var text string
		if text, err = pystr.DecodeUTF8SigFile(raw); err == nil {
			data, err = pyjson.Loads(text)
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("weixin: failed to restore context tokens for %s: %s", safeID(accountID), errText(err)))
		return nil
	}
	tokens, ok := data.(*pyjson.Object)
	if !ok {
		return &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'items'", pyrepr.TypeName(data))}
	}
	restored := 0
	s.mu.Lock()
	for _, userID := range tokens.Keys {
		if token, isStr := tokens.Vals[userID].(string); isStr && token != "" {
			s.put(tokenKey(accountID, userID), token)
			restored++
		}
	}
	s.mu.Unlock()
	if restored > 0 {
		slog.Info(fmt.Sprintf("weixin: restored %d context token(s) for %s", restored, safeID(accountID)))
	}
	return nil
}

// Cached token of a peer, empty when none is known
func (s *contextTokenStore) get(accountID, userID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[tokenKey(accountID, userID)]
}

// Forgets the cached token of a peer without touching the persisted file
func (s *contextTokenStore) drop(accountID, userID string) {
	key := tokenKey(accountID, userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[key]; !ok {
		return
	}
	delete(s.cache, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Caches a token and flushes the account's tokens to disk in call order
func (s *contextTokenStore) set(ctx context.Context, accountID, userID, token string) {
	s.mu.Lock()
	s.put(tokenKey(accountID, userID), token)
	s.mu.Unlock()
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	prefix := accountID + ":"
	payload := pyjson.NewObject()
	s.mu.Lock()
	for _, key := range s.order {
		if strings.HasPrefix(key, prefix) {
			payload.Set(key[len(prefix):], s.cache[key])
		}
	}
	s.mu.Unlock()
	if err := config.AtomicJSONWrite(ctx, filepath.Join(s.root, accountID+".context-tokens.json"), payload); err != nil {
		slog.Warn(fmt.Sprintf("weixin: failed to persist context tokens for %s: %s", safeID(accountID), errText(err)))
	}
}

// Short lived typing ticket cache filled from getconfig
type typingTicketCache struct {
	ttlSeconds float64
	mu         sync.Mutex
	cache      map[string]typingTicket
}

type typingTicket struct {
	ticket string
	at     float64
}

func newTypingTicketCache() *typingTicketCache {
	return &typingTicketCache{ttlSeconds: 600.0, cache: map[string]typingTicket{}}
}

// Ticket of a user while it is fresh, an expired one evicted
func (c *typingTicketCache) get(userID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.cache[userID]; ok && entry.ticket != "" && wallClock()-entry.at < c.ttlSeconds {
		return entry.ticket
	}
	delete(c.cache, userID)
	return ""
}

func (c *typingTicketCache) set(userID, ticket string) {
	c.mu.Lock()
	c.cache[userID] = typingTicket{ticket, wallClock()}
	c.mu.Unlock()
}

// AES key of a media item, sixteen raw bytes or thirty two hex characters under base64
func parseAESKey(aesKeyB64 any) ([]byte, error) {
	encoded, ok := aesKeyB64.(string)
	if !ok {
		return nil, &pyErr{"TypeError", fmt.Sprintf("argument should be a bytes-like object or ASCII string, not '%s'", pyrepr.TypeName(aesKeyB64))}
	}
	decoded, err := pybase64.B64Decode(encoded)
	if err != nil {
		return nil, err
	}
	if len(decoded) == 16 {
		return decoded, nil
	}
	if len(decoded) == 32 {
		var text strings.Builder
		for _, b := range decoded {
			if b < 0x80 {
				text.WriteByte(b)
			}
		}
		if hexText := text.String(); hexText != "" && strings.Trim(hexText, "0123456789abcdefABCDEF") == "" {
			return pystr.BytesFromHex(hexText)
		}
	}
	return nil, valueError(fmt.Sprintf("unexpected aes_key format (%d decoded bytes)", len(decoded)))
}

// Chat type and effective chat id of an inbound message
func guessChatType(message *pyjson.Object, accountID string) (string, string) {
	roomID := textOf(pyrepr.Or(message.Vals["room_id"], message.Vals["chat_room_id"]))
	toUserID := textOf(message.Vals["to_user_id"])
	if roomID != "" || (toUserID != "" && accountID != "" && toUserID != accountID && equalsInt(message.Vals["msg_type"], 1)) {
		for _, candidate := range []string{roomID, toUserID} {
			if candidate != "" {
				return "group", candidate
			}
		}
		return "group", strOf(message.Vals["from_user_id"])
	}
	return "dm", strOf(message.Vals["from_user_id"])
}

// Runs one request and returns its status, headers and body under a whole call deadline
func (s *ilinkSession) do(ctx context.Context, method, rawURL string, headers []header, body []byte, timeoutSeconds float64) (*pyhttpx.Response, error) {
	rctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds*float64(time.Second)))
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		if h.name == "Content-Length" {
			continue
		}
		req.Header[h.name] = []string{h.value}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, requestError(ctx, rctx, rawURL, err)
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, requestError(ctx, rctx, rawURL, err)
	}
	return &pyhttpx.Response{StatusCode: resp.StatusCode, Header: resp.Header, URL: rawURL, Content: content, Status: resp.Status}, nil
}

// Error of a failed request, the caller's cancellation first, then the deadline TimeoutError, then the connect error
func requestError(ctx, rctx context.Context, rawURL string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if rctx.Err() != nil {
		return &pyErr{"TimeoutError", ""}
	}
	host := rawURL
	if parsed, perr := pyurl.Parse(rawURL); perr == nil {
		host = parsed.Netloc
	}
	return &pyErr{"ClientConnectorError", fmt.Sprintf("Cannot connect to host %s ssl:default [%s]", host, pyhttpx.OSErrorText(err))}
}

// One iLink call returning its decoded JSON, a RuntimeError for an HTTP error status
func apiRequest(ctx context.Context, session *ilinkSession, method, baseURL, endpoint string, headers []header, timeoutMS int, body []byte) (*pyjson.Object, error) {
	resp, err := session.do(ctx, method, pystr.RstripChars(baseURL, "/")+"/"+endpoint, headers, body, float64(timeoutMS)/1000)
	if err != nil {
		return nil, err
	}
	raw := resp.Text()
	if resp.StatusCode >= 400 {
		return nil, runtimeError(fmt.Sprintf("iLink %s %s HTTP %d: %s", method, endpoint, resp.StatusCode, pystr.Slice(raw, 0, 200)))
	}
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		return nil, err
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
	}
	return data, nil
}

// POST of a payload with the channel version base info
func apiPost(ctx context.Context, session *ilinkSession, baseURL, endpoint string, payload *pyjson.Object, token string, timeoutMS int) (*pyjson.Object, error) {
	full := payload.Copy()
	full.Set("base_info", pyjson.ObjectOf("channel_version", channelVersion))
	body, err := pyjson.Dumps(full, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	return apiRequest(ctx, session, http.MethodPost, baseURL, endpoint, ilinkHeaders(token, body), timeoutMS, []byte(body))
}

// GET of an endpoint with the app id and client version headers
func apiGet(ctx context.Context, session *ilinkSession, baseURL, endpoint string, timeoutMS int) (*pyjson.Object, error) {
	headers := []header{{"iLink-App-Id", ilinkAppID}, {"iLink-App-ClientVersion", strconv.Itoa(ilinkAppClientVersion)}}
	return apiRequest(ctx, session, http.MethodGet, baseURL, endpoint, headers, timeoutMS, nil)
}

// Long poll for updates, a timeout answering an empty batch at the same cursor
func getUpdates(ctx context.Context, session *ilinkSession, baseURL, token string, syncBuf any, timeoutMS int) (*pyjson.Object, error) {
	resp, err := apiPost(ctx, session, baseURL, epGetUpdates, pyjson.ObjectOf("get_updates_buf", syncBuf), token, timeoutMS)
	if isTimeout(err) {
		return pyjson.ObjectOf("ret", int64(0), "msgs", []any{}, "get_updates_buf", syncBuf), nil
	}
	return resp, err
}

// Sends an item list to a peer as a finished bot message
func sendItems(ctx context.Context, session *ilinkSession, baseURL, token, to string, itemList []any, contextToken, clientID string) (*pyjson.Object, error) {
	message := pyjson.ObjectOf("from_user_id", "", "to_user_id", to, "client_id", clientID, "message_type", int64(msgTypeBot),
		"message_state", int64(msgStateFinish), "item_list", itemList)
	if contextToken != "" {
		message.Set("context_token", contextToken)
	}
	return apiPost(ctx, session, baseURL, epSendMessage, pyjson.ObjectOf("msg", message), token, apiTimeoutMS)
}

// Sends one text message, a ValueError for blank text
func sendMessage(ctx context.Context, session *ilinkSession, baseURL, token, to, text, contextToken, clientID string) (*pyjson.Object, error) {
	if pystr.Strip(text) == "" {
		return nil, valueError("_send_message: text must not be empty")
	}
	itemList := []any{pyjson.ObjectOf("type", int64(itemText), "text_item", pyjson.ObjectOf("text", text))}
	return sendItems(ctx, session, baseURL, token, to, itemList, contextToken, clientID)
}

// Fetches the per user config that carries the typing ticket
func getConfig(ctx context.Context, session *ilinkSession, baseURL, token, userID, contextToken string) (*pyjson.Object, error) {
	payload := pyjson.ObjectOf("ilink_user_id", userID)
	if contextToken != "" {
		payload.Set("context_token", contextToken)
	}
	return apiPost(ctx, session, baseURL, epGetConfig, payload, token, configTimeoutMS)
}

// Requests an upload slot of the encrypted CDN for one media file
func getUploadURL(ctx context.Context, session *ilinkSession, baseURL, token, toUserID string, mediaType int, filekey string, rawsize int, rawfilemd5 string, filesize int, aeskeyHex string) (*pyjson.Object, error) {
	payload := pyjson.ObjectOf("filekey", filekey, "media_type", int64(mediaType), "to_user_id", toUserID, "rawsize", int64(rawsize),
		"rawfilemd5", rawfilemd5, "filesize", int64(filesize), "no_need_thumb", true, "aeskey", aeskeyHex)
	return apiPost(ctx, session, baseURL, epGetUploadURL, payload, token, apiTimeoutMS)
}

// Posts ciphertext to the CDN and returns the encrypted param of the stored file
func uploadCiphertext(ctx context.Context, session *ilinkSession, ciphertext []byte, uploadURL string) (string, error) {
	resp, err := session.do(ctx, http.MethodPost, uploadURL, []header{{"Content-Type", "application/octet-stream"}}, ciphertext, uploadTimeoutSeconds)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == 200 {
		if param := resp.Header.Get("x-encrypted-param"); param != "" {
			return param, nil
		}
	}
	raw := pystr.Slice(resp.Text(), 0, 200)
	if resp.StatusCode == 200 {
		return "", runtimeError("CDN upload missing x-encrypted-param header: " + raw)
	}
	return "", runtimeError(fmt.Sprintf("CDN upload HTTP %d: %s", resp.StatusCode, raw))
}

// Downloads a URL, the response error of an HTTP error status
func downloadBytes(ctx context.Context, session *ilinkSession, rawURL string, timeoutSeconds float64) ([]byte, error) {
	resp, err := session.do(ctx, http.MethodGet, rawURL, nil, nil, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
		return nil, &pyErr{"ClientResponseError", fmt.Sprintf("%d, message=%s, url=%s", resp.StatusCode, pyrepr.Repr(reason), pyrepr.Repr(rawURL))}
	}
	return resp.Content, nil
}

// Hosts media may be fetched from by full URL
var weixinCDNAllowlist = map[string]bool{"novac2c.cdn.weixin.qq.com": true, "ilinkai.weixin.qq.com": true, "wx.qlogo.cn": true,
	"thirdwx.qlogo.cn": true, "res.wx.qq.com": true, "mmbiz.qpic.cn": true, "mmbiz.qlogo.cn": true}

// ValueError for a media URL that is not http or https on an allowlisted WeChat CDN host
func assertWeixinCDNURL(rawURL string) error {
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		return valueError("Unparseable media URL: " + pyrepr.Repr(rawURL))
	}
	scheme, host := pystr.Lower(parsed.Scheme), parsed.Hostname()
	if scheme != "http" && scheme != "https" {
		return valueError(fmt.Sprintf("Media URL has disallowed scheme %s; only http/https are permitted.", pyrepr.Repr(scheme)))
	}
	if !weixinCDNAllowlist[host] {
		return valueError(fmt.Sprintf("Media URL host %s is not in the WeChat CDN allowlist. Refusing to fetch to prevent SSRF.", pyrepr.Repr(host)))
	}
	return nil
}

// Downloads a media item by its encrypted param or allowlisted full URL and decrypts it when it carries a key
func downloadAndDecryptMedia(ctx context.Context, session *ilinkSession, cdnBaseURL string, encryptedQueryParam, aesKeyB64, fullURL any, timeoutSeconds float64) ([]byte, error) {
	var rawURL string
	switch {
	case pyrepr.Truthy(encryptedQueryParam):
		rawURL = pystr.RstripChars(cdnBaseURL, "/") + "/download?encrypted_query_param=" + pyurl.Quote(pyrepr.Str(encryptedQueryParam), "")
	case pyrepr.Truthy(fullURL):
		rawURL = pyrepr.Str(fullURL)
		if err := assertWeixinCDNURL(rawURL); err != nil {
			return nil, err
		}
	default:
		return nil, runtimeError("media item had neither encrypt_query_param nor full_url")
	}
	raw, err := downloadBytes(ctx, session, rawURL, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(aesKeyB64) {
		return raw, nil
	}
	key, err := parseAESKey(aesKeyB64)
	if err != nil {
		return nil, err
	}
	return aes128ECBDecrypt(raw, key)
}
