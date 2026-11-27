package speech

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/packages"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	speechpackages "github.com/nickheyer/icebath/internal/speech/packages"
)

// Name the recognizer in use takes when none can transcribe
const None = "none"

// Formats the recognizers take
var audioFormats = []string{".aac", ".caf", ".flac", ".m4a", ".mp3", ".mp4", ".mpeg", ".mpga", ".oga", ".ogg", ".opus", ".wav", ".webm"}

// Largest upload of the cloud services that state none of their own
const UploadCap = 25 << 20

// Last 224 tokens of a prompt are all the whisper family reads, at four characters a token
const whisperPromptChars = 224 * 4

// Message when nothing can transcribe
const noRecognizer = "No STT provider available. Install faster-whisper for free local transcription, configure " + speechconfig.LocalCommandEnv +
	" or install a local whisper CLI, set GROQ_API_KEY for free Groq Whisper, set MISTRAL_API_KEY for Mistral Voxtral Transcribe, configure xAI " +
	"OAuth or set XAI_API_KEY for xAI Grok STT, set ELEVENLABS_API_KEY for ElevenLabs Scribe, or set VOICE_TOOLS_OPENAI_KEY or OPENAI_API_KEY " +
	"for the OpenAI Whisper API."

// Recognizer in use, its name or None: the one stt.provider names when it can transcribe, its fallback when that can, else none; when
// stt.provider is unset, the first that can of the local engines and then the cloud services
func (j *Job) Recognizing(ctx context.Context) string {
	if !j.Config.GetBool(speechconfig.STTEnabled) {
		return None
	}
	if chosen := j.chosenRecognizer(); chosen != "" {
		recognizer, ok := j.Speech.Recognizer(j.Config, chosen)
		if !ok {
			return chosen
		}
		missing := recognizer.missing(ctx, j)
		if missing == "" {
			return chosen
		}
		if fallback, ok := j.Speech.Recognizer(j.Config, recognizer.Fallback); ok && fallback.missing(ctx, j) == "" {
			if recognizer.Instead != "" {
				slog.Info(recognizer.Instead)
			}
			return fallback.Name
		}
		warning := recognizer.Warning
		if warning == "" {
			warning = fmt.Sprintf("STT provider '%s' configured but unavailable: %s", chosen, missing)
		}
		slog.Warn(warning)
		return None
	}
	ranked := slices.DeleteFunc(j.Speech.Recognizers(j.Config), func(r Recognizer) bool { return r.Rank == 0 })
	slices.SortStableFunc(ranked, func(a, b Recognizer) int { return a.Rank - b.Rank })
	for _, recognizer := range ranked {
		if recognizer.missing(ctx, j) == "" {
			if recognizer.Cloud {
				slog.Info("No local STT available, using " + cmpOr(recognizer.Detected, recognizer.label()))
			}
			return recognizer.Name
		}
	}
	return None
}

// Recognizer stt.provider names, empty when it holds only its default; the default set by the session's layer alone, over a profile that
// chose none, counts as unset too, so a launch asking for this machine's engine still finds whichever is installed
func (j *Job) chosenRecognizer() string {
	chosen := Chosen(j.Config, speechconfig.STTProvider)
	if chosen == speechconfig.DefaultRecognize && Chosen(j.Config.With(nil), speechconfig.STTProvider) == "" {
		return ""
	}
	return chosen
}

// Short name of the recognizer
func (r Recognizer) label() string {
	return cmpOr(r.Label, r.Title)
}

// Labelled recognizer in use for the voice requirements, empty when none can transcribe
func (j *Job) RecognizerLabel(ctx context.Context) string {
	name := j.Recognizing(ctx)
	if recognizer, ok := j.Speech.Recognizer(j.Config, name); ok && name != None {
		return recognizer.label()
	}
	return ""
}

// Transcribes an audio file with the recognizer in use: the file checked, a SILK note decoded, Apple's CAF converted for a cloud service,
// silence trimmed before an upload and a file too large for one request split at its pauses
func (j *Job) Transcribe(ctx context.Context, path, model, source string) Transcript {
	if j.Unreadable != nil {
		if reason := j.Unreadable(path); reason != "" {
			return Failure(reason)
		}
	}
	silk := strings.ToLower(filepath.Ext(path)) == ".silk"
	if failure, ok := sourceFile(path, silk); !ok {
		return failure
	}
	if silk {
		decoded, cleanup, failure := j.silk(ctx, path)
		if decoded == "" {
			return failure
		}
		defer os.RemoveAll(cleanup)
		path = decoded
	}
	if failure, ok := audioFile(path); !ok {
		return failure
	}
	if !j.Config.GetBool(speechconfig.STTEnabled) {
		return Failure("STT is disabled in config.yaml (stt.enabled: false).")
	}
	name := j.Recognizing(ctx)
	recognizer, known := j.Speech.Recognizer(j.Config, name)
	if !known {
		return j.unclaimed(ctx, name)
	}
	if !recognizer.Local && strings.ToLower(filepath.Ext(path)) == ".caf" {
		work, err := os.MkdirTemp("", "icebath-caf-")
		if err != nil {
			return Failure(err.Error())
		}
		defer os.RemoveAll(work)
		if path = j.caf(ctx, path, work); path == "" {
			return Failure("CAF audio could not be converted to WAV.")
		}
	}
	if recognizer.Cloud {
		if trimmed := j.trim(ctx, path); trimmed != "" {
			defer os.RemoveAll(filepath.Dir(trimmed))
			path = trimmed
		}
	}
	// Every upload is capped, the services that state no cap of their own at 25 MB
	if !recognizer.Local {
		limit := Limit{Bytes: UploadCap}
		if recognizer.Limit != nil {
			limit = recognizer.Limit(j.model(recognizer, model))
		}
		if j.over(ctx, path, limit) {
			hints := j.hints(ctx, recognizer, path, model, source)
			return j.oversized(ctx, path, limit, func(part string) Transcript { return recognizer.Transcribe(ctx, j, part, hints) })
		}
	}
	return recognizer.Transcribe(ctx, j, path, j.hints(ctx, recognizer, path, model, source))
}

// Transcription of a voice recording, a known hallucination on silence read as nothing said unless it is a stop phrase
func (j *Job) Recording(ctx context.Context, path string) Transcript {
	return j.Settle(j.Transcribe(ctx, path, "", "voice_mode"))
}

// A recording's transcript with what Whisper hears in silence read as nothing said, a stop phrase kept
func (j *Job) Settle(t Transcript) Transcript {
	if t.Success && Hallucination(t.Text) && !StopPhrase(StopPhrases(j.Config), t.Text) {
		slog.Info(fmt.Sprintf("Filtered Whisper hallucination: %q", t.Text))
		return Transcript{Success: true, Filtered: true, Provider: t.Provider}
	}
	if t.NoSpeech {
		return Transcript{Success: true, NoSpeech: true, Provider: t.Provider}
	}
	return t
}

// Model a recognizer transcribes with: the call's own, then its setting
func (j *Job) model(recognizer Recognizer, model string) string {
	if recognizer.Model != nil {
		return recognizer.Model(j, model)
	}
	return model
}

// Hint of a pre_transcription hook, nil when empty
func hinted(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// Model, language and prompt of a transcription: what the pre_transcription hooks set over the call's model, the recognizer's language
// setting and stt.prompt; then the recognizer's model setting under the model, stt.language under the language, and the prompt cut to what
// the whisper family reads
func (j *Job) hints(ctx context.Context, recognizer Recognizer, path, model, source string) Hints {
	prompt := j.Config.GetString(speechconfig.Prompt)
	if strings.TrimSpace(prompt) == "" {
		prompt = ""
	}
	own := ""
	if recognizer.Language != nil {
		own = strings.TrimSpace(recognizer.Language(j))
	}
	var language string
	if j.Speech.hooks != nil {
		model, language, prompt = j.Speech.hooks.Transcribe(ctx, j.Config, hooks.Transcription{File: path, Provider: recognizer.Name, Model: hinted(model),
			Language: hinted(own), Prompt: hinted(prompt), Source: hinted(source)})
	}
	hints := Hints{Model: j.model(recognizer, model), Language: strings.TrimSpace(language), Prompt: prompt}
	if hints.Language == "" {
		hints.Language = cmpOr(own, j.Text(speechconfig.Language, ""))
	}
	if runes := []rune(hints.Prompt); recognizer.Whisper && len(runes) > whisperPromptChars {
		slog.Warn(fmt.Sprintf("Transcription prompt is ~%d tokens; whisper-family provider '%s' only uses the final ~224 — truncating to the last %d characters.",
			len(runes)/4, recognizer.Name, whisperPromptChars))
		hints.Prompt = string(runes[len(runes)-whisperPromptChars:])
	}
	return hints
}

// Failure of a transcription nothing claimed: a name no recognizer has, else the setup hint
func (j *Job) unclaimed(ctx context.Context, name string) Transcript {
	chosen := Chosen(j.Config, speechconfig.STTProvider)
	if name != None && name != "" {
		return Transcript{Error: fmt.Sprintf("stt.provider='%s' is set but no built-in, command, or plugin provider registered that name. Run `icebath "+
			"plugins list` to see installed STT plugins, or configure a command provider under `stt.providers.%s.command`.", name, name), Provider: name,
			Kind: "provider_not_registered"}
	}
	if recognizer, ok := j.Speech.Recognizer(j.Config, chosen); ok && recognizer.Explains {
		if missing := recognizer.missing(ctx, j); missing != "" {
			return Failure(missing)
		}
	}
	return Failure(noRecognizer)
}

// Failure of a source file a decoder must not read, or that is too large when the cap applies
func sourceFile(path string, capped bool) (Transcript, bool) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return Failure("Path is a symbolic link: " + path), false
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return Failure("Audio file not found: " + path), false
	case !info.Mode().IsRegular():
		return Failure("Path is not a file: " + path), false
	case capped && info.Size() > UploadCap:
		return Failure(fmt.Sprintf("File too large: %.1fMB (max %.0fMB)", float64(info.Size())/(1<<20), float64(UploadCap)/(1<<20))), false
	}
	return Transcript{}, true
}

// Failure of a file no recognizer takes
func audioFile(path string) (Transcript, bool) {
	if failure, ok := sourceFile(path, false); !ok {
		return failure, false
	}
	if ext := filepath.Ext(path); !slices.Contains(audioFormats, strings.ToLower(ext)) {
		return Failure(fmt.Sprintf("Unsupported format: %s. Supported: %s", ext, strings.Join(audioFormats, ", "))), false
	}
	return Transcript{}, true
}

// Script that decodes SILK to WAV with pilk at a sample rate, its failure written as JSON
const silkScript = `import json, sys
try:
    import pilk
    pilk.silk_to_wav(sys.argv[1], sys.argv[2], rate=int(sys.argv[3]))
except BaseException as exc:
    sys.stdout.write(json.dumps({"error": str(exc)}))
    sys.exit(1)
`

// Decodes a SILK voice note of QQ or WeChat to WAV at the sample rate with pilk
func (j *Job) SilkToWAV(ctx context.Context, source, output string, rate int) error {
	python, ok := j.Speech.packages.ExtraPython(speechpackages.Silk)
	if !ok {
		return Misconfigured("the pilk Python package is not installed; run `" + packages.InstallHint(speechpackages.Silk) + "`")
	}
	captured, err := Run(ctx, []string{python, "-I", "-c", silkScript, source, output, strconv.Itoa(rate)}, 300*time.Second, nil)
	if err != nil && captured != nil {
		var reply struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(captured.Stdout), &reply) == nil && reply.Error != "" {
			return fmt.Errorf("%s", reply.Error)
		}
	}
	return err
}

// WAV of a SILK note in a folder to remove after, or the failure
func (j *Job) silk(ctx context.Context, path string) (string, string, Transcript) {
	if _, ok := j.Speech.packages.ExtraPython(speechpackages.Silk); !ok {
		return "", "", Failure("Unsupported format: .silk. Run `" + packages.InstallHint(speechpackages.Silk) + "` to enable WeChat voice transcription.")
	}
	dir, err := os.MkdirTemp("", "icebath-silk-")
	if err != nil {
		return "", "", Failure("Failed to convert .silk audio for transcription: " + err.Error())
	}
	decoded := filepath.Join(dir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".wav")
	err = j.SilkToWAV(ctx, path, decoded, 24000)
	if info, serr := os.Stat(decoded); err == nil && (serr != nil || !info.Mode().IsRegular() || info.Size() == 0) {
		err = fmt.Errorf("pilk did not produce a readable WAV file")
	}
	if err != nil {
		os.RemoveAll(dir)
		slog.Error(fmt.Sprintf("Failed to convert .silk audio %s: %v", path, err))
		return "", "", Failure(fmt.Sprintf("Failed to convert .silk audio for transcription: %v", err))
	}
	return decoded, dir, Transcript{}
}

// WAV of Apple's CAF in the folder through ffmpeg or afconvert, empty when neither manages
func (j *Job) caf(ctx context.Context, path, work string) string {
	wav := filepath.Join(work, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".wav")
	var tries [][]string
	if ffmpeg := j.FFmpeg(ctx); ffmpeg != "" {
		tries = append(tries, []string{ffmpeg, "-y", "-i", path, wav})
	}
	if afconvert, err := exec.LookPath("afconvert"); err == nil {
		tries = append(tries, []string{afconvert, path, wav, "-d", "LEI16", "-f", "WAVE"})
	}
	for _, argv := range tries {
		if _, err := Run(ctx, argv, 300*time.Second, nil); err == nil {
			return wav
		} else {
			slog.Warn(fmt.Sprintf("%s CAF to WAV failed for %s: %v", filepath.Base(argv[0]), path, err))
		}
	}
	return ""
}

// Arguments of the compact 16 kHz mono AAC encode an upload is made of
var uploadEncode = []string{"-vn", "-ac", "1", "-ar", "16000", "-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart"}

// Encodes audio to the compact upload form in the folder, with an audio filter when one is given
func (j *Job) encode(ctx context.Context, path, output, filter string) error {
	ffmpeg := j.FFmpeg(ctx)
	if ffmpeg == "" {
		return fmt.Errorf("ffmpeg was not found")
	}
	argv := []string{ffmpeg, "-y", "-i", path}
	if filter != "" {
		argv = append(argv, "-af", filter)
	}
	captured, err := Run(ctx, append(append(argv, uploadEncode...), output), 120*time.Second, nil)
	if err != nil {
		return fmt.Errorf("%s", Detail(captured, err))
	}
	return nil
}

// Compact 16 kHz mono copy of audio for an upload, or why it could not be made
func (j *Job) Compact(ctx context.Context, path, work string) (string, string) {
	if j.FFmpeg(ctx) == "" {
		return "", "audio needs transcoding for the STT API, but ffmpeg was not found"
	}
	output := filepath.Join(work, cmpOr(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), "audio")+"-stt.m4a")
	if err := j.encode(ctx, path, output, ""); err != nil {
		slog.Error(fmt.Sprintf("ffmpeg STT transcode failed for %s: %s", path, err))
		return "", "failed to transcode audio for the STT API: " + err.Error()
	}
	return output, ""
}

// Trim of a recording ahead of a cloud upload
const (
	trimLeast        = 12.0
	trimLeastResult  = 0.3
	trimLeastSavings = 0.10
)

// Copy of a recording with its silence trimmed for a cloud upload, empty to upload the original; its folder is the caller's to remove
func (j *Job) trim(ctx context.Context, path string) string {
	if !j.Config.GetBool(speechconfig.TrimSilence) {
		return ""
	}
	if j.FFmpeg(ctx) == "" {
		slog.Debug("Cloud STT silence trim skipped: ffmpeg not found")
		return ""
	}
	original, ok := j.Duration(ctx, path)
	if !ok || original <= 0 {
		slog.Debug("Cloud STT silence trim skipped: could not probe " + path)
		return ""
	}
	name := filepath.Base(path)
	if original < trimLeast {
		slog.Debug(fmt.Sprintf("Cloud STT silence trim skipped for %s: %.1fs is below the %.0fs gate", name, original, trimLeast))
		return ""
	}
	// Padding in whole milliseconds, as the filter is given it
	padding := max(0, j.Config.GetDuration(speechconfig.TrimKeep)) / time.Millisecond
	threshold, keep := j.Config.GetInt(speechconfig.TrimThreshold), float64(padding)/1000
	kept := strconv.FormatFloat(keep, 'f', -1, 64)
	filter := fmt.Sprintf("silenceremove=start_periods=1:start_threshold=%ddB:start_silence=%s:stop_periods=-1:stop_threshold=%ddB:stop_silence=%s",
		threshold, kept, threshold, kept)
	work, err := os.MkdirTemp("", "icebath-stt-trim-")
	if err != nil {
		return ""
	}
	trimmed := filepath.Join(work, cmpOr(strings.TrimSuffix(name, filepath.Ext(name)), "audio")+"-trimmed.m4a")
	if err := j.encode(ctx, path, trimmed, filter); err != nil {
		slog.Debug(fmt.Sprintf("Cloud STT silence trim failed for %s: %v", path, err))
		os.RemoveAll(work)
		return ""
	}
	result, _ := j.Duration(ctx, trimmed)
	switch {
	case result == 0 || result < math.Max(trimLeastResult, 2*keep):
		slog.Debug(fmt.Sprintf("Cloud STT silence trim discarded for %s: trimmed result ~empty (%.2fs)", name, result))
	case result > original*(1-trimLeastSavings):
		slog.Debug(fmt.Sprintf("Cloud STT silence trim discarded for %s: saves <%.0f%% (%.1fs -> %.1fs)", name, trimLeastSavings*100, original, result))
	default:
		slog.Info(fmt.Sprintf("Trimmed silence from %s before cloud STT upload (%.1fs -> %.1fs, -%d%%)", name, original, result,
			int(math.RoundToEven((1-result/original)*100))))
		return trimmed
	}
	os.RemoveAll(work)
	return ""
}

func size(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// Whether a file fits the caps, a duration not known left for the service to judge
func fits(path string, limit Limit, seconds float64, known bool) bool {
	if size(path) > limit.Bytes {
		return false
	}
	return limit.Seconds == 0 || !known || seconds <= limit.Seconds
}

// Whether a file is over the byte cap or over a duration cap ffprobe can measure
func (j *Job) over(ctx context.Context, path string, limit Limit) bool {
	if size(path) > limit.Bytes {
		return true
	}
	if limit.Seconds == 0 {
		return false
	}
	seconds, known := j.Duration(ctx, path)
	return !fits(path, limit, seconds, known)
}

func tooLarge(path string, limit Limit, reason string) Transcript {
	return Failure(fmt.Sprintf("File too large: %.1fMB (max %.0fMB) and it could not be split: %s", float64(size(path))/(1<<20), float64(limit.Bytes)/(1<<20), reason))
}

var (
	silenceStart = regexp.MustCompile(`silence_start: (-?[\d.]+)`)
	silenceEnd   = regexp.MustCompile(`silence_end: ([\d.]+)`)
)

// Middles of the pauses ffmpeg hears, none when it can't tell
func (j *Job) pauses(ctx context.Context, path string) []float64 {
	captured, err := Run(ctx, []string{j.FFmpeg(ctx), "-hide_banner", "-nostats", "-i", path, "-af", "silencedetect=noise=-35dB:d=0.4", "-f", "null", "-"},
		300*time.Second, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("silencedetect failed for %s: %v", path, err))
		return nil
	}
	times := func(re *regexp.Regexp) []float64 {
		var out []float64
		for _, m := range re.FindAllStringSubmatch(captured.Stderr, -1) {
			if t, err := strconv.ParseFloat(m[1], 64); err == nil {
				out = append(out, t)
			}
		}
		return out
	}
	starts, ends := times(silenceStart), times(silenceEnd)
	var middles []float64
	for i := 0; i < len(starts) && i < len(ends); i++ {
		middles = append(middles, (math.Max(starts[i], 0)+ends[i])/2)
	}
	return middles
}

// Cut times of pieces no longer than the target, each at the latest pause before its ideal cut within a lookback
func cuts(duration, target float64, pauses []float64) []float64 {
	var out []float64
	position, lookback := 0.0, math.Min(60, target/4)
	for duration-position > target {
		ideal := position + target
		best, found := 0.0, false
		for _, p := range pauses {
			if ideal-lookback <= p && p <= ideal && p > position && (!found || p > best) {
				best, found = p, true
			}
		}
		if found {
			position = best
		} else {
			position = ideal
		}
		out = append(out, position)
	}
	return out
}

// Brings a file under the caps by encoding it compactly, else by cutting it at its pauses, and transcribes each piece in turn
func (j *Job) oversized(ctx context.Context, path string, limit Limit, transcribe func(part string) Transcript) Transcript {
	if j.FFmpeg(ctx) == "" {
		return tooLarge(path, limit, "ffmpeg was not found")
	}
	work, err := os.MkdirTemp("", "icebath-stt-split-")
	if err != nil {
		return Failure(err.Error())
	}
	defer os.RemoveAll(work)
	compact, failure := j.Compact(ctx, path, work)
	if compact == "" {
		return tooLarge(path, limit, cmpOr(failure, "re-encoding produced no file"))
	}
	duration, known := j.Duration(ctx, compact)
	if fits(compact, limit, duration, known) {
		slog.Info(fmt.Sprintf("Re-encoded oversized audio %s to %.1fMB for a single STT upload", filepath.Base(path), float64(size(compact))/(1<<20)))
		return transcribe(compact)
	}
	if !known || duration == 0 {
		return tooLarge(path, limit, "its duration could not be read")
	}
	target := float64(limit.Bytes) * 0.9 * duration / float64(size(compact))
	if limit.Seconds > 0 {
		target = math.Min(target, limit.Seconds*0.9)
	}
	marks := cuts(duration, target, j.pauses(ctx, compact))
	times := make([]string, len(marks))
	for i, t := range marks {
		times[i] = fmt.Sprintf("%.3f", t)
	}
	argv := []string{j.FFmpeg(ctx), "-y", "-loglevel", "error", "-i", compact, "-vn", "-c", "copy", "-f", "segment", "-segment_times", strings.Join(times, ","),
		"-reset_timestamps", "1", filepath.Join(work, "part%03d.m4a")}
	if captured, err := Run(ctx, argv, 300*time.Second, nil); err != nil {
		return tooLarge(path, limit, Detail(captured, err))
	}
	parts, _ := filepath.Glob(filepath.Join(work, "part*"))
	slices.Sort(parts)
	slog.Info(fmt.Sprintf("Transcribing %s (%.0fs) in %d segments of <= %.0fs", filepath.Base(path), duration, len(parts), math.Ceil(target)))
	var texts []string
	provider := ""
	for i, part := range parts {
		result := transcribe(part)
		provider = cmpOr(result.Provider, provider)
		if result.NoSpeech {
			continue
		}
		if !result.Success {
			return Transcript{Error: fmt.Sprintf("Segment %d/%d failed: %s", i+1, len(parts), cmpOr(result.Error, "unknown error")), Provider: provider}
		}
		if text := strings.TrimSpace(result.Text); text != "" && !Hallucination(text) {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return Transcript{Error: "Transcription returned an empty transcript", NoSpeech: true, Provider: provider}
	}
	return Transcript{Success: true, Text: strings.Join(texts, " "), Provider: provider, Segments: len(parts)}
}

// Live transcription session of the recognizer in use, nil when stt.streaming is off or it transcribes whole files only
func (j *Job) Listen(ctx context.Context, partial func(string)) (Live, string, error) {
	if !j.Config.GetBool(speechconfig.Streaming) {
		return nil, "", nil
	}
	name := j.Recognizing(ctx)
	recognizer, ok := j.Speech.Recognizer(j.Config, name)
	if !ok || recognizer.Local || recognizer.Live == nil {
		return nil, "", nil
	}
	// Live sessions take no pre_transcription hook: the recognizer's language else stt.language, and stt.prompt
	hints := Hints{Model: j.model(recognizer, ""), Prompt: strings.TrimSpace(j.Config.GetString(speechconfig.Prompt))}
	if recognizer.Language != nil {
		hints.Language = strings.TrimSpace(recognizer.Language(j))
	}
	hints.Language = cmpOr(hints.Language, j.Text(speechconfig.Language, ""))
	live, err := recognizer.Live(ctx, j, hints, partial)
	if err != nil {
		slog.Warn(fmt.Sprintf("Live STT session for %s could not open: %s", name, err))
		return nil, "", nil
	}
	return live, name, nil
}

// Log line of a transcription
func Transcribed(path, via, model string, text string) {
	slog.Info(fmt.Sprintf("Transcribed %s via %s (%s, %d chars)", filepath.Base(path), via, model, utf8.RuneCountInString(text)))
}

// Text of a command's output decoded as the whisper programs write it, a byte order mark dropped
func Decoded(data []byte) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.ToValidUTF8(string(data), "�"), "\ufeff"))
}

// Line kept of a log of what a prompt did, the backend ignoring prompts
func PromptIgnored(label string) {
	slog.Debug(label + " does not support transcription prompts — proceeding without the prompt.")
}
