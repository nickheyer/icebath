package telegram

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Measured geometry of a local video, the duration only when the probe read one
type VideoGeometry struct {
	Width, Height int
	Duration      *int
}

var errDurationOverflow = errors.New("cannot convert float infinity to integer")

// Raw length rounded half to even to whole positive seconds, false when unusable
func coerceDurationSeconds(value any) (int, bool, error) {
	f, err := pyint.Float(value)
	if err != nil || math.IsNaN(f) {
		return 0, false, nil
	}
	if math.IsInf(f, 0) {
		return 0, false, errDurationOverflow
	}
	secs := math.RoundToEven(f)
	if secs <= 0 {
		return 0, false, nil
	}
	return int(secs), true, nil
}

// Frame rate and frame count of a PCM WAVE file the way the wave module reads them
func waveFrames(path string) (rate, frames int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return 0, 0, err
	}
	if string(head[:4]) != "RIFF" || string(head[8:]) != "WAVE" {
		return 0, 0, errors.New("file does not start with RIFF id")
	}
	frameSize := 0
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return 0, 0, errors.New("fmt chunk and/or data chunk missing")
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		switch string(ch[:4]) {
		case "fmt ":
			buf := make([]byte, size)
			if _, err := io.ReadFull(f, buf); err != nil || len(buf) < 16 {
				return 0, 0, errors.New("fmt chunk truncated")
			}
			tag := binary.LittleEndian.Uint16(buf[0:])
			channels := int(binary.LittleEndian.Uint16(buf[2:]))
			rate = int(binary.LittleEndian.Uint32(buf[4:]))
			bits := int(binary.LittleEndian.Uint16(buf[14:]))
			pcmGUID := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
			if tag == 0xFFFE && len(buf) >= 40 && bytes.Equal(buf[24:40], pcmGUID) {
				tag = 1
			}
			if tag != 1 {
				return 0, 0, fmt.Errorf("unknown format: %d", tag)
			}
			if channels == 0 {
				return 0, 0, errors.New("bad # of channels")
			}
			if bits == 0 {
				return 0, 0, errors.New("bad sample width")
			}
			frameSize = channels * ((bits + 7) / 8)
			if size%2 == 1 {
				_, _ = f.Seek(1, io.SeekCurrent)
			}
		case "data":
			if frameSize == 0 {
				return 0, 0, errors.New("data chunk before fmt chunk")
			}
			return rate, int(size) / frameSize, nil
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				return 0, 0, err
			}
		}
	}
}

// Output of a tool run with stdin closed and a timeout, the exit code alongside
func runProbe(ctx context.Context, timeout time.Duration, name string, args ...string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	if cctx.Err() != nil {
		return "", -1, cctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return pystr.DecodeUTF8Replace(out.Bytes()), 0, nil
}

// Whole second audio length from the WAVE header then ffprobe, false when unreadable
func ProbeVoiceDurationSeconds(ctx context.Context, path string) (int, bool) {
	if _, ext := pypath.Splitext(path); pystr.Lower(ext) == ".wav" {
		if rate, frames, err := waveFrames(path); err == nil && rate != 0 {
			if secs, ok, _ := coerceDurationSeconds(float64(frames) / float64(rate)); ok {
				return secs, true
			}
		}
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, false
	}
	out, code, err := runProbe(ctx, 5*time.Second, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil || code != 0 {
		return 0, false
	}
	secs, ok, _ := coerceDurationSeconds(pystr.Strip(out))
	return secs, ok
}

// Width, height and duration of a local video from ffprobe, false when it cannot read them
func ProbeVideoGeometry(ctx context.Context, path string) (VideoGeometry, bool) {
	found := false
	geometry, err := func() (VideoGeometry, error) {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			return VideoGeometry{}, nil
		}
		out, code, err := runProbe(ctx, 20*time.Second, "ffprobe", "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=width,height", "-show_entries", "format=duration", "-of", "json", path)
		if err != nil || code != 0 {
			return VideoGeometry{}, err
		}
		if out == "" {
			out = "{}"
		}
		var blob struct {
			Streams []map[string]json.Number `json:"streams"`
			Format  map[string]any           `json:"format"`
		}
		if err := json.Unmarshal([]byte(out), &blob); err != nil {
			return VideoGeometry{}, err
		}
		if len(blob.Streams) == 0 {
			return VideoGeometry{}, nil
		}
		w, werr := strconv.Atoi(blob.Streams[0]["width"].String())
		h, herr := strconv.Atoi(blob.Streams[0]["height"].String())
		if werr != nil || herr != nil {
			return VideoGeometry{}, errors.New("stream width or height missing")
		}
		g := VideoGeometry{Width: w, Height: h}
		secs, ok, err := coerceDurationSeconds(blob.Format["duration"])
		if err != nil {
			return VideoGeometry{}, err
		}
		if ok {
			g.Duration = &secs
		}
		found = true
		return g, nil
	}()
	if err != nil {
		slog.Default().With("logger", "plugins.platforms.telegram.adapter").Debug(
			fmt.Sprintf("[Telegram] video geometry probe failed for %s", path), "error", err)
		return VideoGeometry{}, false
	}
	return geometry, found
}

// Path of a 320px wide JPEG frame for the thumbnail field, empty on failure
func VideoThumbnailJPEG(ctx context.Context, path string, duration *int) string {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return ""
	}
	d := 3
	if duration != nil && *duration != 0 {
		d = *duration
	}
	seek := max(1, int(float64(d)*0.25))
	tmp, err := os.CreateTemp("", "icebath-tg-thumb-*.jpg")
	if err != nil {
		return ""
	}
	out := tmp.Name()
	_ = tmp.Close()
	_, code, err := runProbe(ctx, 30*time.Second, "ffmpeg", "-y", "-ss", strconv.Itoa(seek), "-i", path, "-frames:v", "1",
		"-vf", "scale=320:-2", "-q:v", "6", out)
	if err != nil || code != 0 {
		_ = os.Remove(out)
		return ""
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		_ = os.Remove(out)
		return ""
	}
	return out
}
