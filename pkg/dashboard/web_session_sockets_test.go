package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestASocketAdmittedWhileTheTokenIsReplacedClosesAtOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	previous := SessionToken()
	t.Cleanup(func() { setSessionToken(previous) })
	type closeFrame struct {
		code   int
		reason string
	}
	ended := make(chan closeFrame, 1)
	send := func(ctx context.Context, m Message) error {
		if m.Type == "websocket.close" {
			ended <- closeFrame{m.Code, m.Reason}
		}
		return nil
	}
	receive := func(ctx context.Context) (Message, error) { return Message{Type: "websocket.connect"}, nil }
	socket := NewWebSocket(&Scope{Type: "websocket"}, receive, send)
	if err := socket.Accept(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	setSessionToken(strings.Repeat("b", 43))

	trackBrowserSocket(context.Background(), socket, BrowserCredentialGenerationOf(strings.Repeat("a", 43)))

	select {
	case got := <-ended:
		if got != (closeFrame{4401, "Signed out"}) {
			t.Fatalf("closed %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("socket admitted under the replaced token stayed open")
	}
}
