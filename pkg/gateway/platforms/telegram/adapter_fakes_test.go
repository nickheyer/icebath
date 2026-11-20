package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// One Bot API request the loopback fake received, its method and flattened parameters
type botCall struct {
	method string
	params map[string]string
}

// Loopback Bot API recording every call and answering through an optional per test responder
type scriptedBot struct {
	mu      sync.Mutex
	calls   []botCall
	respond func(method string, params map[string]string) (int, string)
}

// Default Bot API result of a method
func defaultBotResult(method string, params map[string]string) string {
	switch method {
	case "getMe":
		return `{"ok":true,"result":{"id":123456,"is_bot":true,"first_name":"Test","username":"test_bot"}}`
	case "sendMessage", "sendPhoto", "sendVoice", "sendAudio", "sendDocument", "sendVideo", "sendAnimation", "editMessageText":
		return `{"ok":true,"result":{"message_id":42,"date":0,"chat":{"id":123,"type":"private"}}}`
	case "createForumTopic":
		return `{"ok":true,"result":{"message_thread_id":777,"name":"t","icon_color":0}}`
	case "getChat":
		return `{"ok":true,"result":{"id":123,"type":"private","first_name":"Ana"}}`
	}
	return `{"ok":true,"result":true}`
}

func (s *scriptedBot) handler(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]string{}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case mediaType == "multipart/form-data":
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				params[k] = v[0]
			}
			for k := range r.MultipartForm.File {
				params[k] = "<file>"
			}
		}
	case mediaType == "application/json":
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		for k, v := range decoded {
			if s, ok := v.(string); ok {
				params[k] = s
				continue
			}
			b, _ := json.Marshal(v)
			params[k] = string(b)
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, botCall{method: method, params: params})
	respond := s.respond
	s.mu.Unlock()
	status, body := 200, ""
	if respond != nil {
		status, body = respond(method, params)
	}
	if body == "" {
		status, body = 200, defaultBotResult(method, params)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Calls of one method in arrival order
func (s *scriptedBot) callsOf(method string) []botCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []botCall
	for _, c := range s.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// Bot API error body of a status and description
func botError(status int, description string) (int, string) {
	return status, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, status, description)
}

// Adapter over a hermetic home with the given extra, not yet attached to a Bot API
func newTestAdapter(t *testing.T, extra map[string]any) *TelegramAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Token = true, "123456:test-token"
	for k, v := range extra {
		cfg.Extra[k] = v
	}
	return NewTelegramAdapter(context.Background(), cfg)
}

// Attaches a live application talking to the loopback fake, the connected adapter state
func attachScriptedBot(t *testing.T, a *TelegramAdapter, fake *scriptedBot) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	settings := requestSettings{connectTimeout: 5, readTimeout: 5, writeTimeout: 5, poolTimeout: 5, mediaWriteTimeout: 5}
	general, err := newPTBRequest(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	polling, err := newPTBRequest(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newPTBApp(tokenText(a.Config.Token), srv.URL, general, polling, func(context.Context, *models.Update) {})
	if err != nil {
		t.Fatal(err)
	}
	tgApp := NewTelegramApplication(app.sdk.ID(), BuildUpdateProcessor(a.Config.Extra, a.Name()), a, a.admission)
	a.installApp(app, tgApp)
}

// Adapter attached to a fresh loopback fake
func newScriptedAdapter(t *testing.T, extra map[string]any) (*TelegramAdapter, *scriptedBot) {
	t.Helper()
	a := newTestAdapter(t, extra)
	fake := &scriptedBot{}
	attachScriptedBot(t, a, fake)
	return a, fake
}

// Event of a private chat message with the given ids
func privateEvent(chatID, messageID string) *gateway.MessageEvent {
	event := gateway.NewMessageEvent("hello")
	user, name := "42", "TestUser"
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: chatID, ChatType: "private", UserID: &user, UserName: &name}
	if messageID != "" {
		event.MessageID = &messageID
	}
	return event
}
