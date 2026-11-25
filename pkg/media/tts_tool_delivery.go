package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/mediacache"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

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

// Input character cap from the service's setting, the ElevenLabs model table, the service table, then the program's own cap
func resolveMaxTextLength(provider string, req *ttsRequest) int {
	key := strings.TrimSpace(strings.ToLower(provider))
	if key == "" {
		return fallbackMaxTextLength
	}
	if req != nil {
		if n := req.MaxTextLength[key]; n > 0 {
			return n
		}
		if key == "elevenlabs" {
			if mapped := elevenLabsModelMaxTextLength[textOr(req.ElevenLabs.ModelID, defaultElevenLabsModelID)]; mapped > 0 {
				return mapped
			}
		}
	}
	if n, ok := providerMaxTextLength[key]; ok {
		return n
	}
	if req != nil {
		if p := req.command(key); p != nil {
			return p.MaxTextLength
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

// Built in upload limits by chat app, default covering the apps not named
var platformAudioDefaults = map[string]deliveryLimit{
	"discord":  {MaxFileBytes: 10 * 1024 * 1024, SafetyRatio: 0.85},
	"telegram": {MaxFileBytes: 50 * 1024 * 1024, SafetyRatio: 0.85},
	"default":  {MaxFileBytes: 10 * 1024 * 1024, SafetyRatio: 0.85},
}

// Upload constraints of the platform with its tts.delivery_profiles entry laid over
func resolveAudioDeliveryProfile(platform string, req *ttsRequest) audioDeliveryProfile {
	key := "default"
	if platform != "" {
		key = strings.TrimSpace(strings.ToLower(platform))
	}
	if key == "" {
		key = "default"
	}
	base, ok := platformAudioDefaults[key]
	if !ok {
		base = platformAudioDefaults["default"]
	}
	if req != nil {
		if own, ok := req.Delivery[key]; ok {
			if own.MaxFileBytes > 0 {
				base.MaxFileBytes = own.MaxFileBytes
			}
			if own.SafetyRatio > 0 && own.SafetyRatio <= 1 {
				base.SafetyRatio = own.SafetyRatio
			}
		}
	}
	return audioDeliveryProfile{Platform: key, MaxFileBytes: base.MaxFileBytes, SafetyRatio: base.SafetyRatio}
}

// Float of a Python int or float value, false for bools and every other type
func realNumber(v any) (float64, bool) {
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
		candidate := strings.TrimSpace(current + " " + piece)
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
	return packUnderCap(strings.Fields(sentence), maxChars, true)
}

// Splits text under a provider cap at sentence punctuation without dropping normalized content
func splitTextForTTS(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = fallbackMaxTextLength
	}
	normalized := strings.Join(strings.Fields(text), " ")
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
		sentence := strings.TrimSpace(s)
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
		size, suffix := int(info.Size()), strings.ToLower(pathutil.Suffix(path))
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
	return fmt.Sprintf("%s exited with status %d", strings.Join(e.Args, " "), e.ReturnCode)
}

// Error of an ffmpeg run that outlived its timeout
type ffmpegTimeoutError struct {
	Args    []string
	Timeout int
}

func (e *ffmpegTimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %d seconds", strings.Join(e.Args, " "), e.Timeout)
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
	if strings.HasSuffix(strings.ToLower(outputPath), ".wav") {
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
		if strings.HasSuffix(strings.ToLower(outputPath), ".ogg") {
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
		pkgLog().Warn(fmt.Sprintf("ffmpeg not found; writing raw WAV to %s (extension may be misleading)", outputPath))
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
	pkgLog().Info(fmt.Sprintf("TTS wrote %s bytes into a .ogg path (%s) — transcoding to real Ogg/Opus", container, fileStr))
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
	pkgLog().Warn(fmt.Sprintf("Could not transcode %s to Ogg/Opus — renamed to %s so the file is delivered with its real format", fileStr, honest))
	return honest, nil
}

// Absolute path the way os.path.abspath spells it
func absPath(p string) string {
	a, err := filepath.Abs(p)
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
	concatPath := withName(destination, "."+pathutil.Name(destination)+"."+randomHex(16)+".concat.txt")
	tempOutput := withName(destination, "."+pathutil.Stem(destination)+"."+randomHex(16)+".combining"+pathutil.Suffix(destination))
	defer func() {
		removeQuietly(concatPath)
		removeQuietly(tempOutput)
	}()
	var entries strings.Builder
	for _, p := range audioPaths {
		entries.WriteString("file " + shellutil.Quote(absPath(p)) + "\n")
	}
	if err := os.WriteFile(concatPath, []byte(entries.String()), 0o666); err != nil {
		pkgLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", errchain.OSText(err)))
		return "", nil
	}
	args := []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", concatPath, "-vn"}
	suffix := strings.ToLower(pathutil.Suffix(destination))
	allMP3 := true
	for _, p := range audioPaths {
		allMP3 = allMP3 && strings.ToLower(pathutil.Suffix(p)) == ".mp3"
	}
	if voiceCompatible || suffix == ".ogg" || suffix == ".opus" {
		args = append(args, "-c:a", "libopus", "-ac", "1", "-b:a", "64k", "-vbr", "off")
	} else if suffix == ".mp3" && allMP3 {
		args = append(args, "-c:a", "copy")
	}
	result, err := ffmpegRun(ffmpeg, append(args, tempOutput), 120, false, true)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %v", err))
		return "", nil
	}
	if info, serr := os.Stat(tempOutput); result.ReturnCode == 0 && serr == nil && info.Size() > 0 {
		if err := os.Rename(tempOutput, destination); err != nil {
			pkgLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", errchain.OSText(err)))
			return "", nil
		}
		return destination, nil
	}
	pkgLog().Warn(fmt.Sprintf("ffmpeg audio combine failed: %s", decodeIgnoreClip(result.Stderr, 500)))
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
		scratch := withName(base, fmt.Sprintf(".%s.delivery%03d.%s%s", pathutil.Stem(base), combineIndex, randomHex(16), pathutil.Suffix(base)))
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
			suffix := pathutil.Suffix(source)
			if suffix == "" {
				suffix = pathutil.Suffix(base)
			}
			destination = withName(base, fmt.Sprintf("%s.part%02d%s", pathutil.Stem(base), index+1, suffix))
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
