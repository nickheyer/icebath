package tuigateway

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lone high surrogate U+D83D as a str holds it
const loneSurrogate = "\xed\xa0\xbd"

func TestSanitizeWsTextMakesUtf8Encodable(t *testing.T) {
	dirty := "gateway.ready " + loneSurrogate + " payload"
	out := sanitizeWSText(dirty)
	if !utf8.ValidString(out) || pystr.StrictUTF8EncodeError(out) != nil {
		t.Fatalf("not encodable: %q", out)
	}
	if strings.Contains(out, loneSurrogate) {
		t.Fatalf("surrogate kept: %q", out)
	}
}

func TestSanitizeWsTextLeavesValidTextUnchanged(t *testing.T) {
	clean := `{"type":"gateway.ready","ok":true}`
	if sanitizeWSText(clean) != clean {
		t.Fatalf("changed: %q", sanitizeWSText(clean))
	}
}

// Socket that encodes each line strictly and fails the encode of any line holding raiseOn
type encodingWS struct {
	mu      sync.Mutex
	sent    []string
	raiseOn string
}

func (w *encodingWS) SendText(ctx context.Context, line string) error {
	if err := pystr.StrictUTF8EncodeError(line); err != nil {
		return err
	}
	if w.raiseOn != "" && strings.Contains(line, w.raiseOn) {
		return pystr.StrictUTF8EncodeError(loneSurrogate)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, line)
	return nil
}

func (w *encodingWS) Close(code int) error { return nil }

func TestSafeSendSanitizesSurrogateAndKeepsConnection(t *testing.T) {
	ws := &encodingWS{}
	transport := NewWSTransport(ws, "127.0.0.1:1", nil)
	dirty := `{"type":"gateway.ready","x":"` + loneSurrogate + `"}`
	transport.safeSendMany(transport.sendLock.reserve(), []string{"first", dirty, "third"}, nil, 0)
	if transport.Closed() {
		t.Fatal("transport closed")
	}
	if len(ws.sent) != 3 || ws.sent[0] != "first" || ws.sent[2] != "third" {
		t.Fatalf("sent = %q", ws.sent)
	}
	if strings.Contains(strings.Join(ws.sent, ""), loneSurrogate) {
		t.Fatalf("surrogate reached the socket: %q", ws.sent)
	}
}

func TestUnicodeEncodeErrorDoesNotCloseSocket(t *testing.T) {
	ws := &encodingWS{raiseOn: "BOOM"}
	transport := NewWSTransport(ws, "127.0.0.1:1", nil)
	transport.safeSendMany(transport.sendLock.reserve(), []string{"ok-a", "BOOM-frame", "ok-b"}, nil, 0)
	if transport.Closed() {
		t.Fatal("transport closed")
	}
	if !slices.Equal(ws.sent, []string{"ok-a", "ok-b"}) {
		t.Fatalf("sent = %q", ws.sent)
	}
}
