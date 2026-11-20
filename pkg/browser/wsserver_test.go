package browser

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server side websocket used by the tests, framing without masks as a server must
type testWSServer struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

// Upgrades an HTTP request into a websocket and hands the hijacked socket to serve
func testWSHandler(serve func(*testWSServer)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") == "" {
			http.Error(w, "not a websocket handshake", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		accept := wsAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
		s := &testWSServer{conn: conn, br: rw.Reader}
		serve(s)
		conn.Close()
	})
}

// Starts an httptest server whose root path upgrades to a websocket
func newTestWSServer(serve func(*testWSServer)) (*httptest.Server, string) {
	srv := httptest.NewServer(testWSHandler(serve))
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// Reads one client frame, unmasking its payload
func (s *testWSServer) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(s.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin := head[0]&0x80 != 0
	op := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if !masked {
		return false, 0, nil, errors.New("client frame was not masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(s.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// Reads the next complete data message, answering pings, until a text or binary message arrives
func (s *testWSServer) readMessage() (byte, []byte, error) {
	var buf []byte
	var firstOp byte
	for {
		fin, op, payload, err := s.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := s.writeFrame(true, opPong, payload); err != nil {
				return 0, nil, err
			}
		case opClose:
			_ = s.writeFrame(true, opClose, payload)
			return opClose, payload, nil
		case opPong:
		case opContinuation:
			buf = append(buf, payload...)
			if fin {
				return firstOp, buf, nil
			}
		default:
			if fin {
				return op, payload, nil
			}
			firstOp, buf = op, append([]byte(nil), payload...)
		}
	}
}

// Writes one unmasked frame
func (s *testWSServer) writeFrame(fin bool, op byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := make([]byte, 2, 10)
	if fin {
		head[0] = 0x80
	}
	head[0] |= op
	n := len(payload)
	switch {
	case n <= 125:
		head[1] = byte(n)
	case n <= 0xFFFF:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	if _, err := s.conn.Write(head); err != nil {
		return err
	}
	_, err := s.conn.Write(payload)
	return err
}

// Writes one text message in a single frame
func (s *testWSServer) writeText(text string) error {
	return s.writeFrame(true, opText, []byte(text))
}
