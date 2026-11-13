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
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media/speech"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultKittenTTSModel = "KittenML/kitten-tts-nano-0.8-int8"
	defaultKittenTTSVoice = "Jasper"
	defaultPiperVoice     = "en_US-lessac-medium"
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
	piperVoiceCache       = newTTSModelCache()
	kittenTTSModelCache   = newTTSModelCache()
	localTTSModelCaches   = map[string]*ttsModelCache{"piper": piperVoiceCache, "kittentts": kittenTTSModelCache}
	errTTSCacheLoadFailed = errors.New("model load failed")
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
	return fmt.Sprintf("Command '%s' timed out after %d seconds", pyrepr.Repr(e.Cmd), e.Timeout)
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

// NeuTTS synthesis in a helper process so the large model exits after use
func generateNeuTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	neuttsConfig, err := orEmptyDict(ttsConfig, "neutts")
	if err != nil {
		return "", err
	}
	samples, err := neuttsSamplesDir(ctx, env)
	if err != nil {
		return "", err
	}
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return "", err
	}
	wavPath := wavSidecarPath(outputPath)
	pick := func(key string, def string) string {
		v := dictGetDefault(neuttsConfig, key, "")
		if !pyTruthy(v) {
			return def
		}
		return pyrepr.Str(v)
	}
	cmd := append(append([]string(nil), python...), "-c", neuttsSynthScript,
		"--text", text, "--out", wavPath,
		"--ref-audio", pick("ref_audio", filepath.Join(samples, "jo.wav")),
		"--ref-text", pick("ref_text", filepath.Join(samples, "jo.txt")),
		"--model", pyrepr.Str(dictGetDefault(neuttsConfig, "model", "neuphonic/neutts-air-q4-gguf")),
		"--device", pyrepr.Str(dictGetDefault(neuttsConfig, "device", "cpu")))
	result, err := runTTSHelper(cmd, 120)
	if err != nil {
		return "", err
	}
	if result.ReturnCode != 0 {
		var errorLines []string
		for _, l := range pystr.Splitlines(pystr.Strip(result.Stderr), false) {
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
	ttsLog().Info(fmt.Sprintf("[Piper] Preparing voice '%s' (first use downloads it)", voice))
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

// Loaded or cached Piper voice of the config with its section
func loadPiperVoiceForConfig(ctx context.Context, env *tools.Env, ttsConfig map[string]any) (localVoice, map[string]any, error) {
	piperConfig := ttsSection(ttsConfig, "piper")
	voiceName := defaultPiperVoice
	if pyTruthy(piperConfig["voice"]) {
		voiceName = pyrepr.Str(piperConfig["voice"])
	}
	voicesDir := home.HermesDir(ttsHome(ctx, env), "cache/piper-voices", "piper_voices_cache")
	if pyTruthy(piperConfig["voices_dir"]) {
		voicesDir = pypath.Expanduser(pyrepr.Str(piperConfig["voices_dir"]))
	}
	useCUDA := pyTruthy(dictGetDefault(piperConfig, "use_cuda", false))
	files, err := resolvePiperVoice(ctx, voiceName, voicesDir)
	if err != nil {
		return nil, nil, err
	}
	for key, scale := range map[string]*float64{"length_scale": &files.LengthScale, "noise_scale": &files.NoiseScale, "noise_w_scale": &files.NoiseW} {
		if configured, ok := piperConfig[key]; ok {
			if *scale, err = ttsFloat(configured); err != nil {
				return nil, nil, err
			}
		}
	}
	cacheKey := fmt.Sprintf("%s::cuda=%t::scales=%g,%g,%g", files.Model, useCUDA, files.LengthScale, files.NoiseScale, files.NoiseW)
	v, err := ttsCacheGetOrLoad(piperVoiceCache, cacheKey, func() (any, error) {
		ttsLog().Info(fmt.Sprintf("[Piper] Loading voice: %s", files.Model))
		voice, err := loadPiperVoice(ctx, files, useCUDA)
		if err != nil {
			return nil, err
		}
		ttsLog().Info("[Piper] Voice loaded")
		return voice, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return v.(localVoice), piperConfig, nil
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
func generatePiperTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	voice, piperConfig, err := loadPiperVoiceForConfig(ctx, env, ttsConfig)
	if err != nil {
		return "", err
	}
	speakerID := 0
	if n, ok := dictGetDefault(piperConfig, "speaker_id", 0).(int); ok {
		speakerID = n
	}
	volume, err := ttsFloat(dictGetDefault(piperConfig, "volume", 1.0))
	if err != nil {
		return "", err
	}
	samples, sampleRate, err := voice.Speak(text, speakerID, 1)
	if err != nil {
		return "", err
	}
	samples = shapePiperAudio(samples, pyTruthy(dictGetDefault(piperConfig, "normalize_audio", true)), volume)
	wavPath := wavSidecarPath(outputPath)
	if err := os.WriteFile(wavPath, wrapPCMAsWAV(pcm16(samples), sampleRate, 1, 2), 0o666); err != nil {
		return "", err
	}
	return finalizeWAVOutput(wavPath, outputPath)
}

// Loaded or cached KittenTTS model of the config with its section
func loadKittenTTSModelForConfig(ctx context.Context, ttsConfig map[string]any) (*localTTSWorker, map[string]any, error) {
	if err := ttsImportKittenTTS(ctx); err != nil {
		return nil, nil, err
	}
	ktConfig := ttsSection(ttsConfig, "kittentts")
	modelName := pyrepr.Str(dictGetDefault(ktConfig, "model", defaultKittenTTSModel))
	v, err := ttsCacheGetOrLoad(kittenTTSModelCache, modelName, func() (any, error) {
		ttsLog().Info(fmt.Sprintf("[KittenTTS] Loading model: %s", modelName))
		w, err := startLocalTTSWorker(ctx, map[string]any{"model_name": modelName})
		if err != nil {
			return nil, err
		}
		ttsLog().Info("[KittenTTS] Model loaded successfully")
		return w, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return v.(*localTTSWorker), ktConfig, nil
}

// KittenTTS synthesis at 24kHz to WAV then the requested container
func generateKittenTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	model, ktConfig, err := loadKittenTTSModelForConfig(ctx, ttsConfig)
	if err != nil {
		return "", err
	}
	wavPath := wavSidecarPath(outputPath)
	if _, err := model.Request(map[string]any{"text": text, "wav_path": wavPath,
		"voice": dictGetDefault(ktConfig, "voice", defaultKittenTTSVoice), "speed": dictGetDefault(ktConfig, "speed", 1.0),
		"clean_text": dictGetDefault(ktConfig, "clean_text", true)}); err != nil {
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
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python[0], append(python[1:], "-c", localTTSWorkerScript)...)
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
        print("Error: neutts not installed. Run icebath setup tts and choose NeuTTS.", file=sys.stderr)
        sys.exit(1)
    tts = NeuTTS(
        backbone_repo=args.model,
        backbone_device="gpu" if args.device == "cuda" else args.device,
        codec_repo="neuphonic/neucodec",
        codec_device=args.device)
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
