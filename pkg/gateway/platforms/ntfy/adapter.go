package ntfy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	defaultServer        = "https://ntfy.sh"
	maxMessageLength     = 4096
	dedupWindowSeconds   = 300
	dedupMaxSize         = 1000
	streamTimeoutSeconds = 90
	echoTag              = "icebath-agent"
)

// Seconds to wait before each reconnect attempt, the last value repeating
var reconnectBackoff = []int{2, 5, 10, 30, 60}

// Lowercased markdown flag values that read as true
var markdownTruthy = map[string]bool{"1": true, "true": true, "yes": true}

// Stream failure the reconnect loop never retries, a 401 or a 404
type fatalStreamError struct{ reason string }

func (e *fatalStreamError) Error() string { return e.reason }

// AttributeError a str method raises on a value of another type
func attributeError(v any, attr string) error {
	return &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// Twelve hex digits of a fresh uuid4
func shortUUID() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

// Authorization header of an ntfy token, Basic for user and password, Bearer otherwise, none when unset
func buildAuthHeader(token any) ([][2]string, error) {
	if !pyrepr.Truthy(token) {
		return nil, nil
	}
	text, ok := token.(string)
	if !ok {
		return nil, attributeError(token, "strip")
	}
	text = pystr.Strip(text)
	if text == "" {
		return nil, nil
	}
	if strings.Contains(text, ":") {
		return [][2]string{{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(text))}}, nil
	}
	return [][2]string{{"Authorization", "Bearer " + text}}, nil
}

// Publish headers in dict order, auth before or after the plain text and echo tag pair, then the markdown flag
func publishHeaders(token any, markdown, authFirst bool) ([][2]string, error) {
	auth, err := buildAuthHeader(token)
	if err != nil {
		return nil, err
	}
	base := [][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"X-Tags", echoTag}}
	var headers [][2]string
	if authFirst {
		headers = append(append(headers, auth...), base...)
	} else {
		headers = append(append(headers, base...), auth...)
	}
	if markdown {
		headers = append(headers, [2]string{"X-Markdown", "true"})
	}
	return headers, nil
}

// Body cut to the ntfy character limit as UTF-8, a warning tagged with the context when it cut
func truncateBody(message, context string) []byte {
	if n := utf8.RuneCountInString(message); n > maxMessageLength {
		slog.Warn(fmt.Sprintf("%s: truncating message from %d to %d chars (ntfy limit)", context, n, maxMessageLength))
	}
	return []byte(pystr.Slice(message, 0, maxMessageLength))
}

// Message id ntfy returned, a random twelve hex id when the body carries none or does not parse
func responseMessageID(resp *pyhttpx.Response) any {
	parsed, err := resp.JSON()
	if err != nil {
		return shortUUID()
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return shortUUID()
	}
	if id, _ := obj.Get("id"); pyrepr.Truthy(id) {
		return id
	}
	return shortUUID()
}

// Extra mapping of a platform config, empty when the config or its extra is unset
func configExtra(config *gateway.PlatformConfig) map[string]any {
	if config == nil || config.Extra == nil {
		return map[string]any{}
	}
	return config.Extra
}

// Server base URL from env then extra then the public default, trailing slashes removed
func serverURL(ctx context.Context, extra map[string]any) (string, error) {
	value, err := gateway.ExtraOrSecret(ctx, extra, "server", "NTFY_SERVER_URL", defaultServer, true)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", attributeError(value, "rstrip")
	}
	return pystr.RstripChars(text, "/"), nil
}

// Whether the scoped env names a topic
func checkRequirements(ctx context.Context) bool {
	topic, err := gateway.GetScopedSecret(ctx, "NTFY_TOPIC", "", false)
	return err == nil && pystr.Strip(topic) != ""
}

// Whether the env or the config extra names a topic
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	topic, err := gateway.ExtraOrSecret(ctx, configExtra(config), "topic", "NTFY_TOPIC", "", true)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(topic), nil
}

// Whether ntfy is configured through the env or the config extra
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	topic, err := gateway.GetScopedSecret(ctx, "NTFY_TOPIC", "", false)
	if err != nil {
		return false, err
	}
	if topic != "" {
		return true, nil
	}
	return pyrepr.Truthy(configExtra(config)["topic"]), nil
}

// ntfy adapter, a streaming JSON subscription in and one POST per reply out
type NtfyAdapter struct {
	*gateway.BasePlatformAdapter

	server       string
	topic        any
	publishTopic any
	token        any

	mu         sync.Mutex
	streamTask *gateway.AdapterTask
	httpClient *http.Client
	dedup      *gateway.MessageDeduplicator
}

// Class attributes the ntfy adapter declares over the base defaults
func ntfyAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	traits.SerialReceiveLoop = true
	return traits
}

// Adapter for a platform config reading server, topics and token from env then extra
func NewNtfyAdapter(ctx context.Context, config *gateway.PlatformConfig) (*NtfyAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &NtfyAdapter{dedup: gateway.NewMessageDeduplicator(dedupMaxSize, dedupWindowSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("ntfy"), a)
	a.Traits = ntfyAdapterTraits()
	extra := configExtra(config)
	var err error
	if a.server, err = serverURL(ctx, extra); err != nil {
		return nil, err
	}
	if a.topic, err = gateway.ExtraOrSecret(ctx, extra, "topic", "NTFY_TOPIC", "", true); err != nil {
		return nil, err
	}
	if a.publishTopic, err = gateway.ExtraOrSecret(ctx, extra, "publish_topic", "NTFY_PUBLISH_TOPIC", "", true); err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(a.publishTopic) {
		a.publishTopic = a.topic
	}
	if a.token, err = gateway.ExtraOrSecret(ctx, extra, "token", "NTFY_TOKEN", "", true); err != nil {
		return nil, err
	}
	return a, nil
}

// HTTP client like a default httpx client, proxy env honoured and redirects returned unfollowed
func newHTTPClient() *http.Client {
	return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), CheckRedirect: pyhttpx.RedirectPolicy(false)}
}

// Starts the streaming subscription task and marks the adapter connected, false without a topic
func (a *NtfyAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if !pyrepr.Truthy(a.topic) {
		slog.Warn(fmt.Sprintf("[%s] NTFY_TOPIC not configured", a.Name()))
		return false, nil
	}
	a.mu.Lock()
	a.httpClient = newHTTPClient()
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	task := gateway.SpawnAdapterTask(ctx, a.runStream)
	a.mu.Lock()
	a.streamTask = task
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Connected — subscribing to %s/%s", a.Name(), a.server, pyrepr.Str(a.topic)))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Subscribes to the topic and reopens the stream with backoff until the adapter stops or a fatal status ends it
func (a *NtfyAdapter) runStream(ctx context.Context) error {
	backoffIdx := 0
	streamURL := a.server + "/" + pyrepr.Str(a.topic) + "/json"
	headers, err := a.authHeaders()
	if err != nil {
		return err
	}
	for a.IsConnected() {
		slog.Debug(fmt.Sprintf("[%s] Opening stream to %s", a.Name(), streamURL))
		streamStart := time.Now()
		err := a.consumeStream(ctx, streamURL, headers)
		if ctx.Err() != nil {
			return nil
		}
		var fatal *fatalStreamError
		if errors.As(err, &fatal) {
			a.SetRunning(false)
			return nil
		}
		if err != nil {
			if !a.IsConnected() {
				return nil
			}
			slog.Warn(fmt.Sprintf("[%s] Stream error: %s", a.Name(), streamErrorText(err)))
		}
		if !a.IsConnected() {
			return nil
		}
		if time.Since(streamStart).Seconds() >= 60.0 {
			backoffIdx = 0
		}
		delay := reconnectBackoff[min(backoffIdx, len(reconnectBackoff)-1)]
		slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds...", a.Name(), delay))
		timer := time.NewTimer(time.Duration(delay) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		backoffIdx++
	}
	return nil
}

// str of an exception the stream raised, the httpx text for a transport failure
func streamErrorText(err error) string {
	var pe *gateway.PyError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.text
	}
	return pyhttpx.ErrorText(err)
}

// HTTPStatusError of a stream response outside the 2xx range
type statusError struct{ text string }

func (e *statusError) Error() string { return e.text }

// Logs a 401 or 404, records the fatal state and returns the stream ending error, nil for any other status
func (a *NtfyAdapter) fatalStatus(ctx context.Context, statusCode int) error {
	var code, detail, reason string
	switch statusCode {
	case 401:
		slog.Error(fmt.Sprintf("[%s] Authentication failed (401) — stopping reconnect loop. Check NTFY_TOKEN.", a.Name()))
		code, detail, reason = "ntfy_unauthorized", "ntfy server rejected auth (401). Check NTFY_TOKEN.", "401 Unauthorized"
	case 404:
		slog.Error(fmt.Sprintf("[%s] Topic not found (404): %s — stopping reconnect loop.", a.Name(), pyrepr.Str(a.topic)))
		code, detail = "ntfy_topic_not_found", fmt.Sprintf("ntfy topic '%s' returned 404. Check NTFY_TOPIC.", pyrepr.Str(a.topic))
		reason = "404 Not Found"
	default:
		return nil
	}
	a.SetFatalError(ctx, code, detail, false)
	return &fatalStreamError{reason: reason}
}

// Incremental UTF-8 decoder with replacement that holds an unfinished trailing sequence for the next chunk
type textDecoder struct{ pending []byte }

func (d *textDecoder) decode(chunk []byte, final bool) string {
	data := append(d.pending, chunk...)
	d.pending = nil
	if !final {
		for i := 1; i <= 3 && i <= len(data); i++ {
			if b := data[len(data)-i]; b&0xC0 != 0x80 {
				if b >= 0xC0 && !utf8.FullRune(data[len(data)-i:]) {
					d.pending = append([]byte{}, data[len(data)-i:]...)
					data = data[:len(data)-i]
				}
				break
			}
		}
	}
	return pystr.DecodeUTF8Replace(data)
}

// Line splitter of httpx aiter_lines, a trailing carriage return carried into the next chunk
type lineDecoder struct {
	buffer     []string
	trailingCR bool
}

// Line boundary characters of str.splitlines
const newlineChars = "\n\r\x0b\x0c\x1c\x1d\x1e\u0085  "

func (d *lineDecoder) decode(text string) []string {
	if d.trailingCR {
		text = "\r" + text
		d.trailingCR = false
	}
	if strings.HasSuffix(text, "\r") {
		d.trailingCR = true
		text = text[:len(text)-1]
	}
	if text == "" {
		return nil
	}
	last, _ := utf8.DecodeLastRuneInString(text)
	trailingNewline := strings.ContainsRune(newlineChars, last)
	lines := pystr.Splitlines(text, false)
	if len(lines) == 1 && !trailingNewline {
		d.buffer = append(d.buffer, lines[0])
		return nil
	}
	if len(d.buffer) > 0 {
		lines[0] = strings.Join(d.buffer, "") + lines[0]
		d.buffer = nil
	}
	if !trailingNewline {
		d.buffer = []string{lines[len(lines)-1]}
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (d *lineDecoder) flush() []string {
	if len(d.buffer) == 0 && !d.trailingCR {
		return nil
	}
	lines := []string{strings.Join(d.buffer, "")}
	d.buffer, d.trailingCR = nil, false
	return lines
}

// Body bytes transcoded to UTF-8 when the response declares another known charset
func utf8Body(header http.Header, body io.Reader) io.Reader {
	charset := ""
	if ct := header.Get("Content-Type"); ct != "" {
		if _, params, err := mime.ParseMediaType(ct); err == nil {
			charset = strings.ToLower(strings.Trim(params["charset"], `"' `))
		}
	}
	switch charset {
	case "", "utf-8", "utf8", "u8", "utf":
		return body
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return body
	}
	return transform.NewReader(body, enc.NewDecoder())
}

// Opens the streaming subscription and dispatches every message event until the stream or the adapter ends
func (a *NtfyAdapter) consumeStream(ctx context.Context, streamURL string, headers [][2]string) error {
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return attributeError(nil, "stream")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pyhttpx.MergeParams(streamURL, pyhttpx.Params{{"poll", "false"}}), nil)
	if err != nil {
		return err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	response, err := pyhttpx.StreamTimeout(client, req, pyhttpx.Timeout{Connect: 15 * time.Second,
		Read: streamTimeoutSeconds * time.Second, Write: 15 * time.Second, Pool: 15 * time.Second})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := a.fatalStatus(ctx, response.StatusCode); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &statusError{text: response.StatusErrorText(response.URL)}
	}
	body := utf8Body(response.Header, response.Body)
	var text textDecoder
	var lines lineDecoder
	handle := func(batch []string) (stop bool, err error) {
		for _, line := range batch {
			if !a.IsConnected() {
				return true, nil
			}
			line = pystr.Strip(line)
			if line == "" {
				continue
			}
			parsed, perr := pyjson.Loads(line)
			if perr != nil {
				var decodeErr *pyjson.DecodeError
				if errors.As(perr, &decodeErr) {
					continue
				}
				return false, perr
			}
			event, ok := parsed.(*pyjson.Object)
			if !ok {
				return false, attributeError(parsed, "get")
			}
			if kind, _ := event.Get("event"); pyrepr.Equal(kind, "message") {
				if err := a.onMessage(ctx, event); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	}
	chunk := make([]byte, 65536)
	for {
		n, rerr := body.Read(chunk)
		if n > 0 {
			if decoded := text.decode(chunk[:n], false); decoded != "" {
				if stop, err := handle(lines.decode(decoded)); stop || err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if decoded := text.decode(nil, true); decoded != "" {
		if stop, err := handle(lines.decode(decoded)); stop || err != nil {
			return err
		}
	}
	_, err = handle(lines.flush())
	return err
}

// Stops the stream task, closes the HTTP client and forgets every seen message id
func (a *NtfyAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	task := a.streamTask
	a.mu.Unlock()
	if task != nil {
		task.Cancel()
		if err := task.Wait(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		a.mu.Lock()
		a.streamTask = nil
		a.mu.Unlock()
	}
	a.mu.Lock()
	client := a.httpClient
	a.httpClient = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
	a.dedup.Clear()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Whether the echo tag is a member of the event's tags value
func hasEchoTag(tags any) (bool, error) {
	if !pyrepr.Truthy(tags) {
		return false, nil
	}
	switch t := tags.(type) {
	case []any:
		for _, item := range t {
			if pyrepr.Equal(item, echoTag) {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(t, echoTag), nil
	case *pyjson.Object:
		_, ok := t.Get(echoTag)
		return ok, nil
	}
	return false, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(tags))}
}

// Turns one ntfy message event into a text message from the topic, duplicates, own echoes and blank bodies dropped
func (a *NtfyAdapter) onMessage(ctx context.Context, event *pyjson.Object) error {
	var msgID string
	if id, _ := event.Get("id"); pyrepr.Truthy(id) {
		msgID = pyrepr.Str(id)
	} else {
		msgID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate message %s, skipping", a.Name(), msgID))
		return nil
	}
	tags, _ := event.Get("tags")
	echo, err := hasEchoTag(tags)
	if err != nil {
		return err
	}
	if echo {
		slog.Debug(fmt.Sprintf("[%s] Skipping own message (echo tag)", a.Name()))
		return nil
	}
	text := ""
	if message, _ := event.Get("message"); pyrepr.Truthy(message) {
		s, ok := message.(string)
		if !ok {
			return attributeError(message, "strip")
		}
		text = pystr.Strip(s)
	}
	if text == "" {
		slog.Debug(fmt.Sprintf("[%s] Empty message body, skipping", a.Name()))
		return nil
	}
	topicValue, _ := event.Get("topic")
	if !pyrepr.Truthy(topicValue) {
		topicValue = a.topic
	}
	topic := pyrepr.Str(topicValue)
	source := a.BuildSource(ctx, topic, gateway.SourceFields{ChatName: &topic, ChatType: "dm", UserID: &topic, UserName: &topic,
		MessageID: &msgID})
	timestamp := time.Now().UTC()
	if unixTS, _ := event.Get("time"); pyrepr.Truthy(unixTS) {
		seconds, err := pyint.Int64(unixTS)
		pe, isPy := err.(*pyint.Error)
		switch {
		case isPy && (pe.Kind == "TypeError" || pe.Kind == "ValueError"):
		case err != nil:
			return err
		default:
			if at := time.Unix(seconds, 0).UTC(); at.Year() >= 1 && at.Year() <= 9999 {
				timestamp = at
			}
		}
	}
	messageEvent := gateway.NewMessageEvent(text)
	messageEvent.MessageType, messageEvent.Source = gateway.MessageTypeText, source
	messageEvent.MessageID = &msgID
	messageEvent.RawMessage = event
	messageEvent.Timestamp = timestamp
	slog.Debug(fmt.Sprintf("[%s] Message on topic %s: %s", a.Name(), topic, pystr.Slice(text, 0, 80)))
	return a.HandleMessage(ctx, messageEvent)
}

// Publishes the content to the metadata topic, the configured publish topic or the chat id
func (a *NtfyAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	var publishTopic any = metadata["publish_topic"]
	if !pyrepr.Truthy(publishTopic) {
		publishTopic = a.publishTopic
	}
	if !pyrepr.Truthy(publishTopic) {
		publishTopic = chatID
	}
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "HTTP client not initialized"}, nil
	}
	headers, err := publishHeaders(a.token, pyrepr.Truthy(configExtra(a.Config)["markdown"]), true)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if n := utf8.RuneCountInString(content); n > maxMessageLength {
		slog.Warn(fmt.Sprintf("[%s] Message truncated from %d to %d chars (ntfy limit)", a.Name(), n, maxMessageLength))
	}
	body := []byte(pystr.Slice(content, 0, maxMessageLength))
	resp, err := publish(ctx, client, a.server+"/"+pyrepr.Str(publishTopic), body, headers)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		var timeout *pyhttpx.TimeoutError
		if errors.As(err, &timeout) {
			return gateway.SendResult{Success: false, Error: "Timeout publishing to ntfy"}, nil
		}
		text := pyhttpx.ErrorText(err)
		slog.Error(fmt.Sprintf("[%s] Send error: %s", a.Name(), text))
		return gateway.SendResult{Success: false, Error: text}, nil
	}
	if resp.StatusCode < 300 {
		return gateway.SendResult{Success: true, MessageID: responseMessageID(resp)}, nil
	}
	bodyText := pystr.Slice(resp.Text(), 0, 200)
	slog.Warn(fmt.Sprintf("[%s] Send failed HTTP %d: %s", a.Name(), resp.StatusCode, bodyText))
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bodyText)}, nil
}

// One publish POST with the fifteen second httpx timeout
func publish(ctx context.Context, client *http.Client, target string, body []byte, headers [][2]string) (*pyhttpx.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	return pyhttpx.Do(client, req, 15*time.Second)
}

// Chat info of a topic chat
func (a *NtfyAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Authorization header of the configured token
func (a *NtfyAdapter) authHeaders() ([][2]string, error) { return buildAuthHeader(a.token) }
