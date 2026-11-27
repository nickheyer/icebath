package speech

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Rate live sessions are fed at
const LiveRate = 16000

// Seconds a live session waits for its final transcript once the audio ends
const FinishTimeout = 20.0

// Resamples s16le mono PCM between rates across chunk boundaries, averaging whole ratios and interpolating others
type Resampler struct {
	from, to int
	at       float64
	carry    []int16
}

func NewResampler(from, to int) *Resampler { return &Resampler{from: from, to: to} }

func samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return out
}

// Little endian bytes of samples
func PCMBytes(in []int16) []byte {
	out := make([]byte, 2*len(in))
	for i, v := range in {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// Converts one chunk, the rest it could not use yet carried into the next
func (r *Resampler) Resample(pcm []byte) []byte {
	if r.from == r.to || len(pcm) == 0 {
		return pcm
	}
	data := append(r.carry, samples(pcm[:len(pcm)-len(pcm)%2])...)
	if r.from%r.to == 0 {
		step := r.from / r.to
		usable := len(data) - len(data)%step
		out := make([]int16, 0, usable/step)
		for i := 0; i < usable; i += step {
			sum := 0
			for _, v := range data[i : i+step] {
				sum += int(v)
			}
			q := sum / step
			if sum%step < 0 {
				q--
			}
			out = append(out, int16(q))
		}
		r.carry = append([]int16(nil), data[usable:]...)
		return PCMBytes(out)
	}
	ratio := float64(r.from) / float64(r.to)
	position, last := r.at, len(data)-1
	var out []int16
	for position < float64(last) {
		i := int(position)
		fraction := position - float64(i)
		out = append(out, int16(float64(data[i])+float64(int(data[i+1])-int(data[i]))*fraction))
		position += ratio
	}
	keep := int(position)
	r.carry = append([]int16(nil), data[min(keep, len(data)):]...)
	r.at = position - float64(keep)
	return PCMBytes(out)
}

// One wire of a live service over a websocket: what it sends first, whether it takes audio yet, how it sends audio and ends it, and the
// transcript an event finishes with
type Wire interface {
	Open(socket *Socket) error
	Ready() bool
	Send(socket *Socket, chunk []byte) error
	// Ends the audio; a transcript it answers finishes the session at once, such as an empty one when no audio was sent
	End(socket *Socket) (*Transcript, error)
	// Final transcript the event carries, nil while the session goes on
	Event(session *Session, event map[string]any) (*Transcript, error)
}

// Websocket of a live session, read on a goroutine of its own so a wait for an event can time out
type Socket struct {
	conn     *websocket.Conn
	messages chan message
}

type message struct {
	binary bool
	data   []byte
	err    error
}

func (s *Socket) Text(text string) error {
	return s.conn.WriteMessage(websocket.TextMessage, []byte(text))
}

func (s *Socket) JSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *Socket) Binary(data []byte) error { return s.conn.WriteMessage(websocket.BinaryMessage, data) }

// A live transcription feeding 16 kHz PCM to a service on one worker, its partial text handed out as it changes
type Session struct {
	provider string
	partial  func(string)
	url      string
	headers  http.Header
	wire     Wire
	ctx      context.Context
	cancel   context.CancelFunc
	audio    chan []byte
	ended    chan struct{}
	endOnce  sync.Once
	done     chan struct{}
	mu       sync.Mutex
	result   Transcript
	finished bool
	last     string
	resample *Resampler
	backlog  [][]byte
}

// Live session of a service at a websocket address, fed audio at the rate given; its worker starts at once
func Socketed(ctx context.Context, provider, url string, headers http.Header, rate int, wire Wire, partial func(string)) *Session {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &Session{provider: provider, partial: partial, url: url, headers: headers, wire: wire, ctx: ctx, cancel: cancel,
		audio: make(chan []byte, 4096), ended: make(chan struct{}), done: make(chan struct{})}
	if rate > 0 && rate != LiveRate {
		s.resample = NewResampler(rate, LiveRate)
	}
	go s.run()
	return s
}

func (s *Session) Provider() string { return s.provider }

// Queues audio until the session finishes
func (s *Session) Push(pcm []byte) {
	if len(pcm) == 0 || s.closed() {
		return
	}
	if s.resample != nil {
		pcm = s.resample.Resample(pcm)
	}
	select {
	case s.audio <- append([]byte(nil), pcm...):
	case <-s.done:
	}
}

func (s *Session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) end() { s.endOnce.Do(func() { close(s.ended) }) }

// Ends the audio and waits for the final transcript, a failure when the service keeps it longer than the timeout
func (s *Session) Finish(timeout float64) Transcript {
	s.end()
	select {
	case <-s.done:
	case <-time.After(time.Duration(timeout * float64(time.Second))):
		s.Cancel()
		return s.failure("live transcription timed out")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Stops the worker, the session finishing as cancelled
func (s *Session) Cancel() {
	s.settle(s.failure("cancelled"))
	s.cancel()
}

func (s *Session) failure(message string) Transcript {
	return Transcript{Error: message, Provider: s.provider}
}

// Records a changed partial and hands it on
func (s *Session) Partial(text string) {
	text = strings.TrimSpace(text)
	s.mu.Lock()
	changed := text != "" && text != s.last
	if changed {
		s.last = text
	}
	s.mu.Unlock()
	if changed && s.partial != nil {
		s.partial(text)
	}
}

// Keeps the first outcome and wakes Finish
func (s *Session) settle(result Transcript) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.result, s.finished = result, true
	close(s.done)
}

// Transcript a session finished with as a success
func (s *Session) Heard(text string) *Transcript {
	return &Transcript{Success: true, Text: strings.TrimSpace(text), Provider: s.provider}
}

// Failure the service reported
func (s *Session) Refused(message string) *Transcript {
	failure := s.failure(message)
	return &failure
}

// Worker: connects, streams the audio while it reads events, and closes the socket behind it
func (s *Session) run() {
	result, err := s.socket()
	if err != nil {
		slog.Warn(fmt.Sprintf("Live STT (%s) failed: %s", s.provider, err))
		s.settle(s.failure(fmt.Sprintf("live transcription failed: %s", err)))
		return
	}
	s.settle(result)
}

func (s *Session) socket() (Transcript, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(s.ctx, s.url, s.headers)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return Transcript{}, err
	}
	socket := &Socket{conn: conn, messages: make(chan message, 64)}
	stopped := make(chan struct{})
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			select {
			case socket.messages <- message{binary: kind == websocket.BinaryMessage, data: data, err: err}:
			case <-stopped:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopped)
		go func() {
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			conn.Close()
		}()
	}()
	return s.loop(socket)
}

// Streams audio while reading events, until one finishes the session or the service stalls past the finish time after the audio ended
func (s *Session) loop(socket *Socket) (Transcript, error) {
	if err := s.wire.Open(socket); err != nil {
		return Transcript{}, err
	}
	var endedAt time.Time
	for {
		if s.ctx.Err() != nil {
			return s.failure("cancelled"), nil
		}
		if endedAt.IsZero() {
			ended, final, err := s.drain(socket)
			if err != nil {
				return Transcript{}, err
			}
			if final != nil {
				return *final, nil
			}
			if ended {
				endedAt = time.Now()
			}
		} else if time.Since(endedAt) > FinishTimeout*time.Second {
			return s.failure("provider never finalized the transcript"), nil
		}
		select {
		case <-s.done:
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.result, nil
		case m := <-socket.messages:
			if m.err != nil {
				return Transcript{}, m.err
			}
			if m.binary {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(m.data, &event); err != nil {
				return Transcript{}, fmt.Errorf("transcription stream event is not a JSON object: %w", err)
			}
			result, err := s.wire.Event(s, event)
			if err != nil {
				return Transcript{}, err
			}
			if result != nil {
				return *result, nil
			}
			if len(s.backlog) > 0 && s.wire.Ready() {
				for _, chunk := range s.backlog {
					if err := s.wire.Send(socket, chunk); err != nil {
						return Transcript{}, err
					}
				}
				s.backlog = nil
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Sends or holds back every queued chunk; true once the end of the audio was sent on, with the transcript the wire finished with then
func (s *Session) drain(socket *Socket) (bool, *Transcript, error) {
	for {
		select {
		case chunk := <-s.audio:
			if !s.wire.Ready() {
				s.backlog = append(s.backlog, chunk)
				continue
			}
			if err := s.wire.Send(socket, chunk); err != nil {
				return false, nil, err
			}
		default:
			select {
			case <-s.ended:
				if len(s.audio) > 0 {
					continue
				}
				// Audio held back for a service that never became ready is dropped, as it would drop it
				if !s.wire.Ready() {
					s.backlog = nil
				}
				final, err := s.wire.End(socket)
				return true, final, err
			default:
				return false, nil, nil
			}
		}
	}
}

// Text of an event field, empty when it is missing or empty
func Field(event map[string]any, key string) string {
	if text, ok := event[key].(string); ok {
		return text
	}
	if value, ok := event[key]; ok && value != nil {
		if b, isBool := value.(bool); isBool && !b {
			return ""
		}
		return fmt.Sprint(value)
	}
	return ""
}

// Whether an event field is true
func Flag(event map[string]any, key string) bool {
	on, _ := event[key].(bool)
	return on
}

// Websocket address of a base URL with a path and query, wss for an https base
func SocketURL(base, suffix, query string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	address := base + suffix
	if query != "" {
		address += "?" + query
	}
	return address
}
