package media

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Saves and restores the provider seams the cloud and dispatch tests replace
func keepSTTSeams(t *testing.T) context.Context {
	ctx := sttTestEnv(t)
	post, refresh, mistral, caf, dispatch, transcode := postAudioMultipart, sttRefreshXAICredentials, sttMistralTranscribe, sttConvertCAF, sttDispatchProvider, sttTranscode
	key, managed, enabled, message := sttResolveOpenAIAudioAPIKey, sttManagedOpenAIAudio, sttManagedNousToolsEnabled, sttNousGatewayUnavailableMessage
	t.Cleanup(func() {
		postAudioMultipart, sttRefreshXAICredentials, sttMistralTranscribe, sttConvertCAF, sttDispatchProvider, sttTranscode = post, refresh, mistral, caf, dispatch, transcode
		sttResolveOpenAIAudioAPIKey, sttManagedOpenAIAudio, sttManagedNousToolsEnabled, sttNousGatewayUnavailableMessage = key, managed, enabled, message
	})
	return ctx
}

// One recorded multipart POST
type sttPostCall struct {
	url     string
	headers map[string]string
	data    map[string]string
}

// Scripted multipart POST double answering each call with the next status and JSON body
func fakeSTTPosts(t *testing.T, replies ...[2]any) *[]sttPostCall {
	calls := &[]sttPostCall{}
	postAudioMultipart = func(_ context.Context, rawURL string, headers map[string]string, _ string, data [][2]string) (*pyrequests.Response, error) {
		d := map[string]string{}
		for _, kv := range data {
			d[kv[0]] = kv[1]
		}
		i := len(*calls)
		*calls = append(*calls, sttPostCall{rawURL, headers, d})
		r := replies[min(i, len(replies)-1)]
		body, err := json.Marshal(r[1])
		if err != nil {
			t.Fatal(err)
		}
		return &pyrequests.Response{Status: r[0].(int), Body: body}, nil
	}
	return calls
}

// Scripted Mistral transcription endpoint double
func fakeMistral(result any, class string, err error) *int {
	n := new(int)
	sttMistralTranscribe = func(context.Context, string, string, [][2]string) (any, string, error) {
		*n++
		return result, class, err
	}
	return n
}

func TestTranscribeMistralSuccessfulTranscription(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	n := fakeMistral(map[string]any{"text": "hello from mistral"}, "", nil)
	r, _ := transcribeMistral(ctx, sampleOGG(t), "voxtral-mini-latest", sttHints{})
	if r["success"] != true || r["transcript"] != "hello from mistral" || r["provider"] != "mistral" || *n != 1 {
		t.Fatalf("result %v calls %d", r, *n)
	}
}

func TestTranscribeMistralApiErrorReturnsFailure(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	fakeMistral(nil, "RuntimeError", errors.New("secret-key-leaked"))
	r, _ := transcribeMistral(ctx, sampleOGG(t), "voxtral-mini-latest", sttHints{})
	e := r["error"].(string)
	if r["success"] != false || !strings.Contains(e, "RuntimeError") || strings.Contains(e, "secret-key-leaked") {
		t.Fatalf("result = %v", r)
	}
}

// No local backend and no OpenAI credential, the auto detection ladder starting at mistral
func noLocalNoOpenAI() {
	hasFasterWhisper = func() bool { return false }
	hasLocalCommand = func(context.Context) bool { return false }
}

func TestGetProviderMistralAutoDetectMistralAfterOpenai(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	noLocalNoOpenAI()
	if got, _ := getProvider(ctx, map[string]any{}); got != "mistral" {
		t.Fatalf("getProvider = %q", got)
	}
}

// Dispatches a sample through TranscribeAudio to a recorded handler and returns the model it received
func dispatchedModel(t *testing.T, ctx context.Context, providerName string, cfg map[string]any) string {
	withSTTConfig(cfg)
	sttGetProvider = func(context.Context, map[string]any) (string, error) { return providerName, nil }
	calls := recordHandler(providerName, map[string]any{"success": true, "transcript": "hi"})
	TranscribeAudio(ctx, sampleOGG(t), "", "")
	if len(*calls) != 1 {
		t.Fatalf("handler calls = %v", *calls)
	}
	return (*calls)[0][1]
}

func TestTranscribeAudioMistralDispatchConfigMistralModelUsed(t *testing.T) {
	ctx := keepSTTSeams(t)
	if m := dispatchedModel(t, ctx, "mistral", map[string]any{"provider": "mistral", "mistral": map[string]any{"model": "voxtral-mini-2602"}}); m != "voxtral-mini-2602" {
		t.Fatalf("model = %q", m)
	}
}

func TestTranscribeXAISuccessfulTranscription(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	withSTTConfig(map[string]any{})
	fakeSTTPosts(t, [2]any{200, map[string]any{"text": "bonjour le monde", "language": "fr", "duration": 3.2}})
	r, _ := transcribeXAI(ctx, sampleOGG(t), "grok-stt", sttHints{})
	if r["success"] != true || r["transcript"] != "bonjour le monde" || r["provider"] != "xai" {
		t.Fatalf("result = %v", r)
	}
}

func TestTranscribeXAIRetriesAuthRejectionWithRefreshedOauthCredentials(t *testing.T) {
	for _, rejected := range []int{401} {
		ctx := keepSTTSeams(t)
		resolved, refreshed := 0, []string{}
		sttResolveXAICredentials = func(context.Context) (string, string, string) {
			resolved++
			return "xai-oauth", "stale-oauth-token", "https://api.x.ai/v1"
		}
		sttRefreshXAICredentials = func(_ context.Context, hint string) (string, string, string, error) {
			refreshed = append(refreshed, hint)
			return "xai-oauth", "fresh-oauth-token", "https://api.x.ai/v1", nil
		}
		calls := fakeSTTPosts(t, [2]any{rejected, map[string]any{"error": map[string]any{"message": "OAuth2 access token could not be validated"}}},
			[2]any{200, map[string]any{"text": "fleet speech transcription proof", "language": "en", "duration": 2.1}})
		withSTTConfig(map[string]any{"provider": "xai"})
		sttGetProvider = func(context.Context, map[string]any) (string, error) { return "xai", nil }
		r, err := TranscribeAudio(ctx, sampleOGG(t), "", "")
		want := map[string]any{"success": true, "transcript": "fleet speech transcription proof", "provider": "xai"}
		if err != nil || !reflect.DeepEqual(r, want) || len(*calls) != 2 {
			t.Fatalf("result %v err %v calls %v", r, err, *calls)
		}
		if (*calls)[0].headers["Authorization"] != "Bearer stale-oauth-token" || (*calls)[1].headers["Authorization"] != "Bearer fresh-oauth-token" {
			t.Fatalf("headers = %v", *calls)
		}
		if resolved != 1 || !reflect.DeepEqual(refreshed, []string{"stale-oauth-token"}) {
			t.Fatalf("resolved %d refreshed %v", resolved, refreshed)
		}
	}
}

func TestTranscribeXAISendsLanguageAndFormat(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	t.Setenv(localSTTLanguageEnv, "fr")
	withSTTConfig(map[string]any{})
	calls := fakeSTTPosts(t, [2]any{200, map[string]any{"text": "test", "language": "fr", "duration": 1.0}})
	transcribeXAI(ctx, sampleOGG(t), "grok-stt", sttHints{})
	d := (*calls)[0].data
	if d["language"] != "fr" || d["format"] != "true" || d["model"] != "grok-voice-transcribe-2.0" {
		t.Fatalf("data = %v", d)
	}
}

func TestTranscribeXAIOmittedModelPinsTranscribe2(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	withSTTConfig(map[string]any{})
	calls := fakeSTTPosts(t, [2]any{200, map[string]any{"text": "test", "language": "en", "duration": 1.0}})
	transcribeXAI(ctx, sampleOGG(t), "", sttHints{})
	if m := (*calls)[0].data["model"]; m != "grok-voice-transcribe-2.0" {
		t.Fatalf("model = %q", m)
	}
}

func TestTranscribeXAIOauthCredentialsIgnoreSttBaseUrlOverride(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("XAI_STT_BASE_URL", "https://attacker.example/v1")
	sttResolveXAICredentials = func(context.Context) (string, string, string) {
		return "xai-oauth", "oauth-bearer-token", "https://api.x.ai/v1"
	}
	withSTTConfig(map[string]any{"xai": map[string]any{"base_url": "https://attacker.example/config"}})
	calls := fakeSTTPosts(t, [2]any{200, map[string]any{"text": "test", "language": "en", "duration": 1.0}})
	r, _ := transcribeXAI(ctx, sampleOGG(t), "grok-stt", sttHints{})
	if r["success"] != true || (*calls)[0].url != "https://api.x.ai/v1/stt" || (*calls)[0].headers["Authorization"] != "Bearer oauth-bearer-token" {
		t.Fatalf("result %v calls %v", r, *calls)
	}
}

func TestGetProviderXAIAutoDetectXaiAfterMistral(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("XAI_API_KEY", "xai-test")
	noLocalNoOpenAI()
	sttResolveXAICredentials = func(context.Context) (string, string, string) { return tools.ResolveXAIHTTPCredentials(nil, false) }
	if got, _ := getProvider(ctx, map[string]any{}); got != "xai" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestTranscribeAudioXAIDispatchModelDefaultIsVoiceTranscribe2(t *testing.T) {
	ctx := keepSTTSeams(t)
	if m := dispatchedModel(t, ctx, "xai", map[string]any{"provider": "xai"}); m != "grok-voice-transcribe-2.0" {
		t.Fatalf("model = %q", m)
	}
}

func TestTranscribeAudioXAIDispatchConfigModelOverridesDefault(t *testing.T) {
	ctx := keepSTTSeams(t)
	if m := dispatchedModel(t, ctx, "xai", map[string]any{"provider": "xai", "xai": map[string]any{"model": "grok-voice-transcribe-1.0"}}); m != "grok-voice-transcribe-1.0" {
		t.Fatalf("model = %q", m)
	}
}

func TestTranscribeAudioXAIDispatchLegacyGrokSttAliasUsesCurrentDefault(t *testing.T) {
	ctx := keepSTTSeams(t)
	if m := dispatchedModel(t, ctx, "xai", map[string]any{"provider": "xai", "xai": map[string]any{"model": "grok-stt"}}); m != "grok-voice-transcribe-2.0" {
		t.Fatalf("model = %q", m)
	}
}

func TestTranscribeElevenLabsSuccessfulTranscription(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("ELEVENLABS_API_KEY", "eleven-test-key")
	withSTTConfig(map[string]any{"elevenlabs": map[string]any{"language_code": "eng", "tag_audio_events": true, "diarize": true}})
	calls := fakeSTTPosts(t, [2]any{200, map[string]any{"text": "hello from elevenlabs"}})
	r, _ := transcribeElevenLabs(ctx, sampleOGG(t), "scribe_v2", sttHints{})
	c := (*calls)[0]
	if r["success"] != true || r["transcript"] != "hello from elevenlabs" || r["provider"] != "elevenlabs" || c.headers["xi-api-key"] != "eleven-test-key" ||
		c.data["model_id"] != "scribe_v2" || c.data["language_code"] != "eng" || c.data["tag_audio_events"] != "true" || c.data["diarize"] != "true" {
		t.Fatalf("result %v call %v", r, c)
	}
}

func TestGetProviderElevenLabsAutoDetectElevenlabsAfterXai(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("ELEVENLABS_API_KEY", "eleven-test")
	noLocalNoOpenAI()
	if got, _ := getProvider(ctx, map[string]any{}); got != "elevenlabs" {
		t.Fatalf("getProvider = %q", got)
	}
}

func TestTranscribeAudioElevenLabsDispatchConfigElevenlabsModelUsed(t *testing.T) {
	ctx := keepSTTSeams(t)
	if m := dispatchedModel(t, ctx, "elevenlabs", map[string]any{"provider": "elevenlabs", "elevenlabs": map[string]any{"model_id": "scribe_v1"}}); m != "scribe_v1" {
		t.Fatalf("model = %q", m)
	}
}

func TestExtractTranscriptTextStripsQwen3AsrLanguageEnvelope(t *testing.T) {
	if got, err := extractTranscriptText("language zh\n<audio_language>zh</audio_language>\n<asr_text>你好，世界"); err != nil || got != "你好，世界" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestExtractTranscriptTextKeepsNonEnvelopeMarkerLiteral(t *testing.T) {
	in := "The user literally said <asr_text> while reading markup."
	if got, _ := extractTranscriptText(in); got != in {
		t.Fatalf("got %q", got)
	}
}

// Asserts the extractor raises an STTResponseError carrying a message fragment
func wantSTTResponseError(t *testing.T, v any, fragment string) {
	t.Helper()
	_, err := extractTranscriptText(v)
	var re *sttResponseError
	if !errors.As(err, &re) || !strings.Contains(re.msg, fragment) || strings.Contains(re.msg, "text=None") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractTranscriptTextStructuredErrorObjectRaisesInsteadOfRepr(t *testing.T) {
	wantSTTResponseError(t, map[string]any{"text": nil, "logprobs": nil, "usage": nil, "error": "Transcription failed"}, "Transcription failed")
}

func TestExtractTranscriptTextStructuredErrorBodyRaisesInsteadOfRepr(t *testing.T) {
	wantSTTResponseError(t, map[string]any{"text": nil, "error": "Transcription failed"}, "Transcription failed")
}

func TestExtractTranscriptTextStructuredResponseWithoutTextOrErrorRaises(t *testing.T) {
	wantSTTResponseError(t, map[string]any{"text": nil, "error": nil}, "no text")
	wantSTTResponseError(t, map[string]any{"text": nil}, "no text")
}

func TestExtractTranscriptTextTextBearingResponsesStillNormalize(t *testing.T) {
	for in, want := range map[*map[string]any]string{{"text": " hello "}: "hello", {"text": "", "error": nil}: "", {"text": ""}: ""} {
		if got, err := extractTranscriptText(*in); err != nil || got != want {
			t.Fatalf("%v -> %q %v", *in, got, err)
		}
	}
}

// Structured error reply of an SDK response object
func sttErrorObject() map[string]any {
	return map[string]any{"text": nil, "logprobs": nil, "usage": nil, "error": "Transcription failed"}
}

// Asserts the failure envelope carries exactly the provider message
func wantProviderFailure(t *testing.T, r map[string]any) {
	t.Helper()
	if r["success"] != false || r["transcript"] != "" || r["error"] != "Transcription failed" {
		t.Fatalf("result = %v", r)
	}
}

func TestStructuredTranscriptionErrorsOpenaiErrorObjectReturnsFailureEnvelope(t *testing.T) {
	ctx := keepSTTSeams(t)
	installFakeSTTClient(&fakeSTTClient{replies: []any{sttErrorObject()}})
	key := "sk-test"
	r, _ := transcribeOpenAI(ctx, sampleWAV(t), "gpt-4o-transcribe", openAIEndpoint{APIKey: &key})
	wantProviderFailure(t, r)
}

func TestStructuredTranscriptionErrorsOpenaiTextObjectStillSucceeds(t *testing.T) {
	ctx := keepSTTSeams(t)
	installFakeSTTClient(&fakeSTTClient{replies: []any{map[string]any{"text": "hello world", "logprobs": nil, "usage": nil, "error": nil}}})
	key := "sk-test"
	r, _ := transcribeOpenAI(ctx, sampleWAV(t), "gpt-4o-transcribe", openAIEndpoint{APIKey: &key})
	if r["success"] != true || r["transcript"] != "hello world" {
		t.Fatalf("result = %v", r)
	}
}

func TestStructuredTranscriptionErrorsGroqErrorObjectReturnsFailureEnvelope(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("GROQ_API_KEY", "gsk-test")
	installFakeSTTClient(&fakeSTTClient{replies: []any{sttErrorObject()}})
	r, _ := transcribeGroq(ctx, sampleWAV(t), "whisper-large-v3-turbo", sttHints{})
	wantProviderFailure(t, r)
}

func TestStructuredTranscriptionErrorsMistralErrorObjectReturnsFailureEnvelope(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	fakeMistral(map[string]any{"text": nil, "error": "Transcription failed"}, "", nil)
	r, _ := transcribeMistral(ctx, sampleOGG(t), "voxtral-mini-latest", sttHints{})
	wantProviderFailure(t, r)
}

func TestStructuredTranscriptionErrorsElevenlabsErrorBodyReturnsFailureEnvelope(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv("ELEVENLABS_API_KEY", "eleven-test-key")
	withSTTConfig(map[string]any{})
	fakeSTTPosts(t, [2]any{200, map[string]any{"text": nil, "error": "Transcription failed"}})
	r, _ := transcribeElevenLabs(ctx, sampleOGG(t), "scribe_v2", sttHints{})
	wantProviderFailure(t, r)
}

func TestShellSafetyAutoDetectedTemplateIsShlexSafe(t *testing.T) {
	ctx := keepSTTSeams(t)
	sttFindBinary = func(name string) string {
		if name == "whisper" {
			return "/usr/bin/whisper"
		}
		return ""
	}
	tpl, err := localCommandTemplate(ctx)
	if err != nil || tpl == "" {
		t.Fatalf("template %q %v", tpl, err)
	}
	cmd, err := pyrepr.FormatMap(tpl, map[string]any{"input_path": pyshlex.Quote("/tmp/test.wav"), "output_dir": pyshlex.Quote("/tmp/out"),
		"language": pyshlex.Quote("en"), "model": pyshlex.Quote("base")})
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := pyshlex.SplitPOSIX(cmd, false)
	found := false
	for _, p := range parts {
		found = found || p == "/tmp/test.wav"
	}
	if parts[0] != "/usr/bin/whisper" || !found {
		t.Fatalf("parts = %v", parts)
	}
}

// Fake local command run that records argv and env and writes transcript.txt into the output dir
func recordLocalRun(t *testing.T) (*[]string, *[]string) {
	argv, env := &[]string{}, &[]string{}
	sttRunQuiet = func(command []string, timeout float64, e []string) (string, string, error) {
		*argv, *env = command, e
		os.WriteFile(filepath.Join(localSTTOutputDir(command), "transcript.txt"), []byte("safe"), 0o644)
		return "", "", nil
	}
	return argv, env
}

func TestShellSafetyEnvVarTemplateMetacharactersAreLiteralArgv(t *testing.T) {
	ctx := keepSTTSeams(t)
	t.Setenv(localSTTCommandEnv, "whisper {input_path} ; printf injected | tee log.txt && echo $(id) `whoami` --output_dir {output_dir}")
	argv, env := recordLocalRun(t)
	wav := sampleWAV(t)
	r, _ := transcribeLocalCommand(ctx, wav, "base", sttHints{})
	got := *argv
	if r["transcript"] != "safe" || len(got) != 14 || *env == nil {
		t.Fatalf("result %v argv %v", r, got)
	}
	want := []string{"whisper", wav, ";", "printf", "injected", "|", "tee", "log.txt", "&&", "echo", "$(id)", "`whoami`", "--output_dir", got[13]}
	if !reflect.DeepEqual(got, want) || !strings.HasPrefix(filepath.Base(got[13]), "icebath-local-stt-") {
		t.Fatalf("argv = %v", got)
	}
}

func TestShellSafetyLocalCommandNormalizesLanguageOrUsesDefault(t *testing.T) {
	for _, c := range [][2]string{{"ZH", "zh"}, {"zh-Hant", "zh"}, {"繁體中文", "zh"}, {"not-a-language", "en"}} {
		ctx := keepSTTSeams(t)
		t.Setenv(localSTTCommandEnv, "whisper {input_path} --language {language} --output_dir {output_dir}")
		withSTTConfig(map[string]any{"local": map[string]any{"language": c[0]}})
		argv, _ := recordLocalRun(t)
		r, _ := transcribeLocalCommand(ctx, sampleWAV(t), "base", sttHints{})
		idx := -1
		for i, a := range *argv {
			if a == "--language" {
				idx = i + 1
			}
		}
		if r["transcript"] != "safe" || idx < 0 || (*argv)[idx] != c[1] {
			t.Fatalf("%s: result %v argv %v", c[0], r, *argv)
		}
	}
}

func TestLocalModelLockConcurrentTranscribeLoadsModelOnce(t *testing.T) {
	ctx := keepSTTSeams(t)
	withSTTConfig(map[string]any{})
	var mu sync.Mutex
	loads := 0
	whisperModelCtor = func(context.Context, string, any, any, bool) (localWhisperModel, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return &fakeWhisperModel{segments: []whisperSegment{{Text: "hello"}}}, nil
	}
	audio := fakeOGG(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); transcribeLocal(ctx, audio, "base", sttHints{}) }()
	}
	wg.Wait()
	if loads != 1 {
		t.Fatalf("model loaded %d times", loads)
	}
}

func TestLocalBaseUrlNoApiKeyLocalBaseUrlReturnsPlaceholderKey(t *testing.T) {
	ctx := keepSTTSeams(t)
	withSTTConfig(map[string]any{"openai": map[string]any{"base_url": "http://localhost:8504/v1"}})
	key, base, err := resolveSTTOpenAIAudioClientConfig(ctx)
	if err != nil || key == "" || base != "http://localhost:8504/v1" {
		t.Fatalf("key %q base %q err %v", key, base, err)
	}
}

func TestLocalBaseUrlNoApiKeyIsLocalOrPrivateUrl(t *testing.T) {
	for _, u := range []string{"http://localhost:8504/v1", "http://127.0.0.1:9000", "http://10.0.0.5/v1", "http://stt.internal/v1"} {
		if !sttIsLocalOrPrivateURL(u) {
			t.Fatalf("%s not local", u)
		}
	}
	for _, u := range []string{"https://api.openai.com/v1", ""} {
		if sttIsLocalOrPrivateURL(u) {
			t.Fatalf("%q reported local", u)
		}
	}
}

// CAF fixture written into a fresh directory
func cafFixture(t *testing.T, content []byte) string {
	p := filepath.Join(t.TempDir(), "voice.caf")
	os.WriteFile(p, content, 0o644)
	return p
}

func TestCafConversionConvertCafWithFfmpeg(t *testing.T) {
	keepSTTSeams(t)
	caf := cafFixture(t, []byte(strings.Repeat("caff\x00", 20)))
	workDir := filepath.Join(filepath.Dir(caf), "converted")
	os.Mkdir(workDir, 0o755)
	wavPath := filepath.Join(workDir, "voice.wav")
	sttFindBinary = func(name string) string {
		if name == "ffmpeg" {
			return "/usr/bin/ffmpeg"
		}
		return ""
	}
	sttRunQuiet = func([]string, float64, []string) (string, string, error) {
		return "", "", os.WriteFile(wavPath, []byte("RIFF\x00\x00\x00\x00"), 0o644)
	}
	got, err := convertCAFToWAV(caf, workDir)
	if _, serr := os.Stat(got); err != nil || got != wavPath || serr != nil {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestCafConversionTranscribeCafNotConvertedForLocal(t *testing.T) {
	ctx := keepSTTSeams(t)
	caf := cafFixture(t, []byte(strings.Repeat("caff\x00", 20)))
	withSTTConfig(map[string]any{"provider": "local"})
	sttGetProvider = func(context.Context, map[string]any) (string, error) { return "local", nil }
	converted := false
	sttConvertCAF = func(string, string) (string, error) { converted = true; return "", nil }
	recordHandler("local", map[string]any{"success": true, "transcript": "hi"})
	r, _ := TranscribeAudio(ctx, caf, "", "")
	if r["success"] != true || converted {
		t.Fatalf("result %v converted %v", r, converted)
	}
}

func TestCafConversionCafConversionPreservesNeighborsAndRemovesOwnedOutput(t *testing.T) {
	for _, outcome := range []string{"success", "provider-error"} {
		ctx := keepSTTSeams(t)
		source := cafFixture(t, []byte("caff fixture"))
		neighbor := strings.TrimSuffix(source, ".caf") + ".wav"
		os.WriteFile(neighbor, []byte("existing recording"), 0o644)
		withSTTConfig(map[string]any{"provider": "groq", "cloud_trim_silence": false})
		sttFindBinary = func(name string) string {
			if name == "ffmpeg" {
				return "ffmpeg"
			}
			return ""
		}
		var outputs []string
		sttRunQuiet = func(command []string, _ float64, _ []string) (string, string, error) {
			out := command[len(command)-1]
			outputs = append(outputs, out)
			return "", "", os.WriteFile(out, []byte("converted recording"), 0o644)
		}
		sttDispatchProvider = func(_ context.Context, path, _ string, _ map[string]any, _, _ string) (map[string]any, error) {
			if b, _ := os.ReadFile(path); string(b) != "converted recording" {
				t.Fatalf("dispatched %q", b)
			}
			if outcome == "provider-error" {
				return nil, errors.New("transcription failed")
			}
			return map[string]any{"success": true, "transcript": "hello"}, nil
		}
		r, err := TranscribeAudio(ctx, source, "", "")
		if outcome == "provider-error" {
			if err == nil || !strings.Contains(err.Error(), "transcription failed") {
				t.Fatalf("err = %v", err)
			}
		} else if err != nil || r["success"] != true {
			t.Fatalf("result %v err %v", r, err)
		}
		if b, _ := os.ReadFile(source); string(b) != "caff fixture" {
			t.Fatal("source changed")
		}
		if b, _ := os.ReadFile(neighbor); string(b) != "existing recording" {
			t.Fatal("neighbor clobbered")
		}
		if len(outputs) == 0 {
			t.Fatal("no conversion ran")
		}
		for _, p := range outputs {
			if _, err := os.Stat(p); err == nil {
				t.Fatalf("%s left behind", p)
			}
			if _, err := os.Stat(filepath.Dir(p)); err == nil {
				t.Fatalf("%s dir left behind", p)
			}
		}
	}
}

func TestTranscribeCredentialReadGuardTranscribeAudioBlocksCredentialRead(t *testing.T) {
	ctx := keepSTTSeams(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(envFile, []byte("OPENAI_API_KEY=sk-secret\n"), 0o644)
	expected := tools.ReadBlockError(nil, envFile)
	if expected == "" {
		t.Fatal("a .env file should be read-blocked")
	}
	r, _ := TranscribeAudio(ctx, envFile, "", "")
	if r["success"] != false || r["error"] != expected {
		t.Fatalf("result = %v", r)
	}
}

func TestCommandSttIdleTimeoutPreservesTranscriptionContract(t *testing.T) {
	for _, progress := range []bool{true, false} {
		ctx := keepSTTSeams(t)
		dir := t.TempDir()
		audio := filepath.Join(dir, "audio.wav")
		os.WriteFile(audio, []byte("fixture audio"), 0o644)
		body := "sleep 30\n"
		if progress {
			body = "i=0\nwhile [ $i -lt 6 ]; do echo progress >&2; sleep 0.6; i=$((i+1)); done\n"
		}
		script := filepath.Join(dir, "transcribe.sh")
		os.WriteFile(script, []byte("[ \"$(cat \"$1\")\" = 'fixture audio' ] || exit 3\n"+body+"printf 'actual transcript' > \"$2\"\n"), 0o755)
		r := transcribeCommandSTT(ctx, audio, "probe", map[string]any{"command": "sh " + pyshlex.Quote(script) + " {input_path} {output_path}", "timeout": 2},
			map[string]any{}, "", "", "")
		if r["success"] != progress || r["provider"] != "probe" {
			t.Fatalf("progress %v result %v", progress, r)
		}
		if progress && r["transcript"] != "actual transcript" {
			t.Fatalf("result = %v", r)
		}
		if !progress && !strings.Contains(r["error"].(string), "STT command provider 'probe' timed out after 2s") {
			t.Fatalf("result = %v", r)
		}
	}
}

// No direct, env or managed OpenAI credentials
func noOpenAICredentials() {
	sttResolveOpenAIAudioAPIKey = func(context.Context) string { return "" }
	sttManagedOpenAIAudio = func(context.Context) (string, string, bool, error) { return "", "", false, nil }
}

func TestExplicitOpenaiSelectionErrorDispatchReturnsSelectionSpecificError(t *testing.T) {
	ctx := keepSTTSeams(t)
	noOpenAICredentials()
	sttManagedNousToolsEnabled = func() bool { return true }
	sttNousGatewayUnavailableMessage = func(what string) string { return "managed route down for " + what + "; run `icebath tools`" }
	withSTTConfig(map[string]any{})
	hasFasterWhisper = func() bool { return false }
	r, _ := dispatchSTTProvider(ctx, "/tmp/nonexistent.wav", "none", map[string]any{"provider": "openai"}, "", "")
	e := r["error"].(string)
	if r["success"] != false || !strings.Contains(e, "managed route down") || !strings.Contains(e, "icebath tools") || strings.Contains(e, "No STT provider available") {
		t.Fatalf("result = %v", r)
	}
}

func TestExplicitOpenaiSelectionErrorAutoDetectNoneKeepsGenericHint(t *testing.T) {
	ctx := keepSTTSeams(t)
	noOpenAICredentials()
	withSTTConfig(map[string]any{})
	hasFasterWhisper = func() bool { return false }
	hasLocalCommand = func(context.Context) bool { return false }
	r, _ := dispatchSTTProvider(ctx, "/tmp/x.wav", "none", map[string]any{}, "", "")
	if r["success"] != false || !strings.Contains(r["error"].(string), "No STT provider available") {
		t.Fatalf("result = %v", r)
	}
}

func TestTranscribeOpenaiFiveXxRetryServerErrorTriggersTranscodeAndRetry(t *testing.T) {
	ctx := keepSTTSeams(t)
	converted := filepath.Join(t.TempDir(), "retry.m4a")
	os.WriteFile(converted, []byte("fake audio"), 0o644)
	f := &fakeSTTClient{errs: []error{&openAIAPIStatusError{Status: 503, Body: map[string]any{"type": "system_error"}}}, replies: []any{nil, "retried transcript"}}
	installFakeSTTClient(f)
	sttTranscode = func(string, string) (string, string) { return converted, "" }
	key := "sk-test"
	r, _ := transcribeOpenAI(ctx, sampleWAV(t), "gpt-4o-transcribe", openAIEndpoint{APIKey: &key})
	if r["success"] != true || r["transcript"] != "retried transcript" || len(f.calls) != 2 {
		t.Fatalf("result %v calls %d", r, len(f.calls))
	}
}

func TestTranscribeOpenaiFiveXxRetryServerErrorWithoutTranscodeKeepsProviderError(t *testing.T) {
	ctx := keepSTTSeams(t)
	f := &fakeSTTClient{errs: []error{&openAIAPIStatusError{Status: 503, Body: map[string]any{"type": "system_error"}}}}
	installFakeSTTClient(f)
	sttTranscode = func(string, string) (string, string) { return "", "ffmpeg not found" }
	key := "sk-test"
	r, _ := transcribeOpenAI(ctx, sampleWAV(t), "whisper-1", openAIEndpoint{APIKey: &key})
	e := r["error"].(string)
	if r["success"] != false || !strings.Contains(e, "503") || strings.Contains(e, "ffmpeg") || len(f.calls) != 1 {
		t.Fatalf("result %v calls %d", r, len(f.calls))
	}
}
