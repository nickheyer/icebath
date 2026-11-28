package speech

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

// Sample rate every wake engine hears, 16 kHz mono 16 bit
const WakeRate = 16000

// An engine that hears a wake word, what a speech plugin registers; it runs where the microphone is
type Engine struct {
	// As wake_word.provider names it, and the other names that pick it
	Name    string
	Aliases []string
	Title   string
	About   string
	// Place in the automatic choice when a model file is set, lower first
	Rank int
	// Detects any phrase, so it is the automatic choice when no model file is set
	Open bool
	// Python set whose native library it runs, empty for one that runs on icebath's own speech engine
	Extra string
	// Whether this machine has a build of it at all
	Supported func(s *Speech) bool
	// Whether what it runs is installed now with its settings by key, never installing anything
	Installed func(s *Speech, settings map[string]string) bool
	// Downloads what it runs onto this computer, saying each step, for an engine whose install is a download rather than a Python set;
	// what it would otherwise fetch the first time it listens
	Fetch func(ctx context.Context, wake *Wake, say func(step string)) error
	// Setting of the model file it needs set, empty for an engine that needs none
	Model string
	// Needs the Picovoice access key of the profile's environment
	Key bool
	// Builds the detector of the wake settings
	New func(ctx context.Context, wake *Wake) (Hotword, error)
}

// Wake settings of a profile as the listener arms with them, and the speech of the machine it listens on
type Wake struct {
	Speech *Speech
	Phrase string
	// 0 to 1, higher catching more
	Sensitivity float64
	// Over threshold frames in a row that fire, 1 to 10
	Frames int
	// The phrases other wake enabled profiles chose are heard too
	Routing bool
	// Profile of the settings, and the phrase of every wake enabled profile
	Profile  string
	Profiles []Phrase
	// Engine settings by key, such as its model file
	Settings map[string]string
	// PORCUPINE_ACCESS_KEY of the profile's environment
	AccessKey string
}

// Phrase a wake enabled profile answers to
type Phrase struct {
	Profile string
	Phrase  string
}

// Setting of the engine, its spaces trimmed
func (w *Wake) Setting(key string) string {
	return strings.TrimSpace(w.Settings[key])
}

// Hotword detector over 16 kHz frames
type Hotword interface {
	FrameLength() int
	// Whether the frame completes the wake word
	Process(frame []int16) (bool, error)
	// Forgets what it heard, so audio from before a pause cannot fire
	Reset() error
	Close()
}

// Detector that tells which phrase, and so which profile, its last fire heard
type Matcher interface {
	LastMatch() (phrase, profile string, ok bool)
}

// The wake phrase in effect for the profile, the one every page, message and listener says: its own, else hey and the profile's name while
// each profile has its own phrase, the profile icebath starts with keeping hey icebath, else hey icebath
func WakePhrase(cfg *config.Config) string {
	if own := strings.TrimSpace(cfg.GetString(speechconfig.WakePhrase)); own != "" {
		return own
	}
	if name := cfg.Profile(); cfg.GetBool(speechconfig.WakeProfileRouting) && name != "" && name != profile.Default {
		return "hey " + name
	}
	return speechconfig.DefaultPhrase
}

// Engine a wake_word.provider setting resolves to, auto taking the open vocabulary engine this machine has a build of unless a model file is
// set, else the first by rank this machine has a build of, else the first by rank; false for a name no engine has. The engines are in the
// fixed order their plugins register them, so the choice is the same on every run
func (r *Registry) WakeEngine(provider string, model bool) (Engine, bool) {
	if provider != "" && provider != "auto" {
		return r.Engine(provider)
	}
	engines := r.Engines()
	builds := func(e Engine) bool { return e.Supported == nil || e.Supported(r.host) }
	if !model {
		for _, engine := range engines {
			if engine.Open && builds(engine) {
				return engine, true
			}
		}
	}
	slices.SortStableFunc(engines, func(a, b Engine) int { return a.Rank - b.Rank })
	for _, engine := range engines {
		if builds(engine) {
			return engine, true
		}
	}
	if len(engines) == 0 {
		return Engine{}, false
	}
	return engines[0], true
}
