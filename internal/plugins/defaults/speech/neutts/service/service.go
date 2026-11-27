// NeuTTS, a voice cloning model run by its Python set in a helper process, so the large model leaves memory after each use
package service

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/neutts/config"
	"github.com/nickheyer/icebath/internal/speech"
)

// Sample voice the model copies when none is set, with the words it speaks
//
//go:embed samples/jo.wav samples/jo.txt
var samples embed.FS

// Helper that speaks the text in the voice of the sample into a WAV at 24 kHz
const script = `import argparse
import struct
import sys
from pathlib import Path


def _write_wav(path, samples, sample_rate=24000):
    import numpy as np
    if not isinstance(samples, np.ndarray):
        samples = np.array(samples, dtype=np.float32)
    pcm = (np.clip(samples.flatten(), -1.0, 1.0) * 32767).astype(np.int16)
    data_size = len(pcm) * 2
    with open(path, "wb") as f:
        f.write(b"RIFF" + struct.pack("<I", 36 + data_size) + b"WAVEfmt ")
        f.write(struct.pack("<IHHIIHH", 16, 1, 1, sample_rate, sample_rate * 2, 2, 16))
        f.write(b"data" + struct.pack("<I", data_size) + pcm.tobytes())


def main():
    parser = argparse.ArgumentParser(description="NeuTTS synthesis helper")
    parser.add_argument("--text", required=True, help="Text to synthesize")
    parser.add_argument("--out", required=True, help="Output WAV path")
    parser.add_argument("--ref-audio", required=True, help="Reference voice audio path")
    parser.add_argument("--ref-text", required=True, help="Reference voice transcript path")
    parser.add_argument("--model", default="neuphonic/neutts-air-q4-gguf", help="HuggingFace backbone model repo")
    parser.add_argument("--device", default="cpu", help="Device (cpu/cuda/mps)")
    args = parser.parse_args()
    ref_audio = Path(args.ref_audio).expanduser()
    ref_text_path = Path(args.ref_text).expanduser()
    if not ref_audio.exists():
        print(f"Error: reference audio not found: {ref_audio}", file=sys.stderr)
        sys.exit(1)
    if not ref_text_path.exists():
        print(f"Error: reference text not found: {ref_text_path}", file=sys.stderr)
        sys.exit(1)
    ref_text = ref_text_path.read_text(encoding="utf-8-sig").strip()
    try:
        from neutts import NeuTTS
    except ImportError:
        print("Error: neutts not installed. Run '` + "icebath pm install --extra neutts" + `', or start icebath, type /voice setup and choose NeuTTS.", file=sys.stderr)
        sys.exit(1)
    if os.environ.get("ESPEAK_DATA_PATH"):
        os.environ["ESPEAK_DATA_PATH"] = short_espeak_data(os.environ["ESPEAK_DATA_PATH"])
        from phonemizer.backend.espeak.wrapper import EspeakWrapper
        if hasattr(EspeakWrapper, "set_data_path"):
            EspeakWrapper.set_data_path(os.environ["ESPEAK_DATA_PATH"])
    try:
        tts = NeuTTS(
            backbone_repo=args.model,
            backbone_device="gpu" if args.device == "cuda" else args.device,
            codec_repo="neuphonic/neucodec",
            codec_device=args.device)
    except Exception as exc:
        if type(exc).__name__ != "GatedRepoError":
            raise
        print(f"Error: Hugging Face serves {args.model} and neuphonic/neucodec only to signed in accounts. Accept their terms at "
              f"https://huggingface.co/{args.model} and https://huggingface.co/neuphonic/neucodec, then set HF_TOKEN to a token of that account.", file=sys.stderr)
        sys.exit(1)
    wav = tts.infer(args.text, tts.encode_reference(str(ref_audio)), ref_text)
    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        import soundfile as sf
        sf.write(str(out_path), wav, 24000)
    except ImportError:
        _write_wav(str(out_path), wav, 24000)
    print(f"OK: {out_path}", file=sys.stderr)


main()
`

// Longest one synthesis may take
const deadline = 120 * time.Second

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "NeuTTS", Label: "NeuTTS (local)", About: "Speaks in the voice of a sample, on this computer",
		Local:      true,
		Ready:      func(job *speech.Job) bool { return job.Provisioned(config.Extra) },
		Limit:      func(*speech.Job) int { return config.DefaultLimit },
		Synthesize: synthesize,
		Opus:       speech.TranscodedOpus,
		Setup:      speech.Setup{Extra: config.Extra, Programs: []string{"espeak-ng"}},
	}}})
}

// Folder of the bundled sample in the profile's cache, written again when it changed
func sampleDir(job *speech.Job) (string, error) {
	dir := filepath.Join(job.Config.ProfileDir(job.Config.Profile()), "cache", "neutts_samples")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for _, name := range []string{"jo.wav", "jo.txt"} {
		data, err := samples.ReadFile("samples/" + name)
		if err != nil {
			return "", err
		}
		target := filepath.Join(dir, name)
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.WriteFile(target, data, 0o666); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	if !job.Provisioned(config.Extra) {
		return "", speech.Absent("NeuTTS provider selected but neutts is not installed. Start icebath, type /voice setup and choose NeuTTS, or run `" +
			packages.InstallHint(config.Extra) + "`; espeak-ng is also required.")
	}
	dir, err := sampleDir(job)
	if err != nil {
		return "", err
	}
	wav := speech.WAVPath(request.Path)
	args := []string{"-c", "import os\n" + speech.ShortEspeak + script, "--text", request.Text, "--out", wav,
		"--ref-audio", job.Text(config.RefAudio, filepath.Join(dir, "jo.wav")), "--ref-text", job.Text(config.RefText, filepath.Join(dir, "jo.txt")),
		"--model", job.Text(config.Model, config.DefaultModel), "--device", job.Text(config.Device, config.DefaultDevice)}
	_, stderr, code, err := job.Helper(ctx, config.Extra, args, deadline)
	if err != nil {
		return "", err
	}
	if code != 0 {
		var lines []string
		for _, line := range strings.Split(strings.ReplaceAll(strings.TrimSpace(stderr), "\r\n", "\n"), "\n") {
			if !strings.HasPrefix(line, "OK:") {
				lines = append(lines, line)
			}
		}
		detail := strings.Join(lines, "\n")
		if detail == "" {
			detail = "unknown error"
		}
		return "", errors.New("NeuTTS synthesis failed: " + detail)
	}
	return job.FromWAV(ctx, wav, request.Path)
}
