package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/xai/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Bounds of live xAI speech
const (
	openTimeout    = 10 * time.Second
	receiveTimeout = 60 * time.Second
	pingInterval   = 20 * time.Second
	pingTimeout    = 20 * time.Second
	closeTimeout   = 10 * time.Second
	messageLimit   = 1 << 20
	sentenceCap    = 16 << 20
	// Chunks read ahead of the player
	queued = 64
	// Redirects an open follows before it gives up
	maxRedirects = 10
)

// PCM of a sentence at 24 kHz as xAI's speech socket makes it: the text sent, then each audio.delta until audio.done, read ahead of the
// player on a goroutine of its own
func stream(ctx context.Context, job *speech.Job, text string) (*speech.PCM, error) {
	return &speech.PCM{Rate: config.DefaultRate, Chunks: func(yield func([]byte, error) bool) {
		queue, stop, failure := make(chan []byte, queued), make(chan struct{}), make(chan error, 1)
		go func() {
			defer close(queue)
			failure <- pump(ctx, job, text, queue, stop)
		}()
		defer close(stop)
		for pcm := range queue {
			if !yield(pcm, nil) {
				return
			}
		}
		if err := <-failure; err != nil {
			yield(nil, err)
		}
	}}, nil
}

// Sends the sentence and queues every decoded audio.delta until audio.done, an error event or the byte cap of a sentence
func pump(ctx context.Context, job *speech.Job, text string, queue chan<- []byte, stop <-chan struct{}) error {
	bearer, ok := job.XAI(true)
	if !ok || bearer.Key == "" {
		return errors.New("No xAI credentials for streaming TTS")
	}
	address := job.Text(config.StreamingURL, config.DefaultStreaming)
	separator := "?"
	if strings.Contains(address, "?") {
		separator = "&"
	}
	address += separator + "voice=" + url.QueryEscape(job.Text(config.VoiceID, config.DefaultVoice)) + "&language=" +
		url.QueryEscape(job.Text(config.TTSLanguage, config.DefaultLanguage)) + "&codec=pcm&sample_rate=" + strconv.Itoa(config.DefaultRate)
	conn, err := connect(ctx, address, http.Header{"Authorization": {"Bearer " + bearer.Key}, "User-Agent": {speech.Agent}})
	if err != nil {
		return err
	}
	conn.SetReadLimit(messageLimit)
	pongs, done := make(chan struct{}, 1), make(chan struct{})
	var failed atomic.Bool
	peerClosed := false
	conn.SetPongHandler(func(string) error {
		select {
		case pongs <- struct{}{}:
		default:
		}
		return nil
	})
	go keepalive(conn, pongs, done, &failed)
	defer func() {
		close(done)
		closeGracefully(conn, peerClosed)
	}()
	for _, frame := range []map[string]any{{"type": "text.delta", "delta": text}, {"type": "text.done"}} {
		if err := conn.WriteJSON(frame); err != nil {
			peerClosed = true
			conn.SetReadDeadline(time.Now().Add(closeTimeout))
			var read error
			for read == nil {
				_, _, read = conn.NextReader()
			}
			closed, _ := closedText(read, failed.Load())
			return errors.New(closed)
		}
	}
	total := 0
	for {
		conn.SetReadDeadline(time.Now().Add(receiveTimeout))
		kind, data, err := conn.ReadMessage()
		if errors.Is(err, websocket.ErrReadLimit) {
			peerClosed = true
			return errors.New("xAI WS closed mid-stream: sent " + closeFrame(websocket.CloseMessageTooBig, "") + "; no close frame received")
		}
		if err != nil {
			_, closing := errors.AsType[*websocket.CloseError](err)
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() && !closing && !failed.Load() {
				return fmt.Errorf("xAI streaming TTS: no audio for %ds", int(receiveTimeout.Seconds()))
			}
			peerClosed = true
			closed, clean := closedText(err, failed.Load())
			if clean {
				return nil
			}
			return errors.New("xAI WS closed mid-stream: " + closed)
		}
		if kind != websocket.TextMessage {
			continue
		}
		if reason, bad := invalidUTF8(data); bad {
			peerClosed = true
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInvalidFramePayloadData, reason), time.Now().Add(time.Second))
			return errors.New("xAI WS closed mid-stream: sent " + closeFrame(websocket.CloseInvalidFramePayloadData, reason) + "; no close frame received")
		}
		var decoded any
		if json.Unmarshal(data, &decoded) != nil {
			continue
		}
		event, isObject := decoded.(map[string]any)
		if !isObject {
			return fmt.Errorf("TTS stream event is %s where a mapping holding type was expected", speech.Kind(decoded))
		}
		switch event["type"] {
		case "audio.delta":
			delta := event["delta"]
			if !speech.Held(delta) {
				continue
			}
			encoded, isText := delta.(string)
			if !isText {
				return fmt.Errorf("audio payload is %s where base64 text was expected", speech.Kind(delta))
			}
			pcm, err := speech.Base64(encoded)
			if err != nil {
				return err
			}
			if total += len(pcm); total > sentenceCap {
				slog.Warn(fmt.Sprintf("xAI streaming TTS exceeded %d bytes for one sentence; closing upstream", sentenceCap))
				return nil
			}
			select {
			case queue <- pcm:
			case <-stop:
				return nil
			}
		case "audio.done":
			return nil
		case "error":
			detail := event["message"]
			if !speech.Held(detail) {
				detail = event["error"]
			}
			if !speech.Held(detail) {
				detail = event
			}
			return errors.New("xAI streaming TTS error: " + speech.Text(detail))
		}
	}
}

// Meanings of close codes, as a closed socket's frames are told
var closeMeanings = map[int]string{
	1000: "OK", 1001: "going away", 1002: "protocol error", 1003: "unsupported data", 1005: "no status received [internal]",
	1006: "abnormal closure [internal]", 1007: "invalid frame payload data", 1008: "policy violation", 1009: "message too big",
	1010: "mandatory extension", 1011: "internal error", 1012: "service restart", 1013: "try again later", 1014: "bad gateway",
	1015: "TLS handshake failure [internal]",
}

// Close frame as text: its code, what the code means and its reason
func closeFrame(code int, reason string) string {
	meaning, ok := closeMeanings[code]
	switch {
	case code >= 3000 && code < 4000:
		meaning = "registered"
	case code >= 4000 && code < 5000:
		meaning = "private use"
	case !ok:
		meaning = "unknown"
	}
	text := fmt.Sprintf("%d (%s)", code, meaning)
	if reason != "" {
		text += " " + reason
	}
	return text
}

// Websocket address, refused when it is not one
func socketAddress(raw string) (*url.URL, error) {
	invalid := func(why string) error { return fmt.Errorf("%s isn't a valid URI: %s", raw, why) }
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "ws") && !strings.EqualFold(parsed.Scheme, "wss") {
		return nil, invalid("scheme isn't ws or wss")
	}
	if parsed.Hostname() == "" {
		return nil, invalid("hostname isn't provided")
	}
	if parsed.Fragment != "" {
		return nil, invalid("fragment identifier is meaningless")
	}
	return parsed, nil
}

// Opens the socket within ten seconds, following redirects that stay on a secure address
func connect(ctx context.Context, address string, headers http.Header) (*websocket.Conn, error) {
	dialing, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	for range maxRedirects {
		current, err := socketAddress(address)
		if err != nil {
			return nil, err
		}
		conn, response, err := dialer.DialContext(dialing, address, headers)
		if err == nil {
			return conn, nil
		}
		if ne, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || ok && ne.Timeout() {
			return nil, errors.New("timed out during opening handshake")
		}
		if response == nil || !errors.Is(err, websocket.ErrBadHandshake) {
			return nil, err
		}
		status := response.StatusCode
		locations, moved := response.Header["Location"]
		response.Body.Close()
		if !moved || status != 300 && status != 301 && status != 302 && status != 303 && status != 307 && status != 308 {
			return nil, fmt.Errorf("server rejected WebSocket connection: HTTP %d", status)
		}
		location := strings.Join(locations, ", ")
		next, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("%s isn't a valid URI: scheme isn't ws or wss", location)
		}
		target := current.ResolveReference(next)
		if _, err := socketAddress(target.String()); err != nil {
			return nil, err
		}
		if strings.EqualFold(current.Scheme, "wss") && !strings.EqualFold(target.Scheme, "wss") {
			return nil, fmt.Errorf("cannot follow redirect to non-secure URI %s", target)
		}
		address = target.String()
	}
	return nil, fmt.Errorf("more than %d redirects", maxRedirects)
}

// Pings every twenty seconds and fails the socket with 1011 when no pong answers in time
func keepalive(conn *websocket.Conn, pongs <-chan struct{}, done <-chan struct{}, failed *atomic.Bool) {
	for {
		select {
		case <-done:
			return
		case <-time.After(pingInterval):
		}
		select {
		case <-pongs:
		default:
		}
		if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingTimeout)) != nil {
			return
		}
		select {
		case <-done:
			return
		case <-pongs:
		case <-time.After(pingTimeout):
			failed.Store(true)
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "keepalive ping timeout"), time.Now().Add(time.Second))
			conn.Close()
			return
		}
	}
}

// What closed a socket whose read failed, true for a clean close by the service
func closedText(err error, failed bool) (string, bool) {
	closed, isClose := errors.AsType[*websocket.CloseError](err)
	switch {
	case failed:
		return "sent " + closeFrame(websocket.CloseInternalServerErr, "keepalive ping timeout") + "; no close frame received", false
	case isClose && closed.Code != websocket.CloseAbnormalClosure:
		frame := closeFrame(closed.Code, closed.Text)
		clean := closed.Code == websocket.CloseNormalClosure || closed.Code == websocket.CloseGoingAway || closed.Code == websocket.CloseNoStatusReceived
		return "received " + frame + "; then sent " + frame, clean
	}
	return "no close frame received or sent", false
}

// Sends the close frame, waits up to ten seconds for the service to answer it, then drops the connection
func closeGracefully(conn *websocket.Conn, peerClosed bool) {
	if !peerClosed {
		if conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(closeTimeout)) == nil {
			conn.SetReadDeadline(time.Now().Add(closeTimeout))
			for {
				if _, _, err := conn.NextReader(); err != nil {
					break
				}
			}
		}
	}
	conn.Close()
}

// Why text is not UTF-8 and where, as a decoder words it; false for valid text
func invalidUTF8(data []byte) (string, bool) {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		return fmt.Sprintf("%s at position %d", utf8Failure(data[i:]), i), true
	}
	return "", false
}

// Failure of the sequence a bad byte starts: a byte no sequence starts with, a continuation byte out of range, or a sequence cut short
func utf8Failure(seq []byte) string {
	lead := seq[0]
	var need int
	low, high := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 2
	case lead >= 0xE0 && lead <= 0xEF:
		need = 3
		if lead == 0xE0 {
			low = 0xA0
		} else if lead == 0xED {
			high = 0x9F
		}
	case lead >= 0xF0 && lead <= 0xF4:
		need = 4
		if lead == 0xF0 {
			low = 0x90
		} else if lead == 0xF4 {
			high = 0x8F
		}
	default:
		return "invalid start byte"
	}
	for j := 1; j < need; j++ {
		if j >= len(seq) {
			return "unexpected end of data"
		}
		if seq[j] < low || seq[j] > high {
			return "invalid continuation byte"
		}
		low, high = 0x80, 0xBF
	}
	return "invalid continuation byte"
}

// Live transcription through xAI's speech socket with interim results; none without XAI_API_KEY
func live(ctx context.Context, job *speech.Job, hints speech.Hints, partial func(string)) (speech.Live, error) {
	key := job.Getenv(config.KeyEnv)
	if key == "" {
		return nil, nil
	}
	query := url.Values{"sample_rate": {strconv.Itoa(speech.LiveRate)}, "encoding": {"pcm"}, "interim_results": {"true"},
		"model": {Model(job.Text(config.STTModel, ""))}}
	if hints.Language != "" {
		query.Set("language", hints.Language)
	}
	address := speech.SocketURL(job.Text(config.STTBaseURL, config.DefaultBase), "/stt", query.Encode())
	return speech.Socketed(ctx, config.Name, address, http.Header{"Authorization": {"Bearer " + key}}, speech.LiveRate, &interim{}, partial), nil
}

// Wire of an xAI live session: whole utterances, final chunks of the one going on, and its interim words
type interim struct {
	created    bool
	utterances []string
	chunks     []string
	words      string
}

func (w *interim) Open(*speech.Socket) error { return nil }

// The service drops audio sent before transcript.created
func (w *interim) Ready() bool { return w.created }

func (w *interim) Send(socket *speech.Socket, pcm []byte) error { return socket.Binary(pcm) }

func (w *interim) End(socket *speech.Socket) (*speech.Transcript, error) {
	return nil, socket.JSON(map[string]any{"type": "audio.done"})
}

func (w *interim) text() string {
	var kept []string
	for _, part := range append(append(append([]string(nil), w.utterances...), w.chunks...), w.words) {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

func (w *interim) Event(session *speech.Session, event map[string]any) (*speech.Transcript, error) {
	switch speech.Field(event, "type") {
	case "transcript.created":
		w.created = true
	case "transcript.partial":
		text := strings.TrimSpace(speech.Field(event, "text"))
		switch {
		case speech.Held(event["speech_final"]):
			w.utterances = append(w.utterances, text)
			w.chunks, w.words = nil, ""
		case speech.Held(event["is_final"]):
			w.chunks = append(w.chunks, text)
			w.words = ""
		default:
			w.words = text
		}
		session.Partial(w.text())
	case "transcript.done":
		return session.Heard(w.text()), nil
	case "error":
		return session.Refused(strs.Filled(speech.Field(event, "message"), "provider error")), nil
	}
	return nil, nil
}
