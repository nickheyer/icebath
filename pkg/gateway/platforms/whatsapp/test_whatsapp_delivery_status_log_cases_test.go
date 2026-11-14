package whatsapp

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Handler keeping every record message
type messageCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *messageCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *messageCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, r.Message)
	c.mu.Unlock()
	return nil
}
func (c *messageCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *messageCapture) WithGroup(string) slog.Handler      { return c }

func TestDeliveryStatusIsLoggedToGatewayLogAndNeverDispatched(t *testing.T) {
	hermeticHome(t)
	noSleep(t)
	a, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Extra: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	batch := `[{"event":"delivery_status","messageId":"3EB0A1","chatId":"15551234567@s.whatsapp.net","status":"read","timestamp":1760000000},
{"event":"delivery_status","messageId":"3EB0A2","chatId":"120363001234567890@g.us","status":"delivered","recipientId":"15550001111@s.whatsapp.net","timestamp":1760000050},
{"event":"delivery_status","messageId":"3EB0A3","chatId":"15551234567@s.whatsapp.net","status":"error","error":"463","timestamp":1760000060}]`
	var paths []string
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		a.MarkDisconnected(context.Background())
		w.Write([]byte(batch))
	})
	dispatched := false
	a.SetMessageHandler(func(ctx context.Context, event *gateway.MessageEvent) (any, error) {
		dispatched = true
		return nil, nil
	})
	capture := &messageCapture{}
	saved := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(saved) })
	a.MarkConnected(context.Background(), "")
	a.httpSession = &http.Client{}
	a.pollMessages(context.Background())
	if !slices.Equal(paths, []string{"GET /messages"}) {
		t.Fatalf("paths = %v", paths)
	}
	tag := "[" + a.Name() + "] WhatsApp delivery status:"
	var lines []string
	for _, m := range capture.msgs {
		if strings.Contains(m, "delivery status") {
			lines = append(lines, m)
		}
	}
	want := []string{
		tag + " message=3EB0A1 chat=15551234567@s.whatsapp.net status=read at=1760000000",
		tag + " message=3EB0A2 chat=120363001234567890@g.us status=delivered recipient=15550001111@s.whatsapp.net at=1760000050",
		tag + " message=3EB0A3 chat=15551234567@s.whatsapp.net status=error at=1760000060 error=463",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q", lines)
	}
	if !slices.ContainsFunc(logging.ComponentPrefixes["gateway"], func(p string) bool { return strings.HasPrefix("plugins.platforms.whatsapp.adapter", p) }) {
		t.Fatal("the adapter logger is outside the gateway component")
	}
	if dispatched {
		t.Fatal("a delivery status was dispatched as a message")
	}
}
