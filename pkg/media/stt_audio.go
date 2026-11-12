package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	cloudTrimThresholdDBDefault = -40
	cloudTrimKeepMSDefault      = 300
	cloudTrimMinSaving          = 0.10
	cloudTrimMinResultSeconds   = 0.3
	cloudTrimMinInputSeconds    = 12.0
)

var sttM4AEncodeArgs = []string{"-vn", "-ac", "1", "-ar", "16000", "-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart"}

// Executable in the Homebrew or local prefixes first, then PATH, empty when absent
var sttFindBinary = func(name string) string {
	for _, dir := range sttCommonLocalBinDirs {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil && pyfs.Access(candidate, 1) {
			return candidate
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func findFFmpegBinary() string { return sttFindBinary("ffmpeg") }

// Shared ffmpeg discovery over PATH and the Homebrew and local prefixes, empty when absent
func FindFFmpegBinary() string  { return findFFmpegBinary() }
func findFFprobeBinary() string { return sttFindBinary("ffprobe") }
func findWhisperBinary() string { return sttFindBinary("whisper") }

// Text mode output decoding with utf-8 replacement and universal newlines
func sttDecodeOutput(b []byte) string {
	s, _ := pycodecs.DecodeReplace(b, "utf-8")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Python OSError text of a helper that failed to start
type sttSpawnError struct {
	cmd string
	err error
}

func (e *sttSpawnError) Error() string {
	if errors.Is(e.err, exec.ErrNotFound) || errors.Is(e.err, fs.ErrNotExist) {
		return "[Errno 2] No such file or directory: " + pyrepr.StrRepr(e.cmd)
	}
	return pypath.OSErrorText(e.err)
}
func (e *sttSpawnError) Unwrap() error { return e.err }

// Checked, captured, no stdin, hidden window run of an STT helper binary, the seam tests replace
var sttRunQuiet = func(command []string, timeout float64, env []string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	hideConsoleWindow(cmd)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", "", &sttSpawnError{cmd: command[0], err: err}
	}
	err := cmd.Wait()
	out, errOut := sttDecodeOutput(stdout.Bytes()), sttDecodeOutput(stderr.Bytes())
	if ctx.Err() == context.DeadlineExceeded {
		return out, errOut, &sttTimeoutError{Cmd: command, Timeout: timeout}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out, errOut, &sttProcessError{Cmd: command, ReturnCode: exitErr.ExitCode(), Stdout: out, Stderr: errOut}
		}
		return out, errOut, err
	}
	return out, errOut, nil
}

// Shared 16 kHz mono 32 kbps AAC encode with an optional audio filter
func runFFmpegSTTEncode(ffmpeg, inputPath, outputPath, audioFilter string) error {
	command := []string{ffmpeg, "-y", "-i", inputPath}
	if audioFilter != "" {
		command = append(command, "-af", audioFilter)
	}
	command = append(append(command, sttM4AEncodeArgs...), outputPath)
	_, _, err := sttRunQuiet(command, 120, nil)
	return err
}

// Stem of a path like PurePath.stem
func sttStem(p string) string {
	base := filepath.Base(p)
	if p == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[:i]
	}
	return base
}

// Lowercased suffix of a path like PurePath.suffix
func sttSuffix(p string) string {
	base := filepath.Base(p)
	if i := strings.LastIndex(base, "."); i > 0 && i < len(base)-1 {
		return base[i:]
	}
	return ""
}

// Compact 16 kHz mono m4a copy for STT upload, or the error text
func transcodeAudioForSTT(filePath, workDir string) (string, string) {
	ffmpeg := findFFmpegBinary()
	if ffmpeg == "" {
		return "", "audio needs transcoding for the STT API, but ffmpeg was not found"
	}
	stem := sttStem(filePath)
	if stem == "" {
		stem = "audio"
	}
	converted := filepath.Join(workDir, stem+"-stt.m4a")
	err := runFFmpegSTTEncode(ffmpeg, filePath, converted, "")
	if err == nil {
		return converted, ""
	}
	var pe *sttProcessError
	if errors.As(err, &pe) {
		details := processErrorDetail(pe)
		sttLog().Error(fmt.Sprintf("ffmpeg STT transcode failed for %s: %s", filePath, details))
		return "", "failed to transcode audio for the STT API: " + details
	}
	sttLog().Error(fmt.Sprintf("unexpected STT transcode failure for %s: %v", filePath, err))
	return "", fmt.Sprintf("failed to transcode audio for the STT API: %v", err)
}

// Error when the path is inaccessible or, when enforced, over the remote upload cap
func validateAudioFileSize(audioPath string, enforce bool) map[string]any {
	st, err := os.Stat(audioPath)
	if err != nil {
		return sttError("Failed to access file: " + pypath.OSErrorText(err))
	}
	if enforce && st.Size() > sttMaxFileSize {
		return sttError(fmt.Sprintf("File too large: %.1fMB (max %.0fMB)", float64(st.Size())/(1024*1024), float64(sttMaxFileSize)/(1024*1024)))
	}
	return nil
}

// Source path safety and optional size check before any decoder runs
func validateAudioSourceFile(filePath string, enforce bool) map[string]any {
	if st, err := os.Lstat(filePath); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return sttError("Path is a symbolic link: " + filePath)
	}
	st, err := os.Stat(filePath)
	if err != nil {
		return sttError("Audio file not found: " + filePath)
	}
	if !st.Mode().IsRegular() {
		return sttError("Path is not a file: " + filePath)
	}
	return validateAudioFileSize(filePath, enforce)
}

// Supported, decoder safe audio file check
func validateAudioFile(filePath string, enforce bool) map[string]any {
	sourceError := validateAudioSourceFile(filePath, enforce)
	suffix := sttSuffix(filePath)
	if sourceError != nil || sttSupportedFormats[strings.ToLower(suffix)] {
		return sourceError
	}
	formats := make([]string, 0, len(sttSupportedFormats))
	for f := range sttSupportedFormats {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	return sttError(fmt.Sprintf("Unsupported format: %s. Supported: %s", suffix, strings.Join(formats, ", ")))
}

const silkToWAVScript = `import json, sys
try:
    import pilk
    pilk.silk_to_wav(sys.argv[1], sys.argv[2])
except BaseException as exc:
    sys.stdout.write(json.dumps({"error": str(exc)}))
    sys.exit(1)
`

// Error raised inside the managed interpreter, carrying the Python message
type sttPythonError struct{ msg string }

func (e *sttPythonError) Error() string { return e.msg }

// pilk silk_to_wav in the managed interpreter, the seam tests replace
var sttSilkToWAV = func(ctx context.Context, src, dst string) error {
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return err
	}
	stdout, _, err := sttRunQuiet(append(append([]string(nil), python...), "-c", silkToWAVScript, src, dst), 300, nil)
	if err != nil {
		var pe *sttProcessError
		if errors.As(err, &pe) {
			var reply struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(stdout), &reply) == nil {
				return &sttPythonError{msg: reply.Error}
			}
		}
		return err
	}
	return nil
}

// Temporary WAV of a silk source with its cleanup directory, other inputs unchanged
func prepareAudioForTranscription(ctx context.Context, filePath string) (string, string, map[string]any) {
	if strings.ToLower(sttSuffix(filePath)) != ".silk" {
		return filePath, "", nil
	}
	if !hasPilk() {
		lazyEnsureQuietly(ctx, "silk")
		if !safeFindSpec("pilk") {
			return "", "", sttError("Unsupported format: .silk. Install the optional 'pilk' dependency to enable WeChat voice transcription.")
		}
	}
	tempDir, err := os.MkdirTemp("", "icebath-silk-")
	if err != nil {
		return "", "", sttError("Failed to convert .silk audio for transcription: " + pypath.OSErrorText(err))
	}
	converted := filepath.Join(tempDir, sttStem(filePath)+".wav")
	err = sttSilkToWAV(ctx, filePath, converted)
	if err == nil {
		if st, serr := os.Stat(converted); serr != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			err = &sttPythonError{msg: "pilk did not produce a readable WAV file"}
		}
	}
	if err != nil {
		os.RemoveAll(tempDir)
		sttLog().Error(fmt.Sprintf("Failed to convert .silk audio %s: %v", filePath, err))
		return "", "", sttError(fmt.Sprintf("Failed to convert .silk audio for transcription: %v", err))
	}
	return converted, tempDir, nil
}

// WAV copy for local CLI STT when the input is not natively readable, the error text, or an escaped failure
func prepareLocalAudio(filePath, workDir string) (string, string, error) {
	if sttLocalNativeAudioFormats[strings.ToLower(sttSuffix(filePath))] {
		return filePath, "", nil
	}
	ffmpeg := findFFmpegBinary()
	if ffmpeg == "" {
		return "", "Local STT fallback requires ffmpeg for non-WAV inputs, but ffmpeg was not found", nil
	}
	converted := filepath.Join(workDir, sttStem(filePath)+".wav")
	_, _, err := sttRunQuiet([]string{ffmpeg, "-y", "-i", filePath, converted}, 300, nil)
	var te *sttTimeoutError
	var pe *sttProcessError
	switch {
	case err == nil:
		return converted, "", nil
	case errors.As(err, &te):
		sttLog().Error("ffmpeg conversion timed out for " + filePath)
		return "", "Audio conversion for local STT timed out", nil
	case errors.As(err, &pe):
		details := processErrorDetail(pe)
		sttLog().Error(fmt.Sprintf("ffmpeg conversion failed for %s: %s", filePath, details))
		return "", "Failed to convert audio for local STT: " + details, nil
	}
	return "", "", err
}

// Afconvert lookup on PATH, the seam tests replace
var sttWhich = func(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// WAV conversion of a CAF file in a caller owned directory via ffmpeg or afconvert, empty on failure
func convertCAFToWAV(filePath, workDir string) (string, error) {
	wavPath := filepath.Join(workDir, sttStem(filePath)+".wav")
	ffmpeg := findFFmpegBinary()
	afconvert := sttWhich("afconvert")
	type candidate struct {
		label   string
		command []string
	}
	var candidates []candidate
	if ffmpeg != "" {
		candidates = append(candidates, candidate{"ffmpeg", []string{ffmpeg, "-y", "-i", filePath, wavPath}})
	}
	if afconvert != "" {
		candidates = append(candidates, candidate{"afconvert", []string{afconvert, filePath, wavPath, "-d", "LEI16", "-f", "WAVE"}})
	}
	for _, c := range candidates {
		_, _, err := sttRunQuiet(c.command, 300, nil)
		if err == nil {
			return wavPath, nil
		}
		var te *sttTimeoutError
		var pe *sttProcessError
		if !errors.As(err, &te) && !errors.As(err, &pe) {
			return "", err
		}
		sttLog().Warn(fmt.Sprintf("%s CAF to WAV failed for %s: %v", c.label, filePath, err))
	}
	return "", nil
}

// Audio duration in seconds via ffprobe, false when unavailable
func probeAudioDuration(filePath string) (float64, bool) {
	ffprobe := findFFprobeBinary()
	if ffprobe == "" {
		return 0, false
	}
	stdout, _, err := sttRunQuiet([]string{ffprobe, "-v", "error", "-show_entries", "format=duration", "-of",
		"default=noprint_wrappers=1:nokey=1", filePath}, 30, nil)
	if err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(pystr.Strip(stdout), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// Enabled flag, threshold dB and keep milliseconds of the cloud silence trim
func cloudTrimSettings(sttConfig map[string]any) (bool, int, int) {
	cfg := sttConfig
	if cfg == nil {
		cfg = map[string]any{}
	}
	enabled := isTruthyValue(dictGetDefault(cfg, "cloud_trim_silence", true), true)
	threshold := configNumberInt(cfg, "cloud_trim_threshold_db", cloudTrimThresholdDBDefault)
	keep := configNumberInt(cfg, "cloud_trim_keep_ms", cloudTrimKeepMSDefault)
	return enabled, threshold, max(keep, 0)
}

// Silence trimmed copy for cloud upload, empty meaning upload the original, the caller owns its parent dir
func trimSilenceForCloudSTT(filePath string, sttConfig map[string]any) string {
	enabled, thresholdDB, keepMS := cloudTrimSettings(sttConfig)
	if !enabled {
		return ""
	}
	ffmpeg := findFFmpegBinary()
	if ffmpeg == "" {
		sttLog().Debug("Cloud STT silence trim skipped: ffmpeg not found")
		return ""
	}
	original, ok := probeAudioDuration(filePath)
	if !ok || original <= 0 {
		sttLog().Debug("Cloud STT silence trim skipped: could not probe " + filePath)
		return ""
	}
	name := filepath.Base(filePath)
	if original < cloudTrimMinInputSeconds {
		sttLog().Debug(fmt.Sprintf("Cloud STT silence trim skipped for %s: %.1fs is below the %.0fs gate", name, original, cloudTrimMinInputSeconds))
		return ""
	}
	keepSeconds := float64(keepMS) / 1000.0
	keep := pyrepr.Str(keepSeconds)
	filter := fmt.Sprintf("silenceremove=start_periods=1:start_threshold=%ddB:start_silence=%s:stop_periods=-1:stop_threshold=%ddB:stop_silence=%s",
		thresholdDB, keep, thresholdDB, keep)
	workDir, err := os.MkdirTemp("", "icebath-stt-trim-")
	if err != nil {
		sttLog().Debug(fmt.Sprintf("Cloud STT silence trim failed for %s: %s", filePath, pypath.OSErrorText(err)))
		return ""
	}
	stem := sttStem(filePath)
	if stem == "" {
		stem = "audio"
	}
	trimmed := filepath.Join(workDir, stem+"-trimmed.m4a")
	minResult := math.Max(cloudTrimMinResultSeconds, 2*keepSeconds)
	keepResult := false
	defer func() {
		if !keepResult {
			os.RemoveAll(workDir)
		}
	}()
	if err := runFFmpegSTTEncode(ffmpeg, filePath, trimmed, filter); err != nil {
		sttLog().Debug(fmt.Sprintf("Cloud STT silence trim failed for %s: %v", filePath, err))
		return ""
	}
	trimmedDuration, _ := probeAudioDuration(trimmed)
	if trimmedDuration == 0 || trimmedDuration < minResult {
		sttLog().Debug(fmt.Sprintf("Cloud STT silence trim discarded for %s: trimmed result ~empty (%.2fs)", name, trimmedDuration))
		return ""
	}
	if trimmedDuration > original*(1-cloudTrimMinSaving) {
		sttLog().Debug(fmt.Sprintf("Cloud STT silence trim discarded for %s: saves <%.0f%% (%.1fs -> %.1fs)", name, cloudTrimMinSaving*100, original, trimmedDuration))
		return ""
	}
	sttLog().Info(fmt.Sprintf("Trimmed silence from %s before cloud STT upload (%.1fs -> %.1fs, -%d%%)", name, original, trimmedDuration,
		int(math.RoundToEven((1-trimmedDuration/original)*100))))
	keepResult = true
	return trimmed
}
