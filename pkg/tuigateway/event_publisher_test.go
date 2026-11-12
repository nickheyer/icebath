package tuigateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Server pings are answered, frames arrive in order, and a server close latches the publisher dead on its next write
func TestPublisherAnswersPingsAndLatchesDeadOnServerClose(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	got := make(chan string, 4)
	write := func(pub *WsPublisherTransport, n int64) bool {
		obj := pyjson.NewObject()
		obj.Set("n", n)
		return pub.Write(obj)
	}
	pong := make(chan string, 1)
	closeNow := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPongHandler(func(data string) error { pong <- data; return nil })
		if err := conn.WriteControl(websocket.PingMessage, []byte("abcd"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("ping %v", err)
			return
		}
		for range 3 {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read %v", err)
				return
			}
			got <- string(data)
		}
		<-closeNow
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "bye"), time.Now().Add(time.Second))
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	pub := NewWsPublisherTransport("ws"+strings.TrimPrefix(srv.URL, "http"), 2*time.Second)
	defer pub.Close()
	for _, n := range []int64{1, 2, 3} {
		if !write(pub, n) {
			t.Fatalf("write %d refused", n)
		}
		if frame := <-got; frame != fmt.Sprintf(`{"n": %d}`, n) {
			t.Fatalf("frame %q", frame)
		}
	}
	select {
	case data := <-pong:
		if data != "abcd" {
			t.Fatalf("pong %q", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server ping never answered")
	}
	close(closeNow)
	select {
	case <-pub.ws.readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server close never reached the reader")
	}
	if err := pub.ws.send("x"); err == nil || err.Error() != "received 1001 (going away) bye; then sent 1001 (going away) bye" {
		t.Fatalf("send after close %v", err)
	}
	write(pub, 4)
	deadline := time.Now().Add(5 * time.Second)
	for !pub.dead.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !pub.dead.Load() || write(pub, 5) {
		t.Fatal("publisher not latched dead after the server closed")
	}
}
