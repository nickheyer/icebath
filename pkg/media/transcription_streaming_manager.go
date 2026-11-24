//go:build !icebath_core && !icebath_noextras

package media

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// Resample converts one chunk, carrying the unconsumed tail into the next call
func (r *Resampler) Resample(pcm []byte) []byte {
	if r.src == r.dst || len(pcm) == 0 {
		return pcm
	}
	data := append(append([]int16(nil), r.carry...), pcmInt16(pcm[:len(pcm)-len(pcm)%2])...)
	if r.src%r.dst == 0 {
		step := r.src / r.dst
		usable := len(data) - len(data)%step
		out := make([]int16, 0, usable/step)
		for i := 0; i < usable; i += step {
			sum := 0
			for _, v := range data[i : i+step] {
				sum += int(v)
			}
			out = append(out, int16(floorDiv(sum, step)))
		}
		r.carry = data[usable:]
		return int16Bytes(out)
	}
	ratio := float64(r.src) / float64(r.dst)
	pos, last := r.pos, len(data)-1
	var out []int16
	for pos < float64(last) {
		i := int(pos)
		frac := pos - float64(i)
		out = append(out, int16(float64(data[i])+float64(float64(int(data[i+1])-int(data[i]))*frac)))
		pos += ratio
	}
	keep := int(pos)
	r.carry = data[min(keep, len(data)):]
	r.pos = pos - float64(keep)
	return int16Bytes(out)
}

// Quotient of a by a positive b rounded toward negative infinity
func floorDiv(a, b int) int {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// Next queued chunk resampled to 16 kHz, end set for the sentinel, an empty chunk when nothing is queued
func (s *StreamingSession) takeAudio(block bool, timeout float64) ([]byte, bool) {
	chunk, end, ok := s.audio.get(block, timeout)
	if !ok {
		return []byte{}, false
	}
	if end {
		return nil, true
	}
	s.mu.Lock()
	resampler := s.to16k
	s.mu.Unlock()
	if len(chunk) > 0 && resampler != nil {
		chunk = resampler.Resample(chunk)
	}
	return chunk, false
}

// Connects, runs the loop, then closes the socket in the background
func (d *wsDriver) session(s *StreamingSession) (map[string]any, error) {
	dialer := websocket.Dialer{HandshakeTimeout: streamingConnectTimeout, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(s.ctx, d.url, d.headers)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	ws := &wsConn{conn: conn, messages: make(chan wsMessage, 64)}
	stopped := make(chan struct{})
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			select {
			case ws.messages <- wsMessage{binary: kind == websocket.BinaryMessage, data: data, err: err}:
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
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			conn.Close()
		}()
	}()
	return d.loop(s, ws)
}

// Streams audio while reading events until one yields the envelope or the provider stalls past the deadline
func (d *wsDriver) loop(s *StreamingSession, ws *wsConn) (map[string]any, error) {
	d.backlog = nil
	if err := d.wire.open(ws); err != nil {
		return nil, err
	}
	endingSince, ending := 0.0, false
	for {
		if s.cancelled.IsSet() {
			return s.errorEnvelope("cancelled"), nil
		}
		if !ending {
			since, ended, err := d.drainAudio(s, ws)
			if err != nil {
				return nil, err
			}
			endingSince, ending = since, ended
		} else if streamMonotonic()-endingSince > streamingFinalizeTimeout {
			return s.errorEnvelope("provider never finalized the transcript"), nil
		}
		msg, ok := ws.recv(20 * time.Millisecond)
		if !ok {
			continue
		}
		if msg.err != nil {
			return nil, msg.err
		}
		if msg.binary {
			continue
		}
		decoded, err := jsonutil.Decode(string(msg.data))
		if err != nil {
			return nil, err
		}
		event, isObject := decoded.(*jsonutil.Object)
		if !isObject {
			return nil, fmt.Errorf("transcription stream event is %s where a mapping was expected", errchain.Kind(decoded))
		}
		result, err := d.wire.event(event)
		if err != nil {
			return nil, err
		}
		if result != nil {
			s.finish(result)
			return result, nil
		}
		if len(d.backlog) > 0 && d.wire.ready() {
			if err := d.flushBacklog(ws); err != nil {
				return nil, err
			}
		}
	}
}

// Sends or holds every queued chunk, reporting the end of audio time once the sentinel is sent on
func (d *wsDriver) drainAudio(s *StreamingSession, ws *wsConn) (float64, bool, error) {
	for {
		chunk, end := s.takeAudio(false, 0)
		if end {
			if err := d.wire.end(ws); err != nil {
				return 0, false, err
			}
			return streamMonotonic(), true, nil
		}
		if len(chunk) == 0 {
			if s.audio.empty() {
				return 0, false, nil
			}
			continue
		}
		if d.wire.ready() {
			if err := d.wire.sendAudio(ws, chunk); err != nil {
				return 0, false, err
			}
		} else {
			d.backlog = append(d.backlog, chunk)
		}
	}
}

func (w *openAIRealtimeWire) sendAudio(ws *wsConn, chunk []byte) error {
	pcm := w.resample.Resample(chunk)
	if len(pcm) == 0 {
		return nil
	}
	w.sentAudio = true
	return ws.sendJSON(jsonObject("type", "input_audio_buffer.append", "audio", base64.StdEncoding.EncodeToString(pcm)))
}

// Pumps audio into a plugin stream session and polls its partial transcript
func (d *pluginStreamDriver) session(s *StreamingSession) (map[string]any, error) {
	for {
		chunk, end := s.takeAudio(true, 0.25)
		if s.cancelled.IsSet() {
			return s.errorEnvelope("cancelled"), nil
		}
		if end {
			result, err := d.inner.Finalize()
			if err != nil {
				return nil, err
			}
			out := make(map[string]any, len(result))
			for k, v := range result {
				out[k] = v
			}
			return out, nil
		}
		if len(chunk) > 0 {
			if err := d.inner.PushAudio(chunk); err != nil {
				return nil, err
			}
		}
		partial, err := d.inner.PartialTranscript()
		if err != nil {
			return nil, err
		}
		s.setPartial(partial)
	}
}
