// Whisper on this machine through faster-whisper in its Python set, one model kept loaded and freed once idle as the settings say
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper/config"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Worker that loads a model, then transcribes each request line
const script = `import json, sys
def reply(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
def describe(exc):
    try:
        from huggingface_hub.errors import LocalEntryNotFoundError as miss
    except ImportError:
        miss = OSError
    return {"error": str(exc), "class": type(exc).__name__, "cache_miss": isinstance(exc, miss),
            "runtime_error": isinstance(exc, RuntimeError), "os_error": isinstance(exc, OSError)}
spec = json.loads(sys.stdin.readline())
try:
    from faster_whisper import WhisperModel
    model = WhisperModel(spec["model"], device=spec["device"], compute_type=spec["compute_type"],
                         local_files_only=spec["local_files_only"])
except BaseException as exc:
    reply(describe(exc))
    sys.exit(0)
langs = getattr(model, "supported_languages", None)
reply({"ok": True, "supported_languages": [str(c) for c in langs] if isinstance(langs, (list, tuple, set, frozenset)) else None})
for line in sys.stdin:
    req = json.loads(line)
    try:
        segments, info = model.transcribe(req["path"], **req["kwargs"])
        rows = [{"text": s.text, "no_speech_prob": getattr(s, "no_speech_prob", None),
                 "avg_logprob": getattr(s, "avg_logprob", None)} for s in segments]
        reply({"segments": rows, "language": info.language, "duration": info.duration})
    except BaseException as exc:
        reply(describe(exc))
`

// Marks of an error that names a missing or broken CUDA library
var cudaMarks = []string{"libcublas", "libcudnn", "libcudart", "cannot be loaded", "cannot open shared object", "no kernel image is available",
	"CUBLAS_STATUS_NOT_SUPPORTED", "no CUDA-capable device", "CUDA driver version is insufficient"}

// Interval the idle watcher looks at the model in
const watchInterval = 30 * time.Second

// What a transcription answers when faster-whisper is not installed
var missing = "faster-whisper not installed; run `" + packages.InstallHint(config.Extra) + "`"

func Register(registry *speech.Registry) {
	e := &engine{}
	registry.Register(speech.Services{Recognizing: []speech.Recognizer{{
		Name: config.Name, Title: "Local Whisper", About: "faster-whisper on-device, no API key", Local: true, Rank: 1,
		Whisper: true, Fallback: "local_command", Label: "local faster-whisper",
		Warning: "STT provider 'local' configured but unavailable (install faster-whisper or set " + speechconfig.LocalCommandEnv + ")",
		Missing: func(_ context.Context, job *speech.Job) string {
			if !job.Provisioned(config.Extra) {
				return missing
			}
			return ""
		},
		Model: func(job *speech.Job, model string) string {
			return speech.WhisperModel(strs.Filled(model, job.Text(config.Model, config.DefaultModel)))
		},
		Language:   func(job *speech.Job) string { return job.Text(config.Language, "") },
		Transcribe: e.transcribe,
		Setup:      speech.Setup{Extra: config.Extra, Badge: "★ recommended · free"},
		Choices:    []speech.Choices{speech.Fixed(config.Model, speech.WhisperModels...)},
	}}})
}

// Model of a worker shared by the transcriptions that hold it, closed with the last
type handle struct {
	worker    *speech.Worker
	languages []string
	mu        sync.Mutex
	refs      int
}

func (h *handle) acquire() {
	h.mu.Lock()
	h.refs++
	h.mu.Unlock()
}

func (h *handle) release() {
	h.mu.Lock()
	h.refs--
	last := h.refs == 0
	h.mu.Unlock()
	if last {
		h.worker.Close()
	}
}

// The one model this process keeps, the time of the last transcription and whether a watcher frees it once idle
type engine struct {
	mu       sync.Mutex
	model    *handle
	name     string
	last     time.Time
	watching bool
	owner    *speech.Job
	// Tells once how to install faster-whisper, at the first transcription that finds it missing
	told sync.Once
}

// Failure a worker answered, with what the load policy reads of it
type failure struct {
	Class        string `json:"class"`
	Message      string `json:"error"`
	CacheMiss    bool   `json:"cache_miss"`
	RuntimeError bool   `json:"runtime_error"`
	OSError      bool   `json:"os_error"`
}

func (f *failure) Error() string { return f.Message }

func cuda(err error) bool {
	return err != nil && slices.ContainsFunc(cudaMarks, func(mark string) bool { return strings.Contains(err.Error(), mark) })
}

// Whether faster-whisper must run on the processor: Apple Silicon, x86 builds under Rosetta included, whose device detection crashes
func forcedCPU() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if runtime.GOARCH == "arm64" {
		return true
	}
	sysctl := func(name string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", name).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return sysctl("sysctl.proc_translated") == "1" || sysctl("hw.optional.arm64") == "1"
}

// One construction of the model in a fresh worker, Intel's OpenMP told to tolerate a second copy of itself where faster-whisper runs on the
// processor of a Mac and the profile's environment says nothing of it
func construct(job *speech.Job, name, device, compute string, offline bool) (*handle, error) {
	env := map[string]string{}
	if _, set := job.Config.LookupEnv("KMP_DUPLICATE_LIB_OK"); !set && forcedCPU() {
		env["KMP_DUPLICATE_LIB_OK"] = "TRUE"
	}
	w, err := job.Worker(config.Extra, script, env)
	if err != nil {
		return nil, err
	}
	var reply struct {
		failure
		OK        bool     `json:"ok"`
		Languages []string `json:"supported_languages"`
	}
	if err := w.Ask(map[string]any{"model": name, "device": device, "compute_type": compute, "local_files_only": offline}, &reply); err != nil {
		w.Close()
		return nil, err
	}
	if !reply.OK {
		w.Close()
		f := reply.failure
		return nil, &f
	}
	return &handle{worker: w, languages: reply.Languages, refs: 1}, nil
}

// The model from the cache without asking the Hub, downloaded only when it is not cached
func create(job *speech.Job, name, device, compute string) (*handle, error) {
	h, err := construct(job, name, device, compute, true)
	if err == nil {
		return h, nil
	}
	f, ok := errors.AsType[*failure](err)
	if !ok || f.RuntimeError && !strings.Contains(f.Message, "Unable to open file") || !f.RuntimeError && !f.CacheMiss {
		return nil, err
	}
	slog.Info(fmt.Sprintf("faster-whisper model '%s' is not cached; downloading it from the Hugging Face Hub", name))
	h, err = construct(job, name, device, compute, false)
	if f, ok := errors.AsType[*failure](err); ok && f.OSError {
		return nil, fmt.Errorf("Unable to download faster-whisper model '%s': %s. If huggingface.co is unreachable, set HF_ENDPOINT to an accessible mirror; "+
			"when using a mirror with hf-xet installed, also set HF_HUB_DISABLE_XET=1.", name, f.Message)
	}
	return h, err
}

// The model on the device asked for, on the processor at int8 where it must run there or the CUDA libraries fail
func load(job *speech.Job, name string) (*handle, error) {
	if forcedCPU() {
		slog.Info("Apple Silicon/Rosetta detected — loading faster-whisper on CPU (int8) to avoid native device autodetection crashes")
		return create(job, name, "cpu", "int8")
	}
	h, err := create(job, name, job.Text(config.Device, "auto"), job.Text(config.ComputeType, "auto"))
	if err == nil || !cuda(err) {
		return h, err
	}
	slog.Warn(fmt.Sprintf("faster-whisper CUDA load failed (%v) — falling back to CPU (int8). Install the NVIDIA CUDA runtime (libcublas/libcudnn) to use GPU.", err))
	return create(job, name, "cpu", "int8")
}

// The cached model of the name, loaded in place of another, held by the caller until it releases it
func (e *engine) acquire(job *speech.Job, name string) (*handle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model == nil || e.name != name {
		slog.Info(fmt.Sprintf("Loading faster-whisper model '%s' (first load downloads the model)...", name))
		h, err := load(job, name)
		if err != nil {
			return nil, err
		}
		if e.model != nil {
			e.model.release()
		}
		e.model, e.name = h, name
	}
	e.model.acquire()
	return e.model, nil
}

// Replaces the cached model with one on the processor at int8 after the GPU failed mid transcription
func (e *engine) onCPU(job *speech.Job, name string) (*handle, error) {
	h, err := create(job, name, "cpu", "int8")
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model != nil {
		e.model.release()
	}
	h.refs = 2
	e.model, e.name = h, name
	return h, nil
}

// Frees the model once it went unused for the time the settings of the latest transcription give
func (e *engine) watch(job *speech.Job) {
	e.mu.Lock()
	e.owner = job
	if e.watching {
		e.mu.Unlock()
		return
	}
	e.watching = true
	e.mu.Unlock()
	go func() {
		defer func() {
			e.mu.Lock()
			e.watching = false
			e.mu.Unlock()
		}()
		for {
			time.Sleep(watchInterval)
			e.mu.Lock()
			loaded, owner, last := e.model != nil, e.owner, e.last
			e.mu.Unlock()
			if !loaded {
				return
			}
			idle := idleAfter(owner)
			if idle <= 0 {
				return
			}
			if time.Since(last) >= idle {
				e.mu.Lock()
				if e.model != nil {
					slog.Info(fmt.Sprintf("Unloading local whisper model '%s' after idle timeout", strs.Filled(e.name, "unknown")))
					e.model.release()
					e.model, e.name = nil, ""
				}
				e.mu.Unlock()
				return
			}
		}
	}()
}

// Whole seconds an unused model stays loaded, 0 keeping it
func idleAfter(job *speech.Job) time.Duration {
	return time.Duration(max(0, int(job.Seconds(config.UnloadIdle, time.Second)))) * time.Second
}

func (e *engine) touch() {
	e.mu.Lock()
	e.last = time.Now()
	e.mu.Unlock()
}

// One decoded piece of the speech
type segment struct {
	Text         string   `json:"text"`
	NoSpeechProb *float64 `json:"no_speech_prob"`
	AvgLogprob   *float64 `json:"avg_logprob"`
}

// Reply of a transcription
type heard struct {
	failure
	Segments []segment `json:"segments"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
}

func ask(h *handle, path string, kwargs map[string]any) (heard, error) {
	var reply heard
	if err := h.worker.Ask(map[string]any{"path": path, "kwargs": kwargs}, &reply); err != nil {
		return reply, err
	}
	if reply.Class != "" {
		f := reply.failure
		return reply, &f
	}
	return reply, nil
}

func (e *engine) transcribe(ctx context.Context, job *speech.Job, path string, hints speech.Hints) speech.Transcript {
	r := speech.Recognizer{Name: config.Name}
	if !job.Provisioned(config.Extra) {
		e.told.Do(func() {
			slog.Warn("Local Whisper is not provisioned. Run `" + packages.InstallHint(config.Extra) + "`, or open /recognition and choose Local Whisper.")
		})
		return r.Failure(missing)
	}
	text, err := e.run(job, path, hints)
	if err != nil {
		slog.Error(fmt.Sprintf("Local transcription failed: %v", err))
		return r.Failure(fmt.Sprintf("Local transcription failed: %v", err))
	}
	return r.Heard(text)
}

// Transcript of the confident pieces, retried once on the processor when the CUDA runtime fails mid decode
func (e *engine) run(job *speech.Job, path string, hints speech.Hints) (string, error) {
	e.touch()
	model := hints.Model
	h, err := e.acquire(job, model)
	if err != nil {
		return "", err
	}
	defer func() { h.release() }()
	vad := job.Config.GetBool(config.VAD)
	noSpeech, logprob := job.Float(config.NoSpeech), job.Float(config.Logprob)
	kwargs := map[string]any{"beam_size": 5, "condition_on_previous_text": false, "vad_filter": vad, "no_speech_threshold": noSpeech, "log_prob_threshold": logprob}
	if vad {
		kwargs["vad_parameters"] = map[string]any{"min_silence_duration_ms": int(job.Seconds(config.VADMinSilence, time.Millisecond) * 1000)}
	}
	if language := speech.WhisperLanguage(hints.Language, h.languages); language != "" {
		kwargs["language"] = language
	}
	if prompt := job.Config.GetString(config.InitialPrompt); strings.TrimSpace(prompt) != "" {
		kwargs["initial_prompt"] = prompt
	}
	if hints.Prompt != "" {
		kwargs["initial_prompt"] = hints.Prompt
	}
	reply, err := ask(h, path, kwargs)
	if err != nil {
		if !cuda(err) {
			return "", err
		}
		slog.Warn(fmt.Sprintf("faster-whisper CUDA runtime failed mid-transcribe (%v) — evicting cached model and retrying on CPU (int8).", err))
		replacement, rerr := e.onCPU(job, model)
		if rerr != nil {
			return "", rerr
		}
		h.release()
		h = replacement
		if reply, err = ask(h, path, kwargs); err != nil {
			return "", err
		}
	}
	var kept []string
	for _, s := range reply.Segments {
		if s.NoSpeechProb != nil && s.AvgLogprob != nil && *s.NoSpeechProb > noSpeech && *s.AvgLogprob < logprob {
			slog.Debug(fmt.Sprintf("Dropping probable hallucinated segment %s (no_speech_prob=%.3f, avg_logprob=%.3f)", s.Text, *s.NoSpeechProb, *s.AvgLogprob))
			continue
		}
		kept = append(kept, strings.TrimSpace(s.Text))
	}
	slog.Info(fmt.Sprintf("Transcribed %s via local whisper (%s, lang=%s, %.1fs audio)", filepath.Base(path), model, reply.Language, reply.Duration))
	e.touch()
	if idleAfter(job) > 0 {
		e.watch(job)
	}
	return strings.TrimSpace(strings.Join(kept, " ")), nil
}
