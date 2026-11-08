package media

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Releases an evicted worker once its in flight request finishes
func releaseEvicted(v any) {
	if w, ok := v.(*localTTSWorker); ok {
		go w.Close()
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

// Piper voice download directory under the home cache, created on demand
func getPiperVoicesDir(ctx context.Context, env *tools.Env) (string, error) {
	root := home.HermesDir(ttsHome(ctx, env), "cache/piper-voices", "piper_voices_cache")
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	return root, nil
}

// Concrete onnx file of a voice path or name, downloading named voices on first use
func resolvePiperVoicePath(ctx context.Context, voice, downloadDir string) (string, error) {
	if voice == "" {
		voice = defaultPiperVoice
	}
	candidate := pypath.Expanduser(voice)
	if pystr.Lower(pypath.Suffix(candidate)) == ".onnx" && fileExists(candidate) {
		return candidate, nil
	}
	cached := filepath.Join(downloadDir, voice+".onnx")
	if fileExists(cached) && fileExists(filepath.Join(downloadDir, voice+".onnx.json")) {
		return cached, nil
	}
	ttsLog().Info(fmt.Sprintf("[Piper] Downloading voice '%s' to %s (first use)", voice, downloadDir))
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return "", err
	}
	result, err := runTTSHelper(append(append([]string(nil), python...), "-m", "piper.download_voices", voice, "--download-dir", downloadDir), 300)
	var timeout *helperTimeoutError
	if errors.As(err, &timeout) {
		return "", &ttsRuntimeError{msg: fmt.Sprintf("Piper voice download timed out after 300s for '%s'", voice), cause: err}
	}
	if err != nil {
		return "", err
	}
	if result.ReturnCode != 0 {
		stderr := pystr.Strip(result.Stderr)
		if stderr == "" {
			stderr = "no stderr output"
		}
		return "", &ttsRuntimeError{msg: fmt.Sprintf("Piper voice download failed for '%s': %s", voice, clipRunes(stderr, 400))}
	}
	if !fileExists(cached) {
		return "", &ttsRuntimeError{msg: fmt.Sprintf("Piper voice download completed but %s is missing — check voice name (see: https://github.com/OHF-Voice/piper1-gpl/blob/main/docs/VOICES.md)", cached)}
	}
	return cached, nil
}

// Whether a path exists like pathlib exists
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Loaded or cached Piper voice of the config with its section
func loadPiperVoiceForConfig(ctx context.Context, env *tools.Env, ttsConfig map[string]any) (*localTTSWorker, map[string]any, error) {
	if err := ttsImportPiper(ctx); err != nil {
		return nil, nil, err
	}
	piperConfig := ttsSection(ttsConfig, "piper")
	voiceName := defaultPiperVoice
	if pyTruthy(piperConfig["voice"]) {
		voiceName = pyrepr.Str(piperConfig["voice"])
	}
	downloadDir := ""
	if pyTruthy(piperConfig["voices_dir"]) {
		downloadDir = pyrepr.Str(piperConfig["voices_dir"])
	} else {
		d, err := getPiperVoicesDir(ctx, env)
		if err != nil {
			return nil, nil, err
		}
		downloadDir = d
	}
	downloadDir = pypath.Expanduser(downloadDir)
	if err := os.MkdirAll(downloadDir, 0o777); err != nil {
		return nil, nil, err
	}
	useCUDA := pyTruthy(dictGetDefault(piperConfig, "use_cuda", false))
	modelPath, err := resolvePiperVoicePath(ctx, voiceName, downloadDir)
	if err != nil {
		return nil, nil, err
	}
	cacheKey := fmt.Sprintf("%s::cuda=%s", modelPath, pyrepr.Repr(useCUDA))
	v, err := ttsCacheGetOrLoad(piperVoiceCache, cacheKey, func() (any, error) {
		ttsLog().Info(fmt.Sprintf("[Piper] Loading voice: %s", modelPath))
		w, err := startLocalTTSWorker(ctx, map[string]any{"engine": "piper", "model_path": modelPath, "use_cuda": useCUDA})
		if err != nil {
			return nil, err
		}
		ttsLog().Info("[Piper] Voice loaded")
		return w, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return v.(*localTTSWorker), piperConfig, nil
}

var piperAdvancedKnobs = []string{"length_scale", "noise_scale", "noise_w_scale", "volume", "normalize_audio", "speaker_id"}

// Piper synthesis to WAV with advanced knobs only when configured, then the requested container
func generatePiperTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	voice, piperConfig, err := loadPiperVoiceForConfig(ctx, env, ttsConfig)
	if err != nil {
		return "", err
	}
	speakerID := 0
	if n, ok := dictGetDefault(piperConfig, "speaker_id", 0).(int); ok {
		speakerID = n
	}
	var synConfig map[string]any
	for _, k := range piperAdvancedKnobs {
		if _, ok := piperConfig[k]; ok {
			synConfig = map[string]any{}
			break
		}
	}
	if synConfig != nil {
		for key, def := range map[string]float64{"length_scale": 1.0, "noise_scale": 0.667, "noise_w_scale": 0.8, "volume": 1.0} {
			f, err := ttsFloat(dictGetDefault(piperConfig, key, def))
			if err != nil {
				return "", err
			}
			synConfig[key] = f
		}
		synConfig["normalize_audio"] = pyTruthy(dictGetDefault(piperConfig, "normalize_audio", true))
		synConfig["speaker_id"] = speakerID
	}
	wavPath := wavSidecarPath(outputPath)
	reply, err := voice.Request(map[string]any{"text": text, "wav_path": wavPath, "syn_config": synConfig})
	if err != nil {
		return "", err
	}
	if missing, _ := reply["syn_config_missing"].(bool); missing {
		ttsLog().Warn("[Piper] SynthesisConfig not available in this piper-tts version — advanced knobs ignored")
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
		w, err := startLocalTTSWorker(ctx, map[string]any{"engine": "kittentts", "model_name": modelName})
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

const localTTSWorkerScript = `import json, os, sys, wave
proto = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr
def reply(obj):
    proto.write(json.dumps(obj) + "\n")
    proto.flush()
spec = json.loads(sys.stdin.readline())
try:
    if spec["engine"] == "piper":
        from piper import PiperVoice
        model = PiperVoice.load(spec["model_path"], use_cuda=spec["use_cuda"])
    else:
        from kittentts import KittenTTS
        model = KittenTTS(spec["model_name"])
except BaseException as exc:
    reply({"error": str(exc), "type": type(exc).__name__})
    sys.exit(0)
reply({"ok": True})
for line in sys.stdin:
    req = json.loads(line)
    try:
        if spec["engine"] == "piper":
            syn_config, missing = None, False
            if req.get("syn_config") is not None:
                try:
                    from piper import SynthesisConfig
                    syn_config = SynthesisConfig(**req["syn_config"])
                except ImportError:
                    missing = True
            with wave.open(req["wav_path"], "wb") as wav_file:
                if syn_config is not None:
                    model.synthesize_wav(req["text"], wav_file, syn_config=syn_config)
                else:
                    model.synthesize_wav(req["text"], wav_file)
            reply({"ok": True, "syn_config_missing": missing})
        else:
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
