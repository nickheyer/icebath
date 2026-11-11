package dashboard

import (
	"bufio"
	"context"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Server on an ephemeral loopback port serving the app, shut down with the test
func startTestServer(t *testing.T, app *FastAPI) *UvicornServer {
	t.Helper()
	srv := NewUvicornServer(&UvicornConfig{App: app.ASGI, Host: "127.0.0.1", Port: 0, TimeoutGracefulShutdown: 3 * time.Second})
	if err := srv.Startup(context.Background()); err != nil || !srv.Started {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv
}

var dateLine = regexp.MustCompile(`date: [A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT\r\n`)

func TestServerWritesUvicornResponseBytesAndKeepsAlive(t *testing.T) {
	srv := startTestServer(t, layerApp())
	c, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	readResponse := func() string {
		var head strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			head.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		n := 0
		for _, l := range strings.Split(head.String(), "\r\n") {
			if v, ok := strings.CutPrefix(l, "content-length: "); ok {
				for _, ch := range v {
					n = n*10 + int(ch-'0')
				}
			}
		}
		body := make([]byte, n)
		io.ReadFull(br, body)
		return dateLine.ReplaceAllString(head.String(), "date: X\r\n") + string(body)
	}
	c.Write([]byte("GET /items HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if got := readResponse(); got != "HTTP/1.1 200 OK\r\ndate: X\r\nserver: uvicorn\r\ncontent-length: 11\r\ncontent-type: application/json\r\n\r\n{\"ok\":true}" {
		t.Fatalf("first response %q", got)
	}
	c.Write([]byte("GET /missing HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if got := readResponse(); got != "HTTP/1.1 404 Not Found\r\ndate: X\r\nserver: uvicorn\r\ncontent-length: 22\r\ncontent-type: application/json\r\n\r\n{\"detail\":\"Not Found\"}" {
		t.Fatalf("second response %q", got)
	}
	c.Write([]byte("GARBAGE\r\n\r\n"))
	if got := readResponse(); !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") || !strings.HasSuffix(got, "Invalid HTTP request received.") {
		t.Fatalf("invalid response %q", got)
	}
}

func TestServerWebSocketEchoAndRefusal(t *testing.T) {
	app := NewFastAPI("t", "1", nil, nil)
	app.AddAPIWebSocketRoute("/echo", &WSEndpoint{Name: "echo", Handle: func(ctx context.Context, c *Call) error {
		if err := c.WebSocket.Accept(ctx, nil, nil); err != nil {
			return err
		}
		text, err := c.WebSocket.ReceiveText(ctx)
		if err != nil {
			return err
		}
		if err := c.WebSocket.SendText(ctx, "echo:"+text); err != nil {
			return err
		}
		return c.WebSocket.Close(ctx, 4001, "bye")
	}})
	app.AddAPIWebSocketRoute("/refuse", &WSEndpoint{Name: "refuse", Handle: func(ctx context.Context, c *Call) error {
		return c.WebSocket.Close(ctx, 1008, "")
	}})
	srv := startTestServer(t, app)
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr().String()+"/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.WriteMessage(websocket.TextMessage, []byte("hi"))
	_, msg, err := conn.ReadMessage()
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("msg %q err %v", msg, err)
	}
	_, _, err = conn.ReadMessage()
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != 4001 || ce.Text != "bye" {
		t.Fatalf("close %v", err)
	}
	conn.Close()
	_, resp, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr().String()+"/refuse", nil)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("refusal %v %v", resp, err)
	}
	_, resp, err = websocket.DefaultDialer.Dial("ws://"+srv.Addr().String()+"/nowhere", nil)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("unrouted %v %v", resp, err)
	}
}

func TestProxyHeadersTrustOnlyListedPeers(t *testing.T) {
	trusted := newTrustedHosts([]string{"127.0.0.1", "10.0.0.0/8", "unix-proxy"})
	if !trusted.contains("10.1.2.3") || trusted.contains("192.168.0.1") || !trusted.contains("unix-proxy") {
		t.Fatal("membership")
	}
	if got := trusted.trustedClientHost("203.0.113.9, 10.0.0.2, 127.0.0.1"); got != "203.0.113.9" {
		t.Fatalf("client %q", got)
	}
	if got := trusted.trustedClientHost("10.0.0.5, 127.0.0.1"); got != "10.0.0.5" {
		t.Fatalf("all trusted %q", got)
	}
}
