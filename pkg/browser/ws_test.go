package browser

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWSHandshakeAndEcho(t *testing.T) {
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		for {
			op, payload, err := s.readMessage()
			if err != nil || op == opClose {
				return
			}
			if err := s.writeFrame(true, op, payload); err != nil {
				return
			}
		}
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL+"/devtools/browser/abc", WSOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(CloseNormal, "done")
	big := bytes.Repeat([]byte("x"), 70000)
	for _, payload := range [][]byte{[]byte("hello"), big} {
		if err := conn.WriteText(ctx, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		op, got, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if op != opText || !bytes.Equal(got, payload) {
			t.Fatalf("echo mismatch: op=%d len=%d", op, len(got))
		}
	}
}

func TestWSFragmentsAndPingsReassemble(t *testing.T) {
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		_ = s.writeFrame(false, opText, []byte("ab"))
		_ = s.writeFrame(true, opPing, []byte("keepalive"))
		_ = s.writeFrame(false, opContinuation, []byte("cd"))
		_ = s.writeFrame(true, opContinuation, []byte("ef"))
		fin, op, payload, err := s.readFrame()
		if err != nil || !fin || op != opPong || string(payload) != "keepalive" {
			_ = s.writeText("no pong")
			return
		}
		_ = s.writeText("pong ok")
		_, _, _ = s.readMessage()
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL, WSOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(CloseNormal, "")
	_, got, err := conn.ReadMessage(ctx)
	if err != nil || string(got) != "abcdef" {
		t.Fatalf("reassembly: %q %v", got, err)
	}
	_, got, err = conn.ReadMessage(ctx)
	if err != nil || string(got) != "pong ok" {
		t.Fatalf("pong reply: %q %v", got, err)
	}
}

func TestWSServerCloseSurfacesCode(t *testing.T) {
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		_ = s.writeFrame(true, opClose, closePayload(CloseGoingAway, "bye"))
		_, _, _, _ = s.readFrame()
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL, WSOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _, err = conn.ReadMessage(ctx)
	var closed *ErrWSClosed
	if !errors.As(err, &closed) || closed.Code != CloseGoingAway || closed.Reason != "bye" {
		t.Fatalf("expected close 1001 bye, got %v", err)
	}
}

func TestWSRejectsMaskedServerFramesAndOversize(t *testing.T) {
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		frame, _ := buildClientFrame(opText, []byte("masked"))
		_, _ = s.conn.Write(frame)
		_, _, _, _ = s.readFrame()
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWS(ctx, wsURL, WSOptions{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, err := conn.ReadMessage(ctx); err == nil || !strings.Contains(err.Error(), "masked") {
		t.Fatalf("expected masked frame rejection, got %v", err)
	}
	srv2, wsURL2 := newTestWSServer(func(s *testWSServer) {
		_ = s.writeText(strings.Repeat("y", 300))
		_, _, _, _ = s.readFrame()
	})
	defer srv2.Close()
	conn2, err := DialWS(ctx, wsURL2, WSOptions{MaxMessageSize: 100})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, err := conn2.ReadMessage(ctx); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected size limit rejection, got %v", err)
	}
}

func TestWSHandshakeRejectsBadAccept(t *testing.T) {
	srv, wsURL := newTestWSServer(func(s *testWSServer) {})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialWS(ctx, "http"+strings.TrimPrefix(wsURL, "ws"), WSOptions{}); err == nil {
		t.Fatal("expected scheme rejection")
	}
	if _, err := DialWS(ctx, strings.Replace(wsURL, "ws://", "ws://user@", 1)+"/", WSOptions{}); err != nil {
		t.Fatalf("userinfo in url should still dial: %v", err)
	}
}
