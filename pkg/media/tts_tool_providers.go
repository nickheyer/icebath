package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media/edgetts"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultEdgeVoice                  = "en-US-AriaNeural"
	defaultElevenLabsVoiceID          = "pNInz6obpgDQGcFmaJgB"
	defaultElevenLabsModelID          = "eleven_multilingual_v2"
	defaultElevenLabsStreamingModelID = "eleven_flash_v2_5"
	defaultMiniMaxModel               = "speech-02-hd"
	defaultMiniMaxVoiceID             = "English_expressive_narrator"
	defaultMiniMaxBaseURL             = "https://api.minimax.io/v1/t2a_v2"
	defaultMiniMaxCNBaseURL           = "https://api.minimaxi.com/v1/t2a_v2"
	defaultMistralTTSModel            = "voxtral-mini-tts-2603"
	defaultMistralTTSVoiceID          = "c69964a6-ab8b-4f8a-9465-ec0925096ec8"
	defaultXAIVoiceID                 = "eve"
	defaultXAILanguage                = "en"
	defaultXAISampleRate              = 24000
	defaultXAIBitRate                 = 128000
	defaultXAIAutoSpeechTags          = false
	defaultXAIBaseURL                 = "https://api.x.ai/v1"
	defaultXAISpeedMin                = 0.7
	defaultXAISpeedMax                = 1.5
	defaultXAISpeedDefault            = 1.0
	defaultXAIOptimizeLatencyDefault  = 0
	defaultXAITextNormalization       = false
	defaultGeminiTTSModel             = "gemini-2.5-flash-preview-tts"
	defaultGeminiTTSVoice             = "Kore"
	defaultGeminiTTSBaseURL           = "https://generativelanguage.googleapis.com/v1beta"
	defaultGeminiAudioTags            = false
	geminiAudioTagRewriteTask         = "tts_audio_tags"
	ttsResponseBodyLimitBytes         = 16 * 1024 * 1024
	ttsResponseBodyChunkBytes         = 64 * 1024
	defaultElevenLabsBaseURL          = "https://api.elevenlabs.io"
	defaultMistralServerURL           = "https://api.mistral.ai"
)

// Python dict.get with a default for a present key holding any value including None
func dictGetDefault(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Python attribute error of calling get on a non mapping value
type ttsAttributeError struct{ msg string }

func (e *ttsAttributeError) Error() string { return e.msg }

// The tts_config.get(key) or {} idiom, a truthy non mapping value failing like Python
func orEmptyDict(cfg map[string]any, key string) (map[string]any, error) {
	v := cfg[key]
	if !pyTruthy(v) {
		return map[string]any{}, nil
	}
	if m, ok := asStrMap(v); ok {
		return m, nil
	}
	return nil, &ttsAttributeError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
}

// Python type error raised by a builtin conversion
type ttsTypeError struct{ msg string }

func (e *ttsTypeError) Error() string { return e.msg }

// Python float() of a config value with the raised error text
func ttsFloat(v any) (float64, error) {
	if s, ok := v.(string); ok {
		f, err := pyint.ParseFloat(s)
		if err != nil {
			return 0, &ttsValueError{msg: fmt.Sprintf("could not convert string to float: %s", pyrepr.Repr(s))}
		}
		return f, nil
	}
	if b, ok := v.(bool); ok {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	if f, ok := pyRealNumber(v); ok {
		return f, nil
	}
	return 0, &ttsTypeError{msg: fmt.Sprintf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))}
}

// Python int() of a config value, truncating floats and parsing base ten strings
func pyInt(v any) (int, error) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, &ttsValueError{msg: "cannot convert float NaN or infinity to integer"}
		}
		return int(n), nil
	case string:
		s := strings.ReplaceAll(pystr.Strip(n), "_", "")
		if i, err := strconv.Atoi(s); err == nil && pystr.Strip(n) != "" {
			return i, nil
		}
		return 0, &ttsValueError{msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))}
	}
	if i := ttsPositiveInt(v); i > 0 {
		return i, nil
	}
	if f, ok := pyRealNumber(v); ok {
		return int(f), nil
	}
	return 0, &ttsTypeError{msg: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))}
}

// OpenAI style response format picked from the output extension
func ttsResponseFormatFromPath(outputPath string) string {
	switch {
	case strings.HasSuffix(outputPath, ".ogg"):
		return "opus"
	case strings.HasSuffix(outputPath, ".wav"):
		return "wav"
	case strings.HasSuffix(outputPath, ".flac"):
		return "flac"
	}
	return "mp3"
}

// Provider key through the shared voice key resolver, config then scope, dotenv and pool
var ttsResolveProviderKey = func(env *tools.Env, envVar, providerID string) string {
	return tools.ResolveProviderSecret(env, envVar, providerID, "", nil)
}

// Required provider key or a value error naming the variable and the hint
func requireTTSKey(env *tools.Env, envVar, providerID, hint string) (string, error) {
	key := ttsResolveProviderKey(env, envVar, providerID)
	if key == "" {
		return "", &ttsValueError{msg: fmt.Sprintf("%s not set. %s", envVar, hint)}
	}
	return key, nil
}

// HTTP error worded like requests raise_for_status
type ttsHTTPError struct {
	Status int
	Reason string
	URL    string
}

func (e *ttsHTTPError) Error() string {
	kind := "Client"
	if e.Status >= 500 {
		kind = "Server"
	}
	return fmt.Sprintf("%d %s Error: %s for url: %s", e.Status, kind, e.Reason, e.URL)
}

// Raises for a 4xx or 5xx reply like requests raise_for_status
func ttsRaiseForStatus(resp *http.Response) error {
	if resp.StatusCode < 400 || resp.StatusCode >= 600 {
		return nil
	}
	reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	return &ttsHTTPError{Status: resp.StatusCode, Reason: reason, URL: resp.Request.URL.String()}
}

// Streaming JSON post with a 60 second connect and read timeout, the seam tests replace
var ttsPostJSON = func(ctx context.Context, rawURL string, payload any, headers map[string]string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 60 * time.Second}).DialContext,
		TLSHandshakeTimeout:   60 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body = idleTimeoutBody{r: resp.Body, idle: 60 * time.Second}
	return resp, nil
}

// Body whose every read fails once no byte arrives within the idle window
type idleTimeoutBody struct {
	r    io.ReadCloser
	idle time.Duration
}

func (b idleTimeoutBody) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := b.r.Read(p)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-time.After(b.idle):
		b.r.Close()
		return 0, fmt.Errorf("Read timed out. (read timeout=%d)", int(b.idle.Seconds()))
	}
}

func (b idleTimeoutBody) Close() error { return b.r.Close() }

// Response body read under a hard byte cap, the response always closed
func readTTSResponseBytes(resp *http.Response, label string, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = ttsResponseBodyLimitBytes
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	buf := make([]byte, ttsResponseBodyChunkBytes)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if out.Len()+n > limit {
				return nil, &ttsRuntimeError{msg: fmt.Sprintf("%s response exceeds %d bytes", label, limit)}
			}
			out.Write(buf[:n])
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// JSON object of an already read body, empty for an empty body or a non object document
func parseTTSJSONBody(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if !utf8.Valid(raw) {
		return nil, &ttsValueError{msg: "'utf-8' codec can't decode the response body"}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, &ttsValueError{msg: err.Error()}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return nil, &ttsTypeError{msg: "response JSON is not an object"}
	}
	return m, nil
}

// Bounded JSON read of a response
func readTTSResponseJSON(resp *http.Response, label string) (map[string]any, error) {
	raw, err := readTTSResponseBytes(resp, label, 0)
	if err != nil {
		return nil, err
	}
	return parseTTSJSONBody(raw)
}

// Writes audio bytes to the output path
func writeTTSBytes(outputPath string, audio []byte) (string, error) {
	if err := os.WriteFile(outputPath, audio, 0o666); err != nil {
		return "", err
	}
	return outputPath, nil
}

var auxFenceRe = mustPyre(pyre.CompileFullmatch("```(?:[A-Za-z0-9_-]+)?\\s*(.*?)\\s*```"))

// Compiled translated pattern, panicking on a pattern bug at init
func mustPyre[T any](re T, err error) T {
	if err != nil {
		panic(err)
	}
	return re
}

// First choice message content with a code fence unwrapped, empty when unreadable
func auxiliaryReplyText(resp any) string {
	m, _ := resp.(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := message["content"]
	if !pyTruthy(content) {
		content = ""
	}
	clean := pystr.Strip(pyrepr.Str(content))
	if match, _ := auxFenceRe.FindStringMatch(clean); match != nil {
		return pystr.Strip(match.GroupByNumber(1).String())
	}
	return clean
}

const tagRewriteRules = "Rules:\n- Preserve the spoken words, order, and meaning.\n- Do not add new spoken sentences or remove existing spoken words.\n"
const tagRewriteTail = "- Do not explain or comment.\n- Return only the tagged TTS script."

// Auxiliary call_llm entry, the seam tests replace
var ttsCallLLM = provider.CallLLM

// Auxiliary model rewrite of a script, the fallback on any failure or empty reply
func rewriteWithAuxiliaryModel(ctx context.Context, systemPrompt, userPrompt, fallback, label, fallbackLabel string, level slog.Level) string {
	resp, err := ttsCallLLM(ctx, map[string]any{"task": geminiAudioTagRewriteTask, "temperature": 0.7, "messages": []any{
		map[string]any{"role": "system", "content": systemPrompt}, map[string]any{"role": "user", "content": userPrompt}}})
	if err != nil {
		ttsLog().Log(ctx, level, fmt.Sprintf("%s audio tag rewrite failed; using %s: %v", label, fallbackLabel, err))
		return fallback
	}
	if text := auxiliaryReplyText(resp); text != "" {
		return text
	}
	return fallback
}

// Command running the managed environment interpreter isolated, the seam tests replace
var managedPythonCommand = func(ctx context.Context) ([]string, error) {
	root := pm.RepoRoot()
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		return nil, err
	}
	return pm.VenvCommand(ctx, root, venv, []string{"-I"})
}

// Edge rate from a speed multiplier as a signed whole percentage
func edgeRate(speed float64) string {
	return fmt.Sprintf("%+d%%", int(math.RoundToEven((speed-1.0)*100)))
}

// Speaks through the Edge read aloud service into a file, the seam tests replace
var edgeSynthesize = func(ctx context.Context, text string, opts edgetts.Options, outputPath string) error {
	var audio bytes.Buffer
	if err := edgetts.Default.Synthesize(ctx, text, opts, &audio); err != nil {
		return err
	}
	return os.WriteFile(outputPath, audio.Bytes(), 0o644)
}

// Free Edge neural speech as MP3 within the sixty second synthesis deadline
func generateEdgeTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	edgeConfig, err := orEmptyDict(ttsConfig, "edge")
	if err != nil {
		return "", err
	}
	speed, err := ttsFloat(dictGetDefault(edgeConfig, "speed", dictGetDefault(ttsConfig, "speed", 1.0)))
	if err != nil {
		return "", err
	}
	opts := edgetts.Options{Voice: pyrepr.Str(dictGetDefault(edgeConfig, "voice", defaultEdgeVoice))}
	if speed != 1.0 {
		opts.Rate = edgeRate(speed)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := edgeSynthesize(rctx, text, opts, outputPath); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return "", &ttsRuntimeError{msg: "Edge TTS did not finish within 60 seconds", cause: err}
		}
		return "", &ttsRuntimeError{msg: err.Error(), cause: err}
	}
	return outputPath, nil
}

// ElevenLabs text to speech convert, Opus 48k for ogg paths and MP3 44.1k otherwise
func generateElevenLabs(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	apiKey, err := requireTTSKey(env, "ELEVENLABS_API_KEY", "elevenlabs", "Get one at https://elevenlabs.io/")
	if err != nil {
		return "", err
	}
	elConfig, err := orEmptyDict(ttsConfig, "elevenlabs")
	if err != nil {
		return "", err
	}
	base := defaultElevenLabsBaseURL
	if b := strings.TrimRight(pyrepr.Str(orEmptyString(elConfig["base_url"])), "/"); b != "" {
		base = b
	}
	format := "mp3_44100_128"
	if strings.HasSuffix(outputPath, ".ogg") {
		format = "opus_48000_64"
	}
	voiceID := pyrepr.Str(dictGetDefault(elConfig, "voice_id", defaultElevenLabsVoiceID))
	endpoint := base + "/v1/text-to-speech/" + url.PathEscape(voiceID) + "?output_format=" + url.QueryEscape(format)
	payload := map[string]any{"text": text, "model_id": dictGetDefault(elConfig, "model_id", defaultElevenLabsModelID)}
	resp, err := ttsElevenLabsPost(ctx, endpoint, apiKey, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		var body any = string(raw)
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			body = parsed
		}
		return "", &ttsRuntimeError{msg: fmt.Sprintf("status_code: %d, body: %s", resp.StatusCode, pyrepr.Str(body))}
	}
	f, err := os.Create(outputPath)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return outputPath, nil
}

// The value or an empty string for a falsy value, the Python x or "" idiom
func orEmptyString(v any) any {
	if !pyTruthy(v) {
		return ""
	}
	return v
}

// ElevenLabs convert request with the xi-api-key header and the SDK 240 second timeout
var ttsElevenLabsPost = func(ctx context.Context, endpoint, apiKey string, payload map[string]any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 240*time.Second)
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

var xaiInlineSpeechTags = []string{"pause", "long-pause", "hum-tune", "laugh", "chuckle", "giggle", "cry", "tsk", "tongue-click", "lip-smack", "breath", "inhale", "exhale", "sigh"}
var xaiWrappingSpeechTags = []string{"soft", "whisper", "loud", "build-intensity", "decrease-intensity", "higher-pitch", "lower-pitch", "slow", "fast", "sing-song", "singing", "laugh-speak", "emphasis"}

var (
	xaiSpeechTagRe     = mustPyre(pyre.CompileIgnoreCase(`(\[(?:` + strings.Join(xaiInlineSpeechTags, "|") + `)\]|</?(?:` + strings.Join(xaiWrappingSpeechTags, "|") + `)>)`))
	xaiFirstSentenceRe = mustPyre(pyre.Compile(`(?s)^(.{12,120}?[.!?…])\s+(?=\S)`))
	paragraphBreakRe   = mustPyre(pyre.Compile(`\n\s*\n+`))
	lineBreakRe        = mustPyre(pyre.Compile(`\s*\n\s*`))
	multiSpaceRe       = mustPyre(pyre.Compile(`\s{2,}`))
)

// Local xAI pause tags then an auxiliary rewrite when the text carried no explicit tags
func applyXAIAutoSpeechTags(ctx context.Context, text string) string {
	clean := pystr.Strip(text)
	if clean == "" {
		return text
	}
	local, _ := paragraphBreakRe.Replace(clean, " [pause] ", -1, -1)
	local, _ = lineBreakRe.Replace(local, " ", -1, -1)
	if m, _ := xaiSpeechTagRe.MatchString(local); !m {
		local, _ = xaiFirstSentenceRe.Replace(local, "$1 [pause] ", -1, 1)
	}
	local, _ = multiSpaceRe.Replace(local, " ", -1, -1)
	local = pystr.Strip(local)
	if m, _ := xaiSpeechTagRe.MatchString(clean); m {
		return local
	}
	systemPrompt := "You rewrite transcripts for the xAI /v1/tts endpoint by inserting expressive speech tags.\n\n" +
		"Valid inline tags (use as `[tag]`): " + strings.Join(xaiInlineSpeechTags, ", ") + ".\n" +
		"Valid wrapping tags (use as `[tag]...[/tag]`): " + strings.Join(xaiWrappingSpeechTags, ", ") + ".\n\n" +
		tagRewriteRules +
		"- Use inline `[tag]` for short modifiers (laughs, sighs, pause, etc.).\n" +
		"- Use wrapping `[tag]...[/tag]` for sustained effects (whisper, soft, slow, fast, loud, etc.).\n" +
		"- Do not use angle-bracket tags like `<tag>...</tag>` — xAI uses BBCode-style closing tags with `[/tag]`.\n" +
		"- Do not use SSML.\n" + tagRewriteTail
	return rewriteWithAuxiliaryModel(ctx, systemPrompt, "TRANSCRIPT TO TAG:\n"+local, local, "xAI TTS", "locally-tagged text", slog.LevelDebug)
}

// Optional numeric knob parsed and clamped, nil when absent or unparseable, an empty string failing on compare
func clampedNumber(raw any, asInt bool, lo, hi float64) (any, error) {
	if raw == nil {
		return nil, nil
	}
	if s, ok := raw.(string); ok && s == "" {
		kind := "float"
		if asInt {
			kind = "int"
		}
		return nil, &ttsTypeError{msg: fmt.Sprintf("'<' not supported between instances of 'str' and '%s'", kind)}
	}
	if asInt {
		i, err := pyInt(raw)
		if err != nil {
			return nil, nil
		}
		return int(math.Max(lo, math.Min(hi, float64(i)))), nil
	}
	f, err := ttsFloat(raw)
	if err != nil {
		return nil, nil
	}
	return math.Max(lo, math.Min(hi, f)), nil
}

// Env value through the scoped process env then the home dotenv
func ttsEnvValue(ctx context.Context, env *tools.Env, key string) string {
	if env != nil && env.Secrets != nil {
		return env.Secrets.String(key)
	}
	v, _, _ := config.GetEnvValue(ctx, ttsHome(ctx, env), key)
	return v
}

// Home of the environment or the context scoped home
func ttsHome(ctx context.Context, env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Dir(ctx)
}

// xAI credentials preferring an API key, the seam tests replace
var ttsResolveXAICredentials = func(env *tools.Env) (string, string, string) {
	return tools.ResolveXAIHTTPCredentials(env, true)
}

// xAI speech through the dedicated tts endpoint with clamped knobs and optional speech tags
func generateXAITTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	credProvider, apiKey, credBase := ttsResolveXAICredentials(env)
	apiKey = pystr.Strip(apiKey)
	if apiKey == "" {
		return "", &ttsValueError{msg: "No xAI credentials found. Configure xAI OAuth in `icebath model` or set XAI_API_KEY."}
	}
	xaiConfig, err := orEmptyDict(ttsConfig, "xai")
	if err != nil {
		return "", err
	}
	voiceID := pystr.Strip(pyrepr.Str(dictGetDefault(xaiConfig, "voice_id", defaultXAIVoiceID)))
	if voiceID == "" {
		voiceID = defaultXAIVoiceID
	}
	language := pystr.Strip(pyrepr.Str(dictGetDefault(xaiConfig, "language", defaultXAILanguage)))
	if language == "" {
		language = defaultXAILanguage
	}
	sampleRate, err := pyInt(dictGetDefault(xaiConfig, "sample_rate", defaultXAISampleRate))
	if err != nil {
		return "", err
	}
	bitRate, err := pyInt(dictGetDefault(xaiConfig, "bit_rate", defaultXAIBitRate))
	if err != nil {
		return "", err
	}
	if configBool(dictGetDefault(xaiConfig, "auto_speech_tags", xaiConfig["speech_tags"]), defaultXAIAutoSpeechTags) {
		text = applyXAIAutoSpeechTags(ctx, text)
	}
	speed, err := clampedNumber(dictGetDefault(xaiConfig, "speed", ttsConfig["speed"]), false, defaultXAISpeedMin, defaultXAISpeedMax)
	if err != nil {
		return "", err
	}
	latency, err := clampedNumber(dictGetDefault(xaiConfig, "optimize_streaming_latency", ttsConfig["optimize_streaming_latency"]), true, 0, 2)
	if err != nil {
		return "", err
	}
	textNormalization := configBool(xaiConfig["text_normalization"], defaultXAITextNormalization)
	var baseURL any
	if credProvider == "xai-oauth" {
		baseURL = credBase
	} else if pyTruthy(xaiConfig["base_url"]) {
		baseURL = xaiConfig["base_url"]
	} else if credBase != "" {
		baseURL = credBase
	} else {
		baseURL = ttsEnvValue(ctx, env, "XAI_BASE_URL")
	}
	if !pyTruthy(baseURL) {
		baseURL = defaultXAIBaseURL
	}
	base := strings.TrimRight(pystr.Strip(pyrepr.Str(baseURL)), "/")
	codec := "mp3"
	if strings.HasSuffix(outputPath, ".wav") {
		codec = "wav"
	}
	payload := map[string]any{"text": text, "voice_id": voiceID, "language": language}
	if codec != "mp3" || sampleRate != defaultXAISampleRate || bitRate != defaultXAIBitRate {
		outputFormat := map[string]any{"codec": codec}
		if sampleRate != 0 {
			outputFormat["sample_rate"] = sampleRate
		}
		if codec == "mp3" && bitRate != 0 {
			outputFormat["bit_rate"] = bitRate
		}
		payload["output_format"] = outputFormat
	}
	if f, ok := speed.(float64); ok && f != defaultXAISpeedDefault {
		payload["speed"] = f
	}
	if i, ok := latency.(int); ok && i != defaultXAIOptimizeLatencyDefault {
		payload["optimize_streaming_latency"] = i
	}
	if textNormalization {
		payload["text_normalization"] = true
	}
	resp, err := ttsPostJSON(ctx, base+"/tts", payload, map[string]string{
		"Authorization": "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": tools.IcebathXAIUserAgent()})
	if err != nil {
		return "", err
	}
	if err := ttsRaiseForStatus(resp); err != nil {
		resp.Body.Close()
		return "", err
	}
	audio, err := readTTSResponseBytes(resp, "xAI TTS", 0)
	if err != nil {
		return "", err
	}
	return writeTTSBytes(outputPath, audio)
}

// Region bound MiniMax endpoint and credential
type miniMaxTTSRuntime struct {
	Region           string
	Endpoint         string
	CredentialSource string
	APIKey           string
}

var miniMaxEndpoints = map[string]string{"global": defaultMiniMaxBaseURL, "cn": defaultMiniMaxCNBaseURL}
var miniMaxOfficialHosts = map[string]map[string]bool{
	"global": {"api.minimax.io": true, "api.minimax.chat": true},
	"cn":     {"api.minimaxi.com": true},
}

// MiniMax region, endpoint and credential chosen together, cn only when it is the sole credential
func resolveMiniMaxTTSRuntime(env *tools.Env, ttsConfig map[string]any) (*miniMaxTTSRuntime, error) {
	mmConfig := ttsSection(ttsConfig, "minimax")
	creds := map[string][2]string{
		"global": {"MINIMAX_API_KEY", pystr.Strip(ttsResolveProviderKey(env, "MINIMAX_API_KEY", "minimax"))},
		"cn":     {"MINIMAX_CN_API_KEY", pystr.Strip(ttsResolveProviderKey(env, "MINIMAX_CN_API_KEY", "minimax"))},
	}
	region := pystr.Lower(pystr.Strip(pyrepr.Str(orEmptyString(mmConfig["region"]))))
	if _, ok := miniMaxEndpoints[region]; region != "" && !ok {
		return nil, &ttsValueError{msg: "tts.minimax.region must be 'global' or 'cn'"}
	}
	if region == "" {
		region = "global"
		if creds["cn"][1] != "" && creds["global"][1] == "" {
			region = "cn"
		}
	}
	source, apiKey := creds[region][0], creds[region][1]
	if apiKey == "" {
		return nil, &ttsValueError{msg: fmt.Sprintf("%s not set for MiniMax TTS region %s", source, pyrepr.Repr(region))}
	}
	endpoint := miniMaxEndpoints[region]
	if pyTruthy(mmConfig["base_url"]) {
		endpoint = pyrepr.Str(mmConfig["base_url"])
	}
	endpoint = pystr.Strip(endpoint)
	other := "global"
	if region == "global" {
		other = "cn"
	}
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if miniMaxOfficialHosts[other][host] {
		return nil, &ttsValueError{msg: fmt.Sprintf("tts.minimax.base_url points to the %s MiniMax endpoint but region is %s", pyrepr.Repr(other), pyrepr.Repr(region))}
	}
	return &miniMaxTTSRuntime{Region: region, Endpoint: endpoint, CredentialSource: source, APIKey: apiKey}, nil
}

// MiniMax API error from a base_resp status other than zero
func raiseMiniMaxAPIError(result map[string]any) error {
	baseResp, _ := asStrMap(dictGetDefault(result, "base_resp", map[string]any{}))
	status := dictGetDefault(baseResp, "status_code", -1)
	if n, ok := pyRealNumber(status); ok && n == 0 {
		return nil
	}
	return &ttsRuntimeError{msg: fmt.Sprintf("MiniMax TTS API error (code %s): %s", pyrepr.Str(status), pyrepr.Str(dictGetDefault(baseResp, "status_msg", "unknown error")))}
}

// MiniMax speech via t2a_v2 hex audio JSON or the legacy raw audio endpoint, detected from the URL
func generateMiniMaxTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	runtime, err := resolveMiniMaxTTSRuntime(env, ttsConfig)
	if err != nil {
		return "", err
	}
	mm := ttsSection(ttsConfig, "minimax")
	model := dictGetDefault(mm, "model", defaultMiniMaxModel)
	voiceID := dictGetDefault(mm, "voice_id", defaultMiniMaxVoiceID)
	baseURL := runtime.Endpoint
	groupID := pystr.Strip(pyrepr.Str(orEmptyString(mm["group_id"])))
	if groupID == "" {
		groupID = pystr.Strip(ttsEnvValue(ctx, env, "MINIMAX_GROUP_ID"))
	}
	if groupID != "" && !strings.Contains(baseURL, "GroupId=") {
		sep := "?"
		if strings.Contains(baseURL, "?") {
			sep = "&"
		}
		baseURL = baseURL + sep + "GroupId=" + groupID
	}
	isT2AV2 := strings.Contains(baseURL, "t2a_v2")
	var payload map[string]any
	if isT2AV2 {
		payload = map[string]any{"model": model, "text": text,
			"voice_setting": map[string]any{"voice_id": voiceID, "speed": dictGetDefault(mm, "speed", 1.0), "vol": dictGetDefault(mm, "vol", 1.0),
				"pitch": dictGetDefault(mm, "pitch", 0), "emotion": dictGetDefault(mm, "emotion", "neutral")},
			"audio_setting": map[string]any{"sample_rate": dictGetDefault(mm, "sample_rate", 32000), "bitrate": dictGetDefault(mm, "bitrate", 128000), "format": "mp3", "channel": 1}}
	} else {
		payload = map[string]any{"model": model, "text": text, "voice_id": voiceID}
	}
	resp, err := ttsPostJSON(ctx, baseURL, payload, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + runtime.APIKey})
	if err != nil {
		return "", err
	}
	if isT2AV2 {
		if err := ttsRaiseForStatus(resp); err != nil {
			resp.Body.Close()
			return "", err
		}
		result, err := readTTSResponseJSON(resp, "MiniMax TTS")
		if err != nil {
			return "", err
		}
		if err := raiseMiniMaxAPIError(result); err != nil {
			return "", err
		}
		data, _ := asStrMap(dictGetDefault(result, "data", map[string]any{}))
		hexAudio, _ := dictGetDefault(data, "audio", "").(string)
		if hexAudio == "" {
			return "", &ttsRuntimeError{msg: "MiniMax TTS returned empty audio data"}
		}
		audio, err := pyBytesFromHex(hexAudio)
		if err != nil {
			return "", err
		}
		return writeTTSBytes(outputPath, audio)
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "audio/") {
		audio, err := readTTSResponseBytes(resp, "MiniMax TTS", 0)
		if err != nil {
			return "", err
		}
		return writeTTSBytes(outputPath, audio)
	}
	raw, err := readTTSResponseBytes(resp, "MiniMax TTS", 0)
	if err != nil {
		return "", err
	}
	var decoded map[string]any
	var parsed any
	if len(raw) == 0 {
		decoded = map[string]any{}
	} else if utf8.Valid(raw) && json.Unmarshal(raw, &parsed) == nil {
		decoded, _ = parsed.(map[string]any)
		if decoded == nil {
			if err := ttsRaiseForStatus(resp); err != nil {
				return "", err
			}
			return "", &ttsRuntimeError{msg: fmt.Sprintf("MiniMax TTS returned unexpected Content-Type '%s' (%d bytes)", contentType, len(raw))}
		}
	} else {
		if err := ttsRaiseForStatus(resp); err != nil {
			return "", err
		}
		return "", &ttsRuntimeError{msg: fmt.Sprintf("MiniMax TTS returned unexpected Content-Type '%s' (%d bytes)", contentType, len(raw))}
	}
	if err := raiseMiniMaxAPIError(decoded); err != nil {
		return "", err
	}
	return "", &ttsRuntimeError{msg: "MiniMax TTS returned no audio data"}
}

// Python bytes.fromhex
func pyBytesFromHex(s string) ([]byte, error) {
	b, err := pystr.BytesFromHex(s)
	if err != nil {
		return nil, &ttsValueError{msg: err.Error()}
	}
	return b, nil
}

// Mistral Voxtral speech decoded from base64 audio, client failures wrapped by error class
func generateMistralTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	apiKey, err := requireTTSKey(env, "MISTRAL_API_KEY", "mistral", "Get one at https://console.mistral.ai/")
	if err != nil {
		return "", err
	}
	miConfig, err := orEmptyDict(ttsConfig, "mistral")
	if err != nil {
		return "", err
	}
	server := defaultMistralServerURL
	if pyTruthy(miConfig["base_url"]) {
		server = pyrepr.Str(miConfig["base_url"])
	}
	voiceID := miConfig["voice_id"]
	if !pyTruthy(voiceID) {
		voiceID = defaultMistralTTSVoiceID
	}
	payload := map[string]any{"model": dictGetDefault(miConfig, "model", defaultMistralTTSModel), "input": text, "voice_id": voiceID,
		"response_format": ttsResponseFormatFromPath(outputPath)}
	audioData, className, err := ttsMistralSpeech(ctx, server, apiKey, payload)
	if err != nil {
		ttsLog().Error(fmt.Sprintf("Mistral TTS failed: %v", err))
		return "", &ttsRuntimeError{msg: "Mistral TTS failed: " + className, cause: err}
	}
	audio, err := pybase64Decode(audioData)
	if err != nil {
		return "", err
	}
	return writeTTSBytes(outputPath, audio)
}

// Python base64.b64decode with non alphabet characters discarded
func pybase64Decode(s string) ([]byte, error) {
	var clean strings.Builder
	for _, r := range s {
		if r < 128 && (r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=') {
			clean.WriteRune(r)
		}
	}
	text := clean.String()
	if i := strings.IndexByte(text, '='); i >= 0 {
		text = text[:i] + strings.Repeat("=", (4-i%4)%4)
	}
	b, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, &ttsValueError{msg: "Incorrect padding"}
	}
	return b, nil
}

// Mistral audio speech complete call returning the base64 audio or the SDK error class name
var ttsMistralSpeech = func(ctx context.Context, server, apiKey string, payload map[string]any) (string, string, error) {
	body, _ := json.Marshal(payload)
	rctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(server, "/")+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return "", "SDKError", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "ConnectError", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "ReadError", err
	}
	if resp.StatusCode == 422 {
		return "", "HTTPValidationError", fmt.Errorf("API error occurred: Status %d. Body: %s", resp.StatusCode, raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "SDKError", fmt.Errorf("API error occurred: Status %d. Body: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "ResponseValidationError", err
	}
	data, ok := out["audio_data"].(string)
	if !ok {
		return "", "ResponseValidationError", errors.New("Response validation failed: audio_data missing")
	}
	return data, "", nil
}

// Persona prompt file text with relative paths under the home, empty with a warning when unreadable
func readGeminiPersonaPrompt(ctx context.Context, env *tools.Env, geminiConfig map[string]any) string {
	raw, ok := geminiConfig["persona_prompt_file"].(string)
	if !ok || pystr.Strip(raw) == "" {
		return ""
	}
	path := pypath.Expanduser(pypath.Expandvars(pystr.Strip(raw)))
	if !filepath.IsAbs(path) {
		path = filepath.Join(ttsHome(ctx, env), path)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		text, derr := pystr.DecodeUTF8Sig(data)
		if derr == nil {
			return pystr.Strip(text)
		}
		err = derr
	}
	ttsLog().Warn(fmt.Sprintf("Gemini TTS persona prompt file unavailable at %s: %s", path, pypath.OSErrorText(err)))
	return ""
}

// Whether audio tags are opted in and the model is a Gemini 3.1 TTS model
func geminiAudioTagsEnabled(geminiConfig map[string]any, model string) bool {
	raw := geminiConfig["audio_tags"]
	if m, ok := asStrMap(raw); ok {
		raw = m["enabled"]
	}
	if !configBool(raw, defaultGeminiAudioTags) {
		return false
	}
	normalized := pystr.Lower(pystr.Strip(model))
	if i := strings.LastIndex(normalized, "/"); i >= 0 {
		normalized = normalized[i+1:]
	}
	if strings.Contains(normalized, "gemini-3.1") && strings.Contains(normalized, "tts") {
		return true
	}
	ttsLog().Warn(fmt.Sprintf("Gemini TTS audio_tags enabled, but model %s is not known to support Gemini audio tags; skipping hidden tag rewrite", model))
	return false
}

// Auxiliary model insertion of Gemini audio tags, the text unchanged on failure
func rewriteGeminiTTSAudioTags(ctx context.Context, text, personaPrompt string) string {
	transcript := pystr.Strip(text)
	if transcript == "" {
		return text
	}
	systemPrompt := "You rewrite transcripts for Gemini 3.1 Flash TTS by inserting expressive audio tags.\n\n" +
		"Audio tags are inline square-bracket modifiers such as [whispers], [excitedly], [very slow], [sarcastically], [laughs], [sighs], or [gasp]. " +
		"There is no fixed allowlist. Use creative freeform tags generously but naturally to control tone, pace, emotional vibe, emphasis, section-level " +
		"delivery, and non-verbal sounds. Use English audio tags even when the spoken transcript is not English.\n\n" +
		tagRewriteRules + "- Use square brackets for every audio tag.\n- Do not use SSML or XML tags.\n" + tagRewriteTail
	persona := pystr.Strip(personaPrompt)
	if persona == "" {
		persona = "(none)"
	}
	userPrompt := "PERSONA AND DIRECTOR CONTEXT:\n" + persona + "\n\nTRANSCRIPT TO TAG:\n" + transcript
	return rewriteWithAuxiliaryModel(ctx, systemPrompt, userPrompt, text, "Gemini TTS", "untagged text", slog.LevelWarn)
}

var geminiTranscriptSlotPatterns = []string{`\{\{\s*transcript\s*\}\}`, `\{\s*transcript\s*\}`}

// Gemini prompt of persona direction and transcript, the transcript slot filled in place
func composeGeminiTTSPrompt(text, personaPrompt string) string {
	transcript := pystr.Strip(text)
	if personaPrompt == "" {
		return transcript
	}
	preamble := "Synthesize speech from the TRANSCRIPT only. Treat AUDIO PROFILE, SCENE, DIRECTOR'S NOTES, and SAMPLE CONTEXT as performance direction; do not speak those sections aloud."
	for _, pattern := range geminiTranscriptSlotPatterns {
		re := mustPyre(pyre.CompileIgnoreCase(pattern))
		if m, _ := re.MatchString(personaPrompt); m {
			replaced, _ := re.ReplaceFunc(personaPrompt, func(regexp2.Match) string { return transcript }, -1, -1)
			return pystr.Strip(preamble + "\n\n" + replaced)
		}
	}
	return pystr.Strip(preamble + "\n\n" + personaPrompt + "\n\n#### TRANSCRIPT\n" + transcript)
}

// Error message of a failed Gemini reply, else the first 300 body characters
func geminiErrorDetail(resp *http.Response) (string, error) {
	raw, err := readTTSResponseBytes(resp, "Gemini TTS", 0)
	if err != nil {
		return "", err
	}
	if parsed, perr := parseTTSJSONBody(raw); perr == nil {
		if e, ok := asStrMap(parsed["error"]); ok {
			if msg := e["message"]; pyTruthy(msg) {
				return pyrepr.Str(msg), nil
			}
		}
	}
	runes := []rune(strings.ToValidUTF8(string(raw), "�"))
	if len(runes) > 300 {
		runes = runes[:300]
	}
	return string(runes), nil
}

// Gemini base URL normalizer of the native adapter, the seam tests replace
var ttsNormalizeGeminiBaseURL = provider.NormalizeGeminiBaseURL

// Gemini generateContent speech, base64 24kHz mono PCM wrapped as WAV then converted to the container
func generateGeminiTTS(ctx context.Context, env *tools.Env, text, outputPath string, ttsConfig map[string]any) (string, error) {
	apiKey := ttsResolveProviderKey(env, "GEMINI_API_KEY", "gemini")
	if apiKey == "" {
		apiKey = ttsResolveProviderKey(env, "GOOGLE_API_KEY", "gemini")
	}
	if apiKey == "" {
		return "", &ttsValueError{msg: "GEMINI_API_KEY not set. Get one at https://aistudio.google.com/app/apikey"}
	}
	gc := ttsSection(ttsConfig, "gemini")
	model := pystr.Strip(pyrepr.Str(dictGetDefault(gc, "model", defaultGeminiTTSModel)))
	if model == "" {
		model = defaultGeminiTTSModel
	}
	voice := pystr.Strip(pyrepr.Str(dictGetDefault(gc, "voice", defaultGeminiTTSVoice)))
	if voice == "" {
		voice = defaultGeminiTTSVoice
	}
	rawBase := ""
	if pyTruthy(gc["base_url"]) {
		rawBase = pyrepr.Str(gc["base_url"])
	} else if v := ttsEnvValue(ctx, env, "GEMINI_BASE_URL"); v != "" {
		rawBase = v
	} else {
		rawBase = defaultGeminiTTSBaseURL
	}
	baseURL := ttsNormalizeGeminiBaseURL(rawBase)
	persona := readGeminiPersonaPrompt(ctx, env, gc)
	script := text
	if geminiAudioTagsEnabled(gc, model) {
		script = rewriteGeminiTTSAudioTags(ctx, text, persona)
	}
	prompt := composeGeminiTTSPrompt(script, persona)
	maxLen := resolveMaxTextLength("gemini", ttsConfig)
	if n := utf8.RuneCountInString(prompt); n > maxLen {
		return "", &ttsValueError{msg: fmt.Sprintf("Gemini TTS composed prompt exceeds the provider request limit (%d > %d chars). Reduce the persona/audio-tag prompt or lower tts.gemini.max_text_length so long-form text is split with enough prompt headroom.", n, maxLen)}
	}
	payload := map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": prompt}}}},
		"generationConfig": map[string]any{"responseModalities": []any{"AUDIO"},
			"speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": voice}}}},
	}
	headers := map[string]string{"Content-Type": "application/json", "x-goog-api-key": apiKey}
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() == "generativelanguage.googleapis.com" {
		if info, err := updaterecord.GetVersionInfo(); err == nil {
			headers["X-Goog-Api-Client"] = "icebath-agent/" + info.BaseVersion
		} else {
			headers["X-Goog-Api-Client"] = "icebath-agent/0.0.0"
		}
	}
	resp, err := ttsPostJSON(ctx, baseURL+"/models/"+model+":generateContent", payload, headers)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		detail, err := geminiErrorDetail(resp)
		if err != nil {
			return "", err
		}
		return "", &ttsRuntimeError{msg: fmt.Sprintf("Gemini TTS API error (HTTP %d): %s", resp.StatusCode, detail)}
	}
	data, err := readTTSResponseJSON(resp, "Gemini TTS")
	if err != nil {
		return "", err
	}
	audioB64, err := geminiAudioPayload(data)
	if err != nil {
		return "", err
	}
	if audioB64 == "" {
		return "", &ttsRuntimeError{msg: "Gemini TTS returned empty audio data"}
	}
	pcm, err := pybase64Decode(audioB64)
	if err != nil {
		return "", err
	}
	return writeWAVBytesAs(wrapPCMAsWAV(pcm, geminiTTSSampleRate, geminiTTSChannels, geminiTTSSampleWidth), outputPath)
}

// Base64 audio of the first inline data part, a malformed reply failing like the Python key and index errors
func geminiAudioPayload(data map[string]any) (string, error) {
	malformed := func(detail string) error {
		return &ttsRuntimeError{msg: "Gemini TTS response was malformed: " + detail}
	}
	candidates, ok := data["candidates"].([]any)
	if !ok {
		if _, present := data["candidates"]; !present {
			return "", malformed("'candidates'")
		}
		return "", malformed("'NoneType' object is not subscriptable")
	}
	if len(candidates) == 0 {
		return "", malformed("list index out of range")
	}
	first, _ := asStrMap(candidates[0])
	content, ok := asStrMap(first["content"])
	if !ok {
		return "", malformed("'content'")
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return "", malformed("'parts'")
	}
	for _, p := range parts {
		part, _ := asStrMap(p)
		_, hasCamel := part["inlineData"]
		_, hasSnake := part["inline_data"]
		if !hasCamel && !hasSnake {
			continue
		}
		inline := part["inlineData"]
		if !pyTruthy(inline) {
			inline = part["inline_data"]
		}
		m, _ := asStrMap(inline)
		s, _ := dictGetDefault(m, "data", "").(string)
		return s, nil
	}
	return "", &ttsRuntimeError{msg: "Gemini TTS response contained no audio data"}
}
