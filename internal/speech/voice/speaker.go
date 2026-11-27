package voice

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/portaudio"
)

const (
	// Sentences of a streaming voice fetched at once
	prefetch = 3
	// Sentences of a file voice waiting while the one before plays, each made after the one before
	ahead = 2
	// Output stream rebuilds after failed writes before the rest goes through files
	reinits = 3
)

// One sentence's speech as it arrives, its clips in order
type sentence struct {
	clips chan *clip
	// Its audio began to arrive
	heard atomic.Bool
}

// Reads a reply aloud as its text arrives: the text cut into sentences, each spoken once, fetched ahead while the one before plays
type speaker struct {
	mode   *Mode
	ctx    context.Context
	cancel context.CancelFunc
	live   bool
	// Closed to cut the speech
	stop     chan struct{}
	stopOnce sync.Once
	// Closed once everything queued has played or been dropped
	done chan struct{}

	mu      sync.Mutex
	text    []string
	ended   bool
	wake    chan struct{}
	chunker *speech.Chunker
	said    []string
	queue   chan *sentence
	slots   chan struct{}
	// Fetch of the sentence before, which a file voice waits for
	last chan struct{}
	// Sentences whose audio arrived and has not finished playing
	ready  atomic.Int32
	device bool
	stream *portaudio.Stream
	rate   int
	reinit int
}

// Speech of a reply cut into sentences of at least least characters; live asks a streaming voice for PCM as it is made
func newSpeaker(m *Mode, ctx context.Context, least int, live bool) *speaker {
	ctx, cancel := context.WithCancel(ctx)
	s := &speaker{mode: m, ctx: ctx, cancel: cancel, live: live, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1),
		chunker: speech.NewChunker(least), queue: make(chan *sentence, ahead), slots: make(chan struct{}, prefetch)}
	if live {
		s.queue = make(chan *sentence, 1024)
	}
	go s.split()
	go s.play()
	return s
}

func (s *speaker) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *speaker) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Adds reply text
func (s *speaker) add(text string) {
	s.mu.Lock()
	if !s.ended {
		s.text = append(s.text, text)
	}
	s.mu.Unlock()
	s.nudge()
}

// Ends the reply text, what is queued still spoken
func (s *speaker) finish() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	s.nudge()
}

// Cuts the speech: nothing more fetched or played
func (s *speaker) halt() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.cancel()
	s.finish()
}

// Whether the speech still has text to say or audio to play
func (s *speaker) pending() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Whether audio of the speech sounds or waits to
func (s *speaker) audible() bool {
	return !s.stopped() && s.ready.Load() > 0
}

// Cuts the queued text into sentences and speaks each, what is held flushed once it passes 100 characters with no text for half a second
func (s *speaker) split() {
	defer close(s.queue)
	for {
		s.mu.Lock()
		batch, ended := s.text, s.ended
		s.text = nil
		s.mu.Unlock()
		for _, delta := range batch {
			for _, line := range s.chunker.Feed(delta) {
				s.say(line)
			}
		}
		if ended {
			for _, line := range s.chunker.Flush() {
				s.say(line)
			}
			return
		}
		select {
		case <-s.wake:
		case <-s.stop:
			return
		case <-time.After(500 * time.Millisecond):
			if utf8.RuneCountInString(s.chunker.Held()) > 100 {
				for _, line := range s.chunker.Flush() {
					s.say(line)
				}
			}
		}
	}
}

// Speaks one sentence unless it was already said: a streaming voice's fetched while two others are, a file voice's after the one before
func (s *speaker) say(line string) {
	if s.stopped() {
		return
	}
	cleaned := strings.TrimSpace(speech.Spoken(line, 0))
	if cleaned == "" {
		return
	}
	key := strings.TrimRight(strings.ToLower(cleaned), ".!,")
	for _, prev := range s.said {
		if strings.TrimRight(strings.ToLower(prev), ".!,") == key {
			return
		}
	}
	s.said = append(s.said, cleaned)
	s.mode.noteSpoken(cleaned)
	if s.live {
		select {
		case s.slots <- struct{}{}:
		case <-s.stop:
			return
		}
	}
	next := &sentence{clips: make(chan *clip, 16)}
	prev, fetched := s.last, make(chan struct{})
	s.last = fetched
	go s.fetch(next, cleaned, prev, fetched)
	select {
	case s.queue <- next:
	case <-s.stop:
	}
}

// Fetches one sentence's speech into its clips
func (s *speaker) fetch(next *sentence, text string, prev, fetched chan struct{}) {
	defer close(fetched)
	defer close(next.clips)
	if s.live {
		defer func() { <-s.slots }()
	} else if prev != nil {
		select {
		case <-prev:
		case <-s.stop:
			return
		}
	}
	if s.stopped() {
		return
	}
	err := s.mode.backend.speak(s.ctx, text, s.live, func(c *clip) {
		if next.heard.CompareAndSwap(false, true) {
			s.ready.Add(1)
		}
		select {
		case next.clips <- c:
		case <-s.stop:
		}
	})
	switch {
	case s.stopped():
		if s.live && next.heard.Load() {
			slog.Info("TTS CUT: prefetch cancelled (stop_event set mid-sentence) — partial audio only")
		}
	case err == nil:
	case !s.live:
		slog.Warn(fmt.Sprintf("Sync per-sentence TTS synthesis failed: %s", err))
	case next.heard.Load():
		slog.Warn(fmt.Sprintf("TTS CUT: streaming TTS prefetch failed mid-sentence (partial audio only): %s", err))
	default:
		slog.Warn(fmt.Sprintf("Streaming TTS synthesis failed: %s", err))
	}
}

// Plays the sentences in order, PCM through the speakers' stream where PortAudio plays it, everything else through files
func (s *speaker) play() {
	defer close(s.done)
	defer s.cancel()
	defer s.closeStream()
	if s.live {
		s.device = usable()
	}
	for next := range s.queue {
		for c := range next.clips {
			if s.stopped() {
				drain(c)
				continue
			}
			switch {
			case c.format == "pcm" && s.device:
				s.pcm(c)
			case c.format == "pcm":
				s.file(c, "wav")
			default:
				s.file(c, c.format)
			}
		}
		if next.heard.Load() {
			s.ready.Add(-1)
		}
	}
}

// Whether PCM may stream to the speakers: never on macOS, else once PortAudio opens
func usable() bool {
	if !deviceOutput() {
		return false
	}
	if _, err := audio(); err != nil {
		slog.Debug(fmt.Sprintf("PortAudio not available, streamer→tempfile: %s", err))
		return false
	}
	return true
}

func drain(c *clip) {
	for range c.data {
	}
}

// Writes a PCM clip to the output stream, a clip the stream can't take played through a file and the rest of one whose stream broke
// past repair dropped
func (s *speaker) pcm(c *clip) {
	s.mode.player.mark(true)
	defer s.mode.player.mark(false)
	var odd []byte
	opened := false
	for chunk := range c.data {
		if s.stopped() {
			continue
		}
		buf := append(odd, chunk...)
		aligned := len(buf) - len(buf)%2
		odd = append([]byte(nil), buf[aligned:]...)
		if aligned == 0 {
			continue
		}
		if !opened {
			if !s.ensure(c.rate) {
				s.fallback(c, buf)
				return
			}
			opened = true
		}
		if _, err := s.stream.Write(samplesOf(buf[:aligned])); err != nil {
			slog.Warn(fmt.Sprintf("PortAudio write failed, attempting stream reinit: %s", err))
			if !s.repair() {
				drain(c)
				return
			}
			s.stream.Write(samplesOf(buf[:aligned]))
		}
	}
}

// Plays what is left of a PCM clip through a temp WAV
func (s *speaker) fallback(c *clip, head []byte) {
	rest := &clip{format: c.format, rate: c.rate, data: make(chan []byte, 1)}
	go func() {
		defer close(rest.data)
		rest.data <- head
		for chunk := range c.data {
			rest.data <- chunk
		}
	}()
	s.file(rest, "wav")
}

func openOutput(rate int) (*portaudio.Stream, error) {
	pa, err := audio()
	if err != nil {
		return nil, err
	}
	stream, err := pa.OpenOutput(portaudio.StreamParams{SampleRate: float64(rate), Channels: 1})
	if err != nil {
		return nil, err
	}
	if err := stream.Start(); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// Opens the output stream at the rate, reopening it when the rate changes; false once it can't, the rest then going through files
func (s *speaker) ensure(rate int) bool {
	if s.stream != nil && s.rate == rate {
		return true
	}
	if s.stream == nil && s.reinit >= reinits {
		return false
	}
	s.closeStream()
	stream, err := openOutput(rate)
	if err != nil {
		slog.Warn(fmt.Sprintf("PortAudio output stream failed: %s", err))
		s.reinit = reinits
		return false
	}
	s.stream, s.rate = stream, rate
	return true
}

// Replaces a stream a write broke, false once the rebuilds are spent or this one failed
func (s *speaker) repair() bool {
	s.closeStream()
	if s.reinit >= reinits {
		slog.Warn(fmt.Sprintf("TTS: PortAudio reinit exhausted after %d attempts, falling back to tempfile for remaining sentences", reinits))
		return false
	}
	s.reinit++
	stream, err := openOutput(s.rate)
	if err != nil {
		slog.Warn(fmt.Sprintf("TTS: PortAudio stream reinit failed: %s", err))
		return false
	}
	slog.Info("TTS: PortAudio output stream reinitialized after error")
	s.stream = stream
	return true
}

func (s *speaker) closeStream() {
	if s.stream != nil {
		s.stream.Stop()
		s.stream.Close()
		s.stream = nil
	}
}

// Plays a clip through a temp file in its format, PCM wrapped as WAV
func (s *speaker) file(c *clip, format string) {
	var data []byte
	for chunk := range c.data {
		data = append(data, chunk...)
	}
	if s.stopped() || len(data) == 0 {
		return
	}
	if c.format == "pcm" {
		data = speech.WAV(data[:len(data)-len(data)%2], c.rate, channels, sampleBits)
	}
	file, err := os.CreateTemp("", "icebath-speech-*."+format)
	if err != nil {
		slog.Warn(fmt.Sprintf("Temp-file TTS fallback failed: %s", err))
		return
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Temp-file TTS fallback failed: %s", err))
		return
	}
	s.mode.player.Play(file.Name())
}
