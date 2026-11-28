// Speech: the voices that read text aloud, the recognizers that turn speech into text and the engines that hear a wake word, each a
// plugin, with the pipelines every surface shares around them
package speech

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/provider"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	speechpackages "github.com/nickheyer/icebath/internal/speech/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
)

// What a speech plugin adds: voices and recognizers, those of the profile's settings among them, and wake engines
type Registrant interface {
	Voices(cfg *config.Config) []Voice
	Recognizers(cfg *config.Config) []Recognizer
	Engines() []Engine
}

// Services fixed once they are registered, whatever the profile
type Services struct {
	Speakers    []Voice
	Recognizing []Recognizer
	Waking      []Engine
}

func (s Services) Voices(*config.Config) []Voice           { return s.Speakers }
func (s Services) Recognizers(*config.Config) []Recognizer { return s.Recognizing }
func (s Services) Engines() []Engine                       { return s.Waking }

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Speech
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Voices(cfg *config.Config) []Voice {
	var out []Voice
	for _, registrant := range r.registrants {
		out = append(out, registrant.Voices(cfg)...)
	}
	return out
}

func (r *Registry) Recognizers(cfg *config.Config) []Recognizer {
	var out []Recognizer
	for _, registrant := range r.registrants {
		out = append(out, registrant.Recognizers(cfg)...)
	}
	return out
}

func (r *Registry) Engines() []Engine {
	var out []Engine
	for _, registrant := range r.registrants {
		out = append(out, registrant.Engines()...)
	}
	return out
}

// Host the registry is nested in, through which surfaces speak, transcribe and listen
func (r *Registry) Host() *Speech {
	return r.host
}

// Lower case name with its spaces trimmed, as the settings name services
func Normal(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Voice of the name for the profile, the first registered of a name winning so a program of yours never shadows a built in one
func (r *Registry) Voice(cfg *config.Config, name string) (Voice, bool) {
	name = Normal(name)
	for _, voice := range r.Voices(cfg) {
		if voice.Name == name {
			return voice, true
		}
	}
	return Voice{}, false
}

func (r *Registry) Recognizer(cfg *config.Config, name string) (Recognizer, bool) {
	name = Normal(name)
	for _, recognizer := range r.Recognizers(cfg) {
		if recognizer.Name == name {
			return recognizer, true
		}
	}
	return Recognizer{}, false
}

// Wake engine a name or one of its aliases picks
func (r *Registry) Engine(name string) (Engine, bool) {
	name = Normal(name)
	for _, engine := range r.Engines() {
		if engine.Name == name || slices.Contains(engine.Aliases, name) {
			return engine, true
		}
	}
	return Engine{}, false
}

// Names of the chat apps speech knows the upload limits of, with what a person calls each, and the entry that covers every other app
func chatApps() []config.Option {
	options := []config.Option{{Value: "default", Title: "Every other app", About: "The chat apps not named on their own"}}
	for _, name := range slices.Sorted(maps.Keys(platformLimits)) {
		if name != "default" {
			options = append(options, config.Option{Value: name, Title: appTitles[name]})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(bubblePlatforms)) {
		if _, limited := platformLimits[name]; !limited {
			options = append(options, config.Option{Value: name, Title: appTitles[name]})
		}
	}
	return options
}

// Speech of every profile: the services the plugins registered, and what they run on
type Speech struct {
	Registry
	config      *config.Config
	packages    *packages.Packages
	terminal    *terminal.Terminal
	providers   *provider.Registry
	credentials *credentials.Registry
	transports  *transport.Registry
	tasks       *auxiliary.Registry
	learned     *auxiliary.State
	// Fires pre_transcription before each recording is transcribed
	hooks *hooks.Hooks
	// Tools whose guards refuse the files speech is asked to write or read, such as credential stores
	guard  *tools.Registry
	leases leases
}

func New(cfg *config.Config, pk *packages.Packages, tm *terminal.Terminal, p *provider.Registry, cr *credentials.Registry, t *transport.Registry,
	ax *auxiliary.Auxiliary, hk *hooks.Hooks, tl *tools.Registry) *Speech {
	s := &Speech{config: cfg, packages: pk, terminal: tm, providers: p, credentials: cr, transports: t, tasks: &ax.Registry, learned: auxiliary.NewState(),
		hooks: hk, guard: tl}
	s.Registry.host = s
	speechconfig.Register(cfg, s.voiceOptions, s.streamerOptions, s.recognizerOptions, s.engineOptions, chatApps)
	// No write keeps a program of yours that can't run, whichever way it comes
	for _, programs := range []Programs{s.SpeechPrograms(), s.RecognitionPrograms()} {
		cfg.Guard(programs.Table, programs.Guard())
	}
	speechpackages.Register(pk, cfg)
	ax.Register(tasks{})
	return s
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (s *Speech) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = s
	}
	s.Registry.Register(registrant)
}

func (s *Speech) voiceOptions() []config.Option {
	var options []config.Option
	for _, voice := range s.Voices(s.config) {
		options = append(options, config.Option{Value: voice.Name, Title: voice.Title, About: voice.About})
	}
	return options
}

// Voices that speak while a reply is still being written, in the order Any that can tries them
func (s *Speech) streamerOptions() []config.Option {
	voices := slices.DeleteFunc(s.Voices(s.config), func(v Voice) bool { return v.Stream == nil })
	slices.SortStableFunc(voices, func(a, b Voice) int { return a.Stream.Rank - b.Stream.Rank })
	var options []config.Option
	for _, voice := range voices {
		options = append(options, config.Option{Value: voice.Name, Title: voice.Title, About: voice.About})
	}
	return options
}

// Automatic, then the recognizers in the order of their rank, those of no rank after in the order registered
func (s *Speech) recognizerOptions() []config.Option {
	options := []config.Option{{Value: "", Title: "Automatic", About: "The first service that is ready, those on this computer first"}}
	recognizers := s.Recognizers(s.config)
	slices.SortStableFunc(recognizers, func(a, b Recognizer) int {
		switch {
		case a.Rank == 0 || b.Rank == 0:
			return cmp.Compare(min(1, b.Rank), min(1, a.Rank))
		}
		return a.Rank - b.Rank
	})
	for _, recognizer := range recognizers {
		options = append(options, config.Option{Value: recognizer.Name, Title: recognizer.Title, About: recognizer.About})
	}
	return options
}

func (s *Speech) engineOptions() []config.Option {
	options := []config.Option{{Value: speechconfig.AutoWake, Title: "Automatic", About: "Sherpa, or openWakeWord once you give it a model file"}}
	for _, engine := range s.Engines() {
		options = append(options, config.Option{Value: engine.Name, Title: engine.Title, About: engine.About})
		for _, alias := range engine.Aliases {
			options = append(options, config.Option{Value: alias, Title: engine.Title, About: "Another name for " + engine.Title})
		}
	}
	return options
}

// Package host of this machine, which installs and finds the programs, libraries and Python sets speech runs on
func (s *Speech) Packages() *packages.Packages { return s.packages }

// Folder of the settings every profile shares, which this machine's speech caches live under
func (s *Speech) Dir() string { return s.config.Dir() }

// Settings of the profile with the name, the one in use for none
func (s *Speech) Profile(name string) (*config.Config, error) {
	return s.config.Of(name)
}

// Helper models of a profile routed by its own settings, for a job no session runs
func (s *Speech) helpers(cfg *config.Config) *auxiliary.Client {
	return &auxiliary.Client{Tasks: s.tasks, Config: cfg, Providers: s.providers, Transports: s.transports, Credentials: s.credentials, State: s.learned}
}

// Job of a profile outside a session: its own helper models, no surface of a chat app, the paths it touches guarded as the tools guard them
func (s *Speech) Job(cfg *config.Config) *Job {
	return &Job{Speech: s, Config: cfg, Aux: s.helpers(cfg),
		Unwritable: func(path string) string { return s.guard.Unwritable(cfg, path) },
		Unreadable: func(path string) string { return s.guard.Unreadable(cfg, path) }}
}

// What one piece of speech work is done for: the profile, its helper models and the surface the result goes to
type Job struct {
	Speech *Speech
	Config *config.Config
	// Helper models, for the tags some voices add to text; those of the profile when the job runs in no session
	Aux *auxiliary.Client
	// Surface the speech is delivered on, such as telegram, which takes Ogg Opus voice bubbles; empty for this machine
	Platform string
	// Why a file the job is asked to write, or to read, may not be touched, such as a credential store; nil where nothing guards paths
	Unwritable, Unreadable func(path string) string
}

// Whether a setting has a value of its own, from the file or the environment, rather than its default: what alpha's unset settings are
func (j *Job) Set(key string) bool {
	_, setting, ok := j.Config.Setting(key)
	if !ok {
		return j.Config.Stored(key) != nil
	}
	source, _ := j.Config.Source(setting)
	return source != config.FromDefault
}

// Value of a setting when it has one of its own, else the fallback
func (j *Job) Or(key string, fallback string) string {
	if !j.Set(key) {
		return fallback
	}
	return cmp.Or(strings.TrimSpace(j.Config.GetString(key)), fallback)
}

// Text setting of a service with its spaces trimmed, the fallback when it is empty
func (j *Job) Text(key, fallback string) string {
	return cmp.Or(strings.TrimSpace(j.Config.GetString(key)), fallback)
}

// Number setting as a float, the integers YAML gives included
func (j *Job) Float(key string) float64 {
	return Number(j.Config.Get(key))
}

// Seconds a setting kept as a plain number holds: its number counts the unit, text such as 500ms or 2m is read as a length. A setting
// declared a length of time is read with Config.GetDuration, by its own unit
func (j *Job) Seconds(key string, unit time.Duration) float64 {
	return Seconds(j.Config.Get(key), unit)
}

// Seconds a length of time holds: a number counts the unit, text such as 500ms or 2m is read as a length
func Seconds(value any, unit time.Duration) float64 {
	length, _ := config.Length(value, unit)
	return length.Seconds()
}

// Variable of the profile's environment: its own, its .env, its secret stores, then this process's
func (j *Job) Getenv(name string) string {
	return strings.TrimSpace(j.Config.Getenv(name))
}

// Job of the profile this process runs in, for what a client does on its own machine such as running a player
func (s *Speech) Local() *Job {
	return s.Job(s.config)
}
