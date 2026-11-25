package weixin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	weixinCopyLineWidth = 120

	ilinkBaseURL          = "https://ilinkai.weixin.qq.com"
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

// Error carrying a kind label beside its message
type kindErr struct{ kind, msg string }

func (e *kindErr) Error() string     { return e.msg }
func (e *kindErr) ErrorKind() string { return e.kind }

func runtimeError(msg string) error { return &kindErr{"RuntimeError", msg} }

// Failure of a call made while the adapter holds no open HTTP session
var errSessionClosed = runtimeError("weixin HTTP session is not open")

func valueError(msg string) error { return &kindErr{"ValueError", msg} }

// Whether the error is the timeout of an awaited request
func isTimeout(err error) bool {
	var pe *kindErr
	return errors.As(err, &pe) && pe.kind == "TimeoutError"
}

// Kind of a failure and the proxy's host when the proxy URL a session to the hosts reads from the environment holds a secret the raw error text could
// repeat, empty when the raw text is safe to show
func proxyErrorShown(ctx context.Context, err error, hosts ...string) string {
	return gateway.DescribeProxyError(err, gateway.TrustedEnvProxyForRedaction(ctx, hosts...))
}

// Text of a failed request to the adapter's hosts for a log line or a send result, redacted when the raw text could repeat a proxy secret
func (a *WeixinAdapter) proxySafeErrText(ctx context.Context, err error) string {
	return cmp.Or(proxyErrorShown(ctx, err, a.baseURL, a.cdnBaseURL), err.Error())
}

// Object view of a decoded value, an empty object for an empty or missing value and an error for anything else
func objOrEmpty(v any) (*jsonutil.Object, error) {
	if !anyutil.IsTrue(v) {
		return jsonutil.NewObject(), nil
	}
	if o, ok := jsonutil.AsObject(v); ok {
		return o, nil
	}
	return nil, fmt.Errorf("expected an object, got %s", anyutil.Kind(v))
}

// Text of a value, empty for an empty or missing one
func strOf(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}

// Trimmed text of a value, empty for an empty or missing one
func textOf(v any) string { return strings.TrimSpace(strOf(v)) }

// Whether a decoded value equals an int the way Python compares numbers
func equalsInt(v any, n int64) bool { return v != nil && anyutil.Equal(v, n) }

// Whether a ret or errcode value is zero or absent
func zeroOrNone(v any) bool { return v == nil || anyutil.Equal(v, int64(0)) }

// Scoped WEIXIN env of the key first, else the extra value, stripped
func extraOrSecret(ctx context.Context, extra map[string]any, key, def string) (string, error) {
	v, err := gateway.ExtraOrSecret(ctx, extra, key, "WEIXIN_"+strings.ToUpper(key), def, true)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(anyutil.String(v)), nil
}

// Whether a minus two response is a stale session rather than a real rate limit
func isStaleSessionRet(ret, errcode, errmsg any) bool {
	if !equalsInt(ret, rateLimitErrcode) && !equalsInt(errcode, rateLimitErrcode) {
		return false
	}
	msg := ""
	if s, ok := errmsg.(string); ok {
		msg = strings.ToLower(s)
	}
	return msg == "unknown error" || msg == "prepare failed"
}

// Whether a response reports an expired session or its stale session variant
func isSessionExpired(resp *jsonutil.Object, ret, errcode any) bool {
	return equalsInt(ret, sessionExpiredErrcode) || equalsInt(errcode, sessionExpiredErrcode) ||
		isStaleSessionRet(ret, errcode, anyutil.Or(resp.Value("errmsg"), resp.Value("msg")))
}

// Error of a stale session that survives the tokenless resend, the peer must message the bot first
func sessionNotReadyError(ret, errcode, errmsg any) error {
	return runtimeError(fmt.Sprintf("iLink sendmessage session not ready: ret=%s errcode=%s errmsg=%s — the user must send the bot a message first (or re-pair)",
		anyutil.String(ret), anyutil.String(errcode), anyutil.String(anyutil.Or(errmsg, "unknown error"))))
}

// One HTTP session with its closed flag
type ilinkSession struct {
	client *http.Client
	mu     sync.Mutex
	closed bool
}

// Session trusting proxy env vars when the gateway does, idle connections reaped after two seconds
func newSession(ctx context.Context) *ilinkSession {
	transport := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
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
	return textutil.First(raw, 8)
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
	data, err := jsonutil.Decode(raw)
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
	if err := config.AtomicJSONWrite(ctx, path, jsonutil.ObjectOf("token", token, "base_url", baseURL, "user_id", userID, "saved_at", savedAt)); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Persisted credentials of an account, nil when none were saved
func loadWeixinAccount(home, accountID string) (*jsonutil.Object, error) {
	dir, err := accountDir(home)
	if err != nil {
		return nil, err
	}
	account, _ := readJSON(filepath.Join(dir, accountID+".json")).(*jsonutil.Object)
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
		data, err = jsonutil.Decode(raw)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("weixin: failed to restore context tokens for %s: %s", safeID(accountID), err.Error()))
		return nil
	}
	tokens, ok := data.(*jsonutil.Object)
	if !ok {
		return fmt.Errorf("context tokens of %s: expected an object, got %s", safeID(accountID), anyutil.Kind(data))
	}
	restored := 0
	s.mu.Lock()
	for _, userID := range tokens.Keys() {
		if token, isStr := tokens.Value(userID).(string); isStr && token != "" {
			s.put(tokenKey(accountID, userID), token)
			restored++
		}
	}
	s.mu.Unlock()
	if restored > 0 {
		pkgLog().Info(fmt.Sprintf("weixin: restored %d context token(s) for %s", restored, safeID(accountID)))
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
	payload := jsonutil.NewObject()
	s.mu.Lock()
	for _, key := range s.order {
		if strings.HasPrefix(key, prefix) {
			payload.Set(key[len(prefix):], s.cache[key])
		}
	}
	s.mu.Unlock()
	if err := config.AtomicJSONWrite(ctx, filepath.Join(s.root, accountID+".context-tokens.json"), payload); err != nil {
		pkgLog().Warn(fmt.Sprintf("weixin: failed to persist context tokens for %s: %s", safeID(accountID), err.Error()))
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
		return nil, fmt.Errorf("aes_key must be base64 text, got %s", anyutil.Kind(aesKeyB64))
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
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
			return hex.DecodeString(hexText)
		}
	}
	return nil, valueError(fmt.Sprintf("unexpected aes_key format (%d decoded bytes)", len(decoded)))
}

// Chat type and effective chat id of an inbound message
func guessChatType(message *jsonutil.Object, accountID string) (string, string) {
	roomID := textOf(anyutil.Or(message.Value("room_id"), message.Value("chat_room_id")))
	toUserID := textOf(message.Value("to_user_id"))
	if roomID != "" || (toUserID != "" && accountID != "" && toUserID != accountID && equalsInt(message.Value("msg_type"), 1)) {
		for _, candidate := range []string{roomID, toUserID} {
			if candidate != "" {
				return "group", candidate
			}
		}
		return "group", strOf(message.Value("from_user_id"))
	}
	return "dm", strOf(message.Value("from_user_id"))
}

// Runs one request and returns its status, headers and body under a whole call deadline
func (s *ilinkSession) do(ctx context.Context, method, rawURL string, headers []header, body []byte, timeoutSeconds float64) (*httputil.Response, error) {
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
	return &httputil.Response{StatusCode: resp.StatusCode, Header: resp.Header, URL: rawURL, Content: content, Status: resp.Status}, nil
}

// Error of a failed request naming the host, the caller's cancellation first, then the deadline timeout, then the transport error
func requestError(ctx, rctx context.Context, rawURL string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	host := rawURL
	if parsed, perr := url.Parse(rawURL); perr == nil {
		host = parsed.Host
	}
	if rctx.Err() != nil {
		return &kindErr{"TimeoutError", "request to " + host + " timed out"}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("request to %s failed: %w", host, err)
}

// One iLink call returning its decoded JSON, an error for an HTTP error status
func apiRequest(ctx context.Context, session *ilinkSession, method, baseURL, endpoint string, headers []header, timeoutMS int, body []byte) (*jsonutil.Object, error) {
	resp, err := session.do(ctx, method, strings.TrimRight(baseURL, "/")+"/"+endpoint, headers, body, float64(timeoutMS)/1000)
	if err != nil {
		return nil, err
	}
	raw := resp.Text()
	if resp.StatusCode >= 400 {
		return nil, runtimeError(fmt.Sprintf("iLink %s %s HTTP %d: %s", method, endpoint, resp.StatusCode, textutil.First(raw, 200)))
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		return nil, err
	}
	data, ok := decoded.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("iLink %s %s: expected an object, got %s", method, endpoint, anyutil.Kind(decoded))
	}
	return data, nil
}

// POST of a payload with the channel version base info
func apiPost(ctx context.Context, session *ilinkSession, baseURL, endpoint string, payload *jsonutil.Object, token string, timeoutMS int) (*jsonutil.Object, error) {
	full := payload.Clone()
	full.Set("base_info", jsonutil.ObjectOf("channel_version", channelVersion))
	body, err := jsonutil.Encode(full)
	if err != nil {
		return nil, err
	}
	return apiRequest(ctx, session, http.MethodPost, baseURL, endpoint, ilinkHeaders(token, body), timeoutMS, []byte(body))
}

// GET of an endpoint with the app id and client version headers
func apiGet(ctx context.Context, session *ilinkSession, baseURL, endpoint string, timeoutMS int) (*jsonutil.Object, error) {
	headers := []header{{"iLink-App-Id", ilinkAppID}, {"iLink-App-ClientVersion", strconv.Itoa(ilinkAppClientVersion)}}
	return apiRequest(ctx, session, http.MethodGet, baseURL, endpoint, headers, timeoutMS, nil)
}

// Long poll for updates, a timeout answering an empty batch at the same cursor
func getUpdates(ctx context.Context, session *ilinkSession, baseURL, token string, syncBuf any, timeoutMS int) (*jsonutil.Object, error) {
	resp, err := apiPost(ctx, session, baseURL, epGetUpdates, jsonutil.ObjectOf("get_updates_buf", syncBuf), token, timeoutMS)
	if isTimeout(err) {
		return jsonutil.ObjectOf("ret", int64(0), "msgs", []any{}, "get_updates_buf", syncBuf), nil
	}
	return resp, err
}

// Sends an item list to a peer as a finished bot message
func sendItems(ctx context.Context, session *ilinkSession, baseURL, token, to string, itemList []any, contextToken, clientID string) (*jsonutil.Object, error) {
	message := jsonutil.ObjectOf("from_user_id", "", "to_user_id", to, "client_id", clientID, "message_type", int64(msgTypeBot),
		"message_state", int64(msgStateFinish), "item_list", itemList)
	if contextToken != "" {
		message.Set("context_token", contextToken)
	}
	return apiPost(ctx, session, baseURL, epSendMessage, jsonutil.ObjectOf("msg", message), token, apiTimeoutMS)
}

// Sends one text message, an error for blank text
func sendMessage(ctx context.Context, session *ilinkSession, baseURL, token, to, text, contextToken, clientID string) (*jsonutil.Object, error) {
	if strings.TrimSpace(text) == "" {
		return nil, valueError("_send_message: text must not be empty")
	}
	itemList := []any{jsonutil.ObjectOf("type", int64(itemText), "text_item", jsonutil.ObjectOf("text", text))}
	return sendItems(ctx, session, baseURL, token, to, itemList, contextToken, clientID)
}

// Fetches the per user config that carries the typing ticket
func getConfig(ctx context.Context, session *ilinkSession, baseURL, token, userID, contextToken string) (*jsonutil.Object, error) {
	payload := jsonutil.ObjectOf("ilink_user_id", userID)
	if contextToken != "" {
		payload.Set("context_token", contextToken)
	}
	return apiPost(ctx, session, baseURL, epGetConfig, payload, token, configTimeoutMS)
}

// Requests an upload slot of the encrypted CDN for one media file
func getUploadURL(ctx context.Context, session *ilinkSession, baseURL, token, toUserID string, mediaType int, filekey string, rawsize int, rawfilemd5 string, filesize int, aeskeyHex string) (*jsonutil.Object, error) {
	payload := jsonutil.ObjectOf("filekey", filekey, "media_type", int64(mediaType), "to_user_id", toUserID, "rawsize", int64(rawsize),
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
	raw := textutil.First(resp.Text(), 200)
	if resp.StatusCode == 200 {
		return "", runtimeError("CDN upload missing x-encrypted-param header: " + raw)
	}
	return "", runtimeError(fmt.Sprintf("CDN upload HTTP %d: %s", resp.StatusCode, raw))
}

// Downloads a URL, the status error of an HTTP error status
func downloadBytes(ctx context.Context, session *ilinkSession, rawURL string, timeoutSeconds float64) ([]byte, error) {
	resp, err := session.do(ctx, http.MethodGet, rawURL, nil, nil, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &httputil.StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: rawURL}
	}
	return resp.Content, nil
}

// Hosts media may be fetched from by full URL
var weixinCDNAllowlist = map[string]bool{"novac2c.cdn.weixin.qq.com": true, "ilinkai.weixin.qq.com": true, "wx.qlogo.cn": true,
	"thirdwx.qlogo.cn": true, "res.wx.qq.com": true, "mmbiz.qpic.cn": true, "mmbiz.qlogo.cn": true}

// Error for a media URL that is not http or https on an allowlisted WeChat CDN host
func assertWeixinCDNURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return valueError("Unparseable media URL: " + strconv.Quote(rawURL))
	}
	scheme, host := strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Hostname())
	if scheme != "http" && scheme != "https" {
		return valueError(fmt.Sprintf("Media URL has disallowed scheme %s; only http/https are permitted.", strconv.Quote(scheme)))
	}
	if !weixinCDNAllowlist[host] {
		return valueError(fmt.Sprintf("Media URL host %s is not in the WeChat CDN allowlist. Refusing to fetch to prevent SSRF.", strconv.Quote(host)))
	}
	return nil
}

// Downloads a media item by its encrypted param or allowlisted full URL and decrypts it when it carries a key
func downloadAndDecryptMedia(ctx context.Context, session *ilinkSession, cdnBaseURL string, encryptedQueryParam, aesKeyB64, fullURL any, timeoutSeconds float64) ([]byte, error) {
	var rawURL string
	switch {
	case anyutil.IsTrue(encryptedQueryParam):
		rawURL = strings.TrimRight(cdnBaseURL, "/") + "/download?encrypted_query_param=" + url.QueryEscape(anyutil.String(encryptedQueryParam))
	case anyutil.IsTrue(fullURL):
		rawURL = anyutil.String(fullURL)
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
	if !anyutil.IsTrue(aesKeyB64) {
		return raw, nil
	}
	key, err := parseAESKey(aesKeyB64)
	if err != nil {
		return nil, err
	}
	return aes128ECBDecrypt(raw, key)
}
