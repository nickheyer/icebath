package media

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	sttMB                      = 1024 * 1024
	sttSegmentHeadroom         = 0.9
	sttMaxSilenceLookbackSecs  = 60.0
	sttElevenLabsMaxUploadSize = 5*1024*sttMB - 1
)

var (
	sttProviderMaxBytes = map[string]int64{"openai": 25 * sttMB, "groq": 25 * sttMB, "mistral": 500 * sttMB, "xai": 500 * sttMB,
		"elevenlabs": sttElevenLabsMaxUploadSize}
	sttProviderMaxSeconds    = map[string]float64{"mistral": 3600.0}
	sttOpenAIModelMaxSeconds = map[string]float64{"gpt-4o-transcribe": 450.0, "gpt-4o-mini-transcribe": 450.0, "whisper-1": 600.0}
	silenceStartRe           = regexp.MustCompile(`silence_start: (-?[\d.]+)`)
	silenceEndRe             = regexp.MustCompile(`silence_end: ([\d.]+)`)
)

// Byte cap and optional duration cap of one upload request
type sttUploadLimit struct {
	MaxBytes   int64
	MaxSeconds float64
	HasSeconds bool
}

// Byte and duration caps of one request to a provider with a model
func uploadLimit(provider, model string) sttUploadLimit {
	limit := sttUploadLimit{MaxBytes: sttMaxFileSize}
	if b, ok := sttProviderMaxBytes[provider]; ok {
		limit.MaxBytes = b
	}
	limit.MaxSeconds, limit.HasSeconds = sttProviderMaxSeconds[provider]
	if provider == "openai" {
		limit.MaxSeconds, limit.HasSeconds = sttOpenAIModelMaxSeconds[model]
	}
	return limit
}

// File size in bytes, zero when unreadable
func sttFileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// Whether a file fits both caps, an unknown duration left for the vendor to judge
func sttFits(path string, limit sttUploadLimit, duration float64, known bool) bool {
	if sttFileSize(path) > limit.MaxBytes {
		return false
	}
	return !limit.HasSeconds || !known || duration <= limit.MaxSeconds
}

// Whether a file is over the byte cap or over a duration cap ffprobe can measure
func exceedsUploadLimit(path string, limit sttUploadLimit) bool {
	if sttFileSize(path) > limit.MaxBytes {
		return true
	}
	if !limit.HasSeconds {
		return false
	}
	duration, known := probeAudioDuration(path)
	return !sttFits(path, limit, duration, known)
}

// Error envelope of a file that could not be brought under the byte cap
func sttTooLarge(path string, maxBytes int64, reason string) map[string]any {
	return sttError(fmt.Sprintf("File too large: %.1fMB (max %.0fMB) and it could not be split: %s",
		float64(sttFileSize(path))/sttMB, float64(maxBytes)/sttMB, reason))
}

// Pause midpoints ffmpeg silencedetect reports, empty when detection fails
func silenceMidpoints(ffmpeg, path string) ([]float64, error) {
	_, stderr, err := sttRunQuiet([]string{ffmpeg, "-hide_banner", "-nostats", "-i", path,
		"-af", "silencedetect=noise=-35dB:d=0.4", "-f", "null", "-"}, 300, nil)
	if err != nil {
		sttLog().Debug(fmt.Sprintf("silencedetect failed for %s: %v", path, err))
		return []float64{}, nil
	}
	parse := func(re *regexp.Regexp) ([]float64, error) {
		var out []float64
		for _, m := range re.FindAllStringSubmatch(stderr, -1) {
			f, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return nil, fmt.Errorf("could not convert string to float: %s", pyrepr.StrRepr(m[1]))
			}
			out = append(out, f)
		}
		return out, nil
	}
	starts, err := parse(silenceStartRe)
	if err != nil {
		return nil, err
	}
	ends, err := parse(silenceEndRe)
	if err != nil {
		return nil, err
	}
	mids := []float64{}
	for i := 0; i < len(starts) && i < len(ends); i++ {
		mids = append(mids, (math.Max(starts[i], 0.0)+ends[i])/2)
	}
	return mids, nil
}

// Cut times giving segments no longer than target, each at the latest pause before the ideal cut
func cutPoints(duration, target float64, silences []float64) []float64 {
	cuts := []float64{}
	position := 0.0
	lookback := math.Min(sttMaxSilenceLookbackSecs, target/4)
	for duration-position > target {
		ideal := position + target
		best, found := 0.0, false
		for _, s := range silences {
			if ideal-lookback <= s && s <= ideal && s > position && (!found || s > best) {
				best, found = s, true
			}
		}
		if found {
			position = best
		} else {
			position = ideal
		}
		cuts = append(cuts, position)
	}
	return cuts
}

// Stream copy segmenting at the cut times, returning the part files in name order
func splitAudio(ffmpeg, path, workDir string, cuts []float64) ([]string, error) {
	times := make([]string, len(cuts))
	for i, c := range cuts {
		times[i] = fmt.Sprintf("%.3f", c)
	}
	pattern := filepath.Join(workDir, "part%03d.m4a")
	if _, _, err := sttRunQuiet([]string{ffmpeg, "-y", "-loglevel", "error", "-i", path, "-vn", "-c", "copy", "-f", "segment",
		"-segment_times", strings.Join(times, ","), "-reset_timestamps", "1", pattern}, 300, nil); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, err
	}
	parts := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "part") {
			parts = append(parts, filepath.Join(workDir, e.Name()))
		}
	}
	sort.Strings(parts)
	return parts, nil
}

// Fits a file under the limit by re-encoding, else splitting at silences, and dispatches each piece
func transcribeOversized(path string, limit sttUploadLimit, dispatch func(string) (map[string]any, error)) (map[string]any, error) {
	ffmpeg := findFFmpegBinary()
	if ffmpeg == "" {
		return sttTooLarge(path, limit.MaxBytes, "ffmpeg was not found"), nil
	}
	workDir, err := os.MkdirTemp("", "icebath-stt-split-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)
	compact, transcodeErr := transcodeAudioForSTT(path, workDir)
	if transcodeErr != "" || compact == "" {
		if transcodeErr == "" {
			transcodeErr = "re-encoding produced no file"
		}
		return sttTooLarge(path, limit.MaxBytes, transcodeErr), nil
	}
	duration, known := probeAudioDuration(compact)
	if sttFits(compact, limit, duration, known) {
		sttLog().Info(fmt.Sprintf("Re-encoded oversized audio %s to %.1fMB for a single STT upload", filepath.Base(path), float64(sttFileSize(compact))/sttMB))
		return dispatch(compact)
	}
	if !known || duration == 0 {
		return sttTooLarge(path, limit.MaxBytes, "its duration could not be read"), nil
	}
	target := float64(limit.MaxBytes) * sttSegmentHeadroom * duration / float64(sttFileSize(compact))
	if limit.HasSeconds && limit.MaxSeconds != 0 {
		target = math.Min(target, limit.MaxSeconds*sttSegmentHeadroom)
	}
	silences, err := silenceMidpoints(ffmpeg, compact)
	if err != nil {
		return nil, err
	}
	parts, err := splitAudio(ffmpeg, compact, workDir, cutPoints(duration, target, silences))
	if err != nil {
		return nil, err
	}
	sttLog().Info(fmt.Sprintf("Transcribing %s (%.0fs) in %d segments of <= %.0fs", filepath.Base(path), duration, len(parts), math.Ceil(target)))
	transcripts := []string{}
	provider := ""
	for index, part := range parts {
		result, err := dispatch(part)
		if err != nil {
			return nil, err
		}
		if p := result["provider"]; pyTruthy(p) {
			provider = pyrepr.Str(p)
		}
		if pyTruthy(result["no_speech"]) {
			continue
		}
		if !pyTruthy(result["success"]) {
			detail, ok := result["error"]
			if !ok {
				detail = "unknown error"
			}
			return sttError(fmt.Sprintf("Segment %d/%d failed: %s", index+1, len(parts), pyrepr.Str(detail)), "provider", provider), nil
		}
		text := ""
		if t := result["transcript"]; pyTruthy(t) {
			text = pystr.Strip(pyrepr.Str(t))
		}
		if text != "" && !isWhisperHallucination(text) {
			transcripts = append(transcripts, text)
		}
	}
	if len(transcripts) == 0 {
		return sttError("Transcription returned an empty transcript", "no_speech", true, "provider", provider), nil
	}
	out := sttOK(strings.Join(transcripts, " "), provider)
	out["segments"] = len(parts)
	return out, nil
}
