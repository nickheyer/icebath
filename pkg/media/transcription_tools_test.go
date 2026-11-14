package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestFasterWhisperImportableHook(t *testing.T) {
	for _, present := range []bool{true, false} {
		var asked []string
		savedProbe, savedFlag := safeFindSpec, hasFasterWhisper
		safeFindSpec = func(module string) bool { asked = append(asked, module); return present }
		hasFasterWhisper = newFindSpecFlag("faster_whisper")
		if provider.FasterWhisperImportable() != present || provider.FasterWhisperImportable() != present {
			t.Fatalf("FasterWhisperImportable differs from the probe answer %v", present)
		}
		if len(asked) != 1 || asked[0] != "faster_whisper" {
			t.Fatalf("probe asked %v, want faster_whisper once", asked)
		}
		safeFindSpec, hasFasterWhisper = savedProbe, savedFlag
	}
}

// Isolated home, scrubbed keys, no lazy installs and deterministic module flags for one STT test
func sttTestEnv(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, k := range []string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY", "ELEVENLABS_API_KEY",
		"DEEPINFRA_API_KEY", "XAI_API_KEY", localSTTCommandEnv, localSTTLanguageEnv, "STT_XAI_MODEL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	saved := []func(){}
	keep := func(restore func()) { saved = append(saved, restore) }
	ensure, find, fw, pi := sttEnsureImport, safeFindSpec, hasFasterWhisper, hasPilk
	keep(func() {
		sttEnsureImport, safeFindSpec, hasFasterWhisper, hasPilk = ensure, find, fw, pi
	})
	sel, inst, bin, sys, client, getProv, prep := sttReadSelection, sttPMInstalled, sttFindBinary, sttPlatformSystem, sttNewOpenAIClient, sttGetProvider, sttPrepareAudio
	keep(func() {
		sttReadSelection, sttPMInstalled, sttFindBinary, sttPlatformSystem, sttNewOpenAIClient, sttGetProvider, sttPrepareAudio = sel, inst, bin, sys, client, getProv, prep
	})
	run, ctor, handlers, load, hasCmd, xai := sttRunQuiet, whisperModelCtor, sttBuiltinHandlers, loadSTTConfig, hasLocalCommand, sttResolveXAICredentials
	keep(func() {
		sttRunQuiet, whisperModelCtor, sttBuiltinHandlers, loadSTTConfig, hasLocalCommand, sttResolveXAICredentials = run, ctor, handlers, load, hasCmd, xai
	})
	sttEnsureImport = func(context.Context, string) error { return nil }
	safeFindSpec = func(string) bool { return false }
	hasFasterWhisper, hasPilk = func() bool { return true }, func() bool { return false }
	sttReadSelection = func(context.Context) string { return "" }
	sttPMInstalled = func(context.Context, string, bool) (*pm.InstalledPackage, error) { return nil, nil }
	sttFindBinary = func(string) string { return "" }
	sttResolveXAICredentials = func(context.Context) (string, string, string) { return "", "", "" }
	sttPlatformSystem = "linux"
	copied := map[string]sttHandler{}
	for k, v := range sttBuiltinHandlers {
		copied[k] = v
	}
	sttBuiltinHandlers = copied
	resetLocalModel()
	t.Cleanup(func() {
		resetLocalModel()
		for i := len(saved) - 1; i >= 0; i-- {
			saved[i]()
		}
	})
	return context.Background()
}

// Drops the cached local model between tests
func resetLocalModel() {
	localModelMu.Lock()
	defer localModelMu.Unlock()
	if localModel != nil {
		localModel.release()
	}
	localModel, localModelName = nil, ""
}

// Fixed config the module loader returns for one test
func withSTTConfig(cfg map[string]any) {
	loadSTTConfig = func(context.Context) map[string]any { return cfg }
}

// One second of 16 kHz mono silence as a WAV file
func sampleWAV(t *testing.T) string {
	t.Helper()
	data := make([]byte, 44+32000)
	copy(data, "RIFF")
	putLE32(data[4:], uint32(36+32000))
	copy(data[8:], "WAVEfmt ")
	putLE32(data[16:], 16)
	data[20], data[22] = 1, 1
	putLE32(data[24:], 16000)
	putLE32(data[28:], 32000)
	data[32], data[34] = 2, 16
	copy(data[36:], "data")
	putLE32(data[40:], 32000)
	p := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// Fake ogg payload for validation tests
func sampleOGG(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.ogg")
	if err := os.WriteFile(p, []byte("fake audio data"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Sparse WAV shaped file one byte above the remote upload cap
func oversizedWAV(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oversized.wav")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{0}, sttMaxFileSize)
	f.Close()
	return p
}

// Recording double of the OpenAI transcription client
type fakeSTTClient struct {
	mu         sync.Mutex
	calls      [][][2]string
	paths      []string
	replies    []any
	errs       []error
	timeout    float64
	maxRetries int
	created    int
}

func (f *fakeSTTClient) Create(_ context.Context, path string, fields [][2]string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.calls)
	f.calls = append(f.calls, fields)
	f.paths = append(f.paths, path)
	var reply any = "hi"
	if i < len(f.replies) {
		reply = f.replies[i]
	} else if len(f.replies) > 0 {
		reply = f.replies[len(f.replies)-1]
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	return reply, nil
}

// Installs the double as the client every OpenAI compatible handler constructs
func installFakeSTTClient(f *fakeSTTClient) {
	sttNewOpenAIClient = func(apiKey, baseURL string, timeout float64, maxRetries int) sttTranscriptionClient {
		f.mu.Lock()
		f.timeout, f.maxRetries = timeout, maxRetries
		f.created++
		f.mu.Unlock()
		return f
	}
}

// Field values of the last create call by name
func lastFields(f *fakeSTTClient) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	if len(f.calls) == 0 {
		return out
	}
	for _, kv := range f.calls[len(f.calls)-1] {
		out[kv[0]] = kv[1]
	}
	return out
}

func TestGetProviderGroqGroqWhenKeySet(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	hasFasterWhisper = func() bool { return false }
	hasLocalCommand = func(context.Context) bool { return false }
	if got, err := getProvider(ctx, map[string]any{"provider": "groq"}); err != nil || got != "groq" {
		t.Fatalf("getProvider = %q, %v", got, err)
	}
}

func TestProcessErrorDetailPrefersStderrOverStdout(t *testing.T) {
	e := &sttProcessError{Cmd: []string{"ffmpeg"}, ReturnCode: 1, Stderr: " stderr detail \n", Stdout: "stdout detail"}
	if got := processErrorDetail(e); got != "stderr detail" {
		t.Fatalf("detail = %q", got)
	}
}

func TestProcessErrorDetailMissingOrByteOutputDoesNotMaskTheFailure(t *testing.T) {
	cases := []struct {
		stderr, stdout any
		expected       string
	}{
		{nil, nil, "returned non-zero exit status 1"},
		{nil, " stdout detail \n", "stdout detail"},
		{[]byte(" bad \xff output \n"), nil, "bad � output"},
	}
	for _, c := range cases {
		got := processErrorDetail(&sttProcessError{Cmd: []string{"ffmpeg"}, ReturnCode: 1, Stderr: c.stderr, Stdout: c.stdout})
		if !strings.Contains(got, c.expected) {
			t.Fatalf("detail %q lacks %q", got, c.expected)
		}
	}
}

func TestGetProviderFallbackPriorityAutoDetectPrefersLocal(t *testing.T) {
	ctx := sttTestEnv(t)
	if got, _ := getProvider(ctx, map[string]any{}); got != "local" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestGetProviderFallbackPriorityUnknownProviderPassedThrough(t *testing.T) {
	ctx := sttTestEnv(t)
	if got, _ := getProvider(ctx, map[string]any{"provider": "custom-endpoint"}); got != "custom-endpoint" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestExplicitProviderRespectedSeededLocalWithoutStoredSelectionAutodetects(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	hasFasterWhisper = func() bool { return false }
	hasLocalCommand = func(context.Context) bool { return false }
	if got, _ := getProvider(ctx, map[string]any{"provider": "local"}); got != "groq" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestExplicitProviderRespectedExplicitLocalUsesLocalCommandFallback(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv(localSTTCommandEnv, "whisper {input_path} --output_dir {output_dir} --language {language}")
	hasFasterWhisper = func() bool { return false }
	if got, _ := getProvider(ctx, map[string]any{"provider": "local"}); got != "local_command" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestExplicitProviderRespectedAutoDetectPrefersGroqOverOpenai(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	t.Setenv("OPENAI_API_KEY", "sk-real-key")
	hasFasterWhisper = func() bool { return false }
	hasLocalCommand = func(context.Context) bool { return false }
	if got, _ := getProvider(ctx, map[string]any{}); got != "groq" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestTranscribeGroqNoKey(t *testing.T) {
	ctx := sttTestEnv(t)
	r, _ := transcribeGroq(ctx, "/tmp/test.ogg", "whisper-large-v3-turbo", sttHints{})
	if r["success"] != false || !strings.Contains(r["error"].(string), "GROQ_API_KEY") {
		t.Fatalf("result = %v", r)
	}
}

func TestOpenAIClientConfigSttOpenaiConfigControlsSdkClient(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	h := os.Getenv("ICEBATH_HOME")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("stt:\n  openai:\n    timeout: 95\n    max_retries: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeSTTClient{}
	installFakeSTTClient(f)
	r, _ := transcribeGroq(ctx, sampleWAV(t), "whisper-large-v3-turbo", sttHints{})
	if r["success"] != true || f.timeout != 95 || f.maxRetries != 3 {
		t.Fatalf("result = %v timeout %v retries %v", r, f.timeout, f.maxRetries)
	}
}

func TestOpenAIClientConfigNullGroqSubsectionIsSafe(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	f := &fakeSTTClient{}
	installFakeSTTClient(f)
	withSTTConfig(map[string]any{"groq": nil})
	r, _ := transcribeGroq(ctx, sampleWAV(t), "whisper-large-v3-turbo", sttHints{})
	if r["success"] != true {
		t.Fatalf("result = %v", r)
	}
	if _, ok := lastFields(f)["language"]; ok {
		t.Fatalf("language sent: %v", lastFields(f))
	}
}

func TestTranscribeLocalCommandCommandProviderUsesSanitizedChildEnv(t *testing.T) {
	sttTestEnv(t)
	t.Setenv("AUXILIARY_VISION_API_KEY", "sk-vision")
	t.Setenv("GATEWAY_RELAY_SECRET", "relay-secret")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("MY_SAFE_STT_VAR", "keep")
	var env []string
	saved := spawnCommandProvider
	t.Cleanup(func() { spawnCommandProvider = saved })
	spawnCommandProvider = func(_ context.Context, cmd *exec.Cmd, purpose string) error { env = cmd.Env; return cmd.Start() }
	result, err := runCommandSTT(context.Background(), nil, "echo hi", 1, nil)
	if err != nil || result.Returncode != 0 {
		t.Fatalf("run = %v, %v", result, err)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, k := range []string{"AUXILIARY_VISION_API_KEY=", "GATEWAY_RELAY_SECRET=", "OPENAI_API_KEY="} {
		if strings.Contains(joined, "\n"+k) {
			t.Fatalf("%s leaked into the child env", k)
		}
	}
	if !strings.Contains(joined, "\nMY_SAFE_STT_VAR=keep\n") {
		t.Fatal("MY_SAFE_STT_VAR missing from the child env")
	}
}

// Output directory argument of a local STT command line
func localSTTOutputDir(argv []string) string {
	for i, a := range argv {
		if a == "--output_dir" && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.Contains(filepath.Base(filepath.Dir(a)), "icebath-local-stt-") {
			return filepath.Dir(a)
		}
	}
	return ""
}

func TestTranscribeLocalCommandLocalWhisperSubprocessUsesSanitizedEnv(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("AUXILIARY_VISION_API_KEY", "sk-vision")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("MY_SAFE_LOCAL_STT", "keep")
	t.Setenv(localSTTCommandEnv, "whisper {input_path} --model {model} --output_dir {output_dir} --language {language}")
	var env []string
	sttRunQuiet = func(command []string, timeout float64, e []string) (string, string, error) {
		env = e
		os.WriteFile(filepath.Join(localSTTOutputDir(command), "transcript.txt"), []byte("hello"), 0o644)
		return "", "", nil
	}
	r, _ := transcribeLocalCommand(ctx, sampleWAV(t), "base", sttHints{})
	if r["success"] != true || env == nil {
		t.Fatalf("result = %v env %v", r, env)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	if strings.Contains(joined, "\nAUXILIARY_VISION_API_KEY=") || strings.Contains(joined, "\nOPENAI_API_KEY=") || !strings.Contains(joined, "\nMY_SAFE_LOCAL_STT=keep\n") {
		t.Fatalf("child env not scrubbed: %v", env)
	}
}

func TestTranscribeLocalCommandCommandFallbackWithTemplate(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv(localSTTCommandEnv, "whisper {input_path} --model {model} --output_dir {output_dir} --language {language}")
	t.Setenv(localSTTLanguageEnv, "en")
	sttFindBinary = func(name string) string {
		if name == "ffmpeg" {
			return "/opt/homebrew/bin/ffmpeg"
		}
		return ""
	}
	sttRunQuiet = func(command []string, timeout float64, e []string) (string, string, error) {
		os.WriteFile(filepath.Join(localSTTOutputDir(command), "test.txt"), []byte("hello from local command\n"), 0o644)
		return "", "", nil
	}
	r, _ := transcribeLocalCommand(ctx, sampleOGG(t), "base", sttHints{})
	if r["success"] != true || r["transcript"] != "hello from local command" || r["provider"] != "local_command" {
		t.Fatalf("result = %v", r)
	}
}

// One recorded WhisperModel construction
type whisperCtorCall struct {
	model, device, compute any
	localOnly              bool
}

// Scripted WhisperModel constructor recording every call
func scriptWhisperCtor(results ...any) *[]whisperCtorCall {
	var mu sync.Mutex
	calls := &[]whisperCtorCall{}
	whisperModelCtor = func(_ context.Context, name string, device, compute any, localOnly bool) (localWhisperModel, error) {
		mu.Lock()
		defer mu.Unlock()
		i := len(*calls)
		*calls = append(*calls, whisperCtorCall{name, device, compute, localOnly})
		r := results[min(i, len(results)-1)]
		if err, ok := r.(error); ok {
			return nil, err
		}
		return r.(localWhisperModel), nil
	}
	return calls
}

// Scripted faster-whisper model double
type fakeWhisperModel struct {
	mu       sync.Mutex
	segments []whisperSegment
	err      error
	kwargs   []map[string]any
}

func (m *fakeWhisperModel) SupportedLanguages() []string { return nil }
func (m *fakeWhisperModel) Transcribe(_ context.Context, _ string, kwargs map[string]any) ([]whisperSegment, whisperInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kwargs = append(m.kwargs, kwargs)
	if m.err != nil {
		return nil, whisperInfo{}, m.err
	}
	return m.segments, whisperInfo{Language: "en", Duration: 1.0}, nil
}
func (m *fakeWhisperModel) Close() {}

func TestLocalModelLoadingCachedModelLoadNeverUsesOnlineResolution(t *testing.T) {
	ctx := sttTestEnv(t)
	cached := &fakeWhisperModel{}
	calls := scriptWhisperCtor(cached)
	m, err := createWhisperModel(ctx, "base", "cpu", "int8")
	if err != nil || m != cached || len(*calls) != 1 || (*calls)[0] != (whisperCtorCall{"base", "cpu", "int8", true}) {
		t.Fatalf("model %v err %v calls %v", m, err, *calls)
	}
}

func TestLocalModelLoadingCacheMissFallsBackWithActionableDownloadFailure(t *testing.T) {
	for _, downloadError := range []string{"", "Got: ConnectTimeout: [Errno 110] Connection timed out"} {
		ctx := sttTestEnv(t)
		miss := &whisperLoadError{Class: "LocalEntryNotFoundError", Msg: "not cached", CacheMiss: true, OSError: true}
		downloaded := &fakeWhisperModel{}
		var online any = downloaded
		if downloadError != "" {
			online = &whisperLoadError{Class: "LocalEntryNotFoundError", Msg: downloadError, CacheMiss: true, OSError: true}
		}
		calls := scriptWhisperCtor(miss, online)
		m, err := createWhisperModel(ctx, "base", "auto", "auto")
		if downloadError != "" {
			var le *whisperLoadError
			if !errors.As(err, &le) || le.Class != "RuntimeError" || !strings.Contains(err.Error(), "HF_ENDPOINT") || !strings.Contains(err.Error(), "HF_HUB_DISABLE_XET=1") {
				t.Fatalf("err = %v", err)
			}
		} else if err != nil || m != downloaded {
			t.Fatalf("model %v err %v", m, err)
		}
		want := []whisperCtorCall{{"base", "auto", "auto", true}, {"base", "auto", "auto", false}}
		if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
			t.Fatalf("calls = %v", *calls)
		}
	}
}

func TestLocalModelLoadingPartialCacheIsTreatedAsACacheMiss(t *testing.T) {
	ctx := sttTestEnv(t)
	downloaded := &fakeWhisperModel{}
	calls := scriptWhisperCtor(&whisperLoadError{Class: "RuntimeError", RuntimeError: true, Msg: "Unable to open file 'model.bin' in model '/cache/snap'"}, downloaded)
	m, err := createWhisperModel(ctx, "base", "cpu", "int8")
	if err != nil || m != downloaded || len(*calls) != 2 || !(*calls)[0].localOnly || (*calls)[1].localOnly {
		t.Fatalf("model %v err %v calls %v", m, err, *calls)
	}
}

// Audio file named test.ogg holding fake bytes
func fakeOGG(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.ogg")
	os.WriteFile(p, []byte("fake"), 0o644)
	return p
}

func TestTranscribeLocalExtendedModelReuseOnSecondCall(t *testing.T) {
	ctx := sttTestEnv(t)
	calls := scriptWhisperCtor(&fakeWhisperModel{segments: []whisperSegment{{Text: "hi"}}})
	audio := fakeOGG(t)
	transcribeLocal(ctx, audio, "base", sttHints{})
	transcribeLocal(ctx, audio, "base", sttHints{})
	if len(*calls) != 1 {
		t.Fatalf("WhisperModel built %d times", len(*calls))
	}
}

func TestTranscribeLocalExtendedConfigDeviceAndComputeTypePassedToWhisper(t *testing.T) {
	ctx := sttTestEnv(t)
	calls := scriptWhisperCtor(&fakeWhisperModel{segments: []whisperSegment{{Text: "hi"}}})
	withSTTConfig(map[string]any{"local": map[string]any{"device": "cpu", "compute_type": "float32"}})
	r, _ := transcribeLocal(ctx, fakeOGG(t), "base", sttHints{})
	if r["success"] != true || len(*calls) != 1 || (*calls)[0] != (whisperCtorCall{"base", "cpu", "float32", true}) {
		t.Fatalf("result %v calls %v", r, *calls)
	}
}

func TestTranscribeLocalExtendedCudaOutOfMemoryDoesNotTriggerCpuFallback(t *testing.T) {
	ctx := sttTestEnv(t)
	calls := scriptWhisperCtor(&whisperLoadError{Class: "RuntimeError", RuntimeError: true, Msg: "CUDA out of memory"})
	r, _ := transcribeLocal(ctx, fakeOGG(t), "base", sttHints{})
	if len(*calls) != 1 || r["success"] != false || !strings.Contains(r["error"].(string), "CUDA out of memory") {
		t.Fatalf("result %v calls %v", r, *calls)
	}
}

func TestTranscribeLocalExtendedIterationTimeCudaDlopenRetriesOnCpu(t *testing.T) {
	ctx := sttTestEnv(t)
	zero := 0.0
	cuda := &fakeWhisperModel{err: &sttPythonError{msg: "Library cublas64_12.dll is not found or cannot be loaded"}}
	cpu := &fakeWhisperModel{segments: []whisperSegment{{Text: "hi", NoSpeechProb: &zero, AvgLogprob: &zero}}}
	calls := scriptWhisperCtor(cuda, cpu)
	r, _ := transcribeLocal(ctx, fakeOGG(t), "base", sttHints{})
	if r["success"] != true || r["transcript"] != "hi" || len(*calls) != 2 || (*calls)[1].device != "cpu" || (*calls)[1].compute != "int8" {
		t.Fatalf("result %v calls %v", r, *calls)
	}
}

func TestTranscribeLocalExtendedIterationTimeNonLibErrorSurfacesWithoutCpuRetry(t *testing.T) {
	ctx := sttTestEnv(t)
	calls := scriptWhisperCtor(&fakeWhisperModel{err: &sttPythonError{msg: "CUDA out of memory"}})
	r, _ := transcribeLocal(ctx, fakeOGG(t), "base", sttHints{})
	if r["success"] != false || !strings.Contains(r["error"].(string), "CUDA out of memory") || len(*calls) != 1 {
		t.Fatalf("result %v calls %v", r, *calls)
	}
}

func TestModelAutoCorrectionGroqCorrectsOpenaiModel(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	f := &fakeSTTClient{replies: []any{"hello world"}}
	installFakeSTTClient(f)
	transcribeGroq(ctx, sampleWAV(t), "whisper-1", sttHints{})
	if lastFields(f)["model"] != sttDefaultGroqModel {
		t.Fatalf("model = %q", lastFields(f)["model"])
	}
}

func TestModelAutoCorrectionUnknownModelPassesThroughGroq(t *testing.T) {
	ctx := sttTestEnv(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	f := &fakeSTTClient{replies: []any{"test"}}
	installFakeSTTClient(f)
	transcribeGroq(ctx, sampleWAV(t), "my-custom-model", sttHints{})
	if lastFields(f)["model"] != "my-custom-model" {
		t.Fatalf("model = %q", lastFields(f)["model"])
	}
}

func TestValidateAudioFileEdgeCasesDirectoryIsNotAFile(t *testing.T) {
	d := filepath.Join(t.TempDir(), "audio.ogg")
	os.Mkdir(d, 0o755)
	if r := validateAudioFile(d, true); r == nil || !strings.Contains(r["error"].(string), "not a file") {
		t.Fatalf("result = %v", r)
	}
}

func TestValidateAudioFileEdgeCasesSymlinkWithSupportedExtensionIsRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("not audio"), 0o644)
	link := filepath.Join(dir, "linked.wav")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if r := validateAudioFile(link, true); r == nil || !strings.Contains(r["error"].(string), "symbolic link") {
		t.Fatalf("result = %v", r)
	}
}

// Replaces one built in handler with a recorder returning the given envelope
func recordHandler(name string, reply map[string]any) *[][2]string {
	var mu sync.Mutex
	calls := &[][2]string{}
	sttBuiltinHandlers[name] = func(_ context.Context, path, model string, _ sttHints) (map[string]any, error) {
		mu.Lock()
		*calls = append(*calls, [2]string{path, model})
		mu.Unlock()
		return reply, nil
	}
	return calls
}

func TestTranscribeAudioDispatchOversizedLocalFileReachesDispatcher(t *testing.T) {
	ctx := sttTestEnv(t)
	withSTTConfig(map[string]any{"provider": "local"})
	sttGetProvider = func(context.Context, map[string]any) (string, error) { return "local", nil }
	calls := recordHandler("local", map[string]any{"success": true, "transcript": "hi"})
	r, err := TranscribeAudio(ctx, oversizedWAV(t), "", "")
	if err != nil || r["success"] != true || len(*calls) != 1 {
		t.Fatalf("result %v err %v calls %v", r, err, *calls)
	}
}

func TestTranscribeAudioDispatchNoProviderReturnsError(t *testing.T) {
	ctx := sttTestEnv(t)
	withSTTConfig(map[string]any{})
	sttGetProvider = func(context.Context, map[string]any) (string, error) { return "none", nil }
	r, _ := TranscribeAudio(ctx, sampleOGG(t), "", "")
	if r["success"] != false || !strings.Contains(r["error"].(string), "No STT provider") {
		t.Fatalf("result = %v", r)
	}
}

func TestTranscribeAudioDispatchSilkSymlinkIsRejectedBeforePreprocessing(t *testing.T) {
	ctx := sttTestEnv(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "voice.silk")
	os.WriteFile(target, []byte("\x02#!SILK_V3fake"), 0o644)
	link := filepath.Join(dir, "linked.silk")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	prepared := false
	sttPrepareAudio = func(context.Context, string) (string, string, map[string]any) { prepared = true; return "", "", nil }
	r, _ := TranscribeAudio(ctx, link, "", "")
	if r["success"] != false || !strings.Contains(r["error"].(string), "symbolic link") || prepared {
		t.Fatalf("result %v prepared %v", r, prepared)
	}
}

func TestTranscribeAudioDispatchConfigLocalModelUsed(t *testing.T) {
	ctx := sttTestEnv(t)
	withSTTConfig(map[string]any{"local": map[string]any{"model": "small"}})
	sttGetProvider = func(context.Context, map[string]any) (string, error) { return "local", nil }
	calls := recordHandler("local", map[string]any{"success": true, "transcript": "hi"})
	TranscribeAudio(ctx, sampleOGG(t), "", "")
	if len(*calls) != 1 || (*calls)[0][1] != "small" {
		t.Fatalf("calls = %v", *calls)
	}
}
