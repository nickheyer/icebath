package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Per sentence PCM byte ceiling matching the 16 MiB bounded body of the sync providers
var streamSentenceByteCap = 16 * 1024 * 1024

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
	New       func(env *tools.Env, req *ttsRequest) (StreamingTTSProvider, error)
}

// Tool env, speech settings and sample rate every built in streamer carries
type streamerBase struct {
	env        *tools.Env
	req        *ttsRequest
	sampleRate atomic.Int64
}

func (b *streamerBase) init(env *tools.Env, req *ttsRequest, rate int) {
	b.env, b.req = env, req
	b.sampleRate.Store(int64(rate))
}

func (b *streamerBase) SampleRate() int  { return int(b.sampleRate.Load()) }
func (b *streamerBase) Channels() int    { return 1 }
func (b *streamerBase) SampleWidth() int { return 2 }

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

// Builds the registered streamer of a name when its credentials are usable, nil otherwise
var tryInstantiate = func(ctx context.Context, env *tools.Env, name string, req *ttsRequest) StreamingTTSProvider {
	streamingRegistryMu.RLock()
	class, ok := streamingRegistry[name]
	streamingRegistryMu.RUnlock()
	if !ok || !class.Available(ctx, env) {
		return nil
	}
	inst, err := class.New(env, req)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("streaming provider %s init failed: %s", name, err))
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
		opts := pluginVoiceKwargs(s.req)
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
func pluginStreamer(ctx context.Context, env *tools.Env, name string, req *ttsRequest) StreamingTTSProvider {
	p, rate, ok := pluginPCMStreamingProvider(ctx, name, req)
	if !ok {
		return nil
	}
	s := &pluginPCMStreamer{provider: p}
	s.init(env, req, rate)
	return s
}

// Returns a ready chunked streamer for the configured provider, nil meaning per sentence synthesis
func ResolveStreamingProvider(ctx context.Context, env *tools.Env, preferred string) StreamingTTSProvider {
	return resolveStreamingProvider(ctx, env, loadTTSRequest(ctx, env), preferred)
}

// Streamer of tts.streaming.provider, auto trying every streaming service, else the preferred or configured one
func resolveStreamingProvider(ctx context.Context, env *tools.Env, req *ttsRequest, preferred string) StreamingTTSProvider {
	if req.StreamingProvider == "auto" {
		for _, name := range streamingProviderPriority {
			if inst := tryInstantiate(ctx, env, name, req); inst != nil {
				return inst
			}
		}
		return pluginStreamer(ctx, env, req.Provider, req)
	}
	name := req.StreamingProvider
	if name == "" {
		name = strings.TrimSpace(strings.ToLower(textOr(preferred, req.Provider)))
	}
	if inst := tryInstantiate(ctx, env, name, req); inst != nil {
		return inst
	}
	return pluginStreamer(ctx, env, name, req)
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
				pkgLog().Warn(fmt.Sprintf("%s exceeded %d bytes for one sentence; truncating", label, streamSentenceByteCap))
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

func newElevenLabsStreamer(env *tools.Env, req *ttsRequest) (StreamingTTSProvider, error) {
	s := &elevenLabsStreamer{}
	s.init(env, req, 24000)
	return s, nil
}

func elevenLabsStreamerAvailable(ctx context.Context, env *tools.Env) bool {
	return streamResolveKey(ctx, env, "ELEVENLABS_API_KEY", "elevenlabs") != ""
}

func (s *elevenLabsStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		apiKey := streamResolveKey(ctx, s.env, "ELEVENLABS_API_KEY", "elevenlabs")
		el := s.req.ElevenLabs
		voiceID := textOr(el.VoiceID, defaultElevenLabsVoiceID)
		modelID := textOr(el.StreamingModelID, textOr(el.ModelID, defaultElevenLabsModelID))
		endpoint := elevenLabsBase(s.req) + "/v1/text-to-speech/" + url.PathEscape(voiceID) + "?output_format=pcm_24000"
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
			yield(nil, &ttsRuntimeError{msg: fmt.Sprintf("status_code: %d, body: %s", resp.StatusCode, anyutil.String(body))})
			return
		}
		bodyChunks(resp.Body, 1024, true)(yield)
	}, nil
}

// The tts.openai.api_key setting, empty when unset
var streamOpenAIConfigAPIKey = func(ctx context.Context, env *tools.Env) string {
	return strings.TrimSpace(loadTTSRequest(ctx, env).OpenAI.APIKey)
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
	n, ok := pyint.Parse(strings.TrimSpace(raw))
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

// OpenAI client for chunked speech, an empty base URL meaning the OpenAI endpoint; the caller resolves the profile's OPENAI_BASE_URL
var newOpenAIStreamClient = func(apiKey, baseURL string) openAIStreamClient {
	if baseURL == "" {
		baseURL = defaultOpenAITTSBaseURL
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

func newOpenAIStreamer(env *tools.Env, req *ttsRequest) (StreamingTTSProvider, error) {
	s := &openAIStreamer{}
	s.init(env, req, 24000)
	if rate := req.OpenAI.PCMSampleRate; rate > 0 {
		s.sampleRate.Store(int64(rate))
		return s, nil
	}
	pkgLog().Warn(fmt.Sprintf("Invalid tts.openai.pcm_sample_rate %d; using %d Hz", req.OpenAI.PCMSampleRate, s.SampleRate()))
	return s, nil
}

func openAIStreamerAvailable(ctx context.Context, env *tools.Env) bool {
	return streamOpenAIConfigAPIKey(ctx, env) != "" || streamResolveOpenAIAudioAPIKey(env) != ""
}

func (s *openAIStreamer) Stream(ctx context.Context, text string) (iter.Seq2[[]byte, error], error) {
	return func(yield func([]byte, error) bool) {
		oai := s.req.OpenAI
		apiKey := strings.TrimSpace(oai.APIKey)
		if apiKey == "" {
			apiKey = streamResolveOpenAIAudioAPIKey(s.env)
		}
		baseURL := strings.TrimSpace(oai.BaseURL)
		if baseURL == "" {
			baseURL = ttsEnvValue(ctx, s.env, "OPENAI_BASE_URL")
		}
		client := newOpenAIStreamClient(apiKey, baseURL)
		extra := openAIExtraBody(oai)
		if len(extra) == 0 {
			extra = nil
		}
		body := map[string]any{
			"model": textOr(oai.Model, defaultOpenAITTSModel), "voice": textOr(oai.Voice, defaultOpenAITTSVoice),
			"input": text, "response_format": "pcm",
		}
		response, err := client.StreamSpeech(ctx, body, extra)
		if err != nil {
			yield(nil, err)
			return
		}
		defer response.Close()
		if rate, ok := sampleRateFromHeaders(response.Headers()); ok && rate != s.SampleRate() {
			pkgLog().Info(fmt.Sprintf("TTS endpoint reports %d Hz PCM (expected %d Hz); honoring it", rate, s.SampleRate()))
			s.sampleRate.Store(int64(rate))
		}
		cappedChunks(response.IterBytes(), "OpenAI streaming TTS")(yield)
	}, nil
}

// Gemini streamGenerateContent server sent events carrying base64 PCM at 24 kHz
type geminiStreamer struct{ streamerBase }

func newGeminiStreamer(env *tools.Env, req *ttsRequest) (StreamingTTSProvider, error) {
	s := &geminiStreamer{}
	s.init(env, req, 24000)
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
	mime := strings.TrimSpace(tokens[0])
	charset, charsetSet, charsetFlag := "", false, false
	for _, param := range tokens[1:] {
		param = strings.TrimSpace(param)
		if param == "" {
			continue
		}
		key, value, hasValue := strings.Cut(param, "=")
		if hasValue {
			key = strings.Trim(key, "\"' ")
		}
		if strings.ToLower(key) == "charset" {
			charset, charsetSet, charsetFlag = strings.Trim(value, "\"' "), true, !hasValue
		}
	}
	if charsetFlag {
		return "", &ttsAttributeError{msg: "Content-Type charset parameter has no value"}
	}
	if charsetSet {
		return strings.Trim(charset, "'\""), nil
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
			if text, err := charsetutil.Decode(pending[:len(pending)-k], encoding); err == nil {
				return text, bytes.Clone(pending[len(pending)-k:]), nil
			}
		}
	}
	text, err := charsetutil.DecodeReplace(pending, encoding)
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
			lines := textutil.Lines(text)
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
		bytesLineError := &ttsTypeError{msg: "the response names no text encoding, so its lines cannot be read as text"}
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
	obj, ok := event.(*jsonutil.Object)
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
	cand, ok := first.(*jsonutil.Object)
	if !ok {
		return nil, false
	}
	content, ok := cand.Get("content")
	if !ok {
		return nil, false
	}
	contentObj, ok := content.(*jsonutil.Object)
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
	case *jsonutil.Object:
		out := make([]any, 0, t.Len())
		for _, k := range t.Keys() {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &ttsTypeError{msg: fmt.Sprintf("TTS stream value is %s where a list was expected", errchain.Kind(v))}
}

// Python dict.get of a decoded JSON value, a non mapping failing like the attribute lookup
func pyJSONGet(v any, key string) (any, error) {
	obj, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, &ttsAttributeError{msg: fmt.Sprintf("TTS stream event is %s where a mapping holding %s was expected", errchain.Kind(v), key)}
	}
	out, _ := obj.Get(key)
	return out, nil
}

// base64.b64decode of a decoded JSON value, a non string failing with the Python type error
func pyB64DecodeValue(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, &ttsTypeError{msg: fmt.Sprintf("audio payload is %s where base64 text was expected", errchain.Kind(v))}
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
			event, err := jsonutil.Decode(data)
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
					obj, isObj := inline.(*jsonutil.Object)
					if !isObj {
						yield(nil, &ttsAttributeError{msg: fmt.Sprintf("Gemini TTS inline data is %s where a mapping was expected", errchain.Kind(inline))})
						return
					}
					b64 = obj.ValueOr("data", "")
				}
				if !pyTruthy(b64) {
					continue
				}
				pcm, err := pyB64DecodeValue(b64)
				if err != nil {
					pkgLog().Warn(fmt.Sprintf("Gemini SSE: bad base64 audio: %s", err))
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
		gc := s.req.Gemini
		model := textOr(gc.Model, defaultGeminiTTSModel)
		voice := textOr(gc.Voice, defaultGeminiTTSVoice)
		baseURL := ttsNormalizeGeminiBaseURL(geminiRawBase(ctx, s.env, gc.BaseURL))
		payload := map[string]any{
			"contents": []any{map[string]any{"parts": []any{map[string]any{"text": text}}}},
			"generationConfig": map[string]any{"responseModalities": []any{"AUDIO"},
				"speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": voice}}}},
		}
		endpoint := baseURL + "/models/" + model + ":streamGenerateContent"
		cappedChunks(s.sseChunks(ctx, endpoint, apiKey, payload), "Gemini streaming TTS")(yield)
	}, nil
}
