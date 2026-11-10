package googlemeet

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var startBotKeys = []string{"url", "guest_name", "duration", "headed", "auth_state", "session_id", "out_dir"}

// Handler level protocol error sent verbatim as an error envelope
type rpcError struct{ msg string }

func (e *rpcError) Error() string { return e.msg }

// Websocket server running meet bot RPCs on this machine
type nodeServer struct {
	host        string
	port        int
	displayName string
	tokenPath   string
	mu          sync.Mutex
	token       string
}

// Server on host and port whose token lives at tokenPath, the profile meetings node_token.json when empty
func newNodeServer(ctx context.Context, host string, port int, tokenPath, displayName string) *nodeServer {
	if tokenPath == "" {
		tokenPath = filepath.Join(home.Dir(ctx), "workspace", "meetings", "node_token.json")
	}
	return &nodeServer{host: host, port: port, displayName: displayName, tokenPath: tokenPath}
}

// Persisted shared secret, a 32 hex char token minted and written owner only on first use
func (s *nodeServer) ensureToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	tok := ""
	if data, ok := readJSON(s.tokenPath).(*pyjson.Object); ok {
		tok, _ = data.Vals["token"].(string)
	}
	if tok == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		tok = hex.EncodeToString(b)
		if err := config.AtomicJSONWriteMode(ctx, s.tokenPath, pyjson.ObjectOf("token", tok, "generated_at", nowSeconds()), 0o600, pyjson.Indent(2), pyjson.EnsureASCII(false)); err != nil {
			return "", err
		}
	}
	s.token = tok
	return tok, nil
}

// String payload value, the TypeError a non string environment value raises otherwise
func payloadStr(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	}
	return "", fmt.Errorf("TypeError: expected str, bytes or os.PathLike object, not %s", pyTypeName(v))
}

// Starts the bot from the whitelisted payload keys
func rpcStartBot(ctx context.Context, payload *pyjson.Object) (*pyjson.Object, error) {
	kw := map[string]any{}
	for _, k := range startBotKeys {
		if v, ok := payload.Get(k); ok {
			kw[k] = v
		}
	}
	rawURL, ok := kw["url"]
	if !ok {
		return nil, &rpcError{"missing 'url' in payload"}
	}
	url, isStr := rawURL.(string)
	if !isStr {
		return pyjson.ObjectOf("ok", false, "error", "refusing: only https://meet.google.com/ URLs are allowed. got: "+pyrepr.Repr(rawURL)), nil
	}
	o := newStartOptions()
	var err error
	if v, ok := kw["guest_name"]; ok {
		if o.GuestName, err = payloadStr(v); err != nil {
			return nil, err
		}
	}
	if o.Duration, err = payloadStr(kw["duration"]); err != nil {
		return nil, err
	}
	if o.AuthState, err = payloadStr(kw["auth_state"]); err != nil {
		return nil, err
	}
	if o.OutDir, err = payloadStr(kw["out_dir"]); err != nil {
		return nil, err
	}
	o.Headed = truthyJSON(kw["headed"])
	o.SessionID = kw["session_id"]
	return pmStart(ctx, url, o)
}

// Appends text to the active meeting's say queue, ok meaning enqueued rather than spoken
func rpcSay(ctx context.Context, payload *pyjson.Object) (*pyjson.Object, error) {
	text, ok := payload.Get("text")
	if !ok {
		text = ""
	}
	active, err := activeRecord(ctx)
	if err != nil {
		return nil, err
	}
	enqueued := false
	if outDir := getOr(active, "out_dir", nil); active != nil && truthyJSON(outDir) {
		enqueued = appendSay(filepath.Join(pyrepr.Str(outDir), "say_queue.jsonl"), text) == nil
	}
	return pyjson.ObjectOf("ok", true, "enqueued", enqueued, "text", text), nil
}

// Writes one say entry under the queue lock
func appendSay(queue string, text any) error {
	if err := os.MkdirAll(filepath.Dir(queue), 0o777); err != nil {
		return err
	}
	line, err := pyjson.Dumps(pyjson.ObjectOf("text", text, "ts", nowSeconds()))
	if err != nil {
		return err
	}
	release, err := sayQueueLock(queue)
	if err != nil {
		return err
	}
	defer release()
	f, err := os.OpenFile(queue, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// Transcript line count argument as Python slicing accepts it
func lastArg(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		if x == 0 {
			return 0, nil
		}
		return 0, errors.New("TypeError: slice indices must be integers or None or have an __index__ method")
	}
	n, err := pyIntOf(v)
	if err != nil {
		return 0, fmt.Errorf("TypeError: slice indices must be integers or None or have an __index__ method")
	}
	return n, nil
}

// Python exception class name of an error crossing the RPC boundary
func pyExcName(err error) string {
	var pathErr *fs.PathError
	var lockErr *provider.LockTimeoutError
	switch {
	case errors.As(err, &lockErr):
		return "TimeoutError"
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case errors.As(err, &pathErr):
		return "OSError"
	}
	return "RuntimeError"
}

// Validates and dispatches one decoded request, always answering with an envelope
func (s *nodeServer) handleRequest(ctx context.Context, msg *pyjson.Object) *pyjson.Object {
	tok, err := s.ensureToken(ctx)
	if err != nil {
		return makeError("", fmt.Sprintf("%s: %v", pyExcName(err), err))
	}
	if ok, reason := validateRequest(msg, tok); !ok {
		id, _ := msg.Get("id")
		if !truthyJSON(id) {
			id = ""
		}
		return makeError(pyrepr.Str(id), reason)
	}
	reqID, _ := msg.Get("id")
	t, _ := msg.Get("type")
	payload := msg.Vals["payload"].(*pyjson.Object)
	if t == "ping" {
		return pyjson.ObjectOf("type", "pong", "id", reqID, "payload", pyjson.ObjectOf("display_name", s.displayName, "ts", nowSeconds()))
	}
	var res *pyjson.Object
	switch t {
	case "start_bot":
		res, err = rpcStartBot(ctx, payload)
	case "stop":
		reason := "requested"
		if v, ok := payload.Get("reason"); ok {
			reason = pyrepr.Str(v)
		}
		res, err = pmStop(ctx, reason)
	case "status":
		res, err = pmStatus(ctx)
	case "transcript":
		var last int
		if last, err = lastArg(payload.Vals["last"]); err == nil {
			res, err = pmTranscript(ctx, last)
		}
	case "say":
		res, err = rpcSay(ctx, payload)
	default:
		return makeError(reqID, "unhandled type: "+pyrepr.Repr(t))
	}
	var rpcErr *rpcError
	switch {
	case errors.As(err, &rpcErr):
		return makeError(reqID, rpcErr.msg)
	case err != nil && strings.HasPrefix(err.Error(), "TypeError: "):
		return makeError(reqID, err.Error())
	case err != nil:
		return makeError(reqID, fmt.Sprintf("%s: %v", pyExcName(err), err))
	}
	resp, _ := makeResponse(reqID, res)
	return resp
}

// Answers every decoded message of one websocket with its envelope
func (s *nodeServer) handleConn(ctx context.Context, ws *serverWS) {
	defer ws.close()
	for {
		raw, err := ws.readMessage()
		if err != nil {
			return
		}
		var reply *pyjson.Object
		if msg, err := decodeEnvelope(raw); err != nil {
			reply = makeError("", "decode: "+err.Error())
		} else {
			reply = s.handleRequest(ctx, msg)
		}
		text, err := encodeEnvelope(reply)
		if err != nil || ws.writeText([]byte(text)) != nil {
			return
		}
	}
}

// Upgrades every HTTP request to a websocket served by handleConn
func (s *nodeServer) handler(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := acceptWS(w, r)
		if err != nil {
			return
		}
		s.handleConn(ctx, ws)
	})
}

// Runs the websocket server until ctx is cancelled
func (s *nodeServer) serve(ctx context.Context) error {
	if _, err := s.ensureToken(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(ctx)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Server side RFC 6455 connection over a hijacked HTTP connection
type serverWS struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Completes the opening handshake, refusing requests that are not websocket upgrades
func acceptWS(w http.ResponseWriter, r *http.Request) (*serverWS, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "Failed to open a WebSocket connection: invalid upgrade request.\n", http.StatusUpgradeRequired)
		return nil, errors.New("not a websocket upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &serverWS{conn: conn, br: rw.Reader}, nil
}

// Writes one unmasked frame
func (c *serverWS) writeFrame(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

func (c *serverWS) writeText(payload []byte) error { return c.writeFrame(0x1, payload) }

// Reads one masked client frame
func (c *serverWS) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op, masked := h[0]&0x80 != 0, h[0]&0x0F, h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if !masked {
		return false, 0, nil, errors.New("client frame not masked")
	}
	if n > 1<<20 {
		return false, 0, nil, errors.New("frame exceeds the 1 MiB message limit")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// Next complete data message, answering pings and echoing a close
func (c *serverWS) readMessage() ([]byte, error) {
	var msg []byte
	assembling := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		case 0x8:
			_ = c.writeFrame(0x8, payload)
			return nil, io.EOF
		case 0x1, 0x2:
			if assembling {
				return nil, errors.New("new data frame inside a fragmented message")
			}
			msg, assembling = payload, true
		case 0x0:
			if !assembling {
				return nil, errors.New("continuation frame without a message")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("unknown opcode %d", op)
		}
		if fin {
			return msg, nil
		}
	}
}

func (c *serverWS) close() { _ = c.conn.Close() }
