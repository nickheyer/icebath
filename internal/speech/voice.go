package speech

import (
	"context"
	"errors"
	"iter"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
)

// A service that speaks text, what a speech plugin registers as a voice
type Voice struct {
	// As tts.provider names it
	Name  string
	Title string
	About string
	// Name the log says it speaks with, such as OpenAI TTS; the title when empty
	Label string
	// Runs on this machine
	Local bool
	// Whether it can speak with the profile's settings now, its key or engine there, never installing anything
	Ready func(job *Job) bool
	// Most characters one request takes, which longer text is split under; tts.<name>.max_text_length wins over it
	Limit func(job *Job) int
	// Writes the speech of the request's text to its path in the format the path's extension names where the service can, answering the
	// path it wrote, which may differ
	Synthesize func(ctx context.Context, job *Job, request Request) (string, error)
	// Speech as PCM while it is made, nil for a voice that writes whole files only
	Stream *Stream
	// How it gives the Ogg Opus a chat app shows as a voice bubble
	Opus Opus
	// Extension of the files it writes whatever it is asked, such as a program of yours that writes wav; empty for one that follows the path
	Format string
	// Readies and frees what it keeps loaded, nil for a voice that keeps nothing
	Warm, Release func(ctx context.Context, job *Job) error
	// Keeps a model loaded between requests, unloaded once no lease holds speech warm
	Resident bool
	// Lets the model pick a voice per call and search for one, nil for a voice whose voice the person sets
	Picker *Picker
	// What a person sets up to use it
	Setup Setup
	// Settings of it a person picks from a list in the voice setup, such as its voice or model
	Choices []Choices
}

// Values a person may pick for one setting of a service in the voice setup
type Choices struct {
	Setting string
	// Values in the order offered, the setting's own value among them or not; none when they can't be listed now
	List func(ctx context.Context, job *Job) []string
}

// Fixed values of a setting
func Fixed(setting string, values ...string) Choices {
	return Choices{Setting: setting, List: func(context.Context, *Job) []string { return values }}
}

// Voices the model may pick from, per call
type Picker struct {
	// What the tool's description says of picking, in place of the line saying the person picks
	Sentence string
	// Description of the voice_id argument
	About string
	// Schema of the find_voices argument
	Search map[string]any
	// Refusal of a voice id the service would not take
	Valid func(id string) error
	// One page of voices for a find_voices argument, as the model reads it; an error the model is told
	Find func(ctx context.Context, job *Job, query map[string]any) (map[string]any, error)
}

// How a voice gives Ogg Opus
type Opus int

const (
	// Gives none, its files sent as plain attachments
	NoOpus Opus = iota
	// Writes Ogg Opus itself when asked for an .ogg path
	NativeOpus
	// Its files are transcoded to Ogg Opus with ffmpeg where the platform shows voice bubbles
	TranscodedOpus
	// Opted in to voice bubbles, its files always transcoded to Ogg Opus, such as a program of yours marked voice_compatible
	OptedOpus
)

// What a person sets up to use a service
type Setup struct {
	// Variables of the profile's environment that hold its key, the first taking a key pasted in setup
	Keys []string
	// Provider kind whose sign in serves it in place of a key, connected in /providers
	SignIn string
	// Python set it runs in, installed on request
	Extra string
	// Programs of this machine it needs besides, such as espeak-ng
	Programs []string
	// Where to get a key
	Link string
	// What it costs and whether it is the one to start with, as the setup lists it: recommended · free, paid, free tier, free
	Badge string
}

// One piece of speech a voice is asked to write
type Request struct {
	Text string
	// File to write, whose extension names the format: .mp3 unless asked otherwise, .ogg for a voice bubble, .wav for PCM
	Path string
	// Speed the call asked for, 0.25 to 4, 0 when it asked none
	Speed float64
	// How to speak, which the services that take voice design follow
	Instructions string
	// Voice the call picked, such as an ElevenLabs voice id, empty for the configured one
	Voice string
}

// PCM a voice makes while it speaks one sentence
type Stream struct {
	// Place among the voices tried when tts.streaming.provider is auto, lower first
	Rank int
	// Name the log gives the stream, such as OpenAI streaming TTS; the title's when empty
	Label string
	// Whether it can stream with the profile's settings now
	Ready func(job *Job) bool
	// Opens the sentence's speech: 16 bit little endian mono PCM at the rate answered, which the reply's own headers may change
	Open func(ctx context.Context, job *Job, text string) (*PCM, error)
}

type PCM struct {
	Rate   int
	Chunks iter.Seq2[[]byte, error]
}

// Settings or credentials keep a service from running: its text tells the person what to set
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// Refusal naming what to set
func Misconfigured(message string) error { return &ConfigError{Message: message} }

// A local engine is not installed: its text is the whole answer, with the way to install it
type AbsentError struct{ Message string }

func (e *AbsentError) Error() string { return e.Message }

func Absent(message string) error { return &AbsentError{Message: message} }

// Something a service needs on this machine is missing, such as a file a local engine reads
type DependencyError struct{ Message string }

func (e *DependencyError) Error() string { return e.Message }

// Failure a Python engine answered, read by the kind of error it raised: a bad value a configuration error, a missing file a missing
// dependency
func Raised(kind, message string) error {
	switch kind {
	case "ValueError", "UnicodeDecodeError", "UnicodeEncodeError", "UnicodeError", "JSONDecodeError", "Error":
		return Misconfigured(message)
	case "FileNotFoundError":
		return &DependencyError{Message: message}
	}
	return errors.New(message)
}

// Whether the error is the configuration error of a service
func IsConfig(err error) bool {
	_, ok := errors.AsType[*ConfigError](err)
	return ok
}

// Float of a setting's value, the integers and text YAML gives included
func Number(value any) float64 {
	return cast.ToFloat64(value)
}

// Speed a service speaks at: the call's own, then the service's setting, then tts.speed; false when none is set
func (j *Job) Speed(request Request, own string) (float64, bool) {
	switch {
	case request.Speed > 0:
		return request.Speed, true
	case own != "" && j.Float(own) > 0:
		return j.Float(own), true
	case j.Float(speedKey) > 0:
		return j.Float(speedKey), true
	}
	return 1, false
}

// Ready check of a voice or recognizer with no check of its own
func always(*Job) bool { return true }

// Whether the voice can speak now
func (v Voice) ready(job *Job) bool {
	if v.Ready == nil {
		return always(job)
	}
	return v.Ready(job)
}

// Settings key a service keeps one of its values under
func Key(section, name string) string {
	return section + "." + name
}

// Configured choice of a kind of service, as written, empty when only its default holds
func Chosen(cfg *config.Config, key string) string {
	_, setting, ok := cfg.Setting(key)
	if !ok {
		return ""
	}
	if source, _ := cfg.Source(setting); source == config.FromDefault {
		return ""
	}
	return Normal(cfg.GetString(key))
}

// Whether the voice can speak with the profile's settings now
func (j *Job) Ready(v Voice) bool {
	return v.ready(j)
}
