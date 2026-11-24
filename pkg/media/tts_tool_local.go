package media

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/media/speech"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	defaultKittenTTSModel = "KittenML/kitten-tts-nano-0.8-int8"
	defaultKittenTTSVoice = "Jasper"
	defaultPiperVoice     = "en_US-lessac-medium"
	defaultNeuTTSModel    = "neuphonic/neutts-air-q4-gguf"
	defaultNeuTTSDevice   = "cpu"
)

// LRU bound of each local model cache
var ttsModelCacheMax = 3

//go:embed neutts_samples/jo.wav neutts_samples/jo.txt
var neuttsSamples embed.FS

// Insertion ordered model cache bounded by the shared LRU size
type ttsModelCache struct {
	mu   sync.Mutex
	keys []string
	vals map[string]any
}

func newTTSModelCache() *ttsModelCache { return &ttsModelCache{vals: map[string]any{}} }

// Keys in recency order, oldest first
func (c *ttsModelCache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keys...)
}

// Drops every entry, releasing each evicted worker
func (c *ttsModelCache) Clear() {
	c.mu.Lock()
	vals := c.vals
	c.keys, c.vals = nil, map[string]any{}
	c.mu.Unlock()
	for _, v := range vals {
		releaseEvicted(v)
	}
}

var (
	piperVoiceCache     = newTTSModelCache()
	kittenTTSModelCache = newTTSModelCache()
	localTTSModelCaches = map[string]*ttsModelCache{"piper": piperVoiceCache, "kittentts": kittenTTSModelCache}
)

// Cached value or a fresh load, a hit refreshing recency and the oldest entries evicted past the bound
func ttsCacheGetOrLoad(cache *ttsModelCache, key string, load func() (any, error)) (any, error) {
	cache.mu.Lock()
	if v, ok := cache.vals[key]; ok {
		cache.keys = append(removeKey(cache.keys, key), key)
		cache.mu.Unlock()
		return v, nil
	}
	cache.mu.Unlock()
	value, err := load()
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if _, ok := cache.vals[key]; ok {
		cache.keys = removeKey(cache.keys, key)
	}
	cache.vals[key] = value
	cache.keys = append(cache.keys, key)
	var evicted []any
	for len(cache.keys) > ttsModelCacheMax {
		oldest := cache.keys[0]
		cache.keys = cache.keys[1:]
		evicted = append(evicted, cache.vals[oldest])
		delete(cache.vals, oldest)
	}
	cache.mu.Unlock()
	for _, v := range evicted {
		releaseEvicted(v)
	}
	return value, nil
}

// Keys without one entry
func removeKey(keys []string, key string) []string {
	out := keys[:0:0]
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	return out
}

// Releases an evicted engine once its in flight request finishes
func releaseEvicted(v any) {
	switch engine := v.(type) {
	case *localTTSWorker:
		go engine.Close()
	case localVoice:
		go engine.Close()
	}
}

// Result of one helper process run
type helperResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Helper process timeout like subprocess TimeoutExpired
type helperTimeoutError struct {
	Cmd     []string
	Timeout int
}

func (e *helperTimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %d seconds", strings.Join(e.Cmd, " "), e.Timeout)
}

// Runs a helper with captured replaced UTF-8 output, no stdin and a timeout, the seam tests replace
var runTTSHelper = func(cmd []string, timeout int) (*helperResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	hideConsoleWindow(c)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &helperTimeoutError{Cmd: cmd, Timeout: timeout}
	}
	res := &helperResult{Stdout: strings.ToValidUTF8(stdout.String(), "�"), Stderr: strings.ToValidUTF8(stderr.String(), "�")}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ReturnCode = exitErr.ExitCode()
	} else if err != nil {
		return nil, err
	}
	return res, nil
}

// Reference voice samples materialized under the home cache, the bundled jo voice
func neuttsSamplesDir(ctx context.Context, env *tools.Env) (string, error) {
	dir := filepath.Join(home.CacheDir(ttsHome(ctx, env)), "neutts_samples")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for _, name := range []string{"jo.wav", "jo.txt"} {
		data, err := neuttsSamples.ReadFile("neutts_samples/" + name)
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

// Bundled voice sample on disk with the words it speaks, the reference a speech to text check transcribes
func SpeechSample(ctx context.Context) (string, string, error) {
	dir, err := neuttsSamplesDir(ctx, scopedToolEnv(ctx))
	if err != nil {
		return "", "", err
	}
	text, err := os.ReadFile(filepath.Join(dir, "jo.txt"))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "jo.wav"), strings.TrimSpace(string(text)), nil
}

// NeuTTS synthesis in a helper process so the large model exits after use
func generateNeuTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	samples, err := neuttsSamplesDir(ctx, env)
	if err != nil {
		return "", err
	}
	python, err := managedPythonCommand(ctx, "neutts")
	if err != nil {
		return "", err
	}
	wavPath := wavSidecarPath(outputPath)
	n := req.NeuTTS
	cmd := append(append([]string(nil), python...), "-c", "import os\n"+shortEspeakDataScript+neuttsSynthScript,
		"--text", text, "--out", wavPath,
		"--ref-audio", textOr(n.RefAudio, filepath.Join(samples, "jo.wav")),
		"--ref-text", textOr(n.RefText, filepath.Join(samples, "jo.txt")),
		"--model", textOr(n.Model, defaultNeuTTSModel),
		"--device", textOr(n.Device, defaultNeuTTSDevice))
	result, err := runTTSHelper(cmd, 120)
	if err != nil {
		return "", err
	}
	if result.ReturnCode != 0 {
		var errorLines []string
		for _, l := range textutil.Lines(strings.TrimSpace(result.Stderr)) {
			if !strings.HasPrefix(l, "OK:") {
				errorLines = append(errorLines, l)
			}
		}
		detail := strings.Join(errorLines, "\n")
		if detail == "" {
			detail = "unknown error"
		}
		return "", &ttsRuntimeError{msg: "NeuTTS synthesis failed: " + detail}
	}
	return finalizeWAVOutput(wavPath, outputPath)
}

// A voice loaded into the native speech engine
type localVoice interface {
	Speak(text string, speaker int, speed float32) ([]float32, int, error)
	Close()
}

// Voice files of a path or name, a file the user has first, then the voices directory, then the pinned catalog
var resolvePiperVoice = func(ctx context.Context, voice, voicesDir string) (speech.PiperVoice, error) {
	if candidate := pypath.Expanduser(voice); strings.EqualFold(filepath.Ext(candidate), ".onnx") && fileExists(candidate) {
		return speech.AdoptPiper(ctx, candidate)
	}
	if local := filepath.Join(voicesDir, voice+".onnx"); fileExists(local) {
		return speech.AdoptPiper(ctx, local)
	}
	pkgLog().Info(fmt.Sprintf("[Piper] Preparing voice '%s' (first use downloads it)", voice))
	return speech.FetchPiper(ctx, voice)
}

// Loads a Piper voice into the speech engine, the seam tests replace
var loadPiperVoice = func(ctx context.Context, files speech.PiperVoice, gpu bool) (localVoice, error) {
	engine, err := speech.Engine(ctx, gpu)
	if err != nil {
		return nil, err
	}
	return engine.NewVoice(files.Options(min(runtime.NumCPU(), 4)))
}

// Whether a path exists like pathlib exists
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Loaded or cached Piper voice of the request
func loadPiperVoiceForConfig(ctx context.Context, env *tools.Env, req *ttsRequest) (localVoice, error) {
	pc := req.Piper
	voiceName := textOr(pc.Voice, defaultPiperVoice)
	voicesDir := home.IcebathDir(ttsHome(ctx, env), "cache/piper-voices", "piper_voices_cache")
	if v := strings.TrimSpace(pc.VoicesDir); v != "" {
		voicesDir = pypath.Expanduser(v)
	}
	useCUDA := pc.UseCUDA
	files, err := resolvePiperVoice(ctx, voiceName, voicesDir)
	if err != nil {
		return nil, err
	}
	for configured, scale := range map[*float64]*float64{pc.LengthScale: &files.LengthScale, pc.NoiseScale: &files.NoiseScale, pc.NoiseWScale: &files.NoiseW} {
		if configured != nil {
			*scale = *configured
		}
	}
	cacheKey := fmt.Sprintf("%s::cuda=%t::scales=%g,%g,%g", files.Model, useCUDA, files.LengthScale, files.NoiseScale, files.NoiseW)
	v, err := ttsCacheGetOrLoad(piperVoiceCache, cacheKey, func() (any, error) {
		pkgLog().Info(fmt.Sprintf("[Piper] Loading voice: %s", files.Model))
		voice, err := loadPiperVoice(ctx, files, useCUDA)
		if err != nil {
			return nil, err
		}
		pkgLog().Info("[Piper] Voice loaded")
		return voice, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(localVoice), nil
}

// Samples scaled to full range when asked, multiplied by the volume and clipped
func shapePiperAudio(samples []float32, normalize bool, volume float64) []float32 {
	scale := float32(volume)
	if normalize {
		var peak float32
		for _, s := range samples {
			peak = max(peak, float32(math.Abs(float64(s))))
		}
		if peak < 1e-8 {
			scale = 0
		} else {
			scale /= peak
		}
	}
	out := make([]float32, len(samples))
	for i, s := range samples {
		out[i] = min(max(s*scale, -1), 1)
	}
	return out
}

// Signed 16 bit little endian PCM of float samples
func pcm16(samples []float32) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(math.Round(float64(min(max(s, -1), 1))*32767))))
	}
	return out
}

// Piper synthesis in the native speech engine to WAV, then the requested container
func generatePiperTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	voice, err := loadPiperVoiceForConfig(ctx, env, req)
	if err != nil {
		return "", err
	}
	speed, _ := req.speedFor(nil)
	samples, sampleRate, err := voice.Speak(text, max(0, req.Piper.SpeakerID), float32(speed))
	if err != nil {
		return "", err
	}
	samples = shapePiperAudio(samples, req.Piper.NormalizeAudio, req.Piper.Volume)
	wavPath := wavSidecarPath(outputPath)
	if err := os.WriteFile(wavPath, wrapPCMAsWAV(pcm16(samples), sampleRate, 1, 2), 0o666); err != nil {
		return "", err
	}
	return finalizeWAVOutput(wavPath, outputPath)
}

// Loaded or cached KittenTTS model of the request
func loadKittenTTSModelForConfig(ctx context.Context, req *ttsRequest) (*localTTSWorker, error) {
	if err := ttsImportKittenTTS(ctx); err != nil {
		return nil, err
	}
	modelName := textOr(req.KittenTTS.Model, defaultKittenTTSModel)
	v, err := ttsCacheGetOrLoad(kittenTTSModelCache, modelName, func() (any, error) {
		pkgLog().Info(fmt.Sprintf("[KittenTTS] Loading model: %s", modelName))
		w, err := startLocalTTSWorker(ctx, map[string]any{"model_name": modelName})
		if err != nil {
			return nil, err
		}
		pkgLog().Info("[KittenTTS] Model loaded successfully")
		return w, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*localTTSWorker), nil
}

// KittenTTS synthesis at 24kHz to WAV then the requested container
func generateKittenTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	model, err := loadKittenTTSModelForConfig(ctx, req)
	if err != nil {
		return "", err
	}
	kt := req.KittenTTS
	speed, _ := req.speedFor(kt.Speed)
	wavPath := wavSidecarPath(outputPath)
	if _, err := model.Request(map[string]any{"text": text, "wav_path": wavPath,
		"voice": textOr(kt.Voice, defaultKittenTTSVoice), "speed": speed, "clean_text": kt.CleanText}); err != nil {
		return "", err
	}
	return finalizeWAVOutput(wavPath, outputPath)
}

// Exception raised inside a local engine process, carrying its Python class name
type localEngineError struct {
	Class string
	Msg   string
}

func (e *localEngineError) Error() string { return e.Msg }

// Long lived managed interpreter holding one loaded local model
type localTTSWorker struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	closed bool
}

// Starts a model worker and waits for its load verdict
var startLocalTTSWorker = func(ctx context.Context, spec map[string]any) (*localTTSWorker, error) {
	python, err := managedPythonCommand(ctx, "kittentts")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python[0], append(python[1:], "-c", "import os\n"+shortEspeakDataScript+localTTSWorkerScript)...)
	hideConsoleWindow(cmd)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &localTTSWorker{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
	if _, err := w.Request(spec); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// Sends one JSON request line and reads the JSON reply line
func (w *localTTSWorker) Request(req map[string]any) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, &localEngineError{Class: "RuntimeError", Msg: "local TTS model was released"}
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	raw, err := w.out.ReadBytes('\n')
	if err != nil {
		return nil, &localEngineError{Class: "RuntimeError", Msg: "local TTS process exited: " + err.Error()}
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if msg, ok := reply["error"].(string); ok {
		class, _ := reply["type"].(string)
		return nil, &localEngineError{Class: class, Msg: msg}
	}
	return reply, nil
}

// Ends the worker after any in flight request
func (w *localTTSWorker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.stdin.Close()
	return w.cmd.Wait()
}

// Python that moves eSpeak NG data under a short path, the C library failing on data paths of 160 or more bytes
const shortEspeakDataScript = `
def short_espeak_data(data):
    import shutil, tempfile
    import hashlib
    if not data or len(data) < 150:
        return data
    tag = hashlib.sha256(data.encode()).hexdigest()[:16]
    short = os.path.join(tempfile.gettempdir(), "icebath-espeak-ng-data-" + tag)
    if not os.path.isfile(os.path.join(short, "phontab")):
        staging = short + ".%d" % os.getpid()
        shutil.copytree(data, staging)
        try:
            os.replace(staging, short)
        except OSError:
            shutil.rmtree(staging, ignore_errors=True)
    return short
`

const localTTSWorkerScript = `import json, os, sys
proto = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr
def reply(obj):
    proto.write(json.dumps(obj) + "\n")
    proto.flush()
spec = json.loads(sys.stdin.readline())
try:
    from kittentts import KittenTTS
    import espeakng_loader
    from phonemizer.backend.espeak.wrapper import EspeakWrapper
    EspeakWrapper.set_data_path(short_espeak_data(espeakng_loader.get_data_path()))
    model = KittenTTS(spec["model_name"])
except BaseException as exc:
    reply({"error": str(exc), "type": type(exc).__name__})
    sys.exit(0)
reply({"ok": True})
for line in sys.stdin:
    req = json.loads(line)
    try:
        audio = model.generate(req["text"], voice=req["voice"], speed=req["speed"], clean_text=req["clean_text"])
        import soundfile as sf
        sf.write(req["wav_path"], audio, 24000)
        reply({"ok": True})
    except BaseException as exc:
        reply({"error": str(exc), "type": type(exc).__name__})
`

const neuttsSynthScript = `import argparse
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
        print("Error: neutts not installed. Run 'icebath pm install --extra neutts', or start icebath, type /setup voice and choose NeuTTS.", file=sys.stderr)
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
