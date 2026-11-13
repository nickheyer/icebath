package googlechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	chatHost     = "chat.googleapis.com"
	pubsubHost   = "pubsub.googleapis.com"
	callTimeout  = 30 * time.Second
	mediaTimeout = 5 * time.Minute
	pullTimeout  = 2 * time.Minute

	retryMaxAttempts = 3
	retryBaseDelay   = time.Second
	retryMaxDelay    = 8 * time.Second
	retryJitter      = 0.3
)

var (
	chatScopes = []string{"https://www.googleapis.com/auth/chat.bot", "https://www.googleapis.com/auth/pubsub"}
	redactions = []struct {
		re   *regexp.Regexp
		repl string
	}{
		{regexp.MustCompile(`projects/[^/\s]+/subscriptions/[^/\s]+`), "projects/<redacted>/subscriptions/<redacted>"},
		{regexp.MustCompile(`projects/[^/\s]+/topics/[^/\s]+`), "projects/<redacted>/topics/<redacted>"},
		{regexp.MustCompile(`[a-zA-Z0-9_.+-]+@[a-zA-Z0-9-]+\.iam\.gserviceaccount\.com`), "<sa>@<project>.iam.gserviceaccount.com"},
	}
)

// Pub/Sub resource paths and service account emails replaced in user facing error text
func redact(text string) string {
	for _, r := range redactions {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// Non 2xx answer of a Google API with its status and decoded error message
type apiError struct {
	status  int
	reason  string
	message string
	op      string
}

func (e *apiError) Error() string {
	if e.reason != "" {
		return fmt.Sprintf("%s returned %d %s: %s", e.op, e.status, e.reason, e.message)
	}
	return fmt.Sprintf("%s returned %d: %s", e.op, e.status, e.message)
}

// HTTP status of an API error, zero for any other error
func errStatus(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 0
}

// Whether a failure is transient, 429, 5xx and transport errors but never auth or other 4xx
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var te *tokenError
	if errors.As(err, &te) {
		return te.retryable
	}
	switch status := errStatus(err); status {
	case 0:
		return true
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// API error decoded from a Google JSON error body
func decodeAPIError(op string, status int, raw []byte) *apiError {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	e := &apiError{status: status, op: op}
	if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
		e.reason, e.message = body.Error.Status, body.Error.Message
		return e
	}
	e.message = strings.TrimSpace(string(raw))
	if len(e.message) > 300 {
		e.message = e.message[:300]
	}
	return e
}

// Google REST client authorizing every call with one token source
type googleAPI struct {
	client *http.Client
	auth   tokenSource
}

// URL of a resource path on a Google API host with its query
func apiURL(host, path string, query url.Values) string {
	u := url.URL{Scheme: "https", Host: host, Path: path}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// Sends one authorized request and decodes a JSON answer into out, the raw bytes when out is a byte slice pointer
func (g *googleAPI) do(ctx context.Context, op, method, target string, contentType string, body []byte, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if err := g.auth.authorize(ctx, req.Header); err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(op, resp.StatusCode, raw)
	}
	switch dst := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*dst = raw
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// JSON request whose answer decodes into out
func (g *googleAPI) json(ctx context.Context, op, method, target string, in, out any) error {
	var body []byte
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, contentType = b, "application/json"
	}
	return g.do(ctx, op, method, target, contentType, body, callTimeout, out)
}

// Runs fn with bounded jittered backoff over transient failures, permanent ones returned at once
func callWithRetry[T any](ctx context.Context, op string, fn func() (T, error)) (T, error) {
	delay := retryBaseDelay
	for attempt := 1; ; attempt++ {
		v, err := fn()
		if err == nil || !isTransient(err) || attempt >= retryMaxAttempts {
			return v, err
		}
		wait := min(delay+time.Duration(float64(delay)*retryJitter*rand.Float64()), retryMaxDelay+time.Duration(retryJitter*float64(time.Second)))
		slog.Warn(fmt.Sprintf("[GoogleChat] %s attempt %d/%d failed (%s); retrying in %.2fs", op, attempt, retryMaxAttempts,
			redact(err.Error()), wait.Seconds()))
		if err := sleepCtx(ctx, wait); err != nil {
			return v, err
		}
		delay = min(delay*2, retryMaxDelay)
	}
}

// Chat message resource as sent and received
type chatMessage struct {
	Name         string           `json:"name,omitempty"`
	Sender       *chatUser        `json:"sender,omitempty"`
	Text         string           `json:"text,omitempty"`
	ArgumentText string           `json:"argumentText,omitempty"`
	Thread       *chatThread      `json:"thread,omitempty"`
	Space        *chatSpace       `json:"space,omitempty"`
	Attachment   []chatAttachment `json:"attachment,omitempty"`
	CardsV2      []cardV2         `json:"cardsV2,omitempty"`
	SlashCommand *slashCommand    `json:"slashCommand,omitempty"`
}

type chatUser struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	Type        string `json:"type,omitempty"`
}

type chatThread struct {
	Name string `json:"name,omitempty"`
}

type chatSpace struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	SpaceType   string `json:"spaceType,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type slashCommand struct {
	CommandID json.RawMessage `json:"commandId,omitempty"`
}

type attachmentDataRef struct {
	ResourceName          string `json:"resourceName,omitempty"`
	AttachmentUploadToken string `json:"attachmentUploadToken,omitempty"`
}

type chatAttachment struct {
	Name              string             `json:"name,omitempty"`
	ContentName       string             `json:"contentName,omitempty"`
	ContentType       string             `json:"contentType,omitempty"`
	DownloadURI       string             `json:"downloadUri,omitempty"`
	Source            string             `json:"source,omitempty"`
	AttachmentDataRef *attachmentDataRef `json:"attachmentDataRef,omitempty"`
}

// Space type upper cased, the legacy type field read when spaceType is absent
func (s *chatSpace) kind() string {
	if s == nil {
		return ""
	}
	if s.Type != "" {
		return strings.ToUpper(s.Type)
	}
	return strings.ToUpper(s.SpaceType)
}

// Message body with the thread set when replying into one
func threadBody(text, threadID string) *chatMessage {
	m := &chatMessage{Text: text}
	if threadID != "" {
		m.Thread = &chatThread{Name: threadID}
	}
	return m
}

// Creates a message, the reply option set so a thread name is honored and a vanished thread starts a new one
func (g *googleAPI) createMessage(ctx context.Context, parent string, msg *chatMessage) (*chatMessage, error) {
	query := url.Values{}
	if msg.Thread != nil && msg.Thread.Name != "" {
		query.Set("messageReplyOption", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	}
	var out chatMessage
	err := g.json(ctx, "messages.create", http.MethodPost, apiURL(chatHost, "/v1/"+parent+"/messages", query), msg, &out)
	return &out, err
}

// Updates the text and cards of a message in place, its thread being immutable
func (g *googleAPI) patchMessage(ctx context.Context, name string, msg *chatMessage) (*chatMessage, error) {
	var mask []string
	if msg.Text != "" {
		mask = append(mask, "text")
	}
	if len(msg.CardsV2) > 0 {
		mask = append(mask, "cardsV2")
	}
	if len(mask) == 0 {
		mask = []string{"text"}
	}
	body := &chatMessage{Text: msg.Text, CardsV2: msg.CardsV2}
	var out chatMessage
	err := g.json(ctx, "messages.patch", http.MethodPatch, apiURL(chatHost, "/v1/"+name, url.Values{"updateMask": {strings.Join(mask, ",")}}), body, &out)
	if out.Name == "" {
		out.Name = name
	}
	return &out, err
}

// Deletes a message
func (g *googleAPI) deleteMessage(ctx context.Context, name string) error {
	return g.json(ctx, "messages.delete", http.MethodDelete, apiURL(chatHost, "/v1/"+name, nil), nil, nil)
}

// Space resource of a name
func (g *googleAPI) getSpace(ctx context.Context, name string) (*chatSpace, error) {
	var out chatSpace
	err := g.json(ctx, "spaces.get", http.MethodGet, apiURL(chatHost, "/v1/"+name, nil), nil, &out)
	return &out, err
}

// First page of a space's memberships
func (g *googleAPI) listMembers(ctx context.Context, space string) ([]chatUser, error) {
	var out struct {
		Memberships []struct {
			Member chatUser `json:"member"`
		} `json:"memberships"`
	}
	if err := g.json(ctx, "members.list", http.MethodGet, apiURL(chatHost, "/v1/"+space+"/members", url.Values{"pageSize": {"50"}}), nil, &out); err != nil {
		return nil, err
	}
	members := make([]chatUser, 0, len(out.Memberships))
	for _, m := range out.Memberships {
		members = append(members, m.Member)
	}
	return members, nil
}

// Bytes of an uploaded attachment through media.download
func (g *googleAPI) downloadMedia(ctx context.Context, resourceName string) ([]byte, error) {
	var raw []byte
	err := g.do(ctx, "media.download", http.MethodGet, apiURL(chatHost, "/v1/media/"+resourceName, url.Values{"alt": {"media"}}), "", nil, mediaTimeout, &raw)
	return raw, err
}

// Bytes behind an attachment download URI fetched with the bearer token
func (g *googleAPI) downloadURI(ctx context.Context, target string) ([]byte, error) {
	var raw []byte
	err := g.do(ctx, "attachment download", http.MethodGet, target, "", nil, callTimeout, &raw)
	return raw, err
}

// Uploads a local file as a space attachment through a multipart media.upload
func (g *googleAPI) uploadMedia(ctx context.Context, parent, path, filename, mimeType string) (*attachmentDataRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	meta, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(meta).Encode(map[string]string{"filename": filename}); err != nil {
		return nil, err
	}
	media, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {mimeType}})
	if err != nil {
		return nil, err
	}
	if _, err := media.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var out struct {
		AttachmentDataRef *attachmentDataRef `json:"attachmentDataRef"`
	}
	target := apiURL(chatHost, "/upload/v1/"+parent+"/attachments:upload", url.Values{"uploadType": {"multipart"}})
	if err := g.do(ctx, "media.upload", http.MethodPost, target, "multipart/related; boundary="+w.Boundary(), body.Bytes(), mediaTimeout, &out); err != nil {
		return nil, err
	}
	return out.AttachmentDataRef, nil
}

// Pub/Sub subscription existence and access probe
func (g *googleAPI) getSubscription(ctx context.Context, subscription string) error {
	return g.json(ctx, "subscriptions.get", http.MethodGet, apiURL(pubsubHost, "/v1/"+subscription, nil), nil, nil)
}

// One message a Pub/Sub pull leased to this subscriber
type receivedMessage struct {
	AckID   string `json:"ackId"`
	Message struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
}

// Long polled pull of up to max messages, an empty batch when the server or the poll window times out
func (g *googleAPI) pull(ctx context.Context, subscription string, max int) ([]receivedMessage, error) {
	body, err := json.Marshal(map[string]any{"maxMessages": max})
	if err != nil {
		return nil, err
	}
	var out struct {
		ReceivedMessages []receivedMessage `json:"receivedMessages"`
	}
	err = g.do(ctx, "subscriptions.pull", http.MethodPost, apiURL(pubsubHost, "/v1/"+subscription+":pull", nil), "application/json", body, pullTimeout, &out)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, nil
	}
	return out.ReceivedMessages, err
}

// Acknowledges leased messages
func (g *googleAPI) acknowledge(ctx context.Context, subscription string, ackIDs []string) error {
	if len(ackIDs) == 0 {
		return nil
	}
	return g.json(ctx, "subscriptions.acknowledge", http.MethodPost, apiURL(pubsubHost, "/v1/"+subscription+":acknowledge", nil),
		map[string]any{"ackIds": ackIDs}, nil)
}

// Returns leased messages for immediate redelivery
func (g *googleAPI) nack(ctx context.Context, subscription string, ackIDs []string) error {
	if len(ackIDs) == 0 {
		return nil
	}
	return g.json(ctx, "subscriptions.modifyAckDeadline", http.MethodPost, apiURL(pubsubHost, "/v1/"+subscription+":modifyAckDeadline", nil),
		map[string]any{"ackIds": ackIDs, "ackDeadlineSeconds": 0}, nil)
}
