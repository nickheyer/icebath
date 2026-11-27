// A whisper program on this machine: the one stt.local_command names, else the bundled whisper.cpp engine, else a whisper command on PATH
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/packages"
	localconfig "github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper/config"
	whisperpackages "github.com/nickheyer/icebath/internal/plugins/defaults/speech/whispercpp/packages"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Name the service takes
const Name = "local_command"

// Base model and voice detection model the bundled engine runs, fetched on first use
var (
	baseModel = speech.Asset{Name: "ggml-base.bin", URL: "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base.bin",
		SHA256: "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"}
	vadModel = speech.Asset{Name: "ggml-silero-v6.2.0.bin",
		URL:    "https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b/ggml-silero-v6.2.0.bin",
		SHA256: "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987"}
)

// Formats a local program reads as they are
var native = strs.Set(".wav", ".aiff", ".aif")

// Longest a transcription may take
const deadline = 300 * time.Second

// What a transcription answers when there is no program to run
const absent = speechconfig.LocalCommandEnv + " not configured and no local whisper binary was found"

// A program that exited with a failure, with what it said: its error output, else its output, else how it exited
type exitedError struct{ detail string }

func (e *exitedError) Error() string { return e.detail }

func Register(registry *speech.Registry, host *packages.Packages) {
	w := &whisper{packages: host}
	registry.Register(speech.Services{Recognizing: []speech.Recognizer{{
		Name: Name, Title: "Local command", About: "A program you name, else the bundled whisper.cpp engine or a whisper command on this machine",
		Local: true, Rank: 2, Fallback: localconfig.Name, Label: "local command", Warning: "STT provider 'local_command' configured but unavailable",
		Instead: "Local STT command unavailable, using local faster-whisper",
		Missing: func(_ context.Context, job *speech.Job) string {
			if w.template(job) == "" {
				return absent
			}
			return ""
		},
		Model: func(job *speech.Job, model string) string {
			return speech.WhisperModel(strs.Filled(model, job.Text(localconfig.Model, localconfig.DefaultModel)))
		},
		Language:   func(job *speech.Job) string { return job.Text(localconfig.Language, "") },
		Transcribe: w.transcribe,
	}}})
}

type whisper struct {
	packages *packages.Packages
}

// Binary of the bundled engine, empty when it is not installed
func (w *whisper) bundled() string {
	installed, err := w.packages.Installed(whisperpackages.WhisperCpp, false)
	if err != nil || installed == nil {
		return ""
	}
	return installed.Binary
}

// Template the bundled engine runs with the two models at their paths
func managed(binary, model, vad string) string {
	quote := func(p string) string { return terminal.Quote(filepath.ToSlash(p)) }
	return quote(binary) + " -m " + quote(model) + " -f {input_path} -l {language} -t 4 -bs 5 -bo 5 -ng -nt -otxt -of {output_dir}/transcript --vad -vm " +
		quote(vad) + " -vsd 500"
}

// Command template of the service: stt.local_command, else the bundled engine, else a whisper command on PATH; empty when there is none
func (w *whisper) template(job *speech.Job) string {
	if own := job.Text(speechconfig.LocalCommand, ""); own != "" {
		return own
	}
	if binary := w.bundled(); binary != "" {
		return managed(binary, baseModel.Name, vadModel.Name)
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if candidate := filepath.Join(dir, "whisper"); executable(candidate) {
			return terminal.Quote(candidate) + " {input_path} --model {model} --output_format txt --output_dir {output_dir} --language {language}"
		}
	}
	if found, err := exec.LookPath("whisper"); err == nil {
		return terminal.Quote(found) + " {input_path} --model {model} --output_format txt --output_dir {output_dir} --language {language}"
	}
	return ""
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// A placeholder a template names that is not one of those given
type missingError struct{ name string }

func (e *missingError) Error() string { return "missing placeholder: " + e.name }

// A template with each {name} replaced by its value and doubled braces made single; a name it does not know is an error
func format(template string, values map[string]string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(template); i++ {
		c := template[i]
		switch {
		case c == '{' && i+1 < len(template) && template[i+1] == '{':
			out.WriteByte('{')
			i++
		case c == '}' && i+1 < len(template) && template[i+1] == '}':
			out.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				return "", errors.New("Single '{' encountered in format string")
			}
			name := template[i+1 : i+end]
			value, ok := values[name]
			if !ok {
				return "", &missingError{name: name}
			}
			out.WriteString(value)
			i += end
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), nil
}

func (w *whisper) transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: Name}
	if hints.Prompt != "" {
		speech.PromptIgnored("STT provider 'local_command'")
	}
	template := w.template(job)
	if template == "" {
		return r.Failure(absent)
	}
	language := strs.Filled(speech.WhisperLanguage(hints.Language, nil), "en")
	text, failure, err := w.run(ctx, job, path, template, language, hints.Model)
	switch {
	case err == nil && failure != "":
		return r.Failure(failure)
	case err == nil:
		slog.Info(fmt.Sprintf("Transcribed %s via local STT command (%s, %d chars)", filepath.Base(path), hints.Model, utf8.RuneCountInString(text)))
		return r.Heard(text)
	}
	if missing, ok := errors.AsType[*missingError](err); ok {
		return r.Failure(fmt.Sprintf("Invalid %s template, missing placeholder: %s", speechconfig.LocalCommandEnv, missing.name))
	}
	if exited, ok := errors.AsType[*exitedError](err); ok {
		slog.Error(fmt.Sprintf("Local STT command failed for %s: %s", path, exited.detail))
		return r.Failure("Local STT failed: " + exited.detail)
	}
	slog.Error(fmt.Sprintf("Unexpected error during local command transcription: %v", err))
	return r.Failure(fmt.Sprintf("Local transcription failed: %v", err))
}

// Text of the first .txt the program leaves, or why it left none
func (w *whisper) run(ctx context.Context, job *speech.Job, path, template, language, model string) (string, string, error) {
	if job.Text(speechconfig.LocalCommand, "") == "" {
		if binary := w.bundled(); binary != "" && template == managed(binary, baseModel.Name, vadModel.Name) {
			if model != "base" {
				return "", "", speech.Misconfigured("Windows ARM64 whisper.cpp currently supports stt.local.model: base; requested " + model)
			}
			if !job.Speech.Fetched(baseModel) || !job.Speech.Fetched(vadModel) {
				slog.Info("Preparing whisper.cpp Base and VAD models (first use downloads about 149 MB)")
			}
			base, err := job.Speech.File(ctx, baseModel)
			if err != nil {
				return "", "", err
			}
			vad, err := job.Speech.File(ctx, vadModel)
			if err != nil {
				return "", "", err
			}
			template = managed(binary, base, vad)
		}
	}
	output, err := os.MkdirTemp("", "icebath-local-stt-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(output)
	audio := path
	if !native[strings.ToLower(filepath.Ext(path))] {
		ffmpeg := job.FFmpeg(ctx)
		if ffmpeg == "" {
			return "", "Local STT fallback requires ffmpeg for non-WAV inputs, but ffmpeg was not found", nil
		}
		audio = filepath.Join(output, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".wav")
		if captured, err := speech.Run(ctx, []string{ffmpeg, "-y", "-i", path, audio}, deadline, nil); err != nil {
			if strings.Contains(err.Error(), "timed out") {
				slog.Error("ffmpeg conversion timed out for " + path)
				return "", "Audio conversion for local STT timed out", nil
			}
			detail := speech.Detail(captured, err)
			slog.Error(fmt.Sprintf("ffmpeg conversion failed for %s: %s", path, detail))
			return "", "Failed to convert audio for local STT: " + detail, nil
		}
	}
	command, err := format(template, map[string]string{"input_path": terminal.Quote(audio), "output_dir": terminal.Quote(output),
		"language": terminal.Quote(language), "model": terminal.Quote(model)})
	if err != nil {
		return "", "", err
	}
	argv, err := terminal.Split(command)
	if err != nil {
		return "", "", err
	}
	if captured, err := speech.Run(ctx, argv, deadline, job.Environ(nil)); err != nil {
		if _, ok := errors.AsType[*terminal.ExitError](err); ok {
			return "", "", &exitedError{detail: speech.Detail(captured, err)}
		}
		return "", "", err
	}
	texts, _ := filepath.Glob(filepath.Join(output, "*.txt"))
	slices.Sort(texts)
	if len(texts) == 0 {
		return "", "Local STT command completed but did not produce a .txt transcript", nil
	}
	data, err := os.ReadFile(texts[0])
	if err != nil {
		return "", "", err
	}
	return speech.Decoded(data), "", nil
}
