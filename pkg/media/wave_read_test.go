package media

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Bytes the wave module writes for two mono 16 bit frames at 16 kHz
const waveMono16 = "RIFF(\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00\x01\x00\xff\x7f"

func writeWaveCase(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.wav")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadWaveParsesTheWaveModuleLayout(t *testing.T) {
	w, err := ReadWave(writeWaveCase(t, waveMono16))
	if err != nil {
		t.Fatal(err)
	}
	if w.Channels != 1 || w.SampWidth != 2 || w.FrameRate != 16000 || !bytes.Equal(w.Frames, []byte{0x01, 0x00, 0xff, 0x7f}) {
		t.Fatalf("wave %+v", w)
	}
}

func TestReadWaveSkipsUnknownChunksAndTrimsAPartialFrame(t *testing.T) {
	raw := "RIFF4\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x40\x1f\x00\x00\x00\x7d\x00\x00\x04\x00\x10\x00" +
		"LIST\x01\x00\x00\x00x\x00data\x06\x00\x00\x00\x01\x02\x03\x04\x05\x06"
	w, err := ReadWave(writeWaveCase(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if w.Channels != 2 || w.SampWidth != 2 || w.FrameRate != 8000 || !bytes.Equal(w.Frames, []byte{1, 2, 3, 4}) {
		t.Fatalf("wave %+v", w)
	}
}

func TestReadWaveRefusesWhatTheWaveModuleRefuses(t *testing.T) {
	for name, raw := range map[string]string{
		"not riff":     "RIFX(\x00\x00\x00WAVE",
		"not wave":     "RIFF\x04\x00\x00\x00AVI ",
		"float format": "RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x03\x00\x01\x00\x80>\x00\x00\x00\xfa\x00\x00\x04\x00\x20\x00data\x00\x00\x00\x00",
		"data first":   "RIFF\x0c\x00\x00\x00WAVEdata\x00\x00\x00\x00",
		"no chunks":    "RIFF\x04\x00\x00\x00WAVE",
	} {
		if _, err := ReadWave(writeWaveCase(t, raw)); err == nil {
			t.Fatalf("%s: read without error", name)
		}
	}
}
