package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Per sentence PCM byte ceiling matching the 16 MiB bounded body of the sync providers
var streamSentenceByteCap = 16 * 1024 * 1024

func ttsStreamingLog() *slog.Logger { return slog.Default().With("logger", "tools.tts_streaming") }

// Provider secret through config, env, dotenv then the credential pool, a plain env read when that resolver fails
var streamResolveKey = func(ctx context.Context, env *tools.Env, envVar, providerID string) string {
	key := ""
	if err := pluginCall(func() error {
		key = ttsResolveProviderKey(env, envVar, providerID)
		return nil
	}); err != nil {
		return ttsEnvValue(ctx, env, envVar)
	}
	return key
}

// Gemini key, the Google key standing in when it is unset
func geminiStreamKey(ctx context.Context, env *tools.Env) string {
	if key := streamResolveKey(ctx, env, "GEMINI_API_KEY", "gemini"); key != "" {
		return key
	}
	return streamResolveKey(ctx, env, "GOOGLE_API_KEY", "gemini")
}

// Yields int16 little endian mono PCM chunks of one sentence at its current sample rate
type StreamingTTSProvider interface {
	SampleRate() int
	Channels() int
	SampleWidth() int
	Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error)
}

// Availability probe and constructor of one registered chunked streamer
type StreamingTTSClass struct {
	Available func(ctx context.Context, env *tools.Env) bool
	New       func(env *tools.Env, ttsConfig map[string]any, section any) (StreamingTTSProvider, error)
}

// Config, provider section and sample rate every built in streamer carries
type streamerBase struct {
	env        *tools.Env
	ttsConfig  map[string]any
	section    any
	sampleRate atomic.Int64
}

func (b *streamerBase) init(env *tools.Env, ttsConfig map[string]any, section any, rate int) {
	b.env, b.ttsConfig, b.section = env, ttsConfig, section
	b.sampleRate.Store(int64(rate))
}

func (b *streamerBase) SampleRate() int  { return int(b.sampleRate.Load()) }
func (b *streamerBase) Channels() int    { return 1 }
func (b *streamerBase) SampleWidth() int { return 2 }

// Provider section as a mapping, any other truthy value failing like a Python attribute lookup
func (b *streamerBase) sectionMap() (map[string]any, error) {
	if m, ok := asStrMap(b.section); ok {
		return m, nil
	}
	return nil, &ttsAttributeError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(b.section))}
}

var (
	streamingRegistryMu sync.RWMutex
	streamingRegistry   = map[string]StreamingTTSClass{
		"elevenlabs": {Available: elevenLabsStreamerAvailable, New: newElevenLabsStreamer},
		"openai":     {Available: openAIStreamerAvailable, New: newOpenAIStreamer},
		"gemini":     {Available: geminiStreamerAvailable, New: newGeminiStreamer},
		"xai":        {Available: xaiStreamerAvailable, New: newXAIStreamer},
	}
	streamingProviderPriority = []string{"elevenlabs", "gemini", "openai", "xai"}
)

// Files a chunked streamer class under a provider name
func RegisterStreamingTTSProvider(name string, class StreamingTTSClass) {
	streamingRegistryMu.Lock()
	streamingRegistry[name] = class
	streamingRegistryMu.Unlock()
}

// Builds the registered streamer of a name when its credentials are usable, nil otherwise
var tryInstantiate = func(ctx context.Context, env *tools.Env, name string, ttsConfig map[string]any) StreamingTTSProvider {
	streamingRegistryMu.RLock()
	class, ok := streamingRegistry[name]
	streamingRegistryMu.RUnlock()
	if !ok || !class.Available(ctx, env) {
		return nil
	}
	var section any = map[string]any{}
	if v := ttsConfig[name]; pyTruthy(v) {
		section = v
		if m, isMap := asStrMap(v); isMap {
			section = m
		}
	}
	inst, err := class.New(env, ttsConfig, section)
	if err != nil {
		ttsStreamingLog().Debug(fmt.Sprintf("streaming provider %s init failed: %s", name, err))
		return nil
	}
	return inst
}

// Plugin provider that opted into raw PCM, served through the streamer interface
type pluginPCMStreamer struct {
	streamerBase
	provider TTSProvider
}

func (s *pluginPCMStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		opts := pluginVoiceKwargs(s.ttsConfig)
		opts.Format = "pcm"
		var chunks iter.Seq2[[]byte, error]
		if err := pluginCall(func() (err error) {
			chunks, err = s.provider.Stream(ctx, text, opts)
			return err
		}); err != nil {
			yield(nil, err)
			return
		}
		cappedChunks(chunks, "plugin streamer "+s.provider.Name())(yield)
	}, nil
}

// Streamer over the plugin servicing a provider name when it opted into PCM streaming, nil otherwise
func pluginStreamer(ctx context.Context, env *tools.Env, name string, ttsConfig map[string]any) StreamingTTSProvider {
	p, rate, ok := pluginPCMStreamingProvider(ctx, name, ttsConfig)
	if !ok {
		return nil
	}
	var section any = map[string]any{}
	if v := ttsConfig[p.Name()]; pyTruthy(v) {
		section = v
	}
	s := &pluginPCMStreamer{provider: p}
	s.init(env, ttsConfig, section, rate)
	return s
}

// Returns a ready chunked streamer for the configured provider, nil meaning per sentence synthesis
func ResolveStreamingProvider(ctx context.Context, env *tools.Env, ttsConfig map[string]any, preferred string) (StreamingTTSProvider, error) {
	streaming, err := orEmptyDict(ttsConfig, "streaming")
	if err != nil {
		return nil, err
	}
	pinned := ""
	if p := streaming["provider"]; pyTruthy(p) {
		pinned = pystr.Strip(pystr.Lower(pyrepr.Str(p)))
	}
	if pinned == "auto" {
		for _, name := range streamingProviderPriority {
			if inst := tryInstantiate(ctx, env, name, ttsConfig); inst != nil {
				return inst, nil
			}
		}
		return pluginStreamer(ctx, env, getTTSProvider(ttsConfig), ttsConfig), nil
	}
	name := pinned
	if name == "" {
		name = preferred
		if name == "" {
			name = getTTSProvider(ttsConfig)
		}
		name = pystr.Strip(pystr.Lower(name))
	}
	if inst := tryInstantiate(ctx, env, name, ttsConfig); inst != nil {
		return inst, nil
	}
	return pluginStreamer(ctx, env, name, ttsConfig), nil
}

// Passes chunks through until one sentence exceeds the byte ceiling, then stops with a warning
func cappedChunks(chunks iter.Seq2[[]byte, error], label string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		total := 0
		for chunk, err := range chunks {
			if err != nil {
				yield(nil, err)
				return
			}
			total += len(chunk)
			if total > streamSentenceByteCap {
				ttsStreamingLog().Warn(fmt.Sprintf("%s exceeded %d bytes for one sentence; truncating", label, streamSentenceByteCap))
				return
			}
			if !yield(chunk, nil) {
				return
			}
		}
	}
}

// Reads a reply body in pieces of at most size bytes, exact sized when fill is set
func bodyChunks(body io.Reader, size int, fill bool) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		buf := make([]byte, size)
		for {
			var n int
			var err error
			if fill {
				n, err = io.ReadFull(body, buf)
				if err == io.ErrUnexpectedEOF {
					err = io.EOF
				}
			} else {
				n, err = body.Read(buf)
			}
			if n > 0 && !yield(bytes.Clone(buf[:n]), nil) {
				return
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// ElevenLabs chunked HTTP convert at pcm_24000
type elevenLabsStreamer struct{ streamerBase }

func newElevenLabsStreamer(env *tools.Env, ttsConfig map[string]any, section any) (StreamingTTSProvider, error) {
	s := &elevenLabsStreamer{}
	s.init(env, ttsConfig, section, 24000)
	return s, nil
}

func elevenLabsStreamerAvailable(ctx context.Context, env *tools.Env) bool {
	return streamResolveKey(ctx, env, "ELEVENLABS_API_KEY", "elevenlabs") != ""
}

func (s *elevenLabsStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		if err := ttsImportElevenLabs(ctx); err != nil {
			yield(nil, err)
			return
		}
		apiKey := streamResolveKey(ctx, s.env, "ELEVENLABS_API_KEY", "elevenlabs")
		section, err := s.sectionMap()
		if err != nil {
			yield(nil, err)
			return
		}
		base := defaultElevenLabsBaseURL
		if b := strings.TrimRight(pyrepr.Str(orEmptyString(section["base_url"])), "/"); b != "" {
			base = b
		}
		voiceID := pyrepr.Str(dictGetDefault(section, "voice_id", defaultElevenLabsVoiceID))
		modelID := dictGetDefault(section, "streaming_model_id", dictGetDefault(section, "model_id", defaultElevenLabsStreamingModelID))
		endpoint := base + "/v1/text-to-speech/" + url.PathEscape(voiceID) + "?output_format=pcm_24000"
		resp, err := ttsElevenLabsPost(ctx, endpoint, apiKey, map[string]any{"text": text, "model_id": modelID})
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(resp.Body)
			var body any = string(raw)
			var parsed any
			if json.Unmarshal(raw, &parsed) == nil {
				body = parsed
			}
			yield(nil, &ttsRuntimeError{msg: fmt.Sprintf("status_code: %d, body: %s", resp.StatusCode, pyrepr.Str(body))})
			return
		}
		bodyChunks(resp.Body, 1024, true)(yield)
	}, nil
}

// The tts.openai.api_key config value, empty when unset or unreadable
var streamOpenAIConfigAPIKey = func(ctx context.Context, env *tools.Env) string {
	key := ""
	_ = pluginCall(func() error {
		section, err := orEmptyDict(loadTTSConfig(ctx, env), "openai")
		if err != nil {
			return err
		}
		if v := section["api_key"]; pyTruthy(v) {
			key = pyrepr.Str(v)
		}
		return nil
	})
	return key
}

// OpenAI audio key resolution, the seam tests replace
var streamResolveOpenAIAudioAPIKey = tools.ResolveOpenAIAudioAPIKey

var contentTypeRateRe = regexp.MustCompile("(?i)(?:^|[;\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}])rate[\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}]*=[\\s\\x0b\\x1c-\\x1f\\x{85}\\x{a0}]*([0-9]+)")

// Header bytes read as Latin-1 text, the decoding httpx applies to header values
func latin1Text(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// Machine int of a Python int, saturating past the int64 range
func saturatedInt(n *big.Int) int {
	if n.IsInt64() {
		return int(n.Int64())
	}
	if n.Sign() < 0 {
		return math.MinInt
	}
	return math.MaxInt
}

// PCM rate an OpenAI compatible endpoint advertises in X-Audio-Sample-Rate or the Content-Type rate parameter
func sampleRateFromHeaders(headers http.Header) (int, bool) {
	if len(headers) == 0 {
		return 0, false
	}
	raw := ""
	if vals := headers.Values("X-Audio-Sample-Rate"); len(vals) > 0 {
		raw = latin1Text(strings.Join(vals, ", "))
	} else {
		m := contentTypeRateRe.FindStringSubmatch(latin1Text(strings.Join(headers.Values("Content-Type"), ", ")))
		if m == nil {
			return 0, false
		}
		raw = m[1]
	}
	n, ok := pyint.Parse(pystr.Strip(raw))
	if !ok || n.Sign() <= 0 {
		return 0, false
	}
	return saturatedInt(n), true
}

// Open speech reply of the OpenAI client, its headers and its body chunks
type openAISpeechStream interface {
	Headers() http.Header
	IterBytes() iter.Seq2[[]byte, error]
	Close() error
}

// One OpenAI compatible audio.speech.create call answered as a chunked body
type openAIStreamClient interface {
	StreamSpeech(ctx context.Context, body, extraBody map[string]any) (openAISpeechStream, error)
}

// OpenAI client for chunked speech, an empty base URL resolving to OPENAI_BASE_URL then the SDK default
var newOpenAIStreamClient = func(apiKey, baseURL string) openAIStreamClient {
	if baseURL == "" {
		if v, ok := os.LookupEnv("OPENAI_BASE_URL"); ok {
			baseURL = v
		} else {
			baseURL = defaultOpenAITTSBaseURL
		}
	}
	return &openAISpeechClient{apiKey: apiKey, baseURL: baseURL, maxRetries: 2, timeout: 600 * time.Second}
}

type openAIHTTPSpeechStream struct{ resp *http.Response }

func (r *openAIHTTPSpeechStream) Headers() http.Header { return r.resp.Header }
func (r *openAIHTTPSpeechStream) Close() error         { return r.resp.Body.Close() }
func (r *openAIHTTPSpeechStream) IterBytes() iter.Seq2[[]byte, error] {
	return bodyChunks(r.resp.Body, 64*1024, false)
}

// Posts the speech request with the SDK retry policy and hands back the open reply for chunked reading
func (c *openAISpeechClient) StreamSpeech(ctx context.Context, body, extraBody map[string]any) (openAISpeechStream, error) {
	merged := maps.Clone(body)
	maps.Copy(merged, extraBody)
	payload, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/audio/speech"
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, endpoint, payload, nil, attempt)
		if err != nil {
			if attempt < c.maxRetries {
				ttsOpenAISleep(retryDelay(nil, attempt))
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
				return nil, &openAITimeoutError{err: err}
			}
			return nil, &openAIConnectionError{err: err}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &openAIHTTPSpeechStream{resp: resp}, nil
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if attempt < c.maxRetries && shouldRetryOpenAI(resp) {
			ttsOpenAISleep(retryDelay(resp.Header, attempt))
			continue
		}
		var decoded any = string(raw)
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				if inner, ok := m["error"]; ok {
					parsed = inner
				}
			}
			decoded = parsed
		}
		return nil, &openAIAPIStatusError{Status: resp.StatusCode, Body: decoded}
	}
}

// OpenAI speech with response_format pcm, the endpoint reported rate overriding the configured one
type openAIStreamer struct{ streamerBase }

// Whether every character of a non empty text is a Python digit
func pyIsDigit(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

func newOpenAIStreamer(env *tools.Env, ttsConfig map[string]any, section any) (StreamingTTSProvider, error) {
	s := &openAIStreamer{}
	s.init(env, ttsConfig, section, 24000)
	m, err := s.sectionMap()
	if err != nil {
		return nil, err
	}
	configured := dictGetDefault(m, "pcm_sample_rate", 24000)
	_, isBool := configured.(bool)
	_, isStr := configured.(string)
	_, isBig := configured.(*big.Int)
	_, isNum := pyRealNumber(configured)
	text := pystr.Strip(pyrepr.Str(configured))
	valid := !isBool && (isStr || isBig || isNum) && pyIsDigit(text)
	if valid {
		n, ok := pyint.Parse(text)
		if !ok {
			return nil, &ttsValueError{msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))}
		}
		if n.Sign() > 0 {
			s.sampleRate.Store(int64(saturatedInt(n)))
			return s, nil
		}
	}
	ttsStreamingLog().Warn(fmt.Sprintf("Invalid tts.openai.pcm_sample_rate %s; using %d Hz", pyrepr.Repr(configured), s.SampleRate()))
	return s, nil
}

func openAIStreamerAvailable(ctx context.Context, env *tools.Env) bool {
	return streamOpenAIConfigAPIKey(ctx, env) != "" || streamResolveOpenAIAudioAPIKey(env) != ""
}

func (s *openAIStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		section, err := s.sectionMap()
		if err != nil {
			yield(nil, err)
			return
		}
		apiKey := ""
		if v := section["api_key"]; pyTruthy(v) {
			apiKey = pyrepr.Str(v)
		} else {
			apiKey = streamResolveOpenAIAudioAPIKey(s.env)
		}
		baseURL := ""
		if v := section["base_url"]; pyTruthy(v) {
			baseURL = pyrepr.Str(v)
		} else {
			baseURL = ttsEnvValue(ctx, s.env, "OPENAI_BASE_URL")
		}
		client := newOpenAIStreamClient(apiKey, baseURL)
		extra := openAIExtraBody(section)
		if len(extra) == 0 {
			extra = nil
		}
		body := map[string]any{
			"model": dictGetDefault(section, "model", defaultOpenAITTSModel), "voice": dictGetDefault(section, "voice", defaultOpenAITTSVoice),
			"input": text, "response_format": "pcm",
		}
		response, err := client.StreamSpeech(ctx, body, extra)
		if err != nil {
			yield(nil, err)
			return
		}
		defer response.Close()
		if rate, ok := sampleRateFromHeaders(response.Headers()); ok && rate != s.SampleRate() {
			ttsStreamingLog().Info(fmt.Sprintf("TTS endpoint reports %d Hz PCM (expected %d Hz); honoring it", rate, s.SampleRate()))
			s.sampleRate.Store(int64(rate))
		}
		cappedChunks(response.IterBytes(), "OpenAI streaming TTS")(yield)
	}, nil
}

// Gemini streamGenerateContent server sent events carrying base64 PCM at 24 kHz
type geminiStreamer struct{ streamerBase }

func newGeminiStreamer(env *tools.Env, ttsConfig map[string]any, section any) (StreamingTTSProvider, error) {
	s := &geminiStreamer{}
	s.init(env, ttsConfig, section, 24000)
	return s, nil
}

func geminiStreamerAvailable(ctx context.Context, env *tools.Env) bool {
	return geminiStreamKey(ctx, env) != ""
}

// Body text encoding requests derives from the Content-Type header, empty when it derives none
func requestsEncodingFromHeaders(headers http.Header) (string, error) {
	contentType := strings.Join(headers.Values("Content-Type"), ", ")
	if contentType == "" {
		return "", nil
	}
	tokens := strings.Split(contentType, ";")
	mime := pystr.Strip(tokens[0])
	charset, charsetSet, charsetFlag := "", false, false
	for _, param := range tokens[1:] {
		param = pystr.Strip(param)
		if param == "" {
			continue
		}
		key, value, hasValue := strings.Cut(param, "=")
		if hasValue {
			key = pystr.StripChars(key, "\"' ")
		}
		if pystr.Lower(key) == "charset" {
			charset, charsetSet, charsetFlag = pystr.StripChars(value, "\"' "), true, !hasValue
		}
	}
	if charsetFlag {
		return "", &ttsAttributeError{msg: "'bool' object has no attribute 'strip'"}
	}
	if charsetSet {
		return pystr.StripChars(charset, "'\""), nil
	}
	if strings.Contains(mime, "text") {
		return "ISO-8859-1", nil
	}
	if strings.Contains(mime, "application/json") {
		return "utf-8", nil
	}
	return "", nil
}

// Decodes the longest strictly valid prefix, holding back up to three trailing bytes of a split character
func decodeStreamChunk(pending []byte, encoding string, final bool) (string, []byte, error) {
	if !final {
		for k := 0; k <= 3 && k <= len(pending); k++ {
			if text, err := pycodecs.Decode(pending[:len(pending)-k], encoding); err == nil {
				return text, bytes.Clone(pending[len(pending)-k:]), nil
			}
		}
	}
	text, err := pycodecs.DecodeReplace(pending, encoding)
	return text, nil, err
}

// Lines of a streamed reply the way requests iter_lines cuts them with unicode decoding on
func responseLines(resp *http.Response) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		encoding, err := requestsEncodingFromHeaders(resp.Header)
		if err != nil {
			yield("", err)
			return
		}
		var raw []byte
		pending, havePending, undecodedLine := "", false, false
		emit := func(text string) bool {
			if text == "" {
				return true
			}
			if havePending {
				text = pending + text
			}
			lines := pystr.Splitlines(text, false)
			pending, havePending = "", false
			if n := len(lines); n > 0 && lines[n-1] != "" {
				last, _ := utf8.DecodeLastRuneInString(lines[n-1])
				end, _ := utf8.DecodeLastRuneInString(text)
				if last == end {
					pending, havePending, lines = lines[n-1], true, lines[:n-1]
				}
			}
			for _, line := range lines {
				if !yield(line, nil) {
					return false
				}
			}
			return true
		}
		bytesLineError := &ttsTypeError{msg: "startswith first arg must be bytes or a tuple of bytes, not str"}
		for chunk, err := range bodyChunks(resp.Body, 512, false) {
			if err != nil {
				yield("", err)
				return
			}
			if encoding == "" {
				for _, b := range chunk {
					if b != '\n' && b != '\r' {
						undecodedLine = true
					} else if undecodedLine {
						yield("", bytesLineError)
						return
					}
				}
				continue
			}
			text, rest, err := decodeStreamChunk(append(raw, chunk...), encoding, false)
			if err != nil {
				yield("", err)
				return
			}
			raw = rest
			if !emit(text) {
				return
			}
		}
		if encoding == "" {
			if undecodedLine {
				yield("", bytesLineError)
			}
			return
		}
		text, _, err := decodeStreamChunk(raw, encoding, true)
		if err != nil {
			yield("", err)
			return
		}
		if !emit(text) {
			return
		}
		if havePending {
			yield(pending, nil)
		}
	}
}

// The candidates[0].content.parts value of one SSE event, false when any lookup would raise
func geminiEventParts(event any) (any, bool) {
	obj, ok := event.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	candidates, ok := obj.Get("candidates")
	if !ok {
		return nil, false
	}
	var first any
	switch c := candidates.(type) {
	case []any:
		if len(c) == 0 {
			return nil, false
		}
		first = c[0]
	default:
		return nil, false
	}
	cand, ok := first.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	content, ok := cand.Get("content")
	if !ok {
		return nil, false
	}
	contentObj, ok := content.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	return contentObj.Get("parts")
}

// The elements a Python for loop walks over a decoded JSON value, failing for a scalar
func pyIterJSON(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, 0, len(t.Keys))
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &ttsTypeError{msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// Python dict.get of a decoded JSON value, a non mapping failing like the attribute lookup
func pyJSONGet(v any, key string) (any, error) {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, &ttsAttributeError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
	}
	out, _ := obj.Get(key)
	return out, nil
}

// base64.b64decode of a decoded JSON value, a non string failing with the Python type error
func pyB64DecodeValue(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, &ttsTypeError{msg: fmt.Sprintf("argument should be a bytes-like object or ASCII string, not '%s'", pyrepr.TypeName(v))}
	}
	return pybase64.B64Decode(s)
}

// Decoded PCM of every inline audio part in the SSE reply
func (s *geminiStreamer) sseChunks(ctx context.Context, endpoint, apiKey string, payload map[string]any) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		resp, err := ttsPostJSON(ctx, endpoint+sep+"alt=sse", payload, map[string]string{"x-goog-api-key": apiKey, "Content-Type": "application/json"})
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()
		if err := ttsRaiseForStatus(resp); err != nil {
			yield(nil, err)
			return
		}
		for line, err := range responseLines(resp) {
			if err != nil {
				yield(nil, err)
				return
			}
			data, isData := strings.CutPrefix(line, "data: ")
			if line == "" || !isData {
				continue
			}
			event, err := pyjson.Loads(data)
			if err != nil {
				continue
			}
			partsValue, ok := geminiEventParts(event)
			if !ok {
				continue
			}
			parts, err := pyIterJSON(partsValue)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, part := range parts {
				inline, err := pyJSONGet(part, "inlineData")
				if err == nil && !pyTruthy(inline) {
					inline, err = pyJSONGet(part, "inline_data")
				}
				if err != nil {
					yield(nil, err)
					return
				}
				var b64 any = ""
				if pyTruthy(inline) {
					obj, isObj := inline.(*pyjson.Object)
					if !isObj {
						yield(nil, &ttsAttributeError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(inline))})
						return
					}
					b64 = obj.GetOr("data", "")
				}
				if !pyTruthy(b64) {
					continue
				}
				pcm, err := pyB64DecodeValue(b64)
				if err != nil {
					ttsStreamingLog().Warn(fmt.Sprintf("Gemini SSE: bad base64 audio: %s", err))
					continue
				}
				if !yield(pcm, nil) {
					return
				}
			}
		}
	}
}

func (s *geminiStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		apiKey := geminiStreamKey(ctx, s.env)
		section, err := s.sectionMap()
		if err != nil {
			yield(nil, err)
			return
		}
		model := pystr.Strip(pyrepr.Str(dictGetDefault(section, "model", defaultGeminiTTSModel)))
		if model == "" {
			model = defaultGeminiTTSModel
		}
		voice := pystr.Strip(pyrepr.Str(dictGetDefault(section, "voice", defaultGeminiTTSVoice)))
		if voice == "" {
			voice = defaultGeminiTTSVoice
		}
		rawBase := defaultGeminiTTSBaseURL
		if v := section["base_url"]; pyTruthy(v) {
			rawBase = pyrepr.Str(v)
		} else if v := ttsEnvValue(ctx, s.env, "GEMINI_BASE_URL"); v != "" {
			rawBase = v
		}
		baseURL := ttsNormalizeGeminiBaseURL(rawBase)
		payload := map[string]any{
			"contents": []any{map[string]any{"parts": []any{map[string]any{"text": text}}}},
			"generationConfig": map[string]any{"responseModalities": []any{"AUDIO"},
				"speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": voice}}}},
		}
		endpoint := baseURL + "/models/" + model + ":streamGenerateContent"
		cappedChunks(s.sseChunks(ctx, endpoint, apiKey, payload), "Gemini streaming TTS")(yield)
	}, nil
}
