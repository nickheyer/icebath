// Your own speech and recognition programs, declared under tts.providers and stt.providers, or under tts and stt by their names
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Formats a speech program may write, and a recognition program
var (
	audioFormats = strs.Set("mp3", "wav", "ogg", "flac", "m4a", "aac", "amr", "opus")
	textFormats  = strs.Set("txt", "json", "srt", "vtt")
)

// Names of the built in services, and the recognizer of none, which no program of yours takes
var (
	builtinVoices      = []string{"edge", "elevenlabs", "openai", "minimax", "xai", "mistral", "gemini", "neutts", "kittentts", "piper", "deepinfra"}
	builtinRecognizers = []string{"local", "local_command", "groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra", speech.None}
)

func Register(registry *speech.Registry) {
	registry.Register(programs{})
}

// Programs of the profile's settings, read each time they are asked for
type programs struct{}

// One program of yours
type program struct {
	name, command, format, voice, model, language string
	speed                                         float64
	timeout                                       time.Duration
	compatible, normalize                         bool
	limit                                         int
	passthrough                                   []string
	warm, release                                 string
}

// Program of one entry over the defaults, false when the entry runs no command; a format not among those given keeps the default, a time
// limit under a second is ignored
func parse(name string, raw any, format string, formats map[string]bool, timeout time.Duration) (program, bool) {
	entry := cast.ToStringMap(raw)
	command := strings.TrimSpace(cast.ToString(entry["command"]))
	kind := strings.ToLower(strings.TrimSpace(cast.ToString(entry["type"])))
	if command == "" || kind != "" && kind != "command" {
		return program{}, false
	}
	p := program{name: name, command: command, format: format, timeout: timeout, limit: 5000}
	for _, key := range []string{"output_format", "format"} {
		if v := strings.ToLower(strings.TrimSpace(cast.ToString(entry[key]))); formats[v] {
			p.format = v
		}
	}
	p.voice, p.model, p.language = strings.TrimSpace(cast.ToString(entry["voice"])), strings.TrimSpace(cast.ToString(entry["model"])),
		strings.TrimSpace(cast.ToString(entry["language"]))
	if speed, err := cast.ToFloat64E(entry["speed"]); err == nil {
		p.speed = max(0, speed)
	}
	for _, key := range []string{"timeout_seconds", "timeout"} {
		if value, ok := entry[key]; ok && value != nil {
			if s := speech.Seconds(value, time.Second); s >= 1 {
				p.timeout = time.Duration(s * float64(time.Second))
			}
		}
	}
	p.compatible, p.normalize = cast.ToBool(entry["voice_compatible"]), cast.ToBool(entry["normalize"])
	if n := cast.ToInt(entry["max_text_length"]); n > 0 {
		p.limit = n
	}
	for _, name := range cast.ToStringSlice(entry["env_passthrough"]) {
		if name = strings.TrimSpace(name); name != "" {
			p.passthrough = append(p.passthrough, name)
		}
	}
	p.warm, p.release = cast.ToString(entry["warm_command"]), cast.ToString(entry["release_command"])
	return p, true
}

// Programs of a section by lower case name: those of its providers map, then those sitting directly under it, which the section's own
// settings never are as they run no command; the reserved names left out
func declared(cfg *config.Config, section, providers, format string, formats map[string]bool, timeout time.Duration, reserved ...string) []program {
	found := map[string]program{}
	add := func(entries map[string]any) {
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			key := speech.Normal(name)
			if _, taken := found[key]; key == "" || taken || key == "providers" || slices.Contains(reserved, key) {
				continue
			}
			if p, ok := parse(key, entries[name], format, formats, timeout); ok {
				found[key] = p
			}
		}
	}
	add(cast.ToStringMap(cfg.Get(providers)))
	add(cast.ToStringMap(cfg.Get(section)))
	out := make([]program, 0, len(found))
	for _, key := range slices.Sorted(maps.Keys(found)) {
		out = append(out, found[key])
	}
	return out
}

// Speech programs, the voice, model and format of every program in place of those a program names none of, by the one rule of values
// kept both ways
func (programs) Voices(cfg *config.Config) []speech.Voice {
	format := strings.ToLower(strings.TrimSpace(cfg.GetString(speechconfig.OutputFormat)))
	if !audioFormats[format] {
		format = "mp3"
	}
	voice, model := strings.TrimSpace(cfg.GetString(speechconfig.PluginVoice)), strings.TrimSpace(cfg.GetString(speechconfig.PluginModel))
	var out []speech.Voice
	for _, p := range declared(cfg, speechconfig.TTS, speechconfig.TTSCommands, format, audioFormats, 120*time.Second, builtinVoices...) {
		p.voice, p.model = speech.Either(p.voice, voice), speech.Either(p.model, model)
		v := speech.Voice{Name: p.name, Title: p.name, About: "A speech program of yours, run on this computer", Label: "command TTS provider '" + p.name + "'",
			Local: true, Program: true, Limit: func(*speech.Job) int { return p.limit }, Format: p.format,
			Synthesize: func(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
				return p.synthesize(ctx, job, request)
			},
		}
		if p.compatible {
			v.Opus = speech.OptedOpus
		}
		if strings.TrimSpace(p.warm) != "" {
			v.Warm = func(ctx context.Context, job *speech.Job) error {
				p.signal(ctx, job, "warm", p.warm, speech.Request{})
				return nil
			}
		}
		if strings.TrimSpace(p.release) != "" {
			v.Release = func(ctx context.Context, job *speech.Job) error {
				p.signal(ctx, job, "release", p.release, speech.Request{})
				return nil
			}
		}
		out = append(out, v)
	}
	return out
}

func (programs) Recognizers(cfg *config.Config) []speech.Recognizer {
	var out []speech.Recognizer
	for _, p := range declared(cfg, speechconfig.STT, speechconfig.STTCommands, "txt", textFormats, 300*time.Second, builtinRecognizers...) {
		out = append(out, speech.Recognizer{Name: p.name, Title: p.name, About: "A speech to text program of yours, run on this computer", Label: "command: " + p.name,
			Program:  true,
			Model:    func(job *speech.Job, model string) string { return strs.Filled(model, p.model) },
			Language: func(*speech.Job) string { return p.language },
			Transcribe: func(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
				return p.transcribe(ctx, job, path, hints)
			},
		})
	}
	return out
}

func (programs) Engines() []speech.Engine { return nil }

// Text of a float as Python writes it, a whole number keeping its .0
func pythonFloat(v float64) string {
	text := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eE") && !strings.Contains(text, "Inf") && !strings.Contains(text, "NaN") {
		text += ".0"
	}
	return text
}

// Speed the program is handed: the call's own, else its own or tts.speed by the one rule of values kept both ways; empty when none is set
func (p program) speedText(job *speech.Job, request speech.Request) string {
	if request.Speed > 0 {
		return pythonFloat(request.Speed)
	}
	if speed := speech.Either(p.speed, max(0, job.Float(speechconfig.Speed))); speed > 0 {
		return pythonFloat(speed)
	}
	return ""
}

// Format the program writes: that of the path when it can write it, else its own
func (p program) formatFor(path string) string {
	if suffix := strings.TrimLeft(strings.ToLower(filepath.Ext(path)), "."); audioFormats[suffix] {
		return suffix
	}
	return p.format
}

func (p program) synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	output := fsx.ExpandHome(request.Path)
	if err := os.MkdirAll(filepath.Dir(output), 0o777); err != nil {
		return "", err
	}
	if _, err := os.Stat(output); err == nil {
		if err := os.Remove(output); err != nil {
			return "", err
		}
	}
	work, err := os.MkdirTemp("", "tmp")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	text := filepath.Join(work, "input.txt")
	if err := os.WriteFile(text, []byte(request.Text), 0o666); err != nil {
		return "", err
	}
	command := speech.Render(p.command, map[string]string{"input_path": text, "text_path": text, "output_path": output, "format": p.formatFor(output),
		"voice": p.voice, "model": p.model, "speed": p.speedText(job, request)})
	if _, _, err := job.Shell(ctx, command, p.timeout, p.passthrough, "tts-command"); err != nil {
		if _, ok := errors.AsType[*speech.TimeoutError](err); ok {
			return "", fmt.Errorf("TTS provider '%s' timed out after %ss", p.name, strconv.FormatFloat(p.timeout.Seconds(), 'g', 6, 64))
		}
		if exited, ok := errors.AsType[*speech.ExitError](err); ok {
			return "", fmt.Errorf("TTS provider '%s' exited with code %d: %s", p.name, exited.Code, exited.Detail())
		}
		return "", err
	}
	if info, err := os.Stat(output); err != nil || info.Size() <= 0 {
		return "", fmt.Errorf("TTS provider '%s' produced no output at %s", p.name, output)
	}
	return output, nil
}

// Runs the program's warm or release command in the background, its failure only logged
func (p program) signal(ctx context.Context, job *speech.Job, hook, template string, request speech.Request) {
	command := speech.Render(strings.TrimSpace(template), map[string]string{"voice": p.voice, "model": p.model, "speed": p.speedText(job, request)})
	bound := context.WithoutCancel(ctx)
	go func() {
		if _, _, err := job.Shell(bound, command, p.timeout, p.passthrough, "tts-command"); err != nil {
			slog.Debug(fmt.Sprintf("[TTS] %s_command for %s failed: %s", hook, p.name, err))
		}
	}()
}

func (p program) transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: p.name}
	if hints.Prompt != "" {
		speech.PromptIgnored(fmt.Sprintf("Command STT provider '%s'", p.name))
	}
	audio := fsx.ExpandHome(path)
	if _, err := os.Stat(audio); err != nil {
		return r.Failure("Audio file not found: " + path)
	}
	format := p.format
	language := strs.Filled(hints.Language, "en")
	work, err := os.MkdirTemp("", "icebath-cmd-stt-"+p.name+"-")
	if err != nil {
		return r.Failure(fmt.Sprintf("STT command provider '%s' failed: %v", p.name, err))
	}
	defer os.RemoveAll(work)
	input := resolved(audio)
	if p.normalize {
		converted, why := job.Compact(ctx, input, work)
		if converted == "" {
			return r.Failure(fmt.Sprintf("stt.providers.%s.normalize: true but the audio could not be normalized for the command provider: %s", p.name,
				strs.Filled(why, "unknown ffmpeg failure")))
		}
		slog.Info(fmt.Sprintf("Normalized %s to 16 kHz mono for command STT provider '%s'", filepath.Base(audio), p.name))
		input = converted
	}
	output := filepath.Join(work, "transcript."+format)
	command := speech.Render(p.command, map[string]string{"input_path": input, "output_path": output, "output_dir": work, "format": format,
		"language": language, "model": hints.Model})
	slog.Info(fmt.Sprintf("Transcribing %s via command STT provider '%s'...", filepath.Base(audio), p.name))
	stdout, _, err := job.Shell(ctx, command, p.timeout, p.passthrough, "stt-command")
	if err != nil {
		if _, ok := errors.AsType[*speech.TimeoutError](err); ok {
			return r.Failure(fmt.Sprintf("STT command provider '%s' timed out after %ss", p.name, strconv.FormatFloat(p.timeout.Seconds(), 'g', 6, 64)))
		}
		if exited, ok := errors.AsType[*speech.ExitError](err); ok {
			return r.Failure(fmt.Sprintf("STT command provider '%s' exited with code %d: %s", p.name, exited.Code, exited.Detail()))
		}
		return r.Failure(fmt.Sprintf("STT command provider '%s' failed: %v", p.name, err))
	}
	text := ""
	if data, err := os.ReadFile(output); err == nil {
		text = speech.Decoded(data)
	}
	if text == "" {
		text = strings.TrimSpace(stdout)
	}
	if text == "" {
		return r.Failure(fmt.Sprintf("Command STT provider wrote no output file at %s and produced no stdout", output))
	}
	slog.Info(fmt.Sprintf("Transcribed %s via command STT provider '%s' (%d chars)", filepath.Base(audio), p.name, utf8.RuneCountInString(text)))
	return r.Heard(text)
}

// Absolute path with its links resolved as far as they exist
func resolved(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
