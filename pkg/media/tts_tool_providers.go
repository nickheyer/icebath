package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/media/edgetts"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	defaultEdgeVoice                 = "en-US-AriaNeural"
	defaultElevenLabsVoiceID         = "pNInz6obpgDQGcFmaJgB"
	defaultElevenLabsModelID         = "eleven_multilingual_v2"
	defaultMiniMaxModel              = "speech-02-hd"
	defaultMiniMaxVoiceID            = "English_expressive_narrator"
	defaultMiniMaxBaseURL            = "https://api.minimax.io/v1/t2a_v2"
	defaultMiniMaxCNBaseURL          = "https://api.minimaxi.com/v1/t2a_v2"
	defaultMistralTTSModel           = "voxtral-mini-tts-2603"
	defaultMistralTTSVoiceID         = "c69964a6-ab8b-4f8a-9465-ec0925096ec8"
	defaultXAIVoiceID                = "eve"
	defaultXAILanguage               = "en"
	defaultXAISampleRate             = 24000
	defaultXAIBitRate                = 128000
	defaultXAIBaseURL                = "https://api.x.ai/v1"
	defaultXAISpeedMin               = 0.7
	defaultXAISpeedMax               = 1.5
	defaultXAISpeedDefault           = 1.0
	defaultXAIOptimizeLatencyDefault = 0
	defaultGeminiTTSModel            = "gemini-2.5-flash-preview-tts"
	defaultGeminiTTSVoice            = "Kore"
	defaultGeminiTTSBaseURL          = "https://generativelanguage.googleapis.com/v1beta"
	geminiAudioTagRewriteTask        = "tts_audio_tags"
	ttsResponseBodyLimitBytes        = 16 * 1024 * 1024
	ttsResponseBodyChunkBytes        = 64 * 1024
	defaultElevenLabsBaseURL         = "https://api.elevenlabs.io"
	defaultMistralServerURL          = "https://api.mistral.ai"
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
	if !anyutil.IsTrue(v) {
		return map[string]any{}, nil
	}
	if m, ok := asStrMap(v); ok {
		return m, nil
	}
	return nil, &ttsAttributeError{msg: fmt.Sprintf("config %s is %s where a mapping was expected", key, errchain.Kind(v))}
}

// Python type error raised by a builtin conversion
type ttsTypeError struct{ msg string }

func (e *ttsTypeError) Error() string { return e.msg }

// Python float() of a config value with the raised error text
func ttsFloat(v any) (float64, error) {
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, &ttsValueError{msg: fmt.Sprintf("%s is not a number", s)}
		}
		return f, nil
	}
	if b, ok := v.(bool); ok {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	if f, ok := realNumber(v); ok {
		return f, nil
	}
	return 0, &ttsTypeError{msg: fmt.Sprintf("expected a number, got %s", errchain.Kind(v))}
}

// Python int() of a config value, truncating floats and parsing base ten strings
func wholeNumber(v any) (int, error) {
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
		s := strings.ReplaceAll(strings.TrimSpace(n), "_", "")
		if i, err := strconv.Atoi(s); err == nil && strings.TrimSpace(n) != "" {
			return i, nil
		}
		return 0, &ttsValueError{msg: fmt.Sprintf("%s is not a whole number", errchain.Show(n))}
	}
	if i := ttsPositiveInt(v); i > 0 {
		return i, nil
	}
	if f, ok := realNumber(v); ok {
		return int(f), nil
	}
	return 0, &ttsTypeError{msg: fmt.Sprintf("expected a whole number, got %s", errchain.Kind(v))}
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
		DialContext:           netpref.DialContext((&net.Dialer{Timeout: 60 * time.Second}).DialContext),
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
		return nil, &ttsValueError{msg: "the response body is not valid UTF-8"}
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

var auxFenceRe = mustPyre(regexutil.Compile("```(?:[A-Za-z0-9_-]+)?\\s*(.*?)\\s*```", regexutil.Whole()))

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
	if !anyutil.IsTrue(content) {
		content = ""
	}
	clean := strings.TrimSpace(anyutil.String(content))
	if match, _ := auxFenceRe.FindStringMatch(clean); match != nil {
		return strings.TrimSpace(match.GroupByNumber(1).String())
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
		pkgLog().Log(ctx, level, fmt.Sprintf("%s audio tag rewrite failed; using %s: %v", label, fallbackLabel, err))
		return fallback
	}
	if text := auxiliaryReplyText(resp); text != "" {
		return text
	}
	return fallback
}

// Command running the isolated interpreter of a provisioned extra, the seam tests replace
var managedPythonCommand = func(ctx context.Context, extra string) ([]string, error) {
	return pm.ExtraPythonCommand(ctx, extra)
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
func generateEdgeTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	speed, _ := req.speedFor(req.Edge.Speed)
	opts := edgetts.Options{Voice: textOr(req.Edge.Voice, defaultEdgeVoice)}
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
func generateElevenLabs(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	apiKey, err := requireTTSKey(env, "ELEVENLABS_API_KEY", "elevenlabs", "Get one at https://elevenlabs.io/")
	if err != nil {
		return "", err
	}
	format := "mp3_44100_128"
	if strings.HasSuffix(outputPath, ".ogg") {
		format = "opus_48000_64"
	}
	voiceID := textOr(req.ElevenLabs.VoiceID, defaultElevenLabsVoiceID)
	endpoint := elevenLabsBase(req) + "/v1/text-to-speech/" + url.PathEscape(voiceID) + "?output_format=" + url.QueryEscape(format)
	payload := map[string]any{"text": text, "model_id": textOr(req.ElevenLabs.ModelID, defaultElevenLabsModelID)}
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
		return "", &ttsRuntimeError{msg: fmt.Sprintf("status_code: %d, body: %s", resp.StatusCode, anyutil.String(body))}
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

// Trimmed text, or the fallback when it is empty
func textOr(text, fallback string) string {
	if t := strings.TrimSpace(text); t != "" {
		return t
	}
	return fallback
}

// ElevenLabs API base without a trailing slash, the configured endpoint first
func elevenLabsBase(req *ttsRequest) string {
	return strings.TrimRight(textOr(req.ElevenLabs.BaseURL, defaultElevenLabsBaseURL), "/")
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
	xaiSpeechTagRe     = mustPyre(regexutil.Compile(`(\[(?:`+strings.Join(xaiInlineSpeechTags, "|")+`)\]|</?(?:`+strings.Join(xaiWrappingSpeechTags, "|")+`)>)`, regexutil.IgnoreCase()))
	xaiFirstSentenceRe = mustPyre(regexutil.Compile(`(?s)^(.{12,120}?[.!?…])\s+(?=\S)`))
	paragraphBreakRe   = mustPyre(regexutil.Compile(`\n\s*\n+`))
	lineBreakRe        = mustPyre(regexutil.Compile(`\s*\n\s*`))
	multiSpaceRe       = mustPyre(regexutil.Compile(`\s{2,}`))
)

// Local xAI pause tags then an auxiliary rewrite when the text carried no explicit tags
func applyXAIAutoSpeechTags(ctx context.Context, text string) string {
	clean := strings.TrimSpace(text)
	if clean == "" {
		return text
	}
	local, _ := paragraphBreakRe.Replace(clean, " [pause] ", -1, -1)
	local, _ = lineBreakRe.Replace(local, " ", -1, -1)
	if m, _ := xaiSpeechTagRe.MatchString(local); !m {
		local, _ = xaiFirstSentenceRe.Replace(local, "$1 [pause] ", -1, 1)
	}
	local, _ = multiSpaceRe.Replace(local, " ", -1, -1)
	local = strings.TrimSpace(local)
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
func generateXAITTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	credProvider, apiKey, credBase := ttsResolveXAICredentials(env)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", &ttsValueError{msg: "No xAI credentials found. Configure xAI OAuth in `icebath model` or set XAI_API_KEY."}
	}
	x := req.XAI
	voiceID := textOr(x.VoiceID, defaultXAIVoiceID)
	language := textOr(x.Language, defaultXAILanguage)
	sampleRate, bitRate := x.SampleRate, x.BitRate
	if x.AutoSpeechTags {
		text = applyXAIAutoSpeechTags(ctx, text)
	}
	speed, speedSet := req.speedFor(x.Speed)
	speed = math.Max(defaultXAISpeedMin, math.Min(defaultXAISpeedMax, speed))
	latency := x.Latency
	if latency == nil {
		latency = req.Latency
	}
	baseURL := ""
	switch {
	case credProvider == "xai-oauth":
		baseURL = credBase
	case strings.TrimSpace(x.BaseURL) != "":
		baseURL = x.BaseURL
	case credBase != "":
		baseURL = credBase
	default:
		baseURL = ttsEnvValue(ctx, env, "XAI_BASE_URL")
	}
	base := strings.TrimRight(textOr(baseURL, defaultXAIBaseURL), "/")
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
	if speedSet && speed != defaultXAISpeedDefault {
		payload["speed"] = speed
	}
	if latency != nil {
		if i := max(0, min(2, *latency)); i != defaultXAIOptimizeLatencyDefault {
			payload["optimize_streaming_latency"] = i
		}
	}
	if x.TextNormalization {
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
func resolveMiniMaxTTSRuntime(env *tools.Env, req *ttsRequest) (*miniMaxTTSRuntime, error) {
	creds := map[string][2]string{
		"global": {"MINIMAX_API_KEY", strings.TrimSpace(ttsResolveProviderKey(env, "MINIMAX_API_KEY", "minimax"))},
		"cn":     {"MINIMAX_CN_API_KEY", strings.TrimSpace(ttsResolveProviderKey(env, "MINIMAX_CN_API_KEY", "minimax"))},
	}
	region := strings.ToLower(strings.TrimSpace(req.MiniMax.Region))
	if region == "" {
		region = "global"
		if creds["cn"][1] != "" && creds["global"][1] == "" {
			region = "cn"
		}
	}
	source, apiKey := creds[region][0], creds[region][1]
	if apiKey == "" {
		return nil, &ttsValueError{msg: fmt.Sprintf("%s not set for MiniMax TTS region %s", source, errchain.Show(region))}
	}
	endpoint := textOr(req.MiniMax.BaseURL, miniMaxEndpoints[region])
	other := "global"
	if region == "global" {
		other = "cn"
	}
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if miniMaxOfficialHosts[other][host] {
		return nil, &ttsValueError{msg: fmt.Sprintf("tts.minimax.base_url points to the %s MiniMax endpoint but region is %s", errchain.Show(other), errchain.Show(region))}
	}
	return &miniMaxTTSRuntime{Region: region, Endpoint: endpoint, CredentialSource: source, APIKey: apiKey}, nil
}

// MiniMax API error from a base_resp status other than zero
func raiseMiniMaxAPIError(result map[string]any) error {
	baseResp, _ := asStrMap(dictGetDefault(result, "base_resp", map[string]any{}))
	status := dictGetDefault(baseResp, "status_code", -1)
	if n, ok := realNumber(status); ok && n == 0 {
		return nil
	}
	return &ttsRuntimeError{msg: fmt.Sprintf("MiniMax TTS API error (code %s): %s", anyutil.String(status), anyutil.String(dictGetDefault(baseResp, "status_msg", "unknown error")))}
}

// MiniMax speech via t2a_v2 hex audio JSON or the legacy raw audio endpoint, detected from the URL
func generateMiniMaxTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	runtime, err := resolveMiniMaxTTSRuntime(env, req)
	if err != nil {
		return "", err
	}
	mm := req.MiniMax
	model := textOr(mm.Model, defaultMiniMaxModel)
	voiceID := textOr(mm.VoiceID, defaultMiniMaxVoiceID)
	speed, _ := req.speedFor(mm.Speed)
	baseURL := runtime.Endpoint
	groupID := strings.TrimSpace(mm.GroupID)
	if groupID == "" {
		groupID = strings.TrimSpace(ttsEnvValue(ctx, env, "MINIMAX_GROUP_ID"))
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
			"voice_setting": map[string]any{"voice_id": voiceID, "speed": speed, "vol": mm.Vol, "pitch": mm.Pitch, "emotion": textOr(mm.Emotion, "neutral")},
			"audio_setting": map[string]any{"sample_rate": mm.SampleRate, "bitrate": mm.Bitrate, "format": "mp3", "channel": 1}}
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
		audio, err := bytesFromHex(hexAudio)
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
func bytesFromHex(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, &ttsValueError{msg: err.Error()}
	}
	return b, nil
}

// Mistral Voxtral speech decoded from base64 audio, client failures wrapped by error class
func generateMistralTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	apiKey, err := requireTTSKey(env, "MISTRAL_API_KEY", "mistral", "Get one at https://console.mistral.ai/")
	if err != nil {
		return "", err
	}
	server := textOr(req.Mistral.BaseURL, defaultMistralServerURL)
	payload := map[string]any{"model": textOr(req.Mistral.Model, defaultMistralTTSModel), "input": text, "voice_id": textOr(req.Mistral.VoiceID, defaultMistralTTSVoiceID),
		"response_format": ttsResponseFormatFromPath(outputPath)}
	audioData, className, err := ttsMistralSpeech(ctx, server, apiKey, payload)
	if err != nil {
		pkgLog().Error(fmt.Sprintf("Mistral TTS failed: %v", err))
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
func readGeminiPersonaPrompt(ctx context.Context, env *tools.Env, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	path := pathutil.ExpandUser(pathutil.ExpandEnv(strings.TrimSpace(raw)))
	if !filepath.IsAbs(path) {
		path = filepath.Join(ttsHome(ctx, env), path)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		text, derr := textutil.DecodeUTF8(textutil.TrimBOM(data))
		if derr == nil {
			return strings.TrimSpace(text)
		}
		err = derr
	}
	pkgLog().Warn(fmt.Sprintf("Gemini TTS persona prompt file unavailable at %s: %s", path, errchain.OSText(err)))
	return ""
}

// Whether audio tags are opted in and the model is a Gemini 3.1 TTS model
func geminiAudioTagsEnabled(enabled bool, model string) bool {
	if !enabled {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(normalized, "/"); i >= 0 {
		normalized = normalized[i+1:]
	}
	if strings.Contains(normalized, "gemini-3.1") && strings.Contains(normalized, "tts") {
		return true
	}
	pkgLog().Warn(fmt.Sprintf("Gemini TTS audio_tags enabled, but model %s is not known to support Gemini audio tags; skipping hidden tag rewrite", model))
	return false
}

// Auxiliary model insertion of Gemini audio tags, the text unchanged on failure
func rewriteGeminiTTSAudioTags(ctx context.Context, text, personaPrompt string) string {
	transcript := strings.TrimSpace(text)
	if transcript == "" {
		return text
	}
	systemPrompt := "You rewrite transcripts for Gemini 3.1 Flash TTS by inserting expressive audio tags.\n\n" +
		"Audio tags are inline square-bracket modifiers such as [whispers], [excitedly], [very slow], [sarcastically], [laughs], [sighs], or [gasp]. " +
		"There is no fixed allowlist. Use creative freeform tags generously but naturally to control tone, pace, emotional vibe, emphasis, section-level " +
		"delivery, and non-verbal sounds. Use English audio tags even when the spoken transcript is not English.\n\n" +
		tagRewriteRules + "- Use square brackets for every audio tag.\n- Do not use SSML or XML tags.\n" + tagRewriteTail
	persona := strings.TrimSpace(personaPrompt)
	if persona == "" {
		persona = "(none)"
	}
	userPrompt := "PERSONA AND DIRECTOR CONTEXT:\n" + persona + "\n\nTRANSCRIPT TO TAG:\n" + transcript
	return rewriteWithAuxiliaryModel(ctx, systemPrompt, userPrompt, text, "Gemini TTS", "untagged text", slog.LevelWarn)
}

var geminiTranscriptSlotPatterns = []string{`\{\{\s*transcript\s*\}\}`, `\{\s*transcript\s*\}`}

// Gemini prompt of persona direction and transcript, the transcript slot filled in place
func composeGeminiTTSPrompt(text, personaPrompt string) string {
	transcript := strings.TrimSpace(text)
	if personaPrompt == "" {
		return transcript
	}
	preamble := "Synthesize speech from the TRANSCRIPT only. Treat AUDIO PROFILE, SCENE, DIRECTOR'S NOTES, and SAMPLE CONTEXT as performance direction; do not speak those sections aloud."
	for _, pattern := range geminiTranscriptSlotPatterns {
		re := mustPyre(regexutil.Compile(pattern, regexutil.IgnoreCase()))
		if m, _ := re.MatchString(personaPrompt); m {
			replaced, _ := re.ReplaceFunc(personaPrompt, func(regexp2.Match) string { return transcript }, -1, -1)
			return strings.TrimSpace(preamble + "\n\n" + replaced)
		}
	}
	return strings.TrimSpace(preamble + "\n\n" + personaPrompt + "\n\n#### TRANSCRIPT\n" + transcript)
}

// Error message of a failed Gemini reply, else the first 300 body characters
func geminiErrorDetail(resp *http.Response) (string, error) {
	raw, err := readTTSResponseBytes(resp, "Gemini TTS", 0)
	if err != nil {
		return "", err
	}
	if parsed, perr := parseTTSJSONBody(raw); perr == nil {
		if e, ok := asStrMap(parsed["error"]); ok {
			if msg := e["message"]; anyutil.IsTrue(msg) {
				return anyutil.String(msg), nil
			}
		}
	}
	runes := []rune(strings.ToValidUTF8(string(raw), "�"))
	if len(runes) > 300 {
		runes = runes[:300]
	}
	return string(runes), nil
}

// Gemini endpoint before normalizing: the configured one, then GEMINI_BASE_URL, then the public API
func geminiRawBase(ctx context.Context, env *tools.Env, configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return textOr(ttsEnvValue(ctx, env, "GEMINI_BASE_URL"), defaultGeminiTTSBaseURL)
}

// Gemini base URL normalizer of the native adapter, the seam tests replace
var ttsNormalizeGeminiBaseURL = provider.NormalizeGeminiBaseURL

// Gemini generateContent speech, base64 24kHz mono PCM wrapped as WAV then converted to the container
func generateGeminiTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	apiKey := ttsResolveProviderKey(env, "GEMINI_API_KEY", "gemini")
	if apiKey == "" {
		apiKey = ttsResolveProviderKey(env, "GOOGLE_API_KEY", "gemini")
	}
	if apiKey == "" {
		return "", &ttsValueError{msg: "GEMINI_API_KEY not set. Get one at https://aistudio.google.com/app/apikey"}
	}
	gc := req.Gemini
	model := textOr(gc.Model, defaultGeminiTTSModel)
	voice := textOr(gc.Voice, defaultGeminiTTSVoice)
	baseURL := ttsNormalizeGeminiBaseURL(geminiRawBase(ctx, env, gc.BaseURL))
	persona := readGeminiPersonaPrompt(ctx, env, gc.PersonaPromptFile)
	script := text
	if geminiAudioTagsEnabled(gc.AudioTags, model) {
		script = rewriteGeminiTTSAudioTags(ctx, text, persona)
	}
	prompt := composeGeminiTTSPrompt(script, persona)
	maxLen := resolveMaxTextLength("gemini", req)
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
			return "", malformed("no candidates")
		}
		return "", malformed(fmt.Sprintf("candidates is %s where a list was expected", errchain.Kind(data["candidates"])))
	}
	if len(candidates) == 0 {
		return "", malformed("the candidates list is empty")
	}
	first, _ := asStrMap(candidates[0])
	content, ok := asStrMap(first["content"])
	if !ok {
		return "", malformed("the first candidate has no content")
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return "", malformed("the first candidate content has no parts")
	}
	for _, p := range parts {
		part, _ := asStrMap(p)
		_, hasCamel := part["inlineData"]
		_, hasSnake := part["inline_data"]
		if !hasCamel && !hasSnake {
			continue
		}
		inline := part["inlineData"]
		if !anyutil.IsTrue(inline) {
			inline = part["inline_data"]
		}
		m, _ := asStrMap(inline)
		s, _ := dictGetDefault(m, "data", "").(string)
		return s, nil
	}
	return "", &ttsRuntimeError{msg: "Gemini TTS response contained no audio data"}
}
