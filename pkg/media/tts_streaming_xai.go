package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	xaiQueueMax            = 64
	xaiRecvTimeoutSeconds  = 60
	xaiDefaultStreamingURL = "wss://api.x.ai/v1/tts"
	wsOpenTimeout          = 10 * time.Second
	wsPingInterval         = 20 * time.Second
	wsPingTimeout          = 20 * time.Second
	wsCloseTimeout         = 10 * time.Second
	wsMaxMessageSize       = 1 << 20
)

// End marker the pump goroutine queues after its last frame
type xaiPumpDone struct{}

// Puts an item on a bounded queue, giving up once stop is set, false when dropped
func putUnlessStopped(q chan any, item any, stop *pythread.Event, pollS float64) bool {
	for !stop.IsSet() {
		timer := time.NewTimer(time.Duration(pollS * float64(time.Second)))
		select {
		case q <- item:
			timer.Stop()
			return true
		case <-timer.C:
		}
	}
	return false
}

// xAI WebSocket TTS, base64 PCM audio.delta frames at 24 kHz mono int16
type xaiStreamer struct{ streamerBase }

func newXAIStreamer(env *tools.Env, ttsConfig map[string]any, section any) (StreamingTTSProvider, error) {
	s := &xaiStreamer{}
	s.init(env, ttsConfig, section, defaultXAISampleRate)
	return s, nil
}

// API key of the xAI credentials resolved key first, a failing resolver reported as the error
func xaiStreamAPIKey(env *tools.Env) (string, error) {
	key := ""
	err := pluginCall(func() error {
		_, k, _ := ttsResolveXAICredentials(env)
		key = pystr.Strip(k)
		return nil
	})
	return key, err
}

func xaiStreamerAvailable(_ context.Context, env *tools.Env) bool {
	key, err := xaiStreamAPIKey(env)
	return err == nil && key != ""
}

func (s *xaiStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return s.queuedFrames(ctx, text), nil
}

// PCM chunks as the socket delivers them, pumped by a goroutine through a bounded queue
func (s *xaiStreamer) queuedFrames(ctx context.Context, text string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		q := make(chan any, xaiQueueMax)
		stop := &pythread.Event{}
		defer stop.Set()
		go func() {
			if err := pluginCall(func() error { return s.pump(ctx, text, q, stop) }); err != nil {
				putUnlessStopped(q, err, stop, 0.1)
			}
			putUnlessStopped(q, xaiPumpDone{}, stop, 0.1)
		}()
		for {
			switch item := (<-q).(type) {
			case xaiPumpDone:
				return
			case error:
				yield(nil, item)
				return
			case []byte:
				if !yield(item, nil) {
					return
				}
			}
		}
	}
}

var wsCloseCodeExplanations = map[int]string{
	1000: "OK", 1001: "going away", 1002: "protocol error", 1003: "unsupported data",
	1005: "no status received [internal]", 1006: "abnormal closure [internal]",
	1007: "invalid frame payload data", 1008: "policy violation", 1009: "message too big",
	1010: "mandatory extension", 1011: "internal error", 1012: "service restart",
	1013: "try again later", 1014: "bad gateway", 1015: "TLS handshake failure [internal]",
}

// Close frame text as the websockets library renders it
func wsCloseFrameText(code int, reason string) string {
	explanation, ok := wsCloseCodeExplanations[code]
	switch {
	case code >= 3000 && code < 4000:
		explanation = "registered"
	case code >= 4000 && code < 5000:
		explanation = "private use"
	case !ok:
		explanation = "unknown"
	}
	out := fmt.Sprintf("%d (%s)", code, explanation)
	if reason != "" {
		out += " " + reason
	}
	return out
}

// Python exception of a named class raised by the websockets client
type wsError struct {
	class, msg string
	cause      error
}

func (e *wsError) Error() string      { return e.msg }
func (e *wsError) Unwrap() error      { return e.cause }
func (e *wsError) PyTypeName() string { return e.class }

// Parsed ws or wss URI, refused the way the websockets parser refuses it
func wsParseURI(raw string) (*url.URL, error) {
	invalid := func(msg string) error {
		return &wsError{class: "InvalidURI", msg: fmt.Sprintf("%s isn't a valid URI: %s", raw, msg)}
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "ws") && !strings.EqualFold(u.Scheme, "wss") {
		return nil, invalid("scheme isn't ws or wss")
	}
	if u.Hostname() == "" {
		return nil, invalid("hostname isn't provided")
	}
	if u.Fragment != "" {
		return nil, invalid("fragment identifier is meaningless")
	}
	return u, nil
}

// Opens the socket within ten seconds, following redirects the way the websockets client does
func wsConnect(ctx context.Context, rawURL string, headers http.Header) (*websocket.Conn, error) {
	maxRedirects := 10
	if v, ok := os.LookupEnv("WEBSOCKETS_MAX_REDIRECTS"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, &ttsValueError{msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(v))}
		}
		maxRedirects = n
	}
	dctx, cancel := context.WithTimeout(ctx, wsOpenTimeout)
	defer cancel()
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	for i := 0; i < maxRedirects; i++ {
		current, err := wsParseURI(rawURL)
		if err != nil {
			return nil, err
		}
		conn, resp, err := dialer.DialContext(dctx, rawURL, headers)
		if err == nil {
			return conn, nil
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
			return nil, &wsError{class: "TimeoutError", msg: "timed out during opening handshake", cause: err}
		}
		if resp == nil || !errors.Is(err, websocket.ErrBadHandshake) {
			return nil, err
		}
		status := resp.StatusCode
		locations, hasLocation := resp.Header["Location"]
		resp.Body.Close()
		rejected := &wsError{class: "InvalidStatus", msg: fmt.Sprintf("server rejected WebSocket connection: HTTP %d", status), cause: err}
		if !hasLocation || status != 300 && status != 301 && status != 302 && status != 303 && status != 307 && status != 308 {
			return nil, rejected
		}
		next, err := url.Parse(strings.Join(locations, ", "))
		if err != nil {
			return nil, &wsError{class: "InvalidURI", msg: fmt.Sprintf("%s isn't a valid URI: scheme isn't ws or wss", strings.Join(locations, ", ")), cause: rejected}
		}
		target := current.ResolveReference(next)
		if _, err := wsParseURI(target.String()); err != nil {
			return nil, err
		}
		if strings.EqualFold(current.Scheme, "wss") && !strings.EqualFold(target.Scheme, "wss") {
			return nil, &wsError{class: "SecurityError", msg: fmt.Sprintf("cannot follow redirect to non-secure URI %s", target), cause: rejected}
		}
		rawURL = target.String()
	}
	return nil, &wsError{class: "SecurityError", msg: fmt.Sprintf("more than %d redirects", maxRedirects)}
}

// Pings every twenty seconds and fails the socket with 1011 when a pong does not answer in time
func wsKeepalive(conn *websocket.Conn, pongs <-chan struct{}, done <-chan struct{}, failed *atomic.Bool) {
	for {
		select {
		case <-done:
			return
		case <-time.After(wsPingInterval):
		}
		select {
		case <-pongs:
		default:
		}
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsPingTimeout)); err != nil {
			return
		}
		select {
		case <-done:
			return
		case <-pongs:
		case <-time.After(wsPingTimeout):
			failed.Store(true)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "keepalive ping timeout"), time.Now().Add(time.Second))
			conn.Close()
			return
		}
	}
}

// Reads one message, reporting the oversize reason once a frame header pushes it past the size limit
func wsReadMessage(conn *websocket.Conn) (int, []byte, string, error) {
	kind, r, err := conn.NextReader()
	if err != nil {
		return 0, nil, "", err
	}
	length := reflect.ValueOf(conn).Elem().FieldByName("readLength")
	var data []byte
	seen := int64(0)
	buf := make([]byte, 32*1024)
	for {
		if total := length.Int(); total > seen {
			if total > wsMaxMessageSize {
				reason := fmt.Sprintf("frame with %d bytes ", total-seen)
				if seen > 0 {
					reason += fmt.Sprintf("after reading %d bytes ", seen)
				}
				return 0, nil, reason + fmt.Sprintf("exceeds limit of %d bytes", wsMaxMessageSize), nil
			}
			seen = total
		}
		n, err := r.Read(buf)
		data = append(data, buf[:n]...)
		if err == io.EOF {
			return kind, data, "", nil
		}
		if err != nil {
			return 0, nil, "", err
		}
	}
}

// ConnectionClosed text of the websockets client for a failed read, true for a clean close
func wsClosedText(conn *websocket.Conn, err error, keepaliveFailed bool) (string, bool) {
	var closeErr *websocket.CloseError
	switch {
	case keepaliveFailed:
		return "sent " + wsCloseFrameText(websocket.CloseInternalServerErr, "keepalive ping timeout") + "; no close frame received", false
	case errors.As(err, &closeErr) && closeErr.Code != websocket.CloseAbnormalClosure:
		frame := wsCloseFrameText(closeErr.Code, closeErr.Text)
		clean := closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway || closeErr.Code == websocket.CloseNoStatusReceived
		return "received " + frame + "; then sent " + frame, clean
	}
	return "no close frame received or sent", false
}

// Sends the close frame, waits up to ten seconds for the peer to answer, then drops the connection
func wsCloseGracefully(conn *websocket.Conn, peerClosed bool) {
	if !peerClosed {
		if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(wsCloseTimeout)); err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(wsCloseTimeout))
			for {
				if _, _, err := conn.NextReader(); err != nil {
					break
				}
			}
		}
	}
	conn.Close()
}

// Sends the sentence and queues every decoded audio.delta until audio.done, an error frame or the byte ceiling
func (s *xaiStreamer) pump(ctx context.Context, text string, q chan any, stop *pythread.Event) error {
	apiKey, err := xaiStreamAPIKey(s.env)
	if err != nil {
		return err
	}
	if apiKey == "" {
		return &ttsRuntimeError{msg: "No xAI credentials for streaming TTS"}
	}
	section, err := s.sectionMap()
	if err != nil {
		return err
	}
	voice := pystr.Strip(pyrepr.Str(dictGetDefault(section, "voice_id", defaultXAIVoiceID)))
	if voice == "" {
		voice = defaultXAIVoiceID
	}
	language := pystr.Strip(pyrepr.Str(dictGetDefault(section, "language", defaultXAILanguage)))
	if language == "" {
		language = defaultXAILanguage
	}
	base := xaiDefaultStreamingURL
	if v := section["streaming_url"]; pyTruthy(v) {
		base = pyrepr.Str(v)
	}
	base = pystr.Strip(base)
	params := "voice=" + url.QueryEscape(voice) + "&language=" + url.QueryEscape(language) + "&codec=pcm&sample_rate=" + strconv.Itoa(s.SampleRate())
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	conn, err := wsConnect(ctx, base+sep+params, http.Header{"Authorization": {"Bearer " + apiKey}, "User-Agent": {UserAgent}})
	if err != nil {
		return err
	}
	pongs, done := make(chan struct{}, 1), make(chan struct{})
	var keepaliveFailed atomic.Bool
	peerClosed := false
	conn.SetPongHandler(func(string) error {
		select {
		case pongs <- struct{}{}:
		default:
		}
		return nil
	})
	go wsKeepalive(conn, pongs, done, &keepaliveFailed)
	defer func() {
		close(done)
		wsCloseGracefully(conn, peerClosed)
	}()
	for _, frame := range []*pyjson.Object{jsonObject("type", "text.delta", "delta", text), jsonObject("type", "text.done")} {
		raw, err := pyjson.Dumps(frame)
		if err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
			peerClosed = true
			_ = conn.SetReadDeadline(time.Now().Add(wsCloseTimeout))
			var readErr error
			for readErr == nil {
				_, _, readErr = conn.NextReader()
			}
			closed, clean := wsClosedText(conn, readErr, keepaliveFailed.Load())
			class := "ConnectionClosedError"
			if clean {
				class = "ConnectionClosedOK"
			}
			return &wsError{class: class, msg: closed, cause: err}
		}
	}
	enqueued := 0
	for {
		_ = conn.SetReadDeadline(time.Now().Add(xaiRecvTimeoutSeconds * time.Second))
		kind, data, tooBig, err := wsReadMessage(conn)
		if tooBig != "" {
			peerClosed = true
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, tooBig), time.Now().Add(time.Second))
			return &ttsRuntimeError{msg: "xAI WS closed mid-stream: sent " + wsCloseFrameText(websocket.CloseMessageTooBig, tooBig) + "; no close frame received"}
		}
		if err != nil {
			var closeErr *websocket.CloseError
			var ne net.Error
			if !keepaliveFailed.Load() && !errors.As(err, &closeErr) && errors.As(err, &ne) && ne.Timeout() {
				return &ttsRuntimeError{msg: fmt.Sprintf("xAI streaming TTS: no audio for %ds", xaiRecvTimeoutSeconds)}
			}
			peerClosed = true
			closed, clean := wsClosedText(conn, err, keepaliveFailed.Load())
			if clean {
				return nil
			}
			return &ttsRuntimeError{msg: "xAI WS closed mid-stream: " + closed, cause: err}
		}
		if kind == websocket.TextMessage {
			var bad *pycodecs.UnicodeDecodeError
			if _, derr := pycodecs.Decode(data, "utf-8"); errors.As(derr, &bad) {
				peerClosed = true
				reason := fmt.Sprintf("%s at position %d", bad.Reason, bad.Pos)
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInvalidFramePayloadData, reason), time.Now().Add(time.Second))
				return &ttsRuntimeError{msg: "xAI WS closed mid-stream: sent " + wsCloseFrameText(websocket.CloseInvalidFramePayloadData, reason) + "; no close frame received", cause: derr}
			}
		}
		if kind != websocket.TextMessage {
			continue
		}
		decoded, err := pyjson.Loads(string(data))
		if err != nil {
			continue
		}
		msgType, err := pyJSONGet(decoded, "type")
		if err != nil {
			return err
		}
		envelope := decoded.(*pyjson.Object)
		switch msgType {
		case "audio.delta":
			b64, _ := envelope.Get("delta")
			if !pyTruthy(b64) {
				continue
			}
			pcm, err := pyB64DecodeValue(b64)
			if err != nil {
				return err
			}
			enqueued += len(pcm)
			if enqueued > streamSentenceByteCap {
				ttsStreamingLog().Warn(fmt.Sprintf("xAI streaming TTS exceeded %d bytes for one sentence; closing upstream", streamSentenceByteCap))
				return nil
			}
			if !putUnlessStopped(q, pcm, stop, 0.1) {
				return nil
			}
		case "audio.done":
			return nil
		case "error":
			detail, _ := envelope.Get("message")
			if !pyTruthy(detail) {
				detail, _ = envelope.Get("error")
			}
			if !pyTruthy(detail) {
				detail = envelope
			}
			return &ttsRuntimeError{msg: "xAI streaming TTS error: " + pyrepr.Str(detail)}
		}
	}
}
