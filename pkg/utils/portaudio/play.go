package portaudioutil

import "sync"

// One buffer playing through its own output stream callback
type playback struct {
	data   []int16
	frame  int
	stream *stream
}

var (
	playMu  sync.Mutex
	playing *playback
)

// Copies the next block into out and zero fills the rest, aborting the stream once the buffer is spent
func (p *playback) fill(out []int16, frames int, _ CallbackFlags) CallbackResult {
	n := min(len(p.data)-p.frame, frames, len(out))
	copy(out[:n], p.data[p.frame:p.frame+n])
	clear(out[n:])
	if n == 0 {
		return Abort
	}
	p.frame += n
	return Continue
}

// Stops the previous playback and plays a mono buffer on the default output device; blockSize zero lets PortAudio choose
func (audio) Play(data []int16, sampleRate float64, blockSize int) error {
	playMu.Lock()
	defer playMu.Unlock()
	stopPlayback()
	p := &playback{data: data}
	s, err := openStream(Output, StreamParams{SampleRate: sampleRate, BlockSize: blockSize, Channels: 1}, nil, p.fill)
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		_ = s.Close()
		return err
	}
	p.stream = s
	playing = p
	return nil
}

// Stops and closes the stream of the last Play
func (audio) Stop() {
	playMu.Lock()
	defer playMu.Unlock()
	stopPlayback()
}

// Stream of the last Play, still returned once it is stopped and closed; ErrNoPlayback before any Play
func (audio) Playback() (Stream, error) {
	playMu.Lock()
	defer playMu.Unlock()
	if playing == nil {
		return nil, ErrNoPlayback
	}
	return playing.stream, nil
}

func stopPlayback() {
	if playing != nil {
		_ = playing.stream.Stop()
		_ = playing.stream.Close()
	}
}
