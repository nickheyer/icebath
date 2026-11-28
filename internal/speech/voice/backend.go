package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/speech"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
)

// Server side of voice input for one profile: its settings, its recognizer and its voice
type backend struct {
	client  speechv1connect.SpeechServiceClient
	profile func() string
}

func (b *backend) voice(ctx context.Context) (*speechv1.VoiceReply, error) {
	return b.client.Voice(ctx, &speechv1.VoiceRequest{Profile: b.profile()})
}

// Transcript of a recording on disk
func (b *backend) transcribe(ctx context.Context, path string) (speech.Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return speech.Transcript{}, err
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	reply, err := b.client.Transcribe(ctx, &speechv1.TranscribeRequest{Profile: b.profile(), Audio: data, Format: format})
	if err != nil {
		return speech.Transcript{}, err
	}
	return heard(reply), nil
}

func heard(t *speechv1.Transcript) speech.Transcript {
	return speech.Transcript{Success: t.GetSuccess(), Text: t.GetText(), Error: t.GetError(), Provider: t.GetProvider(), NoSpeech: t.GetNoSpeech(),
		Filtered: t.GetFiltered(), Segments: int(t.GetSegments()), Kind: t.GetKind()}
}

// One clip of speech as the server sends it
type clip struct {
	format string
	rate   int
	data   chan []byte
}

// Longest a live clip's next bytes wait for the player before the rest of the sentence is given up
const chunkWait = 30 * time.Second

var errFull = errors.New("audio chunk queue stayed full for " + chunkWait.String())

// Speech of text in the voice of the profile as the server makes it, each clip handed over as it starts with its bytes following on its
// channel
func (b *backend) speak(ctx context.Context, profile, text string, live bool, each func(c *clip)) error {
	stream, err := b.client.Speak(ctx, &speechv1.SpeakRequest{Profile: profile, Text: text, Live: live})
	if err != nil {
		return err
	}
	defer stream.Close()
	var current *clip
	defer func() {
		if current != nil {
			close(current.data)
		}
	}()
	for {
		part, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch part := part.GetPart().(type) {
		case *speechv1.Audio_Clip:
			if current != nil {
				close(current.data)
			}
			current = &clip{format: part.Clip.GetFormat(), rate: int(part.Clip.GetRate()), data: make(chan []byte, 256)}
			each(current)
		case *speechv1.Audio_Data:
			if current == nil {
				return errors.New("the server sent speech before naming its format")
			}
			var full <-chan time.Time
			if live {
				full = time.After(chunkWait)
			}
			select {
			case current.data <- part.Data:
			case <-full:
				return errFull
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Live transcription a recording streams into as it is made
type liveSession struct {
	backend  *backend
	id       string
	provider string
	cancel   context.CancelFunc
	final    chan speech.Transcript
	mu       sync.Mutex
	// Audio not yet sent, in order, and the nudge that wakes its sender
	queue [][]byte
	wake  chan struct{}
	ended bool
	fed   chan struct{}
}

// Opens a live transcription fed at the rate, nil when the recognizer has none or it could not open
func (b *backend) listen(ctx context.Context, rate int, partial func(string)) (*liveSession, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stream, err := b.client.Listen(ctx, &speechv1.ListenRequest{Profile: b.profile(), Rate: int32(rate)})
	if err != nil {
		cancel()
		return nil, err
	}
	first, err := stream.Receive()
	if err != nil {
		cancel()
		stream.Close()
		return nil, err
	}
	opened := first.GetOpened()
	if opened.GetId() == "" {
		cancel()
		stream.Close()
		return nil, nil
	}
	s := &liveSession{backend: b, id: opened.GetId(), provider: opened.GetProvider(), cancel: cancel, final: make(chan speech.Transcript, 1),
		wake: make(chan struct{}, 1), fed: make(chan struct{})}
	go func() {
		defer stream.Close()
		for {
			event, err := stream.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					s.final <- speech.Transcript{Error: fmt.Sprintf("live transcription failed: %s", err), Provider: s.provider}
				} else {
					s.final <- speech.Transcript{Error: "cancelled", Provider: s.provider}
				}
				return
			}
			switch event := event.GetEvent().(type) {
			case *speechv1.Heard_Partial:
				if partial != nil {
					partial(event.Partial)
				}
			case *speechv1.Heard_Final:
				s.final <- heard(event.Final)
				return
			}
		}
	}()
	// Audio goes out in order on one worker, so a capture callback never waits on the network
	go func() {
		defer close(s.fed)
		for {
			s.mu.Lock()
			batch, ended := s.queue, s.ended
			s.queue = nil
			s.mu.Unlock()
			for _, chunk := range batch {
				if _, err := b.client.Feed(ctx, &speechv1.FeedRequest{Id: s.id, Pcm: chunk}); err != nil &&
					(connect.CodeOf(err) == connect.CodeNotFound || ctx.Err() != nil) {
					return
				}
			}
			if ended && len(batch) == 0 {
				return
			}
			if !ended {
				select {
				case <-s.wake:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return s, nil
}

func (s *liveSession) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Queues audio, dropped once the audio ended
func (s *liveSession) push(pcm []byte) {
	s.mu.Lock()
	if !s.ended {
		s.queue = append(s.queue, append([]byte(nil), pcm...))
	}
	s.mu.Unlock()
	s.nudge()
}

// Ends the audio, the final transcript following
func (s *liveSession) end() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	s.nudge()
	go func() {
		<-s.fed
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.backend.client.Feed(ctx, &speechv1.FeedRequest{Id: s.id, End: true, Timeout: speech.FinishTimeout})
	}()
}

// Final transcript, a failure when it takes longer than the timeout
func (s *liveSession) finish(timeout float64) speech.Transcript {
	s.end()
	select {
	case t := <-s.final:
		s.cancel()
		return t
	case <-time.After(time.Duration(timeout * float64(time.Second))):
		s.drop()
		return speech.Transcript{Error: "live transcription timed out", Provider: s.provider}
	}
}

// Drops the session
func (s *liveSession) drop() {
	s.mu.Lock()
	s.ended, s.queue = true, nil
	s.mu.Unlock()
	s.nudge()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.backend.client.Feed(ctx, &speechv1.FeedRequest{Id: s.id, Cancel: true})
	s.cancel()
}
