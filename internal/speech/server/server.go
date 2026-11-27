// Connect service of speech, through which voice mode and the voice setup reach the voices and recognizers of the server's profiles
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
)

// Largest recording a client uploads for one transcription
const recordingCap = 200 << 20

// Extension a recording may carry
var formats = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

type service struct {
	config   *config.Config
	speech   *speech.Speech
	profiles *profile.Profiles

	mu sync.Mutex
	// Live transcriptions by id
	lives map[string]*live
}

// One live transcription: the recognizer's session, the rate fed to it and where its events go
type live struct {
	session  speech.Live
	resample *speech.Resampler
	partials chan string
	final    chan speech.Transcript
	ended    sync.Once
}

func Register(srv *server.Server, cfg *config.Config, sp *speech.Speech, pr *profile.Profiles) {
	srv.Register(&service{config: cfg, speech: sp, profiles: pr, lives: map[string]*live{}})
}

func (s *service) Mount(services *connect.Server) {
	speechv1connect.RegisterSpeechServiceHandler(services, s)
}

func (s *service) job(name string) (*speech.Job, error) {
	cfg, err := s.config.Of(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return s.speech.Job(cfg), nil
}

func (s *service) Voice(ctx context.Context, request *speechv1.VoiceRequest) (*speechv1.VoiceReply, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	cfg := job.Config
	// A beep volume outside 0 to 1 gives way to the default
	volume := job.Float(speechconfig.BeepVolume)
	if volume < 0 || volume > 1 {
		volume = speechconfig.DefaultBeepVolume
	}
	mode := &speechv1.Mode{
		Input: cfg.GetBool(speechconfig.Input), Speak: cfg.GetBool(speechconfig.SpeakReplies), RecordKey: cfg.GetString(speechconfig.RecordKey),
		SubmitMode: cfg.GetString(speechconfig.SubmitMode), BargeIn: cfg.GetBool(speechconfig.BargeIn), BargeInGrace: job.Config.GetDuration(speechconfig.BargeInGrace).Seconds(),
		BargeInMultiplier: job.Float(speechconfig.BargeInMultiplier), Beep: cfg.GetBool(speechconfig.Beep), BeepVolume: volume,
		Debug: cfg.GetBool(speechconfig.Debug) || truthy(cfg.Getenv(speechconfig.DebugEnv)), MaxRecording: job.Config.GetDuration(speechconfig.MaxRecording).Seconds(),
		SilenceDuration: job.Float(speechconfig.SilenceDuration), SilenceThreshold: int32(cfg.GetInt(speechconfig.SilenceThreshold)),
		StopPhrases: speech.StopPhrases(cfg), ThinkingSound: cfg.GetBool(speechconfig.ThinkingSound),
		EchoTranscripts: cfg.GetBool(speechconfig.EchoTranscripts),
	}
	name := job.Recognizing(ctx)
	recognition := &speechv1.Recognition{Enabled: cfg.GetBool(speechconfig.STTEnabled), Provider: name, Label: job.RecognizerLabel(ctx)}
	if recognizer, ok := s.speech.Recognizer(cfg, name); ok && name != speech.None {
		recognition.Live = cfg.GetBool(speechconfig.Streaming) && !recognizer.Local && recognizer.Live != nil
	}
	speaker := job.Speaker()
	speaking := &speechv1.Speaking{Voice: speaker.Name, Title: speaker.Title, Ready: job.CanSpeak(), Streams: job.Streams(),
		Least: int32(max(1, cfg.GetInt(speechconfig.StreamingMinLen)))}
	return &speechv1.VoiceReply{Profile: cfg.Profile(), Mode: mode, Recognition: recognition, Speaking: speaking, Wake: s.wake(job)}, nil
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Wake settings of the profile, with the phrase of every wake enabled profile when routing is on
func (s *service) wake(job *speech.Job) *speechv1.Wake {
	cfg := job.Config
	settings := map[string]string{}
	for _, section := range cfg.Sections() {
		if section.Key != speechconfig.Wake && !strings.HasPrefix(section.Key, speechconfig.Wake+".") {
			continue
		}
		for _, setting := range section.Settings {
			key := setting.Key
			settings[key] = strings.TrimSpace(fmt.Sprint(cfg.Get(key)))
		}
	}
	wake := &speechv1.Wake{
		Enabled: cfg.GetBool(speechconfig.WakeEnabled), Surface: cfg.GetString(speechconfig.WakeSurface), InputDevice: cfg.GetString(speechconfig.WakeInput),
		Provider: speech.Normal(cfg.GetString(speechconfig.WakeProvider)), Phrase: strings.TrimSpace(cfg.GetString(speechconfig.WakePhrase)),
		Sensitivity: job.Float(speechconfig.WakeSensitivity), Frames: int32(min(10, max(1, cfg.GetInt(speechconfig.WakeConfirmationFrames)))),
		NewSession: cfg.GetBool(speechconfig.WakeNewSession), Routing: cfg.GetBool(speechconfig.WakeProfileRouting), Settings: settings,
		AccessKey: job.Getenv("PORCUPINE_ACCESS_KEY"),
	}
	if wake.Provider == "" {
		wake.Provider = speechconfig.AutoWake
	}
	if wake.Phrase == "" {
		wake.Phrase = speechconfig.DefaultPhrase
	}
	wake.Sensitivity = min(1, max(0, wake.Sensitivity))
	if wake.Routing {
		wake.Profiles = s.enrolled()
	}
	return wake
}

// Phrase of every wake enabled profile: its own wake_word.phrase, else hey and its name
func (s *service) enrolled() []*speechv1.Phrase {
	names, err := s.profiles.List()
	if err != nil {
		return nil
	}
	var out []*speechv1.Phrase
	for _, name := range names {
		cfg, err := s.config.Of(name)
		if err != nil || !cfg.GetBool(speechconfig.WakeEnabled) {
			continue
		}
		phrase := "hey " + name
		if s.speech.Job(cfg).Set(speechconfig.WakePhrase) {
			if own := strings.TrimSpace(cfg.GetString(speechconfig.WakePhrase)); own != "" {
				phrase = own
			}
		}
		out = append(out, &speechv1.Phrase{Profile: name, Phrase: phrase})
	}
	return out
}

func (s *service) Speak(ctx context.Context, request *speechv1.SpeakRequest, out speechv1connect.SpeechServiceSpeakServerStream) error {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return err
	}
	sink := speech.Sink{
		Clip: func(format string, rate int) error {
			return out.Send(&speechv1.Audio{Part: &speechv1.Audio_Clip{Clip: &speechv1.Clip{Format: format, Rate: int32(rate)}}})
		},
		Data: func(data []byte) error {
			// Messages stay well under the transport's limit
			for len(data) > 0 {
				n := min(len(data), 256<<10)
				if err := out.Send(&speechv1.Audio{Part: &speechv1.Audio_Data{Data: data[:n]}}); err != nil {
					return err
				}
				data = data[n:]
			}
			return nil
		},
	}
	if err := job.Speak(ctx, request.GetText(), request.GetLive(), sink); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return nil
}

func transcript(t speech.Transcript) *speechv1.Transcript {
	return &speechv1.Transcript{Success: t.Success, Text: t.Text, Error: t.Error, Provider: t.Provider, NoSpeech: t.NoSpeech, Filtered: t.Filtered,
		Segments: int32(t.Segments), Kind: t.Kind}
}

func (s *service) Transcribe(ctx context.Context, request *speechv1.TranscribeRequest) (*speechv1.Transcript, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	format := strings.ToLower(strings.TrimPrefix(request.GetFormat(), "."))
	if !formats.MatchString(format) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%q is not an audio file extension.", request.GetFormat()))
	}
	if len(request.GetAudio()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The recording is empty.")
	}
	if len(request.GetAudio()) > recordingCap {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("The recording is larger than %d MB.", recordingCap>>20))
	}
	dir, err := os.MkdirTemp("", "icebath-recording-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "recording."+format)
	if err := os.WriteFile(path, request.GetAudio(), 0o600); err != nil {
		return nil, err
	}
	return transcript(job.Recording(ctx, path)), nil
}

func newID() string {
	raw := make([]byte, 12)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (s *service) Listen(ctx context.Context, request *speechv1.ListenRequest, out speechv1connect.SpeechServiceListenServerStream) error {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return err
	}
	l := &live{partials: make(chan string, 64), final: make(chan speech.Transcript, 1)}
	session, name, err := job.Listen(ctx, func(text string) {
		select {
		case l.partials <- text:
		default:
		}
	})
	if err != nil || session == nil {
		return out.Send(&speechv1.Heard{Event: &speechv1.Heard_Opened{Opened: &speechv1.Opened{}}})
	}
	l.session = session
	if rate := int(request.GetRate()); rate > 0 && rate != speech.LiveRate {
		l.resample = speech.NewResampler(rate, speech.LiveRate)
	}
	id := newID()
	s.mu.Lock()
	s.lives[id] = l
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.lives, id)
		s.mu.Unlock()
		l.session.Cancel()
	}()
	if err := out.Send(&speechv1.Heard{Event: &speechv1.Heard_Opened{Opened: &speechv1.Opened{Id: id, Provider: name}}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case text := <-l.partials:
			if err := out.Send(&speechv1.Heard{Event: &speechv1.Heard_Partial{Partial: text}}); err != nil {
				return err
			}
		case final := <-l.final:
			return out.Send(&speechv1.Heard{Event: &speechv1.Heard_Final{Final: transcript(job.Settle(final))}})
		}
	}
}

func (s *service) Feed(ctx context.Context, request *speechv1.FeedRequest) (*speechv1.FeedReply, error) {
	s.mu.Lock()
	l, ok := s.lives[request.GetId()]
	s.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "That live transcription is no longer open.")
	}
	switch {
	case request.GetCancel():
		l.ended.Do(func() {
			l.session.Cancel()
			l.final <- speech.Transcript{Error: "cancelled"}
		})
	case len(request.GetPcm()) > 0:
		pcm := request.GetPcm()
		if l.resample != nil {
			pcm = l.resample.Resample(pcm)
		}
		l.session.Push(pcm)
	}
	if request.GetEnd() {
		timeout := request.GetTimeout()
		if timeout <= 0 {
			timeout = speech.FinishTimeout
		}
		l.ended.Do(func() {
			go func() { l.final <- l.session.Finish(timeout) }()
		})
	}
	return &speechv1.FeedReply{}, nil
}

func (s *service) Lease(ctx context.Context, request *speechv1.LeaseRequest) (*speechv1.LeaseReply, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	lease := strings.TrimSpace(request.GetLease())
	if lease == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "A lease needs a name.")
	}
	// Warming loads a local engine, which outlives the call
	bound := context.WithoutCancel(ctx)
	if request.GetActive() {
		go s.speech.Acquire(bound, job, lease)
	} else {
		go s.speech.Drop(bound, job, lease)
	}
	return &speechv1.LeaseReply{}, nil
}

// Settings of a service picked from lists, with their values now
func choices(ctx context.Context, job *speech.Job, given []speech.Choices) []*speechv1.Choice {
	var out []*speechv1.Choice
	for _, c := range given {
		choice := &speechv1.Choice{Setting: c.Setting, Values: c.List(ctx, job), Value: strings.TrimSpace(job.Config.GetString(c.Setting))}
		if _, setting, ok := job.Config.Setting(c.Setting); ok {
			choice.Title, choice.About = setting.Title, setting.About
		}
		out = append(out, choice)
	}
	return out
}

// Other programs that do the work of one a service names
var stands = map[string][]string{"espeak-ng": {"espeak"}}

// Command that installs a program from the system's packages
func systemInstall(program string) string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install " + program
	case "windows":
		return "choco install " + program + " -y"
	}
	return "sudo apt install -y " + program
}

func setup(job *speech.Job, given speech.Setup) *speechv1.Setup {
	out := &speechv1.Setup{Keys: given.Keys, SignIn: given.SignIn, Extra: given.Extra, Programs: given.Programs, Link: given.Link, Badge: given.Badge}
	if given.Extra != "" {
		out.Install = packages.InstallHint(given.Extra)
		out.Installed = job.Provisioned(given.Extra)
		out.Unsupported, _ = job.Speech.Packages().ExtraUnsupported(given.Extra)
	}
	for _, key := range given.Keys {
		if job.Getenv(key) != "" {
			out.Set = append(out.Set, key)
		}
	}
	for _, program := range given.Programs {
		found := false
		for _, name := range append([]string{program}, stands[program]...) {
			if _, err := exec.LookPath(name); err == nil {
				found = true
				break
			}
		}
		if !found {
			out.Absent = append(out.Absent, &speechv1.Program{Name: program, Install: systemInstall(program)})
		}
	}
	return out
}

func (s *service) Services(ctx context.Context, request *speechv1.ServicesRequest) (*speechv1.ServicesReply, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	cfg := job.Config
	reply := &speechv1.ServicesReply{Voice: job.Speaker().Name, Recognizer: speech.Chosen(cfg, speechconfig.STTProvider)}
	// Each recognizer's check may look for a program, a model or a key, and a list may read a catalog, bounded together
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, voice := range s.speech.Voices(cfg) {
		if seen[voice.Name] {
			continue
		}
		seen[voice.Name] = true
		reply.Voices = append(reply.Voices, &speechv1.Service{Name: voice.Name, Title: voice.Title, About: voice.About, Local: voice.Local,
			Ready: job.Ready(voice), Setup: setup(job, voice.Setup), Choices: choices(ctx, job, voice.Choices)})
	}
	clear(seen)
	for _, recognizer := range s.speech.Recognizers(cfg) {
		if seen[recognizer.Name] {
			continue
		}
		seen[recognizer.Name] = true
		missing := job.Missing(ctx, recognizer)
		reply.Recognizers = append(reply.Recognizers, &speechv1.Service{Name: recognizer.Name, Title: recognizer.Title, About: recognizer.About,
			Local: recognizer.Local, Ready: missing == "", Missing: missing, Setup: setup(job, recognizer.Setup),
			Choices: choices(ctx, job, recognizer.Choices)})
	}
	slices.SortStableFunc(reply.Voices, func(a, b *speechv1.Service) int { return boolOrder(a.Local, b.Local) })
	slices.SortStableFunc(reply.Recognizers, func(a, b *speechv1.Service) int { return boolOrder(a.Local, b.Local) })
	return reply, nil
}

// Local services first
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}
