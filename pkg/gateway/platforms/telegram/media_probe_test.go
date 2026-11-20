package telegram

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// Silent mono 8 bit WAV of the frames at the rate
func writeWAV(t *testing.T, path string, rate, frames int) {
	t.Helper()
	buf := []byte("RIFF")
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+frames))
	buf = append(buf, "WAVEfmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1)
	buf = binary.LittleEndian.AppendUint16(buf, 1)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate))
	buf = binary.LittleEndian.AppendUint16(buf, 1)
	buf = binary.LittleEndian.AppendUint16(buf, 8)
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(frames))
	buf = append(buf, make([]byte, frames)...)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeWavRoundsToWholeSeconds(t *testing.T) {
	for _, c := range []struct{ rate, frames, expected int }{{10, 2900, 290}, {10, 2904, 290}, {10, 2906, 291}, {10, 12, 1}} {
		f := filepath.Join(t.TempDir(), "clip.wav")
		writeWAV(t, f, c.rate, c.frames)
		if got, ok := ProbeVoiceDurationSeconds(context.Background(), f); !ok || got != c.expected {
			t.Fatalf("%v got %d %v", c, got, ok)
		}
	}
}

func TestCoerceDurationSeconds(t *testing.T) {
	for _, c := range []struct {
		value    any
		expected int
		ok       bool
	}{{290.0, 290, true}, {290.6, 291, true}, {"12.7", 13, true}, {0, 0, false}, {-3, 0, false}, {nil, 0, false}, {"", 0, false}, {"x", 0, false}} {
		if got, ok, err := coerceDurationSeconds(c.value); err != nil || ok != c.ok || got != c.expected {
			t.Fatalf("%v got %d %v %v", c.value, got, ok, err)
		}
	}
}
