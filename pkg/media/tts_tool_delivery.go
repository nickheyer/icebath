package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// Logger of the text to speech tool resolved per call so tests can swap the default handler
func ttsLog() *slog.Logger { return slog.Default().With("logger", "tools.tts_tool") }

// The tts key as a mapping, null or non mapping sections read as empty
func ttsSection(ttsConfig any, key string) map[string]any {
	m, ok := asStrMap(ttsConfig)
	if !ok {
		return map[string]any{}
	}
	sec, ok := asStrMap(m[key])
	if !ok {
		return map[string]any{}
	}
	return sec
}

const fallbackMaxTextLength = 4000

var providerMaxTextLength = map[string]int{
	"edge": 5000, "openai": 4096, "xai": 15000, "minimax": 10000, "mistral": 4000, "gemini": 32000,
	"elevenlabs": 10000, "neutts": 2000, "kittentts": 2000, "piper": 5000,
}

var elevenLabsModelMaxTextLength = map[string]int{
	"eleven_v3": 5000, "eleven_ttv_v3": 5000,
	"eleven_multilingual_v2": 10000, "eleven_multilingual_v1": 10000,
	"eleven_english_sts_v2": 10000, "eleven_english_sts_v1": 10000,
	"eleven_flash_v2": 30000, "eleven_flash_v2_5": 40000,
}

// Value when it is a positive non bool Python int, zero otherwise
func ttsPositiveInt(v any) int {
	switch n := v.(type) {
	case int:
		if n > 0 {
			return n
		}
	case int64:
		if n > 0 {
			return int(n)
		}
	case int32:
		if n > 0 {
			return int(n)
		}
	case uint64:
		if n > 0 {
			return int(n)
		}
	case uint:
		if n > 0 {
			return int(n)
		}
	}
	return 0
}

// Input character cap from the override, ElevenLabs model table, provider table, then command provider config
func resolveMaxTextLength(provider string, ttsConfig map[string]any) int {
	if provider == "" {
		return fallbackMaxTextLength
	}
	key := pystr.Strip(pystr.Lower(provider))
	cfg := ttsConfig
	if cfg == nil {
		cfg = map[string]any{}
	}
	provCfg := ttsSection(cfg, key)
	if override := ttsPositiveInt(provCfg["max_text_length"]); override > 0 {
		return override
	}
	if key == "elevenlabs" {
		modelID := any(defaultElevenLabsModelID)
		if pyTruthy(provCfg["model_id"]) {
			modelID = provCfg["model_id"]
		}
		if mapped := elevenLabsModelMaxTextLength[pystr.Strip(pyrepr.Str(modelID))]; mapped > 0 {
			return mapped
		}
	}
	if n, ok := providerMaxTextLength[key]; ok {
		return n
	}
	if !builtinTTSProviders[key] {
		named := getNamedTTSProviderConfig(cfg, key)
		if isCommandProviderConfig(named) {
			if n := ttsPositiveInt(named["max_text_length"]); n > 0 {
				return n
			}
			return defaultCommandTTSMaxTextLength
		}
	}
	return fallbackMaxTextLength
}

const (
	geminiTTSSampleRate  = 24000
	geminiTTSChannels    = 1
	geminiTTSSampleWidth = 2
)

var opusVoiceArgs = []string{"-acodec", "libopus", "-ac", "1", "-b:a", "48k", "-vbr", "on", "-application", "voip", "-compression_level", "10"}

// Destination platform constraints for generated speech audio
type audioDeliveryProfile struct {
	Platform     string
	MaxFileBytes int
	SafetyRatio  float64
}

// Conservative packing target below the platform hard limit
func (p audioDeliveryProfile) targetFileBytes() int {
	return max(1, int(float64(p.MaxFileBytes)*p.SafetyRatio))
}

var platformAudioDefaults = map[string]map[string]any{
	"discord":  {"max_file_bytes": 10 * 1024 * 1024, "safety_ratio": 0.85},
	"telegram": {"max_file_bytes": 50 * 1024 * 1024, "safety_ratio": 0.85},
	"default":  {"max_file_bytes": 10 * 1024 * 1024, "safety_ratio": 0.85},
}

// Upload constraints of the platform with tts.delivery_profiles overrides applied
func resolveAudioDeliveryProfile(platform string, ttsConfig map[string]any) audioDeliveryProfile {
	key := "default"
	if platform != "" {
		key = pystr.Strip(pystr.Lower(platform))
	}
	if key == "" {
		key = "default"
	}
	base, ok := platformAudioDefaults[key]
	if !ok {
		base = platformAudioDefaults["default"]
	}
	defaults := map[string]any{}
	for k, v := range base {
		defaults[k] = v
	}
	for k, v := range ttsSection(ttsSection(ttsConfig, "delivery_profiles"), key) {
		if v != nil {
			defaults[k] = v
		}
	}
	maxFileBytes := ttsPositiveInt(defaults["max_file_bytes"])
	if maxFileBytes == 0 {
		maxFileBytes = platformAudioDefaults["default"]["max_file_bytes"].(int)
	}
	ratio := 0.85
	if v, present := defaults["safety_ratio"]; present {
		if f, isNum := pyRealNumber(v); isNum && f > 0 && f <= 1 {
			ratio = f
		}
	}
	return audioDeliveryProfile{Platform: key, MaxFileBytes: maxFileBytes, SafetyRatio: ratio}
}

// Float of a Python int or float value, false for bools and every other type
func pyRealNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// Greedy single space packing under the cap, oversized pieces hard sliced into their own chunks when asked
func packUnderCap(pieces []string, maxChars int, sliceOversized bool) []string {
	var chunks []string
	current := ""
	for _, piece := range pieces {
		if sliceOversized && utf8.RuneCountInString(piece) > maxChars {
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			runes := []rune(piece)
			for i := 0; i < len(runes); i += maxChars {
				chunks = append(chunks, string(runes[i:min(i+maxChars, len(runes))]))
			}
			continue
		}
		candidate := pystr.Strip(current + " " + piece)
		if current != "" && utf8.RuneCountInString(candidate) > maxChars {
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

// Splits one over limit sentence on word boundaries then hard boundaries
func splitOversizedSentence(sentence string, maxChars int) []string {
	return packUnderCap(pystr.SplitWhitespace(sentence, -1), maxChars, true)
}

// Splits text under a provider cap at sentence punctuation without dropping normalized content
func splitTextForTTS(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = fallbackMaxTextLength
	}
	normalized := strings.Join(pystr.SplitWhitespace(text, -1), " ")
	if normalized == "" {
		return []string{}
	}
	if utf8.RuneCountInString(normalized) <= maxChars {
		return []string{normalized}
	}
	var sentences []string
	start := 0
	prev := rune(0)
	for i, r := range normalized {
		if r == ' ' && strings.ContainsRune(".!?;:,", prev) {
			sentences = append(sentences, normalized[start:i])
			start = i + 1
		}
		prev = r
	}
	sentences = append(sentences, normalized[start:])
	var expanded []string
	for _, s := range sentences {
		sentence := pystr.Strip(s)
		if sentence == "" {
			continue
		}
		if utf8.RuneCountInString(sentence) <= maxChars {
			expanded = append(expanded, sentence)
		} else {
			expanded = append(expanded, splitOversizedSentence(sentence, maxChars)...)
		}
	}
	return packUnderCap(expanded, maxChars, false)
}

// Groups final encoded chunks under the size target without mixing suffixes
func packAudioFilesForDelivery(audioPaths []string, profile audioDeliveryProfile) ([][]string, error) {
	var groups [][]string
	var current []string
	currentSize, currentSuffix := 0, ""
	for _, path := range audioPaths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		size, suffix := int(info.Size()), pystr.Lower(pypath.Suffix(path))
		if len(current) > 0 && (currentSize+size > profile.targetFileBytes() || suffix != currentSuffix) {
			groups = append(groups, current)
			current, currentSize = nil, 0
		}
		current = append(current, path)
		currentSize, currentSuffix = currentSize+size, suffix
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups, nil
}

// Result of one headless ffmpeg run
type ffmpegResult struct {
	ReturnCode int
	Stdout     []byte
	Stderr     []byte
}

// Error of a check enabled ffmpeg run exiting non zero
type ffmpegCalledProcessError struct {
	ReturnCode int
	Args       []string
}

func (e *ffmpegCalledProcessError) Error() string {
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pyrepr.Repr(e.Args), e.ReturnCode)
}

// Error of an ffmpeg run that outlived its timeout
type ffmpegTimeoutError struct {
	Args    []string
	Timeout int
}

func (e *ffmpegTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %d seconds", pyrepr.Repr(e.Args), e.Timeout)
}

// Runs ffmpeg with no stdin and a hidden console window, capturing output when asked
var ffmpegRun = func(ffmpeg string, args []string, timeout int, check, capture bool) (*ffmpegResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	argv := append([]string{ffmpeg}, args...)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	hideConsoleWindow(cmd)
	var stdout, stderr bytes.Buffer
	if capture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &ffmpegTimeoutError{Args: argv, Timeout: timeout}
	}
	result := &ffmpegResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ReturnCode = exitErr.ExitCode()
	} else if err != nil {
		return nil, err
	}
	if check && result.ReturnCode != 0 {
		return nil, &ffmpegCalledProcessError{ReturnCode: result.ReturnCode, Args: argv}
	}
	return result, nil
}

// Full path of an executable on PATH or empty, the shutil.which seam tests replace
var ttsWhich = func(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Path a WAV native engine writes before conversion to the requested format
func wavSidecarPath(outputPath string) string {
	if strings.HasSuffix(outputPath, ".wav") {
		return outputPath
	}
	if i := strings.LastIndex(outputPath, "."); i >= 0 {
		return outputPath[:i] + ".wav"
	}
	return outputPath + ".wav"
}

// Moves WAV output into the requested container, converting with ffmpeg or renaming without it
func finalizeWAVOutput(wavPath, outputPath string) (string, error) {
	if wavPath == outputPath {
		return outputPath, nil
	}
	ffmpeg := ttsWhich("ffmpeg")
	if ffmpeg == "" {
		if err := os.Rename(wavPath, outputPath); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	if _, err := ffmpegRun(ffmpeg, []string{"-i", wavPath, "-y", "-loglevel", "error", outputPath}, 30, true, false); err != nil {
		return "", err
	}
	removeQuietly(wavPath)
	return outputPath, nil
}

// Raw little endian PCM wrapped in a minimal RIFF WAV header
func wrapPCMAsWAV(pcm []byte, sampleRate, channels, sampleWidth int) []byte {
	blockAlign := channels * sampleWidth
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(4+24+8+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(sampleWidth*8))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// Writes in memory WAV under the output path, ffmpeg converting to its container and Opus for ogg
func writeWAVBytesAs(wavBytes []byte, outputPath string) (string, error) {
	if strings.HasSuffix(pystr.Lower(outputPath), ".wav") {
		if err := os.WriteFile(outputPath, wavBytes, 0o666); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	tmp, err := os.CreateTemp("", "tmp*.wav")
	if err != nil {
		return "", err
	}
	wavPath := tmp.Name()
	defer removeQuietly(wavPath)
	_, err = tmp.Write(wavBytes)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if ffmpeg := ttsWhich("ffmpeg"); ffmpeg != "" {
		var opus []string
		if strings.HasSuffix(pystr.Lower(outputPath), ".ogg") {
			opus = opusVoiceArgs
		}
		args := append(append([]string{"-i", wavPath}, opus...), "-y", "-loglevel", "error", outputPath)
		result, err := ffmpegRun(ffmpeg, args, 30, false, true)
		if err != nil {
			return "", err
		}
		if result.ReturnCode != 0 {
			return "", fmt.Errorf("ffmpeg conversion failed: %s", decodeIgnoreClip(result.Stderr, 300))
		}
	} else {
		ttsLog().Warn(fmt.Sprintf("ffmpeg not found; writing raw WAV to %s (extension may be misleading)", outputPath))
		if err := copyFile(wavPath, outputPath); err != nil {
			return "", err
		}
	}
	return outputPath, nil
}

// UTF-8 decode dropping invalid bytes, clipped to n code points
func decodeIgnoreClip(b []byte, n int) string {
	runes := []rune(strings.ToValidUTF8(string(b), ""))
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

// Copies file contents like shutil.copyfile
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o666)
}

// Ogg Opus transcoder of the gateway media layer, the seam tests replace
var transcodeToOggOpus = mediacache.TranscodeToOggOpus

// Path up to the last dot, the whole path when it has none
func rsplitDot(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[:i]
	}
	return p
}

// Converts any ffmpeg readable audio to Ogg Opus beside it, empty on failure
func convertToOpus(mp3Path string) (string, error) {
	return transcodeToOggOpus(context.Background(), mp3Path, "48k", 30*time.Second, rsplitDot(mp3Path)+".ogg")
}

// Container id of the file by magic bytes, unknown when unreadable or unrecognised
func sniffAudioContainer(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := f.Read(head)
	if c, ok := mediacache.SniffContainer(head[:n]); ok && c != "" {
		return c
	}
	return "unknown"
}

// Ogg path holding real Ogg, transcoding in place or renaming to the sniffed real extension
func repairOggContainer(fileStr string) (string, error) {
	container := "ogg"
	if strings.HasSuffix(fileStr, ".ogg") {
		container = sniffAudioContainer(fileStr)
	}
	if container == "ogg" || container == "unknown" {
		return fileStr, nil
	}
	ttsLog().Info(fmt.Sprintf("TTS wrote %s bytes into a .ogg path (%s) — transcoding to real Ogg/Opus", container, fileStr))
	repaired, err := transcodeToOggOpus(context.Background(), fileStr, "48k", 30*time.Second, fileStr)
	if err != nil {
		return "", err
	}
	if repaired != "" {
		return repaired, nil
	}
	honest := fileStr[:len(fileStr)-4] + "." + container
	if err := os.Rename(fileStr, honest); err != nil {
		return fileStr, nil
	}
	ttsLog().Warn(fmt.Sprintf("Could not transcode %s to Ogg/Opus — renamed to %s so the file is delivered with its real format", fileStr, honest))
	return honest, nil
}

// Absolute path the way os.path.abspath spells it
func absPath(p string) string {
	a, err := pypath.Abspath(p)
	if err != nil {
		return p
	}
	return a
}

// Path with its final component replaced like pathlib with_name
func withName(p, name string) string { return filepath.Join(filepath.Dir(p), name) }

// Combines independently encoded chunks with ffmpeg, empty when ffmpeg is missing or fails
func concatAudioFiles(audioPaths []string, outputPath string, voiceCompatible bool) (string, error) {
	if len(audioPaths) == 0 {
		return "", &ttsValueError{msg: "No audio chunks to combine"}
	}
	if len(audioPaths) == 1 {
		if absPath(audioPaths[0]) != absPath(outputPath) {
			if err := copyFile(audioPaths[0], outputPath); err != nil {
				return "", err
			}
		}
		return outputPath, nil
	}
	ffmpeg := ttsWhich("ffmpeg")
	if ffmpeg == "" {
		return "", nil
	}
	destination := outputPath
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return "", err
	}
	concatPath := withName(destination, "."+pypath.Name(destination)+"."+randomHex(16)+".concat.txt")
	tempOutput := withName(destination, "."+pypath.PathStem(destination)+"."+randomHex(16)+".combining"+pypath.Suffix(destination))
	defer func() {
		removeQuietly(concatPath)
		removeQuietly(tempOutput)
	}()
	var entries strings.Builder
	for _, p := range audioPaths {
		entries.WriteString("file " + pyshlex.Quote(absPath(p)) + "\n")
	}
	if err := os.WriteFile(concatPath, []byte(entries.String()), 0o666); err != nil {
		ttsLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", pypath.OSErrorText(err)))
		return "", nil
	}
	args := []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", concatPath, "-vn"}
	suffix := pystr.Lower(pypath.Suffix(destination))
	allMP3 := true
	for _, p := range audioPaths {
		allMP3 = allMP3 && pystr.Lower(pypath.Suffix(p)) == ".mp3"
	}
	if voiceCompatible || suffix == ".ogg" || suffix == ".opus" {
		args = append(args, "-c:a", "libopus", "-ac", "1", "-b:a", "64k", "-vbr", "off")
	} else if suffix == ".mp3" && allMP3 {
		args = append(args, "-c:a", "copy")
	}
	result, err := ffmpegRun(ffmpeg, append(args, tempOutput), 120, false, true)
	if err != nil {
		ttsLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %v", err))
		return "", nil
	}
	if info, serr := os.Stat(tempOutput); result.ReturnCode == 0 && serr == nil && info.Size() > 0 {
		if err := os.Rename(tempOutput, destination); err != nil {
			ttsLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", pypath.OSErrorText(err)))
			return "", nil
		}
		return destination, nil
	}
	ttsLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", decodeIgnoreClip(result.Stderr, 500)))
	return "", nil
}

// Chunk combiner, the seam tests replace
var ttsConcatAudioFiles = concatAudioFiles

// Packs final encoded chunks under the hard upload limit, reporting whether any group was combined
func buildAudioDeliveryFiles(audioPaths []string, outputPath string, profile audioDeliveryProfile, voiceCompatible bool) ([]string, bool, error) {
	if len(audioPaths) == 0 {
		return nil, false, &ttsValueError{msg: "No final-encoded TTS audio chunks"}
	}
	for _, path := range audioPaths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false, err
		}
		if int(info.Size()) > profile.MaxFileBytes {
			return nil, false, &ttsValueError{msg: fmt.Sprintf("Final-encoded TTS chunk exceeds %s delivery limit (%d > %d bytes): %s", profile.Platform, info.Size(), profile.MaxFileBytes, path)}
		}
	}
	base := outputPath
	var scratchOutputs []string
	combinedAny, combineIndex := false, 0
	var emit func(group []string) ([]string, error)
	emit = func(group []string) ([]string, error) {
		if len(group) == 1 {
			return append([]string(nil), group...), nil
		}
		combineIndex++
		scratch := withName(base, fmt.Sprintf(".%s.delivery%03d.%s%s", pypath.PathStem(base), combineIndex, randomHex(16), pypath.Suffix(base)))
		combined, err := ttsConcatAudioFiles(group, scratch, voiceCompatible)
		if err != nil {
			return nil, err
		}
		if combined == "" {
			return append([]string(nil), group...), nil
		}
		scratchOutputs = append(scratchOutputs, combined)
		info, err := os.Stat(combined)
		if err != nil {
			return nil, err
		}
		if int(info.Size()) <= profile.MaxFileBytes {
			combinedAny = true
			return []string{combined}, nil
		}
		removeQuietly(combined)
		midpoint := max(1, len(group)/2)
		left, err := emit(group[:midpoint])
		if err != nil {
			return nil, err
		}
		right, err := emit(group[midpoint:])
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	var finalPaths []string
	defer func() {
		for _, scratch := range scratchOutputs {
			kept := false
			for _, f := range finalPaths {
				kept = kept || f == scratch
			}
			if !kept {
				removeQuietly(scratch)
			}
		}
	}()
	groups, err := packAudioFilesForDelivery(audioPaths, profile)
	if err != nil {
		return nil, false, err
	}
	var packed []string
	for _, group := range groups {
		paths, err := emit(group)
		if err != nil {
			return nil, false, err
		}
		packed = append(packed, paths...)
	}
	for index, source := range packed {
		destination := base
		if len(packed) > 1 {
			suffix := pypath.Suffix(source)
			if suffix == "" {
				suffix = pypath.Suffix(base)
			}
			destination = withName(base, fmt.Sprintf("%s.part%02d%s", pypath.PathStem(base), index+1, suffix))
		}
		if absPath(source) != absPath(destination) {
			if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
				return nil, false, err
			}
			if err := os.Rename(source, destination); err != nil {
				return nil, false, err
			}
		}
		info, err := os.Stat(destination)
		if err != nil {
			return nil, false, err
		}
		if int(info.Size()) > profile.MaxFileBytes {
			return nil, false, &ttsValueError{msg: fmt.Sprintf("Final TTS deliverable exceeds %s delivery limit: %s", profile.Platform, destination)}
		}
		finalPaths = append(finalPaths, destination)
	}
	return finalPaths, combinedAny, nil
}
