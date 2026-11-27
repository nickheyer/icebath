package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	speechpackages "github.com/nickheyer/icebath/internal/speech/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Folders a Homebrew or hand built ffmpeg sits in that a program started from a desktop may not have on its PATH
var localBins = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// Program of the name in the local folders, else on PATH, else in the package store, installed now when the profile allows installs on
// first use; empty when there is none
func (j *Job) program(ctx context.Context, name, pkg string) string {
	for _, dir := range localBins {
		if candidate := filepath.Join(dir, name); fsx.Executable(candidate) {
			return candidate
		}
	}
	if pkg == "" {
		found, _ := exec.LookPath(name)
		return found
	}
	return j.Speech.packages.Program(ctx, j.Config, pkg, name, true)
}

// ffmpeg of this machine, empty when there is none
func (j *Job) FFmpeg(ctx context.Context) string {
	return j.program(ctx, "ffmpeg", speechpackages.FFmpeg)
}

// ffprobe beside ffmpeg, else of this machine, empty when there is none
func (j *Job) FFprobe(ctx context.Context) string {
	if ffmpeg := j.FFmpeg(ctx); ffmpeg != "" {
		beside := filepath.Join(filepath.Dir(ffmpeg), "ffprobe"+filepath.Ext(ffmpeg))
		if fsx.Executable(beside) {
			return beside
		}
	}
	return j.program(ctx, "ffprobe", "")
}

// Runs a helper program with its output captured, an error naming it for a failure, a non-zero exit or a time out
func Run(ctx context.Context, argv []string, timeout time.Duration, env []string) (*terminal.Captured, error) {
	captured, err := terminal.Capture(ctx, argv, timeout, env)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return nil, fmt.Errorf("%s timed out after %d seconds", strings.Join(argv, " "), int(timeout.Seconds()))
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%s is not installed or not on PATH: %w", argv[0], err)
	case err != nil:
		return nil, err
	}
	return captured, captured.Err(argv)
}

// What a failed helper said, its error output first, then its output, else the error itself
func Detail(captured *terminal.Captured, err error) string {
	if exited, ok := errors.AsType[*terminal.ExitError](err); ok {
		if detail := strings.TrimSpace(exited.Stderr); detail != "" {
			return detail
		}
	}
	if captured != nil {
		for _, text := range []string{captured.Stderr, captured.Stdout} {
			if detail := strings.TrimSpace(text); detail != "" {
				return detail
			}
		}
	}
	return err.Error()
}

// Raw little endian PCM in a RIFF WAV
func WAV(pcm []byte, rate, channels, width int) []byte {
	align := channels * width
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*align))
	binary.Write(&b, binary.LittleEndian, uint16(align))
	binary.Write(&b, binary.LittleEndian, uint16(width*8))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// Header and frames of a PCM WAV
type Wave struct {
	Channels, Width, Rate int
	Frames                []byte
}

// PCM WAV a file holds, extensible headers of plain PCM included
func ReadWave(path string) (*Wave, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseWave(data)
}

func ParseWave(data []byte) (*Wave, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a WAVE file")
	}
	end := min(len(data), 8+int(binary.LittleEndian.Uint32(data[4:8])))
	var w Wave
	formatRead := false
	for at := 12; at+8 <= end; {
		name, size := string(data[at:at+4]), int(binary.LittleEndian.Uint32(data[at+4:at+8]))
		body := data[at+8 : min(end, at+8+size)]
		switch name {
		case "fmt ":
			if len(body) < 16 {
				return nil, io.ErrUnexpectedEOF
			}
			tag := binary.LittleEndian.Uint16(body[0:2])
			if tag != 1 && tag != 0xFFFE {
				return nil, fmt.Errorf("unknown WAVE format %d", tag)
			}
			w.Channels, w.Rate = int(binary.LittleEndian.Uint16(body[2:4])), int(binary.LittleEndian.Uint32(body[4:8]))
			w.Width = (int(binary.LittleEndian.Uint16(body[14:16])) + 7) / 8
			if w.Width == 0 || w.Channels == 0 {
				return nil, errors.New("bad WAVE sample width or channel count")
			}
			formatRead = true
		case "data":
			if !formatRead {
				return nil, errors.New("WAVE data chunk before its fmt chunk")
			}
			frame := w.Channels * w.Width
			w.Frames = body[:min(len(body), size/frame*frame)]
			return &w, nil
		}
		at += 8 + size + size%2
	}
	return nil, errors.New("WAVE fmt chunk and/or data chunk missing")
}

// Container of audio bytes by their magic, empty when they match none
func Container(head []byte) string {
	switch {
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		if len(head) >= 12 {
			if brand := strings.ToLower(string(head[8:12])); brand == "m4a " || brand == "m4b " {
				return "m4a"
			}
		}
		return "mp4"
	case bytes.HasPrefix(head, []byte("OggS")):
		return "ogg"
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "flac"
	case bytes.HasPrefix(head, []byte("ID3")):
		return "mp3"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "wav"
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		if head[1]&0xF6 == 0xF0 {
			return "aac"
		}
		return "mp3"
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "webm"
	}
	return ""
}

// Container of a file by its first bytes
func sniff(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := f.Read(head)
	return Container(head[:n])
}

// Arguments of the voip tuned mono Opus encode
var opusArgs = []string{"-acodec", "libopus", "-ac", "1", "-b:a", "48k", "-vbr", "on", "-application", "voip", "-compression_level", "10"}

// Encodes audio ffmpeg reads into voip tuned Ogg Opus at the path, in place when it is the source; empty when ffmpeg is missing or fails
func (j *Job) Opus(ctx context.Context, source, output string) string {
	ffmpeg := j.FFmpeg(ctx)
	if ffmpeg == "" {
		return ""
	}
	work := output
	inPlace := absolute(source) == absolute(output)
	if inPlace {
		work = output + ".tmp.ogg"
	}
	argv := append(append([]string{ffmpeg, "-v", "error", "-y", "-i", source}, opusArgs...), "-f", "ogg", work)
	captured, err := Run(ctx, argv, 30*time.Second, nil)
	if err != nil {
		os.Remove(work)
		slog.Warn(fmt.Sprintf("ffmpeg Ogg/Opus transcode of %s failed: %s", source, strs.Head(Detail(captured, err), 500)))
		return ""
	}
	if info, err := os.Stat(work); err != nil || info.Size() == 0 {
		os.Remove(work)
		return ""
	}
	if inPlace {
		if err := os.Rename(work, output); err != nil {
			os.Remove(work)
			return ""
		}
	}
	return output
}

// Path made absolute, as it is when that fails
func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Path a native engine writes WAV to before it becomes the requested container
func WAVPath(output string) string {
	if strings.HasSuffix(output, ".wav") {
		return output
	}
	return strings.TrimSuffix(output, filepath.Ext(output)) + ".wav"
}

// Moves the WAV an engine wrote into the requested container, converted with ffmpeg, renamed as it is without one
func (j *Job) FromWAV(ctx context.Context, wav, output string) (string, error) {
	if wav == output {
		return output, nil
	}
	ffmpeg := j.FFmpeg(ctx)
	if ffmpeg == "" {
		return output, os.Rename(wav, output)
	}
	if _, err := Run(ctx, []string{ffmpeg, "-i", wav, "-y", "-loglevel", "error", output}, 30*time.Second, nil); err != nil {
		return "", err
	}
	os.Remove(wav)
	return output, nil
}

// Writes WAV bytes under the path, converted with ffmpeg to the path's container and to voip Opus for .ogg, the raw WAV kept under the
// path's name without ffmpeg
func (j *Job) WriteWAV(ctx context.Context, wav []byte, output string) (string, error) {
	if strings.HasSuffix(strings.ToLower(output), ".wav") {
		return output, os.WriteFile(output, wav, 0o666)
	}
	ffmpeg := j.FFmpeg(ctx)
	if ffmpeg == "" {
		slog.Warn(fmt.Sprintf("ffmpeg not found; writing raw WAV to %s (extension may be misleading)", output))
		return output, os.WriteFile(output, wav, 0o666)
	}
	tmp, err := os.CreateTemp("", "speech-*.wav")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(wav)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	argv := []string{ffmpeg, "-i", tmp.Name()}
	if strings.HasSuffix(strings.ToLower(output), ".ogg") {
		argv = append(argv, opusArgs...)
	}
	argv = append(argv, "-y", "-loglevel", "error", output)
	if captured, err := Run(ctx, argv, 30*time.Second, nil); err != nil {
		return "", fmt.Errorf("ffmpeg conversion failed: %s", strs.Head(Detail(captured, err), 300))
	}
	return output, nil
}

// Seconds of audio a file holds as ffprobe reads it, false when it can't
func (j *Job) Duration(ctx context.Context, path string) (float64, bool) {
	ffprobe := j.FFprobe(ctx)
	if ffprobe == "" {
		return 0, false
	}
	captured, err := Run(ctx, []string{ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path},
		30*time.Second, nil)
	if err != nil {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(captured.Stdout), 64)
	return seconds, err == nil
}
