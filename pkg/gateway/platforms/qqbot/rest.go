package qqbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Error answer of the QQ Bot REST API with its HTTP status and business code
type apiError struct {
	status  int
	path    string
	code    int64
	message string
}

func (e *apiError) Error() string {
	if e.code != 0 && !strings.Contains(e.message, strconv.FormatInt(e.code, 10)) {
		return fmt.Sprintf("QQ Bot API error [%d] %s: %s (code %d)", e.status, e.path, e.message, e.code)
	}
	return fmt.Sprintf("QQ Bot API error [%d] %s: %s", e.status, e.path, e.message)
}

// Whether the answer carries the business code in its fields or its message
func hasBizCode(err error, code int64) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.code == code || strings.Contains(ae.message, strconv.FormatInt(code, 10))
}

// Whether the error is a request deadline rather than an answer
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Reply sequence counter of one inbound message id
type msgSeq struct {
	next int
	at   time.Time
}

// Token cache, REST calls, uploads and message posts of one QQ bot app
type restClient struct {
	appID   string
	secret  string
	apiBase string
	logTag  string
	http    *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	seqs    map[string]*msgSeq
	fetchMu sync.Mutex
}

func newRESTClient(appID, secret, apiBase, logTag string, client *http.Client) *restClient {
	return &restClient{appID: appID, secret: secret, apiBase: apiBase, logTag: logTag, http: client, seqs: map[string]*msgSeq{}}
}

// Cached token while it has more than a minute left, empty otherwise
func (r *restClient) freshToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token != "" && time.Now().Before(r.expires.Add(-time.Minute)) {
		return r.token
	}
	return ""
}

// Cached token regardless of its age, empty before the first fetch
func (r *restClient) cachedToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token
}

// Drops the cached token so the next call fetches a new one
func (r *restClient) invalidateToken() {
	r.mu.Lock()
	r.token, r.expires = "", time.Time{}
	r.mu.Unlock()
}

// Valid access token, one fetch at a time when it needs refreshing
func (r *restClient) ensureToken(ctx context.Context) (string, error) {
	if t := r.freshToken(); t != "" {
		return t, nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if t := r.freshToken(); t != "" {
		return t, nil
	}
	status, raw, err := r.send(ctx, http.MethodPost, tokenURL, map[string]string{"Content-Type": "application/json"},
		map[string]any{"appId": r.appID, "clientSecret": r.secret}, defaultAPITimeout)
	if err != nil {
		return "", fmt.Errorf("get QQ Bot access token: %w", err)
	}
	if status >= 400 {
		return "", fmt.Errorf("get QQ Bot access token: HTTP %d: %s", status, cutRunes(string(raw), 200))
	}
	decoded, err := decodeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("get QQ Bot access token: %w", err)
	}
	data := jmap(decoded)
	token := jstr(data["access_token"])
	if token == "" {
		return "", fmt.Errorf("QQ Bot token response missing access_token: %s", cutRunes(string(raw), 200))
	}
	expiresIn, ok := jint(data["expires_in"])
	if !ok {
		expiresIn = 7200
	}
	r.mu.Lock()
	r.token, r.expires = token, time.Now().Add(time.Duration(expiresIn)*time.Second)
	r.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Access token refreshed, expires in %ds", r.logTag, expiresIn))
	return token, nil
}

// JSON REST headers with a fresh bot token
func (r *restClient) authHeaders(ctx context.Context) (map[string]string, error) {
	token, err := r.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "QQBot " + token, "Content-Type": "application/json", "User-Agent": userAgent()}, nil
}

// Status and body of one request with a JSON or raw payload under its own deadline
func (r *restClient) send(ctx context.Context, method, rawURL string, headers map[string]string, payload any, timeout time.Duration) (int, []byte, error) {
	var body io.Reader
	switch p := payload.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(p)
	default:
		encoded, err := json.Marshal(p)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, rawURL, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// Decoded JSON object of a REST call, an apiError for an error status
func (r *restClient) api(ctx context.Context, method, path string, payload any, timeout time.Duration) (map[string]any, error) {
	headers, err := r.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	status, raw, err := r.send(ctx, method, r.apiBase+path, headers, payload, timeout)
	if err != nil {
		if isTimeout(err) && ctx.Err() == nil {
			return nil, fmt.Errorf("QQ Bot API timeout [%s]: %w", path, err)
		}
		return nil, err
	}
	var data map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if decoded, derr := decodeJSON(raw); derr == nil {
			data = jmap(decoded)
		}
	}
	if status >= 400 {
		e := &apiError{status: status, path: path, message: strings.TrimSpace(cutRunes(string(raw), 300))}
		if data != nil {
			if msg := jstr(data["message"]); msg != "" {
				e.message = msg
			}
			if code, ok := jint(data["code"]); ok && code != 0 {
				e.code = code
			} else if code, ok := jint(data["err_code"]); ok {
				e.code = code
			}
		}
		return nil, e
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// WebSocket gateway URL of the bot
func (r *restClient) gatewayURL(ctx context.Context) (string, error) {
	data, err := r.api(ctx, http.MethodGet, gatewayURLPath, nil, defaultAPITimeout)
	if err != nil {
		return "", fmt.Errorf("get QQ Bot gateway URL: %w", err)
	}
	u := jstr(data["url"])
	if u == "" {
		return "", fmt.Errorf("QQ Bot gateway response missing url: %v", data)
	}
	return u, nil
}

// Bytes of a QQ CDN download with the bot token the CDN requires
func (r *restClient) download(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	headers := map[string]string{"User-Agent": userAgent()}
	if token := r.cachedToken(); token != "" {
		headers["Authorization"] = "QQBot " + token
	}
	status, raw, err := r.send(ctx, http.MethodGet, rawURL, headers, nil, timeout)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("download %s: HTTP %d", gateway.SafeURLForLog(rawURL, 80), status)
	}
	return raw, nil
}

// Next reply sequence of an inbound message id, unique per id so passive replies never collide
func (r *restClient) nextSeq(msgID string) int {
	if msgID == "" {
		var b [2]byte
		_, _ = rand.Read(b[:])
		return int((uint32(time.Now().Unix()%100000000) ^ uint32(binary.BigEndian.Uint16(b[:]))) % 65536)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if len(r.seqs) > 512 {
		for id, s := range r.seqs {
			if now.Sub(s.at) > time.Hour {
				delete(r.seqs, id)
			}
		}
	}
	s := r.seqs[msgID]
	if s == nil {
		s = &msgSeq{next: 1}
		r.seqs[msgID] = s
	}
	seq := s.next
	s.next++
	s.at = now
	return seq
}

// REST path of a c2c user or group endpoint
func restPath(chatType, targetID, endpoint string) string {
	kind := "groups"
	if chatType == "c2c" {
		kind = "users"
	}
	return "/v2/" + kind + "/" + targetID + "/" + endpoint
}

// Whether an upload failure will fail the same way when repeated
func permanentUploadError(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) && (ae.status == 400 || ae.status == 401 || strings.Contains(ae.message, "Invalid")) {
		return true
	}
	return isTimeout(err) || strings.Contains(err.Error(), "timeout")
}

// Asks QQ to fetch a URL into its media store, retrying transient failures
func (r *restClient) uploadByURL(ctx context.Context, chatType, targetID string, fileType int, mediaURL, fileName string) (map[string]any, error) {
	body := map[string]any{"file_type": fileType, "srv_send_msg": false, "url": mediaURL}
	if fileType == mediaTypeFile && fileName != "" {
		body["file_name"] = fileName
	}
	for attempt := 0; ; attempt++ {
		data, err := r.api(ctx, http.MethodPost, restPath(chatType, targetID, "files"), body, fileUploadTimeout)
		if err == nil {
			return data, nil
		}
		if attempt == 2 || permanentUploadError(err) || ctx.Err() != nil {
			return nil, err
		}
		if err := sleepCtx(ctx, time.Duration(1500*(attempt+1))*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// File info token of an upload answer, bare or wrapped in data
func fileInfoOf(upload map[string]any) string {
	if fi := jstr(upload["file_info"]); fi != "" {
		return fi
	}
	return jstr(jmap(upload["data"])["file_info"])
}

// Posts a message body and wraps the answer as a successful send
func (r *restClient) postMessage(ctx context.Context, path string, body map[string]any) (gateway.SendResult, error) {
	data, err := r.api(ctx, http.MethodPost, path, body, defaultAPITimeout)
	if err != nil {
		return gateway.SendResult{}, err
	}
	id := jstr(data["id"])
	if id == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	return gateway.SendResult{Success: true, MessageID: id, RawResponse: data}, nil
}

// Sleeps for d, ending early with the context error
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
