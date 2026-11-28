// Connect service of speech, through which voice input and the pages of Voice reach the voices and recognizers of the server's profiles
package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cast"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/voice"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
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
		Input: cfg.GetBool(speechconfig.Input), Speak: cfg.GetBool(speechconfig.SpeakReplies), SubmitMode: cfg.GetString(speechconfig.SubmitMode), BargeIn: cfg.GetBool(speechconfig.BargeIn), BargeInGrace: job.Config.GetDuration(speechconfig.BargeInGrace).Seconds(),
		BargeInMultiplier: job.Float(speechconfig.BargeInMultiplier), Beep: cfg.GetBool(speechconfig.Beep), BeepVolume: volume,
		Debug: cfg.GetBool(speechconfig.Debug) || truthy(cfg.Getenv(speechconfig.DebugEnv)), MaxRecording: job.Config.GetDuration(speechconfig.MaxRecording).Seconds(),
		SilenceDuration: job.Float(speechconfig.SilenceDuration), SilenceThreshold: int32(cfg.GetInt(speechconfig.SilenceThreshold)),
		StopPhrases: speech.StopPhrases(cfg), ThinkingSound: cfg.GetBool(speechconfig.ThinkingSound),
		EchoTranscripts: cfg.GetBool(speechconfig.EchoTranscripts),
	}
	return &speechv1.VoiceReply{Profile: cfg.Profile(), Mode: mode, Recognition: recognition(ctx, job), Speaking: speaking(job), Wake: s.wake(job)}, nil
}

// How the profile's speech recognition stands, from the one answer of the speech host
func recognition(ctx context.Context, job *speech.Job) *speechv1.Recognition {
	cfg := job.Config
	r := job.Recognition(ctx)
	out := &speechv1.Recognition{Enabled: cfg.GetBool(speechconfig.STTEnabled), Provider: r.Name, Label: r.Label(), Chosen: r.Chosen,
		Title: r.Recognizer.Title, State: r.Standing.State, Hindrance: hindrance(r.Hindrance)}
	out.Live = r.Hindrance == nil && cfg.GetBool(speechconfig.Streaming) && !r.Recognizer.Local && r.Recognizer.Live != nil
	return out
}

// How the profile's voice stands, from the one answer of the speech host
func speaking(job *speech.Job) *speechv1.Speaking {
	s := job.Speaking()
	return &speechv1.Speaking{Voice: s.Name, Title: cmp.Or(s.Voice.Title, s.Name), Ready: s.Hindrance == nil, Streams: job.Streams(),
		Least: int32(max(1, job.Config.GetInt(speechconfig.StreamingMinLen))), State: s.Standing.State, Hindrance: hindrance(s.Hindrance)}
}

// What keeps a part of voice from working, nil while nothing does
func hindrance(h *speech.Hindrance) *speechv1.Hindrance {
	if h == nil {
		return nil
	}
	return &speechv1.Hindrance{Cause: h.Cause, Lack: speechv1.Lack(h.Lack), Service: h.Service, Install: h.Install}
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
		Provider: speech.Normal(cfg.GetString(speechconfig.WakeProvider)), Phrase: speech.WakePhrase(cfg),
		Sensitivity: job.Float(speechconfig.WakeSensitivity), Frames: int32(min(10, max(1, cfg.GetInt(speechconfig.WakeConfirmationFrames)))),
		NewSession: cfg.GetBool(speechconfig.WakeNewSession), Routing: cfg.GetBool(speechconfig.WakeProfileRouting), Settings: settings,
		AccessKey: job.Getenv(speechconfig.AccessKeyEnv),
	}
	if wake.Provider == "" {
		wake.Provider = speechconfig.AutoWake
	}
	wake.Sensitivity = min(1, max(0, wake.Sensitivity))
	if wake.Routing {
		wake.Profiles = s.enrolled()
	}
	return wake
}

// Phrase of every wake enabled profile, the one in effect for each
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
		out = append(out, &speechv1.Phrase{Profile: name, Phrase: speech.WakePhrase(cfg)})
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

// Settings of a service picked from lists, with their values now; the lists themselves read when one is opened
func choices(job *speech.Job, given []speech.Choices) []*speechv1.Choice {
	var out []*speechv1.Choice
	for _, c := range given {
		choice := &speechv1.Choice{Setting: c.Setting, Value: strings.TrimSpace(job.Config.GetString(c.Setting))}
		if _, setting, ok := job.Config.Setting(c.Setting); ok {
			choice.Title, choice.About = setting.Title, setting.About
		}
		out = append(out, choice)
	}
	return out
}

func setup(job *speech.Job, given speech.Setup) *speechv1.Setup {
	out := &speechv1.Setup{Keys: given.Keys, SignIn: given.SignIn, Extra: given.Extra, Programs: given.Programs, Link: given.Link, Badge: given.Badge}
	if given.SignIn != "" {
		out.SignInProvider = job.Provider(given.SignIn)
	}
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
	for _, program := range speech.AbsentPrograms(given.Programs) {
		out.Absent = append(out.Absent, &speechv1.Program{Name: program.Name, Install: program.Install})
	}
	return out
}

// A service as a row of its page shows it: its state from the one answer of the speech host, In use for the one in use that runs
func offered(name, title, about string, local bool, standing speech.Standing, inUse bool) *speechv1.Service {
	state := standing.State
	if inUse && standing.Hindrance == nil {
		state = speech.InUse
	}
	return &speechv1.Service{Name: name, Title: title, About: about, Local: local, Ready: standing.Hindrance == nil, Missing: standing.Missing,
		State: state, Hindrance: hindrance(standing.Hindrance), InUse: inUse}
}

func (s *service) Services(ctx context.Context, request *speechv1.ServicesRequest) (*speechv1.ServicesReply, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	cfg := job.Config
	// Each check may look for a program, a model or a key, bounded together
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	speaking, recognizing := job.Speaking(), job.Recognition(ctx)
	reply := &speechv1.ServicesReply{Voice: speaking.Name, Recognizer: recognizing.Chosen}
	if automatic, _, ok := job.Automatic(ctx); ok {
		reply.Automatic = automatic.Name
	}
	seen := map[string]bool{}
	for _, voice := range s.speech.Voices(cfg) {
		if seen[voice.Name] {
			continue
		}
		seen[voice.Name] = true
		row := offered(voice.Name, voice.Title, voice.About, voice.Local, job.VoiceStanding(voice), voice.Name == speaking.Name)
		s.detail(job, row, voice.Setup, voice.Choices, speechconfig.TTS, voice.Program)
		reply.Voices = append(reply.Voices, row)
	}
	clear(seen)
	// Recognizers by rank, the order Automatic tries them in, those of no rank after
	recognizers := s.speech.Recognizers(cfg)
	slices.SortStableFunc(recognizers, func(a, b speech.Recognizer) int { return rank(a.Rank) - rank(b.Rank) })
	for _, recognizer := range recognizers {
		if seen[recognizer.Name] {
			continue
		}
		seen[recognizer.Name] = true
		row := offered(recognizer.Name, recognizer.Title, recognizer.About, recognizer.Local || recognizer.Program, job.RecognizerStanding(ctx, recognizer),
			recognizer.Name == recognizing.Name && recognizing.Hindrance == nil)
		s.detail(job, row, recognizer.Setup, recognizer.Choices, speechconfig.STT, recognizer.Program)
		reply.Recognizers = append(reply.Recognizers, row)
	}
	slices.SortStableFunc(reply.Voices, func(a, b *speechv1.Service) int { return boolOrder(a.Local, b.Local) })
	slices.SortStableFunc(reply.Recognizers, func(a, b *speechv1.Service) int { return boolOrder(a.Local, b.Local) })
	return reply, nil
}

// What the page of a service shows besides its state: its setup, its settings picked from lists, its own section of settings, and for a
// program of yours the fields of its entry with their values
func (s *service) detail(job *speech.Job, row *speechv1.Service, given speech.Setup, picks []speech.Choices, section string, program bool) {
	cfg := job.Config
	row.Setup, row.Choices, row.Program = setup(job, given), choices(job, picks), program
	if _, ok := cfg.Section(speech.Key(section, row.Name)); ok {
		row.Section = speech.Key(section, row.Name)
	}
	if !program {
		return
	}
	programs := s.programs(section)
	_, table, ok := cfg.Setting(programs.Table)
	if !ok || table.Entry == nil {
		return
	}
	entry := cast.ToStringMap(cfg.Stored(programs.Key(cfg, row.Name)))
	for _, field := range table.Entry.Fields {
		held := entry[field.Key]
		if held == nil {
			held = field.DefaultValue()
		}
		described := &configv1.Setting{Key: field.Key, Title: field.Title, Description: field.About, Help: field.Help, Kind: kinds[field.Kind()],
			Zero: field.Zero, Counts: field.Counts, Bounds: field.Bounds(), Need: field.Need, Shown: field.Say(held)}
		if field.Options != nil {
			for _, option := range field.Options() {
				described.Options = append(described.Options, &configv1.Option{Value: option.Value, Title: option.Title, Description: option.About})
			}
		}
		if value, err := yaml.Marshal(entry[field.Key]); err == nil && entry[field.Key] != nil {
			described.Value = strings.TrimSpace(string(value))
		}
		if field.Least != nil {
			described.Least = strconv.FormatFloat(*field.Least, 'f', -1, 64)
		}
		if field.Most != nil {
			described.Most = strconv.FormatFloat(*field.Most, 'f', -1, 64)
		}
		if field.Kind() == config.Duration {
			described.Unit = field.UnitName()
		}
		if field.Entry != nil {
			described.Entry = &configv1.Entry{Noun: field.Entry.Noun}
		}
		row.Fields = append(row.Fields, described)
	}
}

// Kinds of a setting as the settings service names them
var kinds = map[config.Kind]configv1.Kind{config.Toggle: configv1.Kind_KIND_TOGGLE, config.Number: configv1.Kind_KIND_NUMBER, config.List: configv1.Kind_KIND_LIST,
	config.Table: configv1.Kind_KIND_TABLE, config.Duration: configv1.Kind_KIND_DURATION, config.Choice: configv1.Kind_KIND_CHOICE, config.Text: configv1.Kind_KIND_TEXT}

// Programs of yours of the section's kind
func (s *service) programs(section string) speech.Programs {
	if section == speechconfig.STT {
		return s.speech.RecognitionPrograms()
	}
	return s.speech.SpeechPrograms()
}

// Programs of yours of the kind asked for
func (s *service) kind(kind speechv1.Kind) (speech.Programs, error) {
	switch kind {
	case speechv1.Kind_KIND_VOICE:
		return s.speech.SpeechPrograms(), nil
	case speechv1.Kind_KIND_RECOGNIZER:
		return s.speech.RecognitionPrograms(), nil
	}
	return speech.Programs{}, connect.NewError(connect.CodeInvalidArgument, "Say whether the program reads text aloud or turns speech into text.")
}

// Refusal of a change to programs of yours, the reason a person reads kept whole
func refused(err error) error {
	if err == nil {
		return nil
	}
	if locked, ok := errors.AsType[*config.LockedError](err); ok {
		return connect.NewError(connect.CodePermissionDenied, locked.Error())
	}
	return connect.NewError(connect.CodeInvalidArgument, err.Error())
}

func (s *service) AddProgram(_ context.Context, request *speechv1.AddProgramRequest) (*speechv1.ProgramReply, error) {
	programs, err := s.kind(request.GetKind())
	if err != nil {
		return nil, err
	}
	cfg, err := s.config.Of(request.GetProfile())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	name := speech.Normal(request.GetName())
	if err := programs.Add(cfg, name, request.GetCommand()); err != nil {
		return nil, refused(err)
	}
	return &speechv1.ProgramReply{Name: name}, nil
}

func (s *service) ChangeProgram(_ context.Context, request *speechv1.ChangeProgramRequest) (*speechv1.ProgramReply, error) {
	programs, err := s.kind(request.GetKind())
	if err != nil {
		return nil, err
	}
	cfg, err := s.config.Of(request.GetProfile())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	name := speech.Normal(request.GetName())
	if to := strings.TrimSpace(request.GetRename()); to != "" {
		chosen, err := programs.Rename(cfg, name, to)
		if err != nil {
			return nil, refused(err)
		}
		return &speechv1.ProgramReply{Name: speech.Normal(to), Chosen: chosen}, nil
	}
	var value any
	if err := yaml.Unmarshal([]byte(request.GetValue()), &value); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := programs.Change(cfg, name, strings.ToLower(strings.TrimSpace(request.GetField())), value); err != nil {
		return nil, refused(err)
	}
	return &speechv1.ProgramReply{Name: name, Chosen: speech.Chosen(cfg, programs.Choice) == name}, nil
}

func (s *service) RemoveProgram(_ context.Context, request *speechv1.RemoveProgramRequest) (*speechv1.ProgramReply, error) {
	programs, err := s.kind(request.GetKind())
	if err != nil {
		return nil, err
	}
	cfg, err := s.config.Of(request.GetProfile())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	name := speech.Normal(request.GetName())
	chosen, err := programs.Remove(cfg, name)
	if err != nil {
		return nil, refused(err)
	}
	return &speechv1.ProgramReply{Name: name, Chosen: chosen}, nil
}

// Microphones of the computer the server runs on, by the names the Microphone setting takes: what icebath voice microphones lists, for a
// person who sets the microphone from the command line. The app lists those of its own computer itself, since its wake word listens there
func (s *service) Microphones(context.Context, *speechv1.MicrophonesRequest) (*speechv1.MicrophonesReply, error) {
	found, err := voice.Microphones()
	if err != nil {
		return &speechv1.MicrophonesReply{Error: err.Error()}, nil
	}
	reply := &speechv1.MicrophonesReply{}
	for _, m := range found {
		reply.Microphones = append(reply.Microphones, &speechv1.Microphone{Name: m.Name, Host: m.Host, Default: m.Default, Value: m.Value})
	}
	return reply, nil
}

// Place of a rank among others, no rank last
func rank(n int) int {
	if n == 0 {
		return 1 << 20
	}
	return n
}

func (s *service) Values(ctx context.Context, request *speechv1.ValuesRequest) (*speechv1.ValuesReply, error) {
	job, err := s.job(request.GetProfile())
	if err != nil {
		return nil, err
	}
	var given []speech.Choices
	switch request.GetKind() {
	case speechv1.Kind_KIND_VOICE:
		if voice, ok := s.speech.Voice(job.Config, request.GetService()); ok {
			given = voice.Choices
		}
	case speechv1.Kind_KIND_RECOGNIZER:
		if recognizer, ok := s.speech.Recognizer(job.Config, request.GetService()); ok {
			given = recognizer.Choices
		}
	}
	for _, c := range given {
		if c.Setting != request.GetSetting() {
			continue
		}
		// A list may ask a service or read a catalog
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		picks, err := c.List(bounded, job)
		reply := &speechv1.ValuesReply{}
		if err != nil {
			reply.Error = err.Error()
		}
		for _, pick := range picks {
			reply.Values = append(reply.Values, &speechv1.Value{Value: pick.Value, Title: pick.Title, About: pick.About})
		}
		return reply, nil
	}
	return nil, connect.NewError(connect.CodeNotFound, request.GetService()+" picks no "+request.GetSetting()+" from a list.")
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
