package messagetools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Sent Telegram message as the Bot API result carries it
type telegramMessage struct {
	MessageID any
}

// Bot API surface the standalone sender drives, a method name and its python telegram bot keywords
type telegramBotAPI interface {
	Send(ctx context.Context, method string, kwargs map[string]any) (*telegramMessage, error)
}

// Telegram Bot API failure carrying the python telegram bot class, message and flood wait
type telegramError struct {
	Kind       string
	Message    string
	RetryAfter *float64
}

func (e *telegramError) Error() string { return e.Message }

// Error with the leading API error prefixes removed and capitalized like TelegramError
func newTelegramError(kind, message string) *telegramError {
	msg := message
	for _, prefix := range []string{"Error: ", "[Error]: ", "Bad Request: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if msg != message {
		msg = pystr.Capitalize(msg)
	}
	return &telegramError{Kind: kind, Message: msg}
}

var (
	telegramAPIBase        = "https://api.telegram.org/bot"
	telegramConnectTimeout = 5 * time.Second
	telegramReadTimeout    = 5 * time.Second
	telegramWriteTimeout   = 5 * time.Second
	telegramMediaTimeout   = 20 * time.Second
)

// Constructor of the Bot API client, replaced by tests the way hermes patches telegram.Bot
var newTelegramBot = func(token any, proxy string) (telegramBotAPI, error) {
	if !pyrepr.Truthy(token) {
		return nil, newTelegramError("InvalidToken", "You must pass the token you received from https://t.me/Botfather!")
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: telegramConnectTimeout}).DialContext,
		TLSHandshakeTimeout:   telegramConnectTimeout,
		ResponseHeaderTimeout: telegramReadTimeout,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("Unknown scheme for proxy URL %s", pyrepr.Repr(proxy))
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &telegramHTTPBot{token: pyrepr.Str(token), client: &http.Client{Transport: transport}}, nil
}

type telegramHTTPBot struct {
	token  string
	client *http.Client
}

// Bot API method name of a python telegram bot coroutine name
func telegramAPIMethod(method string) string {
	parts := strings.Split(method, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// Posts one Bot API call, uploading open files and local thumbnail paths as multipart attachments
func (b *telegramHTTPBot) Send(ctx context.Context, method string, kwargs map[string]any) (*telegramMessage, error) {
	fields := map[string]string{}
	type upload struct{ field, name, path string }
	var uploads []upload
	keys := make([]string, 0, len(kwargs))
	for k := range kwargs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := kwargs[k]
		if v == nil {
			continue
		}
		switch x := v.(type) {
		case *os.File:
			uploads = append(uploads, upload{field: k, name: filepath.Base(x.Name()), path: x.Name()})
		case string:
			if k == "thumbnail" {
				if st, err := os.Stat(x); err == nil && st.Mode().IsRegular() {
					uploads = append(uploads, upload{field: "attach_thumbnail", name: filepath.Base(x), path: x})
					fields[k] = "attach://attach_thumbnail"
					continue
				}
			}
			fields[k] = x
		case bool:
			if k == "disable_web_page_preview" {
				if x {
					fields["link_preview_options"] = `{"is_disabled":true}`
				}
				continue
			}
			fields[k] = map[bool]string{true: "true", false: "false"}[x]
		default:
			fields[k] = pyrepr.Str(x)
		}
	}
	var body bytes.Buffer
	contentType := "application/x-www-form-urlencoded"
	timeout := telegramWriteTimeout
	if len(uploads) > 0 {
		timeout = telegramMediaTimeout
		mw := multipart.NewWriter(&body)
		for _, k := range sortedKeys(fields) {
			_ = mw.WriteField(k, fields[k])
		}
		for _, u := range uploads {
			data, err := readUpload(kwargs, u.field, u.path)
			if err != nil {
				return nil, err
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, u.field, u.name))
			mt, _, _ := pymimetypes.GuessType(u.name, true)
			if mt == "" {
				mt = "application/octet-stream"
			}
			h.Set("Content-Type", mt)
			part, _ := mw.CreatePart(h)
			_, _ = part.Write(data)
		}
		_ = mw.Close()
		contentType = mw.FormDataContentType()
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body.WriteString(form.Encode())
	}
	rctx, cancel := context.WithTimeout(ctx, telegramConnectTimeout+timeout+telegramReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, telegramAPIBase+b.token+"/"+telegramAPIMethod(method), &body)
	if err != nil {
		return nil, newTelegramError("NetworkError", "httpx.InvalidURL: "+err.Error())
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, newTelegramError("TimedOut", "Timed out")
		}
		return nil, newTelegramError("NetworkError", "httpx.ConnectError: "+err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, newTelegramError("NetworkError", "httpx.ReadError: "+err.Error())
	}
	return parseTelegramResponse(resp.StatusCode, payload)
}

// Bytes of an upload, the open file rewound by the caller or the thumbnail path
func readUpload(kwargs map[string]any, field, path string) ([]byte, error) {
	if f, ok := kwargs[field].(*os.File); ok {
		return io.ReadAll(f)
	}
	return os.ReadFile(path)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Sent message of a 2xx reply, else the python telegram bot exception the status and parameters select
func parseTelegramResponse(code int, payload []byte) (*telegramMessage, error) {
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, newTelegramError("TelegramError", "Invalid server response")
	}
	if code >= 200 && code <= 299 {
		result, _ := data["result"].(map[string]any)
		return &telegramMessage{MessageID: jsonNumberValue(result["message_id"])}, nil
	}
	message := "Unknown HTTPError"
	description, _ := data["description"].(string)
	if description != "" {
		message = description
	}
	if params, ok := data["parameters"].(map[string]any); ok && len(params) > 0 {
		if migrate := jsonNumberValue(params["migrate_to_chat_id"]); pyrepr.Truthy(migrate) {
			return nil, &telegramError{Kind: "ChatMigrated", Message: fmt.Sprintf("Group migrated to supergroup. New chat id: %s", pyrepr.Str(migrate))}
		}
		if retry := jsonNumberValue(params["retry_after"]); pyrepr.Truthy(retry) {
			secs, _ := pyrepr.Int(retry)
			after, _ := new(big.Float).SetInt(secs).Float64()
			return nil, &telegramError{Kind: "RetryAfter", Message: fmt.Sprintf("Flood control exceeded. Retry in %s seconds", secs), RetryAfter: &after}
		}
		message += fmt.Sprintf("\nThe server response contained unknown parameters: %s", pyrepr.Repr(params))
	}
	switch code {
	case http.StatusForbidden:
		return nil, newTelegramError("Forbidden", message)
	case http.StatusNotFound, http.StatusUnauthorized:
		return nil, newTelegramError("InvalidToken", message)
	case http.StatusBadRequest:
		return nil, newTelegramError("BadRequest", message)
	case http.StatusConflict:
		return nil, newTelegramError("Conflict", message)
	case http.StatusBadGateway:
		if description == "" {
			description = "Bad Gateway"
		}
		return nil, newTelegramError("NetworkError", description)
	}
	return nil, newTelegramError("NetworkError", fmt.Sprintf("%s (%d)", message, code))
}

// Python int of a decoded JSON number, the value unchanged otherwise
func jsonNumberValue(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		return n.String()
	}
	return v
}
