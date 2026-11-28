package speech

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spf13/cast"

	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const speedKey = speechconfig.Speed

// Characters one request takes when neither the voice nor its setting says
const fallbackLimit = 4000

// Chat apps that show Ogg Opus as a voice bubble
var bubblePlatforms = strs.Set("telegram", "matrix", "feishu", "whatsapp", "signal")

// Built in upload limits by chat app, default covering the apps not named
var platformLimits = map[string]Delivery{
	"discord":  {Platform: "discord", MaxFileBytes: 10 << 20, SafetyRatio: 0.85},
	"telegram": {Platform: "telegram", MaxFileBytes: 50 << 20, SafetyRatio: 0.85},
	"default":  {Platform: "default", MaxFileBytes: 10 << 20, SafetyRatio: 0.85},
}

// What a person calls each chat app speech knows
var appTitles = map[string]string{"telegram": "Telegram", "discord": "Discord", "matrix": "Matrix", "feishu": "Feishu", "whatsapp": "WhatsApp",
	"signal": "Signal"}

var mediaDirective = regexp.MustCompile("(?i)media:\\s*[`'\"*_]*(?:[`'\"]|[a-z]:[/\\\\]|~?/)")

// Upload limits of the chat app speech goes to
type Delivery struct {
	Platform     string  `json:"platform"`
	MaxFileBytes int     `json:"max_file_bytes"`
	SafetyRatio  float64 `json:"-"`
	TargetBytes  int     `json:"target_file_bytes"`
}

// What one text_to_speech call asks
type Call struct {
	Text       string
	OutputPath string
	// Speed asked for, nil for none
	Speed        *float64
	Instructions string
	// Voice of the call, for a voice that lets the model pick
	Voice string
	// The text is speech text already, as Spoken gives it, and is read as it is
	Speech bool
}

// Result of a speech synthesis, as the model reads it when it succeeded
type Outcome struct {
	Success         bool      `json:"success"`
	Error           string    `json:"error,omitempty"`
	FilePath        string    `json:"file_path"`
	FilePaths       []string  `json:"file_paths"`
	MediaTag        string    `json:"media_tag"`
	Provider        string    `json:"provider"`
	VoiceCompatible bool      `json:"voice_compatible"`
	ChunkCount      int       `json:"chunk_count"`
	DeliveryFiles   int       `json:"delivery_file_count"`
	CombinedChunks  bool      `json:"combined_chunks"`
	DeliveryProfile *Delivery `json:"delivery_profile"`
	// A refused output path, which the model reads whole
	refusal bool
}

func failed(message string) Outcome { return Outcome{Error: message} }

func refused(message string) Outcome { return Outcome{Error: message, refusal: true} }

// Result as the model reads it: a failure with its error cut to the error bound ahead of success, a refused output path whole after it
func (o Outcome) Result() string {
	switch {
	case o.Success:
		return tools.JSON(o)
	case o.refusal:
		return tools.JSON(struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}{Error: o.Error})
	}
	return tools.JSON(map[string]any{"error": tools.BoundError(o.Error), "success": false})
}

// Name of the voice the profile speaks with, Edge when tts.provider is empty
func (j *Job) voiceName() string {
	return cmpOr(Normal(j.Config.GetString(speechconfig.TTSProvider)), speechconfig.DefaultVoice)
}

// Voice the profile speaks with: tts.provider, or Edge for a name no voice registered, which then gives no voice bubble
func (j *Job) speaker() (Voice, string) {
	name := j.voiceName()
	if voice, ok := j.Speech.Voice(j.Config, name); ok {
		return voice, name
	}
	voice, _ := j.Speech.Voice(j.Config, speechconfig.DefaultVoice)
	voice.Opus = NoOpus
	return voice, name
}

// The voice the profile reads replies aloud with, and whether it can now
type Speaking struct {
	// As tts.provider names it, Edge when it names none
	Name string
	// The voice of Name, its zero value for a name no voice has
	Voice    Voice
	Standing Standing
	// What keeps replies from being read aloud, nil while nothing does
	Hindrance *Hindrance
}

// The one answer to which voice reads the profile's replies aloud and what keeps it from it: a name no voice has, or what the voice lacks
func (j *Job) Speaking() Speaking {
	name := j.voiceName()
	voice, ok := j.Speech.Voice(j.Config, name)
	if !ok {
		return Speaking{Name: name, Standing: Standing{State: CantRun}, Hindrance: &Hindrance{Cause: "no text to speech service is called " + name,
			Lack: LacksService, Service: name}}
	}
	standing := j.VoiceStanding(voice)
	return Speaking{Name: name, Voice: voice, Standing: standing, Hindrance: standing.Hindrance}
}

// Whether the voice the profile speaks with can run now, never installing anything; a name no voice has can't
func (j *Job) CanSpeak() bool {
	return j.Speaking().Hindrance == nil
}

// Voice the profile speaks with, for the tool's schema
func (j *Job) Speaker() Voice {
	voice, _ := j.speaker()
	return voice
}

// Characters one request to a voice takes: its own setting, then what the voice says, else 4000
func (j *Job) Limit(voice Voice) int {
	if n := j.Config.GetInt(Key(speechconfig.TTS+"."+voice.Name, speechconfig.MaxTextLength)); n > 0 {
		return n
	}
	if voice.Limit != nil {
		if n := voice.Limit(j); n > 0 {
			return n
		}
	}
	return fallbackLimit
}

// Characters one request of the voice the profile speaks with takes, the cap of a service that names none for a name no voice has
func (j *Job) speakerLimit() int {
	if voice, name := j.speaker(); voice.Name == name {
		return j.Limit(voice)
	}
	return fallbackLimit
}

// Upload limits of the platform, its tts.delivery_profiles entry over the built in one
func (j *Job) delivery() Delivery {
	key := cmpOr(Normal(j.Platform), "default")
	base, ok := platformLimits[key]
	if !ok {
		base = platformLimits["default"]
	}
	base.Platform = key
	// Apps are named in any case, with spaces around
	for name, entry := range cast.ToStringMap(j.Config.Get(speechconfig.Delivery)) {
		own, ok := entry.(map[string]any)
		if !ok || Normal(name) != key {
			continue
		}
		if n := cast.ToInt(own["max_file_bytes"]); n > 0 {
			base.MaxFileBytes = n
		}
		if ratio := cast.ToFloat64(own["safety_ratio"]); ratio > 0 && ratio <= 1 {
			base.SafetyRatio = ratio
		}
	}
	base.TargetBytes = max(1, int(float64(base.MaxFileBytes)*base.SafetyRatio))
	return base
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Folder generated speech is kept in, the profile's audio cache
func (j *Job) AudioCache() string {
	return filepath.Join(j.Config.ProfileDir(j.Config.Profile()), "cache", "audio")
}

// Speaks text into audio files for the model and the chat apps: text cleaned for speech and split under the voice's limit, each piece
// written, the files packed under the platform's upload limit and named by the media tag
func (j *Job) TextToSpeech(ctx context.Context, call Call) Outcome {
	if strings.TrimSpace(call.Text) == "" {
		return failed("Text is required")
	}
	text := call.Text
	if !call.Speech {
		text = Spoken(text, 0)
	}
	if strings.TrimSpace(text) == "" {
		return failed("Text is empty after TTS cleanup")
	}
	voice, name := j.speaker()
	limit := j.speakerLimit()
	// A name no voice has speaks with Edge under its own name
	voice.Name = name
	request := Request{Instructions: call.Instructions}
	if call.Speed != nil {
		request.Speed = math.Max(0.25, math.Min(4, *call.Speed))
	}
	if picked := strings.TrimSpace(call.Voice); picked != "" {
		if voice.Picker == nil {
			slog.Warn(fmt.Sprintf("Ignoring per-call TTS voice_id; tts.provider is %q", name))
		} else if err := voice.Picker.Valid(picked); err != nil {
			return failed(err.Error())
		} else {
			request.Voice = picked
		}
	}
	chunks := Split(text, limit)
	if len(chunks) == 0 {
		return failed("Text is required")
	}
	if len(chunks) > 1 {
		slog.Info(fmt.Sprintf("TTS text for provider %s split into %d chunks (input=%d chars, cap=%d)", name, len(chunks), utf8.RuneCountInString(text), limit))
	}
	bubbles := bubblePlatforms[Normal(j.Platform)]
	profile := j.delivery()
	base, refusal, err := j.outputPath(call.OutputPath, voice, bubbles)
	if err != nil {
		return failed(fmt.Sprintf("TTS generation failed (%s): %s", voice.Name, plain(err)))
	}
	if refusal != "" {
		return refused(refusal)
	}
	made := map[string]bool{}
	var kept []string
	defer func() {
		final := map[string]bool{}
		for _, p := range kept {
			final[absolute(p)] = true
		}
		for p := range made {
			if !final[absolute(p)] {
				os.Remove(p)
			}
		}
	}()
	encoded, results, failure := j.synthesizeAll(ctx, voice, chunks, base, request, bubbles, made)
	if failure != nil {
		return *failure
	}
	compatible := true
	for _, r := range results {
		compatible = compatible && r.VoiceCompatible
	}
	deliveryBase := strings.TrimSuffix(base, filepath.Ext(base)) + filepath.Ext(encoded[0])
	paths, combined, err := j.pack(ctx, encoded, deliveryBase, profile, compatible)
	if err != nil {
		family := "TTS long-form generation failed"
		if IsConfig(err) {
			family = "TTS delivery error"
		}
		message := fmt.Sprintf("%s (%s): %s", family, voice.Name, plain(err))
		slog.Error(message)
		return failed(message)
	}
	kept = paths
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			message := fmt.Sprintf("TTS long-form generation failed (%s): %s", voice.Name, plain(err))
			slog.Error(message)
			return failed(message)
		}
		slog.Info(fmt.Sprintf("TTS audio saved: %s (%s bytes, provider: %s)", p, strs.Thousands(info.Size()), voice.Name))
	}
	return Outcome{Success: true, FilePath: paths[0], FilePaths: paths, MediaTag: mediaTag(paths, compatible), Provider: cmpOr(results[0].Provider, name),
		VoiceCompatible: compatible, ChunkCount: len(chunks), DeliveryFiles: len(paths), CombinedChunks: combined, DeliveryProfile: &profile}
}

// MEDIA lines of the files, marked as a voice bubble when they all are one
func mediaTag(paths []string, bubble bool) string {
	lines := make([]string, len(paths))
	for i, p := range paths {
		lines[i] = "MEDIA:" + p
	}
	tag := strings.Join(lines, "\n")
	if bubble {
		return "[[audio_as_voice]]\n" + tag
	}
	return tag
}

// Text of an error a person reads: a missing program and a missing file named with their reason
func plain(err error) string {
	if missing, ok := errors.AsType[*exec.Error](err); ok {
		return missing.Name + " is not installed or not on PATH"
	}
	if path, ok := errors.AsType[*fs.PathError](err); ok {
		return path.Err.Error() + ": " + path.Path
	}
	return err.Error()
}

// Kind of failure of one synthesis as the model is told it
func family(err error) string {
	switch {
	case IsConfig(err):
		return "TTS configuration error"
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "TTS dependency missing"
	}
	if _, ok := errors.AsType[*DependencyError](err); ok {
		return "TTS dependency missing"
	}
	if _, ok := errors.AsType[*exec.Error](err); ok {
		return "TTS dependency missing"
	}
	return "TTS generation failed"
}

// Unsafe characters a path never holds: control characters and line separators
func unsafePath(p string) bool {
	return strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' || r == ' ' || r == '\u0085' })
}

func traversal(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// File a call writes to: the path it gave once checked, else a timestamped one in the profile's audio cache; a refusal for a path that
// is unsafe, a media directive or a climb out of its folder
func (j *Job) outputPath(given string, voice Voice, bubbles bool) (string, string, error) {
	var path string
	if given != "" {
		switch {
		case unsafePath(given):
			return "", "output_path contains control characters or line separators; use a plain filesystem path", nil
		case mediaDirective.MatchString(given):
			return "", "output_path must not contain a media directive (MEDIA:<path>)", nil
		case traversal(given):
			return "", fmt.Sprintf("output_path contains '..' traversal component: %s. Use an absolute path or one relative to the current directory without '..'.", given), nil
		}
		path = fsx.ExpandHome(given)
		if voice.Format != "" {
			path = strings.TrimSuffix(path, filepath.Ext(path)) + "." + voice.Format
		}
		if j.Unwritable != nil {
			if reason := j.Unwritable(path); reason != "" {
				return "", fmt.Sprintf("output_path targets a protected credential or system path: %s. Choose a normal audio output location.", path), nil
			}
		}
	} else {
		ext := "mp3"
		switch {
		case voice.Format != "":
			ext = voice.Format
		case bubbles && voice.Opus == NativeOpus:
			ext = "ogg"
		}
		now := time.Now()
		path = filepath.Join(j.AudioCache(), fmt.Sprintf("tts_%s_%06d.%s", now.Format("20060102_150405"), now.Nanosecond()/1000, ext))
	}
	return path, "", os.MkdirAll(filepath.Dir(path), 0o777)
}

// Random hex of n bytes
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Speaks every piece into numbered files beside the base, recording each file touched; the outcome of the first piece that failed
func (j *Job) synthesizeAll(ctx context.Context, voice Voice, chunks []string, base string, request Request, bubbles bool,
	made map[string]bool) ([]string, []Outcome, *Outcome) {
	var encoded []string
	var results []Outcome
	stem, ext := strings.TrimSuffix(filepath.Base(base), filepath.Ext(base)), filepath.Ext(base)
	for i, chunk := range chunks {
		path := base
		if len(chunks) > 1 {
			path = filepath.Join(filepath.Dir(base), fmt.Sprintf("%s.chunk%03d%s", stem, i+1, ext))
		}
		if _, err := os.Lstat(path); err == nil {
			name := filepath.Base(path)
			path = filepath.Join(filepath.Dir(path), "."+strings.TrimSuffix(name, filepath.Ext(name))+"."+randomHex(16)+filepath.Ext(name))
		}
		made[path] = true
		piece := request
		piece.Text, piece.Path = chunk, path
		result := j.single(ctx, voice, piece, bubbles)
		if !result.Success {
			failure := failed(fmt.Sprintf("TTS chunk %d failed (%s): %s", i+1, voice.Name, cmpOr(result.Error, "unknown error")))
			return nil, nil, &failure
		}
		if info, err := os.Stat(result.FilePath); err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			failure := failed(fmt.Sprintf("TTS long-form generation failed (%s): TTS chunk %d produced no final audio: %s", voice.Name, i+1, result.FilePath))
			return nil, nil, &failure
		}
		made[result.FilePath] = true
		encoded = append(encoded, result.FilePath)
		results = append(results, result)
	}
	return encoded, results, nil
}

// Speaks one piece into its path, repairing a mislabelled Ogg and converting to a voice bubble where the voice and platform call for it
func (j *Job) single(ctx context.Context, voice Voice, request Request, bubbles bool) Outcome {
	slog.Info(fmt.Sprintf("Generating speech with %s...", cmpOr(voice.Label, voice.Title)))
	path, err := voice.Synthesize(ctx, j, request)
	if err != nil {
		if absent, ok := errors.AsType[*AbsentError](err); ok {
			return failed(absent.Message)
		}
		message := fmt.Sprintf("%s (%s): %s", family(err), voice.Name, plain(err))
		slog.Error(message)
		return failed(message)
	}
	path = cmpOr(path, request.Path)
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return failed(fmt.Sprintf("TTS generation produced no output (provider: %s)", voice.Name))
	}
	path = j.repairOgg(ctx, path)
	final, compatible := j.bubble(ctx, voice, path, bubbles)
	info, err := os.Stat(final)
	if err != nil {
		message := fmt.Sprintf("TTS generation failed (%s): %s", voice.Name, plain(err))
		slog.Error(message)
		return failed(message)
	}
	slog.Info(fmt.Sprintf("TTS audio saved: %s (%s bytes, provider: %s)", final, strs.Thousands(info.Size()), voice.Name))
	return Outcome{Success: true, FilePath: final, Provider: voice.Name, VoiceCompatible: compatible}
}

// An .ogg path holding other audio transcoded to real Ogg in place, else renamed to the extension of what it holds
func (j *Job) repairOgg(ctx context.Context, path string) string {
	if !strings.HasSuffix(path, ".ogg") {
		return path
	}
	container := sniff(path)
	if container == "" || container == "ogg" {
		return path
	}
	slog.Info(fmt.Sprintf("TTS wrote %s bytes into a .ogg path (%s) — transcoding to real Ogg/Opus", container, path))
	if repaired := j.Opus(ctx, path, path); repaired != "" {
		return repaired
	}
	honest := strings.TrimSuffix(path, ".ogg") + "." + container
	if os.Rename(path, honest) != nil {
		return path
	}
	slog.Warn(fmt.Sprintf("Could not transcode %s to Ogg/Opus — renamed to %s so the file is delivered with its real format", path, honest))
	return honest
}

// File delivered and whether it is a voice bubble: native Ogg kept, transcoded Ogg made when the platform shows bubbles, a voice that
// opted in always transcoded
func (j *Job) bubble(ctx context.Context, voice Voice, path string, bubbles bool) (string, bool) {
	ogg := strings.HasSuffix(path, ".ogg")
	switch {
	case voice.Opus == OptedOpus:
		if !ogg {
			if converted := j.Opus(ctx, path, strings.TrimSuffix(path, filepath.Ext(path))+".ogg"); converted != "" {
				path = converted
			}
		}
		return path, strings.HasSuffix(path, ".ogg")
	case voice.Opus == TranscodedOpus && bubbles && !ogg:
		if converted := j.Opus(ctx, path, strings.TrimSuffix(path, filepath.Ext(path))+".ogg"); converted != "" {
			return converted, true
		}
		return path, false
	}
	return path, voice.Opus == NativeOpus && bubbles && ogg
}

// Splits text under the cap at sentence punctuation, an over long sentence at its words and then hard, nothing dropped but repeated space
func Split(text string, limit int) []string {
	if limit <= 0 {
		limit = fallbackLimit
	}
	normalized := strings.Join(strings.Fields(text), " ")
	if normalized == "" {
		return nil
	}
	if utf8.RuneCountInString(normalized) <= limit {
		return []string{normalized}
	}
	var sentences []string
	start, previous := 0, rune(0)
	for i, r := range normalized {
		if r == ' ' && strings.ContainsRune(".!?;:,", previous) {
			sentences = append(sentences, normalized[start:i])
			start = i + 1
		}
		previous = r
	}
	sentences = append(sentences, normalized[start:])
	var expanded []string
	for _, sentence := range sentences {
		if sentence = strings.TrimSpace(sentence); sentence == "" {
			continue
		}
		if utf8.RuneCountInString(sentence) <= limit {
			expanded = append(expanded, sentence)
		} else {
			expanded = append(expanded, packUnder(strings.Fields(sentence), limit, true)...)
		}
	}
	return packUnder(expanded, limit, false)
}

// Greedy packing of pieces joined by a space under the cap, an over long piece cut hard into its own chunks when asked
func packUnder(pieces []string, limit int, cut bool) []string {
	var chunks []string
	current := ""
	for _, piece := range pieces {
		if cut && utf8.RuneCountInString(piece) > limit {
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			runes := []rune(piece)
			for i := 0; i < len(runes); i += limit {
				chunks = append(chunks, string(runes[i:min(i+limit, len(runes))]))
			}
			continue
		}
		candidate := strings.TrimSpace(current + " " + piece)
		if current != "" && utf8.RuneCountInString(candidate) > limit {
			chunks = append(chunks, current)
			current = piece
		} else {
			current = candidate
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

// Groups the encoded pieces under the target size without mixing their kinds of file
func bundles(paths []string, profile Delivery) ([][]string, error) {
	var groups [][]string
	var current []string
	size, suffix := 0, ""
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		n, ext := int(info.Size()), strings.ToLower(filepath.Ext(p))
		if len(current) > 0 && (size+n > profile.TargetBytes || ext != suffix) {
			groups = append(groups, current)
			current, size = nil, 0
		}
		current = append(current, p)
		size, suffix = size+n, ext
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups, nil
}

// Joins independently encoded pieces into the output with ffmpeg, empty when ffmpeg is missing or fails
func (j *Job) join(ctx context.Context, paths []string, output string, bubble bool) (string, error) {
	if len(paths) == 1 {
		if absolute(paths[0]) != absolute(output) {
			data, err := os.ReadFile(paths[0])
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(output, data, 0o666); err != nil {
				return "", err
			}
		}
		return output, nil
	}
	ffmpeg := j.FFmpeg(ctx)
	if ffmpeg == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o777); err != nil {
		return "", err
	}
	name := filepath.Base(output)
	list := filepath.Join(filepath.Dir(output), "."+name+"."+randomHex(16)+".concat.txt")
	work := filepath.Join(filepath.Dir(output), "."+strings.TrimSuffix(name, filepath.Ext(name))+"."+randomHex(16)+".combining"+filepath.Ext(name))
	defer os.Remove(list)
	defer os.Remove(work)
	var entries strings.Builder
	mp3 := true
	for _, p := range paths {
		entries.WriteString("file " + terminal.Quote(absolute(p)) + "\n")
		mp3 = mp3 && strings.ToLower(filepath.Ext(p)) == ".mp3"
	}
	if err := os.WriteFile(list, []byte(entries.String()), 0o666); err != nil {
		slog.Warn("ffmpeg audio combine failed: " + err.Error())
		return "", nil
	}
	argv := []string{ffmpeg, "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", list, "-vn"}
	switch suffix := strings.ToLower(filepath.Ext(output)); {
	case bubble || suffix == ".ogg" || suffix == ".opus":
		argv = append(argv, "-c:a", "libopus", "-ac", "1", "-b:a", "64k", "-vbr", "off")
	case suffix == ".mp3" && mp3:
		argv = append(argv, "-c:a", "copy")
	}
	captured, err := Run(ctx, append(argv, work), 120*time.Second, nil)
	if err != nil {
		slog.Warn("ffmpeg audio combine failed: " + strs.Head(Detail(captured, err), 500))
		return "", nil
	}
	if info, err := os.Stat(work); err != nil || info.Size() == 0 {
		slog.Warn("ffmpeg audio combine produced no audio")
		return "", nil
	}
	if err := os.Rename(work, output); err != nil {
		slog.Warn("ffmpeg audio combine failed: " + err.Error())
		return "", nil
	}
	return output, nil
}

// Packs the encoded pieces into as few files under the upload limit as joining allows; whether any were joined
func (j *Job) pack(ctx context.Context, paths []string, base string, profile Delivery, bubble bool) ([]string, bool, error) {
	if len(paths) == 0 {
		return nil, false, Misconfigured("No final-encoded TTS audio chunks")
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, false, err
		}
		if int(info.Size()) > profile.MaxFileBytes {
			return nil, false, Misconfigured(fmt.Sprintf("Final-encoded TTS chunk exceeds %s delivery limit (%d > %d bytes): %s", profile.Platform, info.Size(),
				profile.MaxFileBytes, p))
		}
	}
	stem, ext := strings.TrimSuffix(filepath.Base(base), filepath.Ext(base)), filepath.Ext(base)
	var scratch []string
	joined, index := false, 0
	var emit func(group []string) ([]string, error)
	emit = func(group []string) ([]string, error) {
		if len(group) == 1 {
			return group, nil
		}
		index++
		out := filepath.Join(filepath.Dir(base), fmt.Sprintf(".%s.delivery%03d.%s%s", stem, index, randomHex(16), ext))
		combined, err := j.join(ctx, group, out, bubble)
		if err != nil {
			return nil, err
		}
		if combined == "" {
			return group, nil
		}
		scratch = append(scratch, combined)
		info, err := os.Stat(combined)
		if err != nil {
			return nil, err
		}
		if int(info.Size()) <= profile.MaxFileBytes {
			joined = true
			return []string{combined}, nil
		}
		os.Remove(combined)
		half := max(1, len(group)/2)
		left, err := emit(group[:half])
		if err != nil {
			return nil, err
		}
		right, err := emit(group[half:])
		return append(left, right...), err
	}
	groups, err := bundles(paths, profile)
	if err != nil {
		return nil, false, err
	}
	var packed []string
	for _, g := range groups {
		files, err := emit(g)
		if err != nil {
			return nil, false, err
		}
		packed = append(packed, files...)
	}
	var final []string
	defer func() {
		for _, s := range scratch {
			if !strs.Set(final...)[s] {
				os.Remove(s)
			}
		}
	}()
	for i, source := range packed {
		destination := base
		if len(packed) > 1 {
			destination = filepath.Join(filepath.Dir(base), fmt.Sprintf("%s.part%02d%s", stem, i+1, cmpOr(filepath.Ext(source), ext)))
		}
		if absolute(source) != absolute(destination) {
			if err := os.Rename(source, destination); err != nil {
				return nil, false, err
			}
		}
		info, err := os.Stat(destination)
		if err != nil {
			return nil, false, err
		}
		if int(info.Size()) > profile.MaxFileBytes {
			return nil, false, Misconfigured(fmt.Sprintf("Final TTS deliverable exceeds %s delivery limit: %s", profile.Platform, destination))
		}
		final = append(final, destination)
	}
	return final, joined, nil
}

// Warm leases on the voice of a profile and the unload that follows the last one out
type leases struct {
	mu         sync.Mutex
	held       map[string]bool
	timer      *time.Timer
	generation int
}

// Loads the voice the profile speaks with ahead of use, the leases holding it until the last one is dropped
func (s *Speech) Acquire(ctx context.Context, job *Job, lease string) {
	s.leases.mu.Lock()
	s.cancelUnload()
	if s.leases.held == nil {
		s.leases.held = map[string]bool{}
	}
	s.leases.held[lease] = true
	s.leases.mu.Unlock()
	s.warm(ctx, job)
	s.leases.mu.Lock()
	defer s.leases.mu.Unlock()
	if len(s.leases.held) == 0 {
		s.scheduleUnload(ctx, job)
	}
}

// Drops a lease, the last one out starting the keep warm window before resident voices unload
func (s *Speech) Drop(ctx context.Context, job *Job, lease string) {
	s.leases.mu.Lock()
	defer s.leases.mu.Unlock()
	held := s.leases.held[lease]
	delete(s.leases.held, lease)
	if held && len(s.leases.held) == 0 {
		s.scheduleUnload(ctx, job)
	}
}

func (s *Speech) cancelUnload() {
	s.leases.generation++
	if s.leases.timer != nil {
		s.leases.timer.Stop()
		s.leases.timer = nil
	}
}

// Starts the keep warm window, or unloads at once for a window of zero; the lease lock held
func (s *Speech) scheduleUnload(ctx context.Context, job *Job) {
	s.cancelUnload()
	window := job.Config.GetDuration(speechconfig.KeepWarm)
	if window <= 0 {
		s.release(context.WithoutCancel(ctx), job)
		return
	}
	generation := s.leases.generation
	bound := context.WithoutCancel(ctx)
	s.leases.timer = time.AfterFunc(window, func() {
		s.leases.mu.Lock()
		defer s.leases.mu.Unlock()
		if generation != s.leases.generation || len(s.leases.held) > 0 {
			return
		}
		s.leases.timer = nil
		s.release(bound, job)
	})
}

// Readies the voice the profile speaks with: a local engine loaded, a program of yours told to warm up
func (s *Speech) warm(ctx context.Context, job *Job) {
	voice, _ := job.speaker()
	if voice.Warm == nil {
		return
	}
	started := time.Now()
	if err := voice.Warm(ctx, job); err != nil {
		slog.Warn(fmt.Sprintf("[TTS] warm-up for %s failed: %s", voice.Name, err))
		return
	}
	slog.Info(fmt.Sprintf("[TTS] warm-up %s in %dms", voice.Name, time.Since(started).Milliseconds()))
}

// Unloads every resident local voice and tells the configured program of yours it is no longer needed
func (s *Speech) release(ctx context.Context, job *Job) {
	speaker, _ := job.speaker()
	released := map[string]bool{}
	for _, voice := range s.Voices(job.Config) {
		if voice.Release == nil || released[voice.Name] || !voice.Resident && voice.Name != speaker.Name {
			continue
		}
		released[voice.Name] = true
		if err := voice.Release(ctx, job); err != nil {
			slog.Debug(fmt.Sprintf("[TTS] release hook for %s failed: %s", voice.Name, err))
		}
	}
}
