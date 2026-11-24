package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const (
	defaultOpenAITTSModel    = "gpt-4o-mini-tts"
	defaultOpenAITTSVoice    = "alloy"
	defaultOpenAITTSBaseURL  = "https://api.openai.com/v1"
	defaultDeepInfraTTSVoice = "default"
)

// Credential and base URL of one OpenAI audio route
type openAIAudioRoute struct {
	APIKey  string
	BaseURL string
}

// OpenAI audio route from the config key then the env key, the stored tts selection named when neither is set
func resolveOpenAIAudioClientConfig(ctx context.Context, env *tools.Env, req *ttsRequest) (*openAIAudioRoute, error) {
	selected := tools.ReadSelection(env, "tts")
	direct := textOr(req.OpenAI.APIKey, tools.ResolveOpenAIAudioAPIKey(env))
	if direct != "" {
		return &openAIAudioRoute{APIKey: direct, BaseURL: textOr(req.OpenAI.BaseURL, defaultOpenAITTSBaseURL)}, nil
	}
	if selected != "" {
		return nil, &ttsValueError{msg: tools.SelectionError("tts", selected, "neither tts.openai.api_key in config nor VOICE_TOOLS_OPENAI_KEY/OPENAI_API_KEY is set")}
	}
	return nil, &ttsValueError{msg: "Neither tts.openai.api_key in config nor VOICE_TOOLS_OPENAI_KEY/OPENAI_API_KEY is set"}
}

// Whether the selected OpenAI audio route resolves without a configuration error
func hasOpenAIAudioBackend(ctx context.Context, env *tools.Env) bool {
	_, err := resolveOpenAIAudioClientConfig(ctx, env, loadTTSRequest(ctx, env))
	var ve *ttsValueError
	if errors.As(err, &ve) {
		return false
	}
	return err == nil
}

// Optional OpenAI compatible body fields, language sent as lang_code and consent_attestation
func openAIExtraBody(oai openAISpeech) map[string]any {
	extra := map[string]any{}
	if v := strings.TrimSpace(oai.Language); v != "" {
		extra["lang_code"] = v
	}
	if v := strings.TrimSpace(oai.ConsentAttestation); v != "" {
		extra["consent_attestation"] = v
	}
	return extra
}

// Explicit OpenAI speech request overrides, empty fields resolved from the auth chain and the settings
type openAITTSOptions struct {
	APIKey       *string
	BaseURL      *string
	Model        string
	Voice        string
	Speed        *float64
	Instructions string
}

// Speech through the OpenAI audio.speech.create shape
func generateOpenAITTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest, opts openAITTSOptions) (string, error) {
	fallbackBase := ""
	apiKey := ""
	if opts.APIKey == nil {
		route, err := resolveOpenAIAudioClientConfig(ctx, env, req)
		if err != nil {
			return "", err
		}
		apiKey, fallbackBase = route.APIKey, route.BaseURL
	} else {
		apiKey = *opts.APIKey
	}
	model := textOr(opts.Model, textOr(req.OpenAI.Model, defaultOpenAITTSModel))
	voice := textOr(opts.Voice, textOr(req.OpenAI.Voice, defaultOpenAITTSVoice))
	baseURL := ""
	switch {
	case opts.BaseURL != nil:
		baseURL = *opts.BaseURL
	case strings.TrimSpace(req.OpenAI.BaseURL) != "":
		baseURL = req.OpenAI.BaseURL
	default:
		baseURL = textOr(fallbackBase, defaultOpenAITTSBaseURL)
	}
	speed, _ := req.speedFor(req.OpenAI.Speed)
	if opts.Speed != nil {
		speed = *opts.Speed
	}
	body := map[string]any{"model": model, "voice": voice, "input": text, "response_format": ttsResponseFormatFromPath(outputPath)}
	if speed != 1.0 {
		body["speed"] = math.Max(0.25, math.Min(4.0, speed))
	}
	if opts.Instructions != "" {
		body["instructions"] = opts.Instructions
	}
	for k, v := range openAIExtraBody(req.OpenAI) {
		body[k] = v
	}
	client := newOpenAISpeechClient(apiKey, baseURL)
	if err := client.CreateSpeech(ctx, body, map[string]string{"x-idempotency-key": uuid4()}, outputPath); err != nil {
		return "", err
	}
	return outputPath, nil
}

// One OpenAI compatible audio.speech.create call streamed to a file
type ttsSpeechClient interface {
	CreateSpeech(ctx context.Context, body map[string]any, headers map[string]string, outputPath string) error
}

// OpenAI compatible speech client of an API key and base URL with two retries and a ten minute timeout
func newOpenAISpeechClient(apiKey, baseURL string) ttsSpeechClient {
	return &openAISpeechClient{apiKey: apiKey, baseURL: baseURL, maxRetries: 2, timeout: 600 * time.Second}
}

// Native OpenAI audio speech client with the SDK retry policy
type openAISpeechClient struct {
	apiKey     string
	baseURL    string
	maxRetries int
	timeout    time.Duration
}

// Error of a non success OpenAI API reply, worded like the SDK APIStatusError
type openAIAPIStatusError struct {
	Status int
	Body   any
}

func (e *openAIAPIStatusError) Error() string {
	return fmt.Sprintf("Error code: %d - %s", e.Status, anyutil.String(e.Body))
}

// Error of a failed connection to the OpenAI API
type openAIConnectionError struct{ err error }

func (e *openAIConnectionError) Error() string { return "Connection error." }
func (e *openAIConnectionError) Unwrap() error { return e.err }

// Error of an OpenAI request that timed out
type openAITimeoutError struct{ err error }

func (e *openAITimeoutError) Error() string { return "Request timed out." }
func (e *openAITimeoutError) Unwrap() error { return e.err }

// Posts the speech request with SDK retries and streams the audio body into the output file
func (c *openAISpeechClient) CreateSpeech(ctx context.Context, body map[string]any, headers map[string]string, outputPath string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/audio/speech"
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, endpoint, payload, headers, attempt)
		if err != nil {
			if attempt < c.maxRetries {
				ttsOpenAISleep(retryDelay(nil, attempt))
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
				return &openAITimeoutError{err: err}
			}
			return &openAIConnectionError{err: err}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			defer resp.Body.Close()
			f, err := os.Create(outputPath)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, resp.Body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return err
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
		return &openAIAPIStatusError{Status: resp.StatusCode, Body: decoded}
	}
}

// One HTTP attempt carrying the SDK headers and retry count
func (c *openAISpeechClient) send(ctx context.Context, endpoint string, payload []byte, headers map[string]string, attempt int) (*http.Response, error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("x-stainless-retry-count", strconv.Itoa(attempt))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// Response body that releases its request context when closed
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// SDK retry predicate, the x-should-retry header first then 408, 409, 429 and 5xx
func shouldRetryOpenAI(resp *http.Response) bool {
	switch resp.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	s := resp.StatusCode
	return s == 408 || s == 409 || s == 429 || s >= 500
}

// SDK retry delay honoring retry-after headers, else jittered exponential backoff capped at 8 seconds
func retryDelay(h http.Header, attempt int) time.Duration {
	if h != nil {
		if ms, err := strconv.ParseFloat(h.Get("retry-after-ms"), 64); err == nil && ms > 0 && ms <= 60000 {
			return time.Duration(ms * float64(time.Millisecond))
		}
		if ra := h.Get("retry-after"); ra != "" {
			if s, err := strconv.ParseFloat(ra, 64); err == nil && s > 0 && s <= 60 {
				return time.Duration(s * float64(time.Second))
			}
			if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 && d <= 60*time.Second {
					return d
				}
			}
		}
	}
	sleep := math.Min(0.5*math.Pow(2, float64(min(attempt, 1000))), 8.0)
	jitter := 1 - 0.25*rand.Float64()
	return time.Duration(sleep * jitter * float64(time.Second))
}

// Sleep between SDK retries, swapped by tests
var ttsOpenAISleep = time.Sleep

// DeepInfra speech through the OpenAI compatible handler with a live catalog model when none is pinned
func generateDeepInfraTTS(ctx context.Context, env *tools.Env, text, outputPath string, req *ttsRequest) (string, error) {
	apiKey := ttsResolveProviderKey(env, "DEEPINFRA_API_KEY", "deepinfra")
	if apiKey == "" {
		return "", &ttsValueError{msg: "DEEPINFRA_API_KEY not set. Start `icebath` and type `/setup voice` to configure, or set the env var directly."}
	}
	model := strings.TrimSpace(req.DeepInfra.Model)
	if model == "" {
		candidates := ttsDeepInfraModelIDs(ctx, env, "tts")
		if len(candidates) == 0 {
			return "", &ttsValueError{msg: "No DeepInfra TTS model available. Pin one in config.yaml under tts.deepinfra.model, or check connectivity to api.deepinfra.com so the live catalog can be fetched."}
		}
		model = candidates[0]
	}
	speed, _ := req.speedFor(req.DeepInfra.Speed)
	baseURL := deepInfraBaseURL(env, req.DeepInfra.BaseURL)
	return ttsGenerateOpenAI(ctx, env, text, outputPath, req, openAITTSOptions{
		APIKey: &apiKey, BaseURL: &baseURL, Model: model, Voice: textOr(req.DeepInfra.Voice, defaultDeepInfraTTSVoice), Speed: &speed,
	})
}

// DeepInfra model ids by tag, the catalog seam tests replace
var ttsDeepInfraModelIDs = deepInfraModelIDs

// OpenAI speech generator, the seam the DeepInfra handler and tests route through
var ttsGenerateOpenAI = generateOpenAITTS
