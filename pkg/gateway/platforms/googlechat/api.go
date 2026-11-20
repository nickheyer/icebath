package googlechat

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	chatAPIBase       = "https://chat.googleapis.com/v1/"
	chatUploadBase    = "https://chat.googleapis.com/upload/v1/"
	pubsubAPIBase     = "https://pubsub.googleapis.com/v1/"
	apiTimeout        = 30 * time.Second
	pullTimeout       = 90 * time.Second
	retryMaxAttempts  = 3
	retryBaseDelay    = 1.0
	retryMaxDelay     = 8.0
	retryJitter       = 0.3
	idTokenCertsURL   = "https://www.googleapis.com/oauth2/v1/certs"
	idTokenCertsTTL   = 300 * time.Second
	idTokenClockSkew  = 10 * time.Second
	googleAuthTimeout = 30 * time.Second
)

// Scopes of the bot identity, messaging plus the Pub/Sub subscription
var chatScopes = []string{"https://www.googleapis.com/auth/chat.bot", "https://www.googleapis.com/auth/pubsub"}

// Statuses a Chat or Pub/Sub call is retried on
var retryableStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// Transport failure wording that marks an error without an HTTP status as transient
var transportErrorPhrases = []string{"timeout", "timed out", "broken pipe", "remote disconnected"}

// Hosts an attachment download URI may target so the bot token never leaves Google
var trustedAttachmentHosts = []string{"googleapis.com", "chat.google.com", "drive.google.com", "docs.google.com",
	"lh3.googleusercontent.com", "lh4.googleusercontent.com", "lh5.googleusercontent.com", "lh6.googleusercontent.com"}

// Resource paths and service account emails removed from user facing errors
var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`projects/[^/\s]+/subscriptions/[^/\s]+`), "projects/<redacted>/subscriptions/<redacted>"},
	{regexp.MustCompile(`projects/[^/\s]+/topics/[^/\s]+`), "projects/<redacted>/topics/<redacted>"},
	{regexp.MustCompile(`[a-zA-Z0-9_.+-]+@[a-zA-Z0-9-]+\.iam\.gserviceaccount\.com`), "<sa>@<project>.iam.gserviceaccount.com"},
}

// Error string with Pub/Sub resource paths and service account emails redacted
func redactSensitive(text string) string {
	for _, r := range redactions {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// Google APIs the adapter calls, each of which NO_PROXY must keep off the proxy before a failed call's text is shown
var apiProxyTargets = []string{"https://chat.googleapis.com", "https://oauth2.googleapis.com", "https://pubsub.googleapis.com", "https://www.googleapis.com"}

// Kind of a failed call and the proxy's host when the proxy URL the clients read from the environment holds a secret the raw error text could repeat,
// empty otherwise: targets are the URLs the call fetched, the APIs when none is given
func proxyError(err error, targets ...string) string {
	if len(targets) == 0 {
		targets = apiProxyTargets
	}
	return gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(targets...))
}

// Text of a failed call for a log line or a send result, the redacted form when it could repeat a proxy secret and the error text without resource
// paths and service account emails otherwise
func excText(err error, targets ...string) string {
	return cmp.Or(proxyError(err, targets...), redactSensitive(err.Error()))
}

// Log attributes carrying the error of a failed call, none when its text could repeat a proxy secret
func errAttr(err error) []any {
	if proxyError(err) != "" {
		return nil
	}
	return []any{"error", err}
}

// Non success answer of a Google REST call
type httpError struct {
	status int
	method string
	url    string
	reason string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("<HttpError %d when requesting %s returned %q>", e.status, e.url, e.reason)
}

// HTTP status of a REST error, zero for transport failures
func httpStatus(err error) int {
	var h *httpError
	if errors.As(err, &h) {
		return h.status
	}
	return 0
}

// Whether a failure is transient, a retryable status or connection level wording
func isRetryableError(err error) bool {
	if status := httpStatus(err); status != 0 {
		return retryableStatuses[status]
	}
	text := strings.ToLower(err.Error())
	for _, p := range transportErrorPhrases {
		if strings.Contains(text, p) {
			return true
		}
	}
	return strings.Contains(text, "connection") && (strings.Contains(text, "reset") || strings.Contains(text, "refused") || strings.Contains(text, "aborted"))
}

// Whether a URL is https on a Google owned host
func isGoogleOwnedHost(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || host == "" {
		return false
	}
	for _, h := range trustedAttachmentHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// REST client authenticated by one Google credential
type restClient struct {
	creds *provider.GoogleCredentials
	http  *http.Client
}

// Client for a credential with proxy env honoured
func newRESTClient(creds *provider.GoogleCredentials) *restClient {
	return &restClient{creds: creds, http: &http.Client{Transport: netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())}}
}

// One authenticated request, the JSON answer decoded into out when given
func (c *restClient) do(ctx context.Context, method, target string, body io.Reader, contentType string, timeout time.Duration, out any) error {
	token, err := c.creds.Token(ctx)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if qp := c.creds.QuotaProject(); qp != "" {
		req.Header.Set("x-goog-user-project", qp)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		reason := strings.TrimSpace(string(data))
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
			reason = envelope.Error.Message
		}
		return &httpError{status: resp.StatusCode, method: method, url: target, reason: reason}
	}
	if out == nil {
		return nil
	}
	if b, ok := out.(*[]byte); ok {
		*b = data
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// JSON request with a JSON answer
func (c *restClient) doJSON(ctx context.Context, method, target string, payload any, out any) error {
	var body io.Reader
	contentType := ""
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body, contentType = bytes.NewReader(data), "application/json"
	}
	return c.do(ctx, method, target, body, contentType, apiTimeout, out)
}

// Runs a call with bounded retry and jittered backoff, permanent failures returned on the first attempt
func callWithRetry[T any](ctx context.Context, opName string, fn func() (T, error)) (T, error) {
	delay := retryBaseDelay
	for attempt := 1; ; attempt++ {
		out, err := fn()
		if err == nil || ctx.Err() != nil || !isRetryableError(err) || attempt >= retryMaxAttempts {
			return out, err
		}
		wait := min(delay+delay*retryJitter*rand.Float64(), retryMaxDelay+retryJitter)
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] %s attempt %d/%d failed (%s); retrying in %.2fs", opName, attempt, retryMaxAttempts, excText(err), wait))
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(time.Duration(wait * float64(time.Second))):
		}
		delay = min(delay*2, retryMaxDelay)
	}
}

// Message resource the Chat API answers with
type chatMessage map[string]any

// Creates a message under a space, replying into the body thread with fallback to a new thread
func (c *restClient) createMessage(ctx context.Context, parent string, body map[string]any) (chatMessage, error) {
	q := url.Values{}
	if thread, _ := body["thread"].(map[string]any); thread != nil && thread["name"] != nil && thread["name"] != "" {
		q.Set("messageReplyOption", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	}
	target := chatAPIBase + parent + "/messages"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var out chatMessage
	err := c.doJSON(ctx, http.MethodPost, target, body, &out)
	return out, err
}

// Updates the masked fields of a message in place
func (c *restClient) patchMessage(ctx context.Context, name, updateMask string, body map[string]any) (chatMessage, error) {
	var out chatMessage
	err := c.doJSON(ctx, http.MethodPatch, chatAPIBase+name+"?"+url.Values{"updateMask": {updateMask}}.Encode(), body, &out)
	return out, err
}

// Deletes a message
func (c *restClient) deleteMessage(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, chatAPIBase+name, nil, nil)
}

// Space resource
func (c *restClient) getSpace(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	err := c.doJSON(ctx, http.MethodGet, chatAPIBase+name, nil, &out)
	return out, err
}

// First page of a space's memberships
func (c *restClient) listMembers(ctx context.Context, parent string, pageSize int) (map[string]any, error) {
	var out map[string]any
	err := c.doJSON(ctx, http.MethodGet, chatAPIBase+parent+"/members?"+url.Values{"pageSize": {fmt.Sprint(pageSize)}}.Encode(), nil, &out)
	return out, err
}

// Bytes of an attachment resource through media download
func (c *restClient) downloadMedia(ctx context.Context, resourceName string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, http.MethodGet, chatAPIBase+"media/"+resourceName+"?alt=media", nil, "", apiTimeout, &out)
	return out, err
}

// Bytes behind an authorized GET of a URI
func (c *restClient) fetchURI(ctx context.Context, uri string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, http.MethodGet, uri, nil, "", apiTimeout, &out)
	return out, err
}

// Uploads a local file as a space attachment with a multipart related body
func (c *restClient) uploadMedia(ctx context.Context, parent, filename, path, mimeType string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	meta, err := json.Marshal(map[string]any{"filename": filename})
	if err != nil {
		return nil, err
	}
	part, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(meta); err != nil {
		return nil, err
	}
	part, err = w.CreatePart(textproto.MIMEHeader{"Content-Type": {mimeType}})
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var out map[string]any
	target := chatUploadBase + parent + "/attachments:upload?uploadType=multipart"
	err = c.do(ctx, http.MethodPost, target, &buf, "multipart/related; boundary="+w.Boundary(), apiTimeout, &out)
	return out, err
}

// Subscription resource, the existence and access check
func (c *restClient) getSubscription(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodGet, pubsubAPIBase+path, nil, nil)
}

// One delivered Pub/Sub message with its ack id
type pulledMessage struct {
	AckID   string `json:"ackId"`
	Message struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
}

// Long polls up to max messages from the subscription
func (c *restClient) pull(ctx context.Context, path string, maxMessages int) ([]pulledMessage, error) {
	payload, err := json.Marshal(map[string]any{"maxMessages": maxMessages})
	if err != nil {
		return nil, err
	}
	var out struct {
		ReceivedMessages []pulledMessage `json:"receivedMessages"`
	}
	err = c.do(ctx, http.MethodPost, pubsubAPIBase+path+":pull", bytes.NewReader(payload), "application/json", pullTimeout, &out)
	return out.ReceivedMessages, err
}

// Acknowledges delivered messages
func (c *restClient) acknowledge(ctx context.Context, path string, ackIDs ...string) error {
	return c.doJSON(ctx, http.MethodPost, pubsubAPIBase+path+":acknowledge", map[string]any{"ackIds": ackIDs}, nil)
}

// Returns delivered messages for immediate redelivery
func (c *restClient) nack(ctx context.Context, path string, ackIDs ...string) error {
	return c.doJSON(ctx, http.MethodPost, pubsubAPIBase+path+":modifyAckDeadline", map[string]any{"ackIds": ackIDs, "ackDeadlineSeconds": 0}, nil)
}

// Google signing certificates cached for the certificate TTL
var idTokenCerts struct {
	mu      sync.Mutex
	fetched time.Time
	keys    map[string]*rsa.PublicKey
}

// Signing keys of Google ID tokens by key id, fetched at most once per TTL
func googleIDTokenKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	idTokenCerts.mu.Lock()
	defer idTokenCerts.mu.Unlock()
	if idTokenCerts.keys != nil && time.Since(idTokenCerts.fetched) < idTokenCertsTTL {
		return idTokenCerts.keys, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, googleAuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, idTokenCertsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not fetch certificates at %s: status %d", idTokenCertsURL, resp.StatusCode)
	}
	var certs map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&certs); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for kid, certPEM := range certs {
		block, _ := pem.Decode([]byte(certPEM))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			keys[kid] = pub
		}
	}
	idTokenCerts.keys, idTokenCerts.fetched = keys, time.Now()
	return keys, nil
}

// Claims of a Google signed ID token after signature, issuer, audience and time checks
func verifyGoogleIDToken(ctx context.Context, token, audience string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("Wrong number of segments in token")
	}
	decode := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")) }
	headerRaw, err := decode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("Can't parse segment: %w", err)
	}
	payloadRaw, err := decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("Can't parse segment: %w", err)
	}
	sig, err := decode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("Can't parse segment: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, err
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("Unsupported token algorithm %s", header.Alg)
	}
	keys, err := googleIDTokenKeys(ctx)
	if err != nil {
		return nil, err
	}
	key, ok := keys[header.Kid]
	if !ok {
		return nil, fmt.Errorf("Certificate for key id %s not found.", header.Kid)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("Could not verify token signature.")
	}
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(payloadRaw))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return nil, err
	}
	now := time.Now()
	iat, iatOK := claimTime(claims["iat"])
	exp, expOK := claimTime(claims["exp"])
	if !iatOK {
		return nil, errors.New("Token does not contain required claim iat")
	}
	if !expOK {
		return nil, errors.New("Token does not contain required claim exp")
	}
	if now.Add(idTokenClockSkew).Before(iat) {
		return nil, errors.New("Token used too early")
	}
	if now.Add(-idTokenClockSkew).After(exp) {
		return nil, errors.New("Token expired")
	}
	if !audienceMatches(claims["aud"], audience) {
		return nil, fmt.Errorf("Token has wrong audience %v, expected one of [%s]", claims["aud"], audience)
	}
	if iss, _ := claims["iss"].(string); iss != "accounts.google.com" && iss != "https://accounts.google.com" {
		return nil, fmt.Errorf("Wrong issuer. 'iss' should be one of the following: ['accounts.google.com', 'https://accounts.google.com'], got %s", iss)
	}
	return claims, nil
}

// Time of a numeric JWT claim
func claimTime(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// Whether the aud claim names the audience, as a string or a list
func audienceMatches(aud any, audience string) bool {
	switch v := aud.(type) {
	case string:
		return v == audience
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == audience {
				return true
			}
		}
	}
	return false
}
