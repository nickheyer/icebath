package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	vadMinSilenceMSDefault       = 500
	noSpeechProbThresholdDefault = 0.6
	logprobThresholdDefault      = -1.0
)

var (
	localLanguageAliases     = map[string]string{"繁體中文": "zh", "繁体中文": "zh", "简体中文": "zh", "簡體中文": "zh"}
	threeLetterWhisperCodes  = sttSet("haw", "yue")
	cudaLibErrorMarkers      = []string{"libcublas", "libcudnn", "libcudart", "cannot be loaded", "cannot open shared object", "no kernel image is available", "CUBLAS_STATUS_NOT_SUPPORTED", "no CUDA-capable device", "CUDA driver version is insufficient"}
	whisperCppModelFiles     = [][3]string{{"ggml-base.bin", "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base.bin", "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"}, {"ggml-silero-v6.2.0.bin", "https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b/ggml-silero-v6.2.0.bin", "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987"}}
	whisperCppVerifiedMu     sync.Mutex
	whisperCppVerifiedFiles  = map[string]whisperCppSignature{}
	sttPlatformSystem        = runtime.GOOS
	sttPlatformMachine       = runtime.GOARCH
	sttProcessEnvLocalScrub  = func() (map[string]string, error) { return tools.IcebathSubprocessEnvChecked(nil, false, nil) }
	sttPMInstalled           = pm.Installed
	sttRunWhisperCppDownload = func(sources []pm.Source, partials string) error {
		_, err := pm.NewDownload(sources, pm.DownloadOptions{PartialsDir: partials}).Run(nil)
		return err
	}
)

// Whisper language code of a hint, empty so the caller falls back when it is not a supported code
func normalizeLocalSTTLanguage(language any, supported []string) string {
	s, ok := language.(string)
	if !ok || pystr.Strip(s) == "" {
		return ""
	}
	raw := pystr.Strip(s)
	folded := strings.ReplaceAll(pystr.Casefold(raw), "_", "-")
	candidate, aliased := localLanguageAliases[raw]
	if !aliased {
		candidate = strings.SplitN(folded, "-", 2)[0]
	}
	shapeValid := utf8.RuneCountInString(candidate) == 2 || threeLetterWhisperCodes[candidate]
	alpha := candidate != ""
	for _, r := range candidate {
		if r >= utf8.RuneSelf || !pystr.IsAlpha(r) {
			alpha = false
		}
	}
	if !alpha || !shapeValid {
		sttLog().Warn(fmt.Sprintf("Local STT language %s is not a language code; using fallback", pyrepr.StrRepr(raw)))
		return ""
	}
	if supported != nil {
		found := false
		for _, code := range supported {
			if pystr.Casefold(code) == candidate {
				found = true
			}
		}
		if !found {
			sttLog().Warn(fmt.Sprintf("Local STT language %s is unsupported; using fallback", pyrepr.StrRepr(raw)))
			return ""
		}
	}
	return candidate
}

// Managed whisper.cpp model directory of the active profile
func whisperCppModelDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "cache", "whisper.cpp")
}

// whisper.cpp command template of the installed managed engine, empty when the install lacks it
func whisperCppCommand(ctx context.Context) (string, error) {
	installed, err := sttPMInstalled(ctx, "whispercpp-cpu", false)
	if err != nil || installed == nil {
		return "", err
	}
	quote := func(p string) string { return pyshlex.Quote(filepath.ToSlash(p)) }
	models := whisperCppModelDir(ctx)
	return quote(installed.Binary) + " -m " + quote(filepath.Join(models, "ggml-base.bin")) + " " +
		"-f {input_path} -l {language} -t 4 -bs 5 -bo 5 -ng -nt -otxt " +
		"-of {output_dir}/transcript --vad " +
		"-vm " + quote(filepath.Join(models, "ggml-silero-v6.2.0.bin")) + " -vsd 500", nil
}

// Digest and file identity a verified whisper.cpp model was checked against
type whisperCppSignature struct {
	digest string
	info   os.FileInfo
}

// Signature of a model file, false when it does not exist
func whisperCppFileSignature(path, digest string) (whisperCppSignature, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return whisperCppSignature{}, false
	}
	return whisperCppSignature{digest: digest, info: st}, true
}

// Whether two signatures name the same unchanged file and digest
func (s whisperCppSignature) same(o whisperCppSignature) bool {
	return s.info != nil && o.info != nil && s.digest == o.digest && os.SameFile(s.info, o.info) &&
		s.info.Size() == o.info.Size() && s.info.ModTime().Equal(o.info.ModTime())
}

// Downloads and verifies the whisper.cpp base and VAD models on first use
func ensureWhisperCppModels(ctx context.Context, modelName string) error {
	if modelName != "base" {
		return &sttValueError{msg: "Windows ARM64 whisper.cpp currently supports stt.local.model: base; requested " + pyrepr.StrRepr(modelName)}
	}
	destination := whisperCppModelDir(ctx)
	var pending []pm.Source
	whisperCppVerifiedMu.Lock()
	for _, f := range whisperCppModelFiles {
		src := pm.Source{URL: f[1], Dest: filepath.Join(destination, f[0]), SHA256: f[2]}
		sig, ok := whisperCppFileSignature(src.Dest, src.SHA256)
		if prev, seen := whisperCppVerifiedFiles[src.Dest]; !ok || !seen || !prev.same(sig) {
			pending = append(pending, src)
		}
	}
	whisperCppVerifiedMu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	sttLog().Info("Preparing whisper.cpp Base and VAD models (first use downloads about 149 MB)")
	if err := sttRunWhisperCppDownload(pending, filepath.Join(destination, "partials")); err != nil {
		return err
	}
	whisperCppVerifiedMu.Lock()
	defer whisperCppVerifiedMu.Unlock()
	for _, src := range pending {
		if sig, ok := whisperCppFileSignature(src.Dest, src.SHA256); ok {
			whisperCppVerifiedFiles[src.Dest] = sig
		}
	}
	return nil
}

// Python ValueError raised by the STT modules
type sttValueError struct{ msg string }

func (e *sttValueError) Error() string { return e.msg }

// Local command template from the env, the managed whisper.cpp engine, or a whisper CLI, empty when none
func localCommandTemplate(ctx context.Context) (string, error) {
	if configured := pystr.Strip(os.Getenv(localSTTCommandEnv)); configured != "" {
		return configured, nil
	}
	managed, err := whisperCppCommand(ctx)
	if err != nil || managed != "" {
		return managed, err
	}
	if bin := findWhisperBinary(); bin != "" {
		return pyshlex.Quote(bin) + " {input_path} --model {model} --output_format txt --output_dir {output_dir} --language {language}", nil
	}
	return "", nil
}

// Whether a local STT command template resolves, the seam tests replace
var hasLocalCommand = func(ctx context.Context) bool {
	tpl, err := localCommandTemplate(ctx)
	return err == nil && tpl != ""
}

// Valid faster-whisper size with cloud only names falling back to the default
func normalizeLocalModel(modelName string) string {
	if modelName == "" {
		return sttDefaultLocalModel
	}
	if sttOpenAIModels[modelName] || sttGroqModels[modelName] || sttRetiredGroqModels[modelName] {
		sttLog().Warn(fmt.Sprintf("STT model '%s' is a cloud-only name and cannot be used with the local provider. Falling back to '%s'. "+
			"Set stt.local.model to a valid faster-whisper size (%s).", modelName, sttDefaultLocalModel, strings.Join(sttModelCatalog["local"], ", ")))
		return sttDefaultLocalModel
	}
	return modelName
}

// Installs faster-whisper on an action path and reports whether it became importable
func tryLazyInstallSTT(ctx context.Context) bool {
	err := pluginCall(func() error { return sttEnsureImport(ctx, "stt-whisper") })
	if err == nil {
		if safeFindSpec("faster_whisper") {
			return true
		}
		sttLog().Warn("faster-whisper was installed but importlib still cannot find it (may require Python restart)")
		return false
	}
	sttLog().Warn(fmt.Sprintf("Lazy install of faster-whisper failed: %v. "+
		"When the message names a restart, this process selected its dependency generation at "+
		"boot and a new one cannot take effect in-flight; otherwise the Icebath process user "+
		"may not be able to write to the dependency environment. Run `icebath tools` as the "+
		"Icebath installation owner and select Local Whisper under Speech-to-Text.", err))
	return false
}

// Whether an error names a missing or broken CUDA runtime library
func looksLikeCudaLibError(err error) bool {
	msg := err.Error()
	for _, m := range cudaLibErrorMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// sysctl value, empty when unavailable
var sttSysctlValue = func(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	s, _ := pycodecs.DecodeReplace(out, "utf-8")
	return pystr.Strip(s)
}

// CPU forced on Apple Silicon including x86_64 under Rosetta
func shouldForceFasterWhisperCPU() bool {
	if sttPlatformSystem != "darwin" {
		return false
	}
	if m := strings.ToLower(sttPlatformMachine); m == "arm64" || m == "aarch64" {
		return true
	}
	return sttSysctlValue("sysctl.proc_translated") == "1" || sttSysctlValue("hw.optional.arm64") == "1"
}

// Idle unload timeout of stt.local, zero meaning never and negatives clamped
func getIdleUnloadSeconds(localCfg map[string]any) int {
	return max(configNumberInt(localCfg, "unload_after_idle_seconds", 0), 0)
}

// One decoded faster-whisper segment
type whisperSegment struct {
	Text         string   `json:"text"`
	NoSpeechProb *float64 `json:"no_speech_prob"`
	AvgLogprob   *float64 `json:"avg_logprob"`
}

// Language and duration faster-whisper reports for one transcription
type whisperInfo struct {
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
}

// A loaded faster-whisper model
type localWhisperModel interface {
	SupportedLanguages() []string
	Transcribe(ctx context.Context, path string, kwargs map[string]any) ([]whisperSegment, whisperInfo, error)
	Close()
}

// Failed WhisperModel construction with the exception class facts the load policy needs
type whisperLoadError struct {
	Class        string `json:"class"`
	Msg          string `json:"error"`
	CacheMiss    bool   `json:"cache_miss"`
	RuntimeError bool   `json:"runtime_error"`
	OSError      bool   `json:"os_error"`
}

func (e *whisperLoadError) Error() string { return e.Msg }

const fasterWhisperWorkerScript = `import json, sys
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

// Managed interpreter process holding one loaded faster-whisper model
type fasterWhisperWorker struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	out       *bufio.Reader
	languages []string
	closed    bool
}

// Reads one JSON reply line from the worker
func (w *fasterWhisperWorker) read(v any) error {
	raw, err := w.out.ReadBytes('\n')
	if err != nil {
		return &sttPythonError{msg: "local whisper process exited: " + err.Error()}
	}
	return json.Unmarshal(raw, v)
}

func (w *fasterWhisperWorker) SupportedLanguages() []string { return w.languages }

func (w *fasterWhisperWorker) Transcribe(_ context.Context, path string, kwargs map[string]any) ([]whisperSegment, whisperInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, whisperInfo{}, &sttPythonError{msg: "local whisper model was released"}
	}
	line, err := json.Marshal(map[string]any{"path": path, "kwargs": kwargs})
	if err != nil {
		return nil, whisperInfo{}, err
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		return nil, whisperInfo{}, &sttPythonError{msg: "local whisper process exited: " + err.Error()}
	}
	var reply struct {
		whisperLoadError
		Segments []whisperSegment `json:"segments"`
		whisperInfo
	}
	if err := w.read(&reply); err != nil {
		return nil, whisperInfo{}, err
	}
	if reply.Class != "" {
		e := reply.whisperLoadError
		return nil, whisperInfo{}, &e
	}
	return reply.Segments, reply.whisperInfo, nil
}

func (w *fasterWhisperWorker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	w.stdin.Close()
	done := make(chan struct{})
	go func() { w.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.cmd.Process.Kill()
		<-done
	}
}

// One WhisperModel construction in a fresh worker, the seam tests replace
var whisperModelCtor = func(ctx context.Context, modelName string, device, computeType any, localFilesOnly bool) (localWhisperModel, error) {
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python[0], append(python[1:], "-c", fasterWhisperWorkerScript)...)
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
	w := &fasterWhisperWorker{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
	spec, _ := json.Marshal(map[string]any{"model": modelName, "device": device, "compute_type": computeType, "local_files_only": localFilesOnly})
	var reply struct {
		whisperLoadError
		OK                 bool     `json:"ok"`
		SupportedLanguages []string `json:"supported_languages"`
	}
	_, werr := stdin.Write(append(spec, '\n'))
	if werr == nil {
		werr = w.read(&reply)
	}
	if werr != nil || !reply.OK {
		w.Close()
		if werr != nil {
			return nil, werr
		}
		e := reply.whisperLoadError
		return nil, &e
	}
	w.languages = reply.SupportedLanguages
	return w, nil
}

// Cached model without contacting the Hub, downloading only on a cache miss
func createWhisperModel(ctx context.Context, modelName string, device, computeType any) (localWhisperModel, error) {
	model, err := whisperModelCtor(ctx, modelName, device, computeType, true)
	if err == nil {
		return model, nil
	}
	var le *whisperLoadError
	if !errors.As(err, &le) || (le.RuntimeError && !strings.Contains(le.Msg, "Unable to open file")) || (!le.RuntimeError && !le.CacheMiss) {
		return nil, err
	}
	sttLog().Info(fmt.Sprintf("faster-whisper model '%s' is not cached; downloading it from the Hugging Face Hub", modelName))
	model, err = whisperModelCtor(ctx, modelName, device, computeType, false)
	if err == nil {
		return model, nil
	}
	if errors.As(err, &le) && le.OSError {
		return nil, &whisperLoadError{Class: "RuntimeError", RuntimeError: true, Msg: fmt.Sprintf("Unable to download faster-whisper model '%s': %s. "+
			"If huggingface.co is unreachable, set HF_ENDPOINT to an accessible mirror; "+
			"when using a mirror with hf-xet installed, also set HF_HUB_DISABLE_XET=1.", modelName, le.Msg)}
	}
	return nil, err
}

// faster-whisper load with the requested device, falling back to CPU int8 on a CUDA library failure
func loadLocalWhisperModel(ctx context.Context, modelName string, device, computeType any) (localWhisperModel, error) {
	if shouldForceFasterWhisperCPU() {
		if _, ok := os.LookupEnv("KMP_DUPLICATE_LIB_OK"); !ok {
			os.Setenv("KMP_DUPLICATE_LIB_OK", "TRUE")
		}
		sttLog().Info("Apple Silicon/Rosetta detected — loading faster-whisper on CPU (int8) to avoid native device autodetection crashes")
		return createWhisperModel(ctx, modelName, "cpu", "int8")
	}
	model, err := createWhisperModel(ctx, modelName, device, computeType)
	if err == nil || !looksLikeCudaLibError(err) {
		return model, err
	}
	sttLog().Warn(fmt.Sprintf("faster-whisper CUDA load failed (%v) — falling back to CPU (int8). "+
		"Install the NVIDIA CUDA runtime (libcublas/libcudnn) to use GPU.", err))
	return createWhisperModel(ctx, modelName, "cpu", "int8")
}

// Keyword arguments of every local faster-whisper transcribe call with the anti hallucination hardening
func buildLocalTranscribeKwargs(ctx context.Context, sttConfig map[string]any) (map[string]any, error) {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	localCfg, err := orEmptyDict(sttConfig, "local")
	if err != nil {
		return nil, err
	}
	vad, present := localCfg["vad"]
	vadFilter := !present || vad == nil || pyTruthy(vad)
	kwargs := map[string]any{"beam_size": 5, "condition_on_previous_text": false, "vad_filter": vadFilter}
	if vadFilter {
		kwargs["vad_parameters"] = map[string]any{"min_silence_duration_ms": configNumberInt(localCfg, "vad_min_silence_ms", vadMinSilenceMSDefault)}
	}
	kwargs["no_speech_threshold"], kwargs["log_prob_threshold"] = confidenceThresholds(localCfg)
	if forced := resolveSTTLanguage(ctx, "local", sttConfig); forced != "" {
		kwargs["language"] = forced
	}
	if prompt, ok := localCfg["initial_prompt"].(string); ok && pystr.Strip(prompt) != "" {
		kwargs["initial_prompt"] = prompt
	}
	return kwargs, nil
}

// No speech probability and average logprob gate thresholds of stt.local
func confidenceThresholds(localCfg map[string]any) (float64, float64) {
	return configNumberFloat(localCfg, "no_speech_prob_threshold", noSpeechProbThresholdDefault),
		configNumberFloat(localCfg, "logprob_threshold", logprobThresholdDefault)
}

// Whether a segment is non speech with low decode confidence, unknown shapes never dropped
func isHallucinatedSegment(s whisperSegment, noSpeechThreshold, logprobThreshold float64) bool {
	if s.NoSpeechProb == nil || s.AvgLogprob == nil {
		return false
	}
	return *s.NoSpeechProb > noSpeechThreshold && *s.AvgLogprob < logprobThreshold
}

// Segment texts joined with probable silence hallucinations dropped
func joinConfidentSegments(segments []whisperSegment, localCfg map[string]any) string {
	noSpeech, logprob := confidenceThresholds(localCfg)
	kept := []string{}
	for _, s := range segments {
		if isHallucinatedSegment(s, noSpeech, logprob) {
			sttLog().Debug(fmt.Sprintf("Dropping probable hallucinated segment %s (no_speech_prob=%.3f, avg_logprob=%.3f)",
				pyrepr.StrRepr(s.Text), *s.NoSpeechProb, *s.AvgLogprob))
			continue
		}
		kept = append(kept, pystr.Strip(s.Text))
	}
	return pystr.Strip(strings.Join(kept, " "))
}

// Environment map as KEY=VALUE pairs
func sttEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Runs the local STT command template and reads back its txt transcript
func transcribeLocalCommand(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	if h.Prompt != "" {
		logPromptUnsupported("STT provider 'local_command'")
	}
	template, err := localCommandTemplate(ctx)
	if err != nil {
		return nil, err
	}
	if template == "" {
		return sttError(localSTTCommandEnv + " not configured and no local whisper binary was found"), nil
	}
	configured := h.Language
	if configured == "" {
		configured = resolveSTTLanguage(ctx, "local", nil)
	}
	language := normalizeLocalSTTLanguage(configured, nil)
	if language == "" {
		language = sttDefaultLocalLanguage
	}
	normalizedModel := normalizeLocalModel(modelName)
	result, err := runLocalCommand(ctx, filePath, template, language, normalizedModel)
	if err == nil {
		return result, nil
	}
	var fe *pyrepr.FormatError
	var pe *sttProcessError
	switch {
	case errors.As(err, &fe) && fe.Class == "KeyError":
		return sttError(fmt.Sprintf("Invalid %s template, missing placeholder: %s", localSTTCommandEnv, fe.Msg)), nil
	case errors.As(err, &pe):
		details := processErrorDetail(pe)
		sttLog().Error(fmt.Sprintf("Local STT command failed for %s: %s", filePath, details))
		return sttError("Local STT failed: " + details), nil
	}
	sttLog().Error(fmt.Sprintf("Unexpected error during local command transcription: %v", err))
	return sttError(fmt.Sprintf("Local transcription failed: %v", err)), nil
}

// Body of the local command transcription whose failures the caller maps to envelopes
func runLocalCommand(ctx context.Context, filePath, template, language, model string) (map[string]any, error) {
	if pystr.Strip(os.Getenv(localSTTCommandEnv)) == "" {
		managed, err := whisperCppCommand(ctx)
		if err != nil {
			return nil, err
		}
		if template == managed {
			if err := ensureWhisperCppModels(ctx, model); err != nil {
				return nil, err
			}
		}
	}
	outputDir, err := os.MkdirTemp("", "icebath-local-stt-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outputDir)
	prepared, prepError, err := prepareLocalAudio(filePath, outputDir)
	if err != nil {
		return nil, err
	}
	if prepError != "" {
		return sttError(prepError), nil
	}
	command, err := pyrepr.FormatMap(template, map[string]any{"input_path": pyshlex.Quote(prepared), "output_dir": pyshlex.Quote(outputDir),
		"language": pyshlex.Quote(language), "model": pyshlex.Quote(model)})
	if err != nil {
		return nil, err
	}
	env, err := sttProcessEnvLocalScrub()
	if err != nil {
		return nil, err
	}
	argv, err := pyshlex.SplitPOSIX(command, false)
	if err != nil {
		return nil, err
	}
	if _, _, err := sttRunQuiet(argv, 300, sttEnvList(env)); err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(outputDir, "*.txt"))
	sort.Strings(matches)
	if len(matches) == 0 {
		return sttError("Local STT command completed but did not produce a .txt transcript"), nil
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	text = pystr.Strip(text)
	sttLog().Info(fmt.Sprintf("Transcribed %s via local STT command (%s, %d chars)", filepath.Base(filePath), model, utf8.RuneCountInString(text)))
	return sttOK(text, "local_command"), nil
}
