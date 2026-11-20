package tui

import (
	"context"
	"encoding/base64"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Speech output of this window, bound by the tier that carries voice and unset in a build without it
var SpeechOutput struct {
	Play          func(path string) bool
	Stop          func()
	StartThinking func(ctx context.Context, shouldPlay func() bool) bool
	StopThinking  func()
}

// File extension a speech clip may carry
var speechFormatRe = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// Spoken replies of the shared gateway sessions this window plays, one clip after another
type speechPlayer struct {
	mu          sync.Mutex
	clips       []string
	wake        chan struct{}
	started     bool
	sounding    bool
	cuts        int
	interrupted bool
	gw          turnGateway
	recording   atomic.Bool
}

// The one player of this process, a terminal has one speaker
var speech = &speechPlayer{wake: make(chan struct{}, 1)}

// Acts on one voice.speech event, a clip queued, the thinking blips switched or the speech cut
func (s *speechPlayer) handle(gw turnGateway, p tuigateway.VoiceSpeechPayload) {
	s.mu.Lock()
	s.gw = gw
	s.mu.Unlock()
	switch p.Kind {
	case "audio":
		s.enqueue(p.Audio, p.Format)
	case "think":
		if SpeechOutput.StartThinking != nil {
			SpeechOutput.StartThinking(context.Background(), func() bool { return !s.isSounding() && !s.recording.Load() })
		}
	case "quiet":
		if SpeechOutput.StopThinking != nil {
			SpeechOutput.StopThinking()
		}
	case "stop":
		s.cut()
	}
}

// Writes a clip to a temp file and queues it after the others
func (s *speechPlayer) enqueue(encoded, format string) {
	if SpeechOutput.Play == nil || !speechFormatRe.MatchString(format) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return
	}
	f, err := os.CreateTemp("", "icebath-speech-*."+format)
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return
	}
	s.mu.Lock()
	s.clips = append(s.clips, f.Name())
	if !s.started {
		s.started = true
		go s.run()
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Plays queued clips in order, telling the gateway when speech starts and stops sounding
func (s *speechPlayer) run() {
	for range s.wake {
		for {
			s.mu.Lock()
			if len(s.clips) == 0 {
				was := s.sounding
				s.sounding = false
				s.mu.Unlock()
				if was {
					s.report(false)
				}
				break
			}
			clip, cuts := s.clips[0], s.cuts
			s.clips = s.clips[1:]
			was := s.sounding
			s.sounding = true
			s.mu.Unlock()
			if !was {
				s.report(true)
			}
			s.mu.Lock()
			current := cuts == s.cuts
			s.mu.Unlock()
			if current {
				SpeechOutput.Play(clip)
			}
			_ = os.Remove(clip)
		}
	}
}

// Sends the sounding state to the gateway off the player goroutine
func (s *speechPlayer) report(playing bool) {
	s.mu.Lock()
	gw := s.gw
	s.mu.Unlock()
	if gw == nil {
		return
	}
	go func() {
		_, _ = gw.request(context.Background(), tuigateway.VoicePlayback.Name(), wireParams(tuigateway.VoicePlaybackParams{Playing: playing}), 5*time.Second)
	}()
}

// Whether a clip is sounding
func (s *speechPlayer) isSounding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sounding
}

// Drops the queued clips and silences the one sounding, a cut reply latched for the next message
func (s *speechPlayer) cut() {
	s.mu.Lock()
	clips := s.clips
	s.clips = nil
	s.cuts++
	if s.sounding || len(clips) > 0 {
		s.interrupted = true
	}
	s.mu.Unlock()
	for _, clip := range clips {
		_ = os.Remove(clip)
	}
	if SpeechOutput.Stop != nil {
		SpeechOutput.Stop()
	}
}

// Latches a spoken interjection that cut the reply
func (s *speechPlayer) latch() {
	s.mu.Lock()
	s.interrupted = true
	s.mu.Unlock()
}

// Cuts speech still playing for a new message and pops the latch, true when the message cuts a reply off
func (s *speechPlayer) takeInterrupted() bool {
	s.mu.Lock()
	live := s.sounding || len(s.clips) > 0
	s.mu.Unlock()
	if live {
		s.cut()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	interrupted := s.interrupted
	s.interrupted = false
	return interrupted
}
