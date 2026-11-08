package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const defaultMistralSTTServerURL = "https://api.mistral.ai"

var asrTextRe = regexp2.MustCompile(`^\s*language\s+[\w.-]+(?:\s*<audio_language>[^<]*</audio_language>)?\s*<asr_text>\s*(?<text>.*)`,
	regexp2.IgnoreCase|regexp2.Singleline|regexp2.Unicode)

// Language and prompt hints a backend receives, empty meaning unset
type sttHints struct {
	Language string
	Prompt   string
}

// One built in backend call returning the envelope or an escaped failure
type sttHandler func(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error)

// xAI credentials preferring nothing, the seam tests replace
var sttResolveXAICredentials = func(ctx context.Context) (string, string, string) {
	return tools.ResolveXAIHTTPCredentials(nil, false)
}

// Force refreshed xAI OAuth credentials for an API key hint, the seam tests replace
var sttRefreshXAICredentials = func(ctx context.Context, apiKeyHint string) (string, string, string, error) {
	p, k, b := tools.ResolveXAIHTTPCredentialsRefreshed(nil, apiKeyHint)
	return p, k, b, nil
}

// Whether xAI credentials resolve
func hasXAISTTCredentials(ctx context.Context) bool {
	_, key, _ := sttResolveXAICredentials(ctx)
	return key != ""
}

// Profile env or dotenv value, empty when unset
func sttEnvValue(ctx context.Context, key string) string {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	if err != nil {
		return ""
	}
	return v
}

// Opened audio upload failed, kept so PermissionError maps to its own envelope
type sttOpenError struct {
	path string
	err  error
}

func (e *sttOpenError) Error() string { return pypath.OSErrorText(e.err) }
func (e *sttOpenError) Unwrap() error { return e.err }

// Shared mapping of a REST or SDK provider failure to the envelope
func sttCloudFailure(err error, filePath, label, detail string) map[string]any {
	if errors.Is(err, fs.ErrPermission) {
		return sttError("Permission denied: " + filePath)
	}
	var re *sttResponseError
	if errors.As(err, &re) {
		return sttError(re.msg)
	}
	sttLog().Error(fmt.Sprintf("%s failed: %v", label, err))
	if detail == "" {
		detail = err.Error()
	}
	return sttError(label + " failed: " + detail)
}

// One multipart form field or file part
type sttFormPart struct {
	Name, Value       string
	File, ContentType string
}

// Multipart body of the parts with its content type
func sttMultipart(parts []sttFormPart) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.File == "" {
			if err := w.WriteField(p.Name, p.Value); err != nil {
				return nil, "", err
			}
			continue
		}
		data, err := os.ReadFile(p.File)
		if err != nil {
			return nil, "", &sttOpenError{path: p.File, err: err}
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, p.Name, filepath.Base(p.File)))
		if p.ContentType != "" {
			h.Set("Content-Type", p.ContentType)
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		pw.Write(data)
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// Settings of one OpenAI compatible transcription client
type openAITranscriptionClient struct {
	APIKey, BaseURL string
	Timeout         float64
	MaxRetries      int
}

// Sleep between SDK retries, swapped by tests
var sttOpenAISleep = time.Sleep

// audio.transcriptions.create with SDK retries, a text body for response_format text, else the decoded JSON
func (c openAITranscriptionClient) Create(ctx context.Context, filePath string, fields [][2]string) (any, error) {
	ctype, _ := pymimetypes.GuessFileType(filePath, true)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	parts := []sttFormPart{}
	for _, f := range fields {
		parts = append(parts, sttFormPart{Name: f[0], Value: f[1]})
	}
	parts = append(parts, sttFormPart{Name: "file", File: filePath, ContentType: ctype})
	body, contentType, err := sttMultipart(parts)
	if err != nil {
		return nil, err
	}
	textFormat := false
	for _, f := range fields {
		if f[0] == "response_format" && f[1] == "text" {
			textFormat = true
		}
	}
	base := c.BaseURL
	if base == "" {
		if base = os.Getenv("OPENAI_BASE_URL"); base == "" {
			base = "https://api.openai.com/v1"
		}
	}
	endpoint := strings.TrimRight(base, "/") + "/audio/transcriptions"
	for attempt := 0; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout*float64(time.Second)))
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("x-stainless-retry-count", strconv.Itoa(attempt))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			if attempt < c.MaxRetries {
				sttOpenAISleep(retryDelay(nil, attempt))
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
				return nil, &openAITimeoutError{err: err}
			}
			return nil, &openAIConnectionError{err: err}
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return nil, &openAIConnectionError{err: rerr}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if textFormat {
				return string(raw), nil
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return string(raw), nil
			}
			return decoded, nil
		}
		if attempt < c.MaxRetries && shouldRetryOpenAI(resp) {
			sttOpenAISleep(retryDelay(resp.Header, attempt))
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

// One audio.transcriptions.create capable client
type sttTranscriptionClient interface {
	Create(ctx context.Context, filePath string, fields [][2]string) (any, error)
}

// OpenAI client constructor, the seam tests replace with a recording double
var sttNewOpenAIClient = func(apiKey, baseURL string, timeout float64, maxRetries int) sttTranscriptionClient {
	return openAITranscriptionClient{APIKey: apiKey, BaseURL: baseURL, Timeout: timeout, MaxRetries: maxRetries}
}

// Runs a body on a fresh OpenAI client shaped by stt.openai timeout and retries, mapping failures to envelopes
func withOpenAIClient(ctx context.Context, apiKey, baseURL, filePath, logLabel string, body func(sttTranscriptionClient) (map[string]any, error)) map[string]any {
	openaiConfig := getProviderSection(loadSTTConfig(ctx), "openai")
	client := sttNewOpenAIClient(apiKey, baseURL, configNumberFloat(openaiConfig, "timeout", sttDefaultTimeout), configNumberInt(openaiConfig, "max_retries", 1))
	result, err := body(client)
	if err == nil {
		return result
	}
	if errors.Is(err, fs.ErrPermission) {
		return sttError("Permission denied: " + filePath)
	}
	var re *sttResponseError
	var ce *openAIConnectionError
	var te *openAITimeoutError
	var se *openAIAPIStatusError
	switch {
	case errors.As(err, &re):
		return sttError(re.msg)
	case errors.As(err, &ce), errors.As(err, &te):
		return sttError("Connection error: " + err.Error())
	case errors.As(err, &se):
		return sttError("API error: " + err.Error())
	}
	sttLog().Error(fmt.Sprintf("%s transcription failed: %v", logLabel, err))
	return sttError(fmt.Sprintf("Transcription failed: %v", err))
}

// Language and prompt create fields, each only when set
func sdkPromptFields(language, prompt string) [][2]string {
	out := [][2]string{}
	if language != "" {
		out = append(out, [2]string{"language", language})
	}
	if prompt != "" {
		out = append(out, [2]string{"prompt", prompt})
	}
	return out
}

// Groq Whisper transcription with the hook, config, env and auto language ladder
func transcribeGroq(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	apiKey := resolveSTTProviderKey(ctx, "GROQ_API_KEY", "groq")
	if apiKey == "" {
		return sttError("GROQ_API_KEY not set"), nil
	}
	if !hasOpenAI() {
		return sttError("openai package not installed"), nil
	}
	if sttOpenAIModels[modelName] || sttRetiredGroqModels[modelName] {
		sttLog().Info(fmt.Sprintf("Model %s not available on Groq, using %s", modelName, sttDefaultGroqModel))
		modelName = sttDefaultGroqModel
	}
	language := h.Language
	if language == "" {
		language = resolveSTTLanguage(ctx, "groq", nil)
	}
	return withOpenAIClient(ctx, apiKey, groqSTTBaseURL, filePath, "Groq", func(c sttTranscriptionClient) (map[string]any, error) {
		fields := append([][2]string{{"model", modelName}, {"response_format", "text"}}, sdkPromptFields(language, h.Prompt)...)
		transcription, err := c.Create(ctx, filePath, fields)
		if err != nil {
			return nil, err
		}
		text, err := extractTranscriptText(transcription)
		if err != nil {
			return nil, err
		}
		lang := language
		if lang == "" {
			lang = "auto"
		}
		sttLog().Info(fmt.Sprintf("Transcribed %s via Groq API (%s, lang=%s, %d chars)", filepath.Base(filePath), modelName, lang, utf8.RuneCountInString(text)))
		return sttOK(text, "groq"), nil
	}), nil
}

// Explicit credentials and label of an OpenAI compatible endpoint, nil key meaning the OpenAI auth chain
type openAIEndpoint struct {
	APIKey   *string
	BaseURL  string
	Label    string
	Language string
	Prompt   string
}

// OpenAI transcription through the shared SDK shape
func transcribeOpenAIHandler(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	return transcribeOpenAI(ctx, filePath, modelName, openAIEndpoint{Label: "openai", Language: h.Language, Prompt: h.Prompt})
}

// audio.transcriptions.create against any OpenAI compatible endpoint with one transcode retry on container rejection
func transcribeOpenAI(ctx context.Context, filePath, modelName string, e openAIEndpoint) (map[string]any, error) {
	label := e.Label
	if label == "" {
		label = "openai"
	}
	var apiKey, baseURL string
	if e.APIKey == nil {
		key, fallbackBase, err := resolveSTTOpenAIAudioClientConfig(ctx)
		if err != nil {
			var ve *sttValueError
			if errors.As(err, &ve) {
				return sttError(ve.msg), nil
			}
			return nil, err
		}
		apiKey, baseURL = key, e.BaseURL
		if baseURL == "" {
			baseURL = fallbackBase
		}
	} else {
		apiKey, baseURL = *e.APIKey, e.BaseURL
	}
	language := e.Language
	if language == "" {
		language = resolveSTTLanguage(ctx, label, nil)
	}
	if !hasOpenAI() {
		return sttError("openai package not installed"), nil
	}
	if label == "openai" && (sttGroqModels[modelName] || sttRetiredGroqModels[modelName]) {
		sttLog().Info(fmt.Sprintf("Model %s not available on OpenAI, using %s", modelName, sttDefaultOpenAIModel))
		modelName = sttDefaultOpenAIModel
	}
	return withOpenAIClient(ctx, apiKey, baseURL, filePath, label, func(c sttTranscriptionClient) (map[string]any, error) {
		create := func(path string) (any, error) {
			format := "json"
			if modelName == "whisper-1" {
				format = "text"
			}
			fields := [][2]string{{"model", modelName}, {"response_format", format}}
			if language != "" {
				if modelName == "gpt-transcribe" {
					fields = append(fields, [2]string{"languages[]", language})
				} else {
					fields = append(fields, [2]string{"language", language})
				}
				sttLog().Debug(fmt.Sprintf("Using language hint '%s' for OpenAI STT", language))
			}
			if e.Prompt != "" {
				fields = append(fields, [2]string{"prompt", e.Prompt})
			}
			return c.Create(ctx, path, fields)
		}
		workDir, err := os.MkdirTemp("", "icebath-stt-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(workDir)
		transcription, err := create(filePath)
		if err != nil {
			var se *openAIAPIStatusError
			if !errors.As(err, &se) {
				return nil, err
			}
			serverError := se.Status >= 500
			lower := strings.ToLower(se.Error())
			if !serverError && !strings.Contains(lower, "unsupported") && !strings.Contains(lower, "corrupted") && !strings.Contains(lower, "invalid file") {
				return nil, err
			}
			converted, transcodeError := sttTranscode(filePath, workDir)
			if transcodeError != "" {
				if serverError {
					return nil, err
				}
				return sttError(transcodeError), nil
			}
			sttLog().Info(fmt.Sprintf("Retrying %s STT after transcoding %s to m4a (API rejected the original container)", label, filepath.Base(filePath)))
			if transcription, err = create(converted); err != nil {
				return nil, err
			}
		}
		text, err := extractTranscriptText(transcription)
		if err != nil {
			return nil, err
		}
		sttLog().Info(fmt.Sprintf("Transcribed %s via %s (%s, %d chars)", filepath.Base(filePath), label, modelName, utf8.RuneCountInString(text)))
		return sttOK(text, label), nil
	}), nil
}

// Mistral audio transcriptions complete call returning the decoded body or the SDK error class name, the seam tests replace
var sttMistralTranscribe = func(ctx context.Context, apiKey, filePath string, fields [][2]string) (any, string, error) {
	parts := []sttFormPart{}
	for _, f := range fields {
		parts = append(parts, sttFormPart{Name: f[0], Value: f[1]})
	}
	ctype, _ := pymimetypes.GuessFileType(filePath, true)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	parts = append(parts, sttFormPart{Name: "file", File: filePath, ContentType: ctype})
	body, contentType, err := sttMultipart(parts)
	if err != nil {
		var oe *sttOpenError
		if errors.As(err, &oe) && errors.Is(err, fs.ErrPermission) {
			return nil, "PermissionError", err
		}
		return nil, pyrepr.ExceptionName(err), err
	}
	rctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, defaultMistralSTTServerURL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return nil, "SDKError", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "ConnectError", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "ReadError", err
	}
	if resp.StatusCode == 422 {
		return nil, "HTTPValidationError", fmt.Errorf("API error occurred: Status %d. Body: %s", resp.StatusCode, raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "SDKError", fmt.Errorf("API error occurred: Status %d. Body: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "ResponseValidationError", err
	}
	return out, "", nil
}

// Mistral Voxtral transcription with the hook, config, env and auto language ladder
func transcribeMistral(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	apiKey := resolveSTTProviderKey(ctx, "MISTRAL_API_KEY", "mistral")
	if apiKey == "" {
		return sttError("MISTRAL_API_KEY not set"), nil
	}
	lazyEnsureQuietly(ctx, "mistral")
	if !safeFindSpec("mistralai") {
		err := &sttPythonError{msg: "No module named 'mistralai'"}
		return sttCloudFailure(err, filePath, "Mistral transcription", "ModuleNotFoundError"), nil
	}
	language := h.Language
	if language == "" {
		language = resolveSTTLanguage(ctx, "mistral", nil)
	}
	fields := append([][2]string{{"model", modelName}}, sdkPromptFields(language, h.Prompt)...)
	result, className, err := sttMistralTranscribe(ctx, apiKey, filePath, fields)
	if err == nil {
		var text string
		text, err = extractTranscriptText(result)
		if err == nil {
			sttLog().Info(fmt.Sprintf("Transcribed %s via Mistral API (%s, %d chars)", filepath.Base(filePath), modelName, utf8.RuneCountInString(text)))
			return sttOK(text, "mistral"), nil
		}
		className = pyrepr.ExceptionName(err)
		var re *sttResponseError
		if errors.As(err, &re) {
			className = "STTResponseError"
		}
	}
	return sttCloudFailure(err, filePath, "Mistral transcription", className), nil
}

// Multipart POST of an audio file with form data under the requests 120 second timeout, the seam tests replace
var postAudioMultipart = func(ctx context.Context, rawURL string, headers map[string]string, filePath string, data [][2]string) (*pyrequests.Response, error) {
	parts := []sttFormPart{}
	for _, d := range data {
		parts = append(parts, sttFormPart{Name: d[0], Value: d[1]})
	}
	parts = append(parts, sttFormPart{Name: "file", File: filePath})
	body, contentType, err := sttMultipart(parts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return pyrequests.Send(ctx, http.DefaultTransport.(*http.Transport), pyrequests.Seconds(120), req)
}

// First n code points of a response body
func sttBodyPrefix(body []byte, n int) string {
	s := strings.ToValidUTF8(string(body), "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Shared multipart REST flow mapping non 200 replies, empty text and failures to envelopes
func restProvider(filePath, providerName, label string, post func() (*pyrequests.Response, error),
	extractDetail func(map[string]any) string, extractText func(map[string]any) (string, error), log func(string, map[string]any)) map[string]any {
	run := func() (map[string]any, error) {
		resp, err := post()
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			detail := sttBodyPrefix(resp.Body, 300)
			var parsed map[string]any
			if json.Unmarshal(resp.Body, &parsed) == nil {
				if d := sttSafeDetail(extractDetail, parsed); d != "" {
					detail = d
				}
			}
			return sttError(fmt.Sprintf("%s API error (HTTP %d): %s", label, resp.Status, detail)), nil
		}
		var decoded any
		if err := json.Unmarshal(resp.Body, &decoded); err != nil {
			return nil, &sttValueError{msg: err.Error()}
		}
		body, ok := decoded.(map[string]any)
		if !ok {
			return nil, &sttValueError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		text, err := extractText(body)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return sttError(label+" returned empty transcript", "no_speech", true), nil
		}
		log(text, body)
		return sttOK(text, providerName), nil
	}
	result, err := run()
	if err != nil {
		return sttCloudFailure(err, filePath, label+" transcription", "")
	}
	return result
}

// Detail extractor call that reads a panic as no detail like the Python except branch
func sttSafeDetail(extract func(map[string]any) string, body map[string]any) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	return extract(body)
}

// xAI Grok STT multipart transcription with one OAuth refresh retry on an auth rejection
func transcribeXAI(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	if h.Prompt != "" {
		logPromptUnsupported("STT provider 'xai'")
	}
	directKey := pystr.Strip(sttEnvValue(ctx, "XAI_API_KEY"))
	var credProvider, apiKey, credBase string
	if directKey != "" {
		base := sttEnvValue(ctx, "XAI_BASE_URL")
		if base == "" {
			base = "https://api.x.ai/v1"
		}
		credProvider, apiKey, credBase = "xai", directKey, strings.TrimRight(pystr.Strip(base), "/")
	} else {
		credProvider, apiKey, credBase = sttResolveXAICredentials(ctx)
	}
	apiKey = pystr.Strip(apiKey)
	if apiKey == "" {
		return sttError("No xAI credentials found. Configure xAI OAuth in `icebath model` or set XAI_API_KEY"), nil
	}
	sttConfig := loadSTTConfig(ctx)
	xaiConfig, err := orEmptyDict(sttConfig, "xai")
	if err != nil {
		return nil, err
	}
	resolveBase := func(provider, base string) string {
		u := base
		if provider != "xai-oauth" {
			if v := xaiConfig["base_url"]; pyTruthy(v) {
				u = pyrepr.Str(v)
			} else if v := sttEnvValue(ctx, "XAI_STT_BASE_URL"); v != "" {
				u = v
			}
		}
		if u == "" {
			u = xaiSTTBaseURL
		}
		return strings.TrimRight(pystr.Strip(u), "/")
	}
	language := h.Language
	if language == "" {
		language = resolveSTTLanguage(ctx, "xai", sttConfig)
	}
	post := func() (*pyrequests.Response, error) {
		data := [][2]string{{"model", normalizeXAISTTModel(modelName)}}
		if language != "" {
			data = append(data, [2]string{"language", language})
		}
		type flag struct {
			name string
			def  bool
		}
		flags := []flag{}
		if language != "" {
			flags = append(flags, flag{"format", true})
		}
		flags = append(flags, flag{"diarize", false})
		for _, f := range flags {
			if isTruthyValue(dictGetDefault(xaiConfig, f.name, f.def), false) {
				data = append(data, [2]string{f.name, "true"})
			}
		}
		send := func(bearer, base string) (*pyrequests.Response, error) {
			return postAudioMultipart(ctx, base+"/stt", map[string]string{"Authorization": "Bearer " + bearer, "User-Agent": tools.IcebathXAIUserAgent()}, filePath, data)
		}
		resp, err := send(apiKey, resolveBase(credProvider, credBase))
		if err != nil {
			return nil, err
		}
		if (resp.Status == 401 || resp.Status == 403) && credProvider == "xai-oauth" {
			sttLog().Info(fmt.Sprintf("xAI STT got HTTP %d; refreshing OAuth credentials and retrying once", resp.Status))
			rp, rk, rb, rerr := sttRefreshXAICredentials(ctx, apiKey)
			if rerr == nil {
				rk = pystr.Strip(rk)
				if rk != "" && rk != apiKey {
					var retried *pyrequests.Response
					retried, rerr = send(rk, resolveBase(rp, rb))
					if rerr == nil {
						resp = retried
					}
				}
			}
			if rerr != nil {
				sttLog().Warn(fmt.Sprintf("xAI STT OAuth refresh-and-retry after HTTP %d failed: %v", resp.Status, rerr))
			}
		}
		return resp, nil
	}
	logFn := func(text string, body map[string]any) {
		lang := any(language)
		if v, ok := body["language"]; ok {
			lang = v
		}
		duration := any(0)
		if v, ok := body["duration"]; ok {
			duration = v
		}
		d, _ := sttCastFloat(duration)
		sttLog().Info(fmt.Sprintf("Transcribed %s via xAI Grok STT (lang=%s, %.1fs audio, %d chars)", filepath.Base(filePath), pyrepr.Str(lang), d, utf8.RuneCountInString(text)))
	}
	detail := func(body map[string]any) string {
		errVal, ok := body["error"]
		if !ok {
			return ""
		}
		m, ok := errVal.(map[string]any)
		if !ok {
			panic("AttributeError")
		}
		if msg, ok := m["message"]; ok {
			return pyrepr.Str(msg)
		}
		return ""
	}
	text := func(body map[string]any) (string, error) {
		v, ok := body["text"]
		if !ok {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", &sttValueError{msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))}
		}
		return pystr.Strip(s), nil
	}
	return restProvider(filePath, "xai", "xAI STT", post, detail, text, logFn), nil
}

// ElevenLabs error detail from a detail or error field, the message of a mapping preferred
func elevenLabsErrorDetail(body map[string]any) string {
	value := body["detail"]
	if !pyTruthy(value) {
		value = body["error"]
	}
	if m, ok := value.(map[string]any); ok {
		if msg := m["message"]; pyTruthy(msg) {
			return pyrepr.Str(msg)
		}
		return pyrepr.Str(m)
	}
	if pyTruthy(value) {
		return pyrepr.Str(value)
	}
	return ""
}

// ElevenLabs Scribe transcription
func transcribeElevenLabs(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	if h.Prompt != "" {
		logPromptUnsupported("STT provider 'elevenlabs'")
	}
	apiKey := resolveSTTProviderKey(ctx, "ELEVENLABS_API_KEY", "elevenlabs")
	if apiKey == "" {
		return sttError("ELEVENLABS_API_KEY not set"), nil
	}
	sttConfig := loadSTTConfig(ctx)
	elCfg, err := orEmptyDict(sttConfig, "elevenlabs")
	if err != nil {
		return nil, err
	}
	base := elevenLabsSTTBaseURL
	if v := elCfg["base_url"]; pyTruthy(v) {
		base = pyrepr.Str(v)
	} else if v := sttEnvValue(ctx, "ELEVENLABS_STT_BASE_URL"); v != "" {
		base = v
	}
	base = strings.TrimRight(pystr.Strip(base), "/")
	languageCode := h.Language
	if languageCode == "" {
		languageCode = resolveSTTLanguage(ctx, "elevenlabs", sttConfig, "language_code")
	}
	post := func() (*pyrequests.Response, error) {
		data := [][2]string{{"model_id", modelName},
			{"tag_audio_events", strconv.FormatBool(isTruthyValue(dictGetDefault(elCfg, "tag_audio_events", false), false))},
			{"diarize", strconv.FormatBool(isTruthyValue(dictGetDefault(elCfg, "diarize", false), false))}}
		if languageCode != "" {
			data = append(data, [2]string{"language_code", languageCode})
		}
		return postAudioMultipart(ctx, base+"/speech-to-text", map[string]string{"xi-api-key": apiKey}, filePath, data)
	}
	logFn := func(text string, _ map[string]any) {
		sttLog().Info(fmt.Sprintf("Transcribed %s via ElevenLabs Scribe (%s, %d chars)", filepath.Base(filePath), modelName, utf8.RuneCountInString(text)))
	}
	return restProvider(filePath, "elevenlabs", "ElevenLabs STT", post, elevenLabsErrorDetail,
		func(body map[string]any) (string, error) { return extractTranscriptText(body) }, logFn), nil
}

// DeepInfra live catalog of STT model ids, the seam tests replace
var sttDeepInfraModelIDs = func(ctx context.Context) []string { return provider.DeepinfraModelIDs(ctx, "stt", false) }

// DeepInfra transcription through the OpenAI compatible handler with a live catalog model when none is pinned
func transcribeDeepInfra(ctx context.Context, filePath, modelName string, h sttHints) (map[string]any, error) {
	apiKey := resolveSTTProviderKey(ctx, "DEEPINFRA_API_KEY", "deepinfra")
	if apiKey == "" {
		return sttError("DEEPINFRA_API_KEY not set"), nil
	}
	base := provider.DeepinfraBaseURL(ctx, getProviderSection(loadSTTConfig(ctx), "deepinfra"))
	if modelName == "" {
		if ids := sttDeepInfraModelIDs(ctx); len(ids) > 0 {
			modelName = ids[0]
		}
	}
	if modelName == "" {
		return sttError("No DeepInfra STT model available. Pin one in config.yaml under stt.deepinfra.model, " +
			"or check connectivity to api.deepinfra.com so the live catalog can be fetched."), nil
	}
	return transcribeOpenAI(ctx, filePath, modelName, openAIEndpoint{APIKey: &apiKey, BaseURL: base, Label: "deepinfra", Language: h.Language, Prompt: h.Prompt})
}

// Whether a URL host is loopback, private or LAN internal where an empty api key is acceptable
func sttIsLocalOrPrivateURL(rawURL string) bool {
	u, err := pyurl.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".internal") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" && !addr.Is6() {
		return false
	}
	return pyipaddress.IsPrivate(addr) || addr.Unmap().IsLoopback()
}

// Config key, keyless local base URL with a placeholder key, then env key, false when none apply
func directOpenAICredentials(ctx context.Context, cfgAPIKey, cfgBaseURL any) (string, string, bool) {
	if pyTruthy(cfgAPIKey) {
		base := openAISTTBaseURL
		if pyTruthy(cfgBaseURL) {
			base = pyrepr.Str(cfgBaseURL)
		}
		return pyrepr.Str(cfgAPIKey), base, true
	}
	if pyTruthy(cfgBaseURL) && sttIsLocalOrPrivateURL(pyrepr.Str(cfgBaseURL)) {
		return "not-needed", pyrepr.Str(cfgBaseURL), true
	}
	if key := sttResolveOpenAIAudioAPIKey(ctx); key != "" {
		return key, openAISTTBaseURL, true
	}
	return "", "", false
}

// Voice tools then OpenAI key, the seam tests replace
var sttResolveOpenAIAudioAPIKey = func(ctx context.Context) string { return tools.ResolveOpenAIAudioAPIKey(nil) }

// Stored stt selection, empty when never configured, the seam tests replace
var sttReadSelection = func(ctx context.Context) string { return tools.ReadSelection(nil, "stt") }

// Managed Nous tools entitlement and the gateway outage remediation, the seams tests replace
var (
	sttManagedNousToolsEnabled       = func() bool { return tools.ManagedNousToolsEnabled(nil, false) }
	sttNousGatewayUnavailableMessage = func(what string) string { return tools.NousToolGatewayUnavailableMessage(nil, what, false) }
	sttTranscode                     = transcodeAudioForSTT
)

// Managed openai-audio gateway key and base URL, false when unavailable, the seam tests replace
var sttManagedOpenAIAudio = func(ctx context.Context) (string, string, bool, error) {
	route, err := managedOpenAIAudioRoute(nil)
	if err != nil || route == nil {
		return "", "", false, err
	}
	return route.APIKey, route.BaseURL, true, nil
}

// OpenAI STT key and base URL strict on the stored stt selection, a ValueError when unresolvable
func resolveSTTOpenAIAudioClientConfig(ctx context.Context) (string, string, error) {
	openaiCfg, err := orEmptyDict(loadSTTConfig(ctx), "openai")
	if err != nil {
		return "", "", err
	}
	selected := sttReadSelection(ctx)
	if selected == NousManagedProvider {
		key, base, ok, err := sttManagedOpenAIAudio(ctx)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", &sttValueError{msg: tools.SelectionError("stt", NousManagedProvider, "the Nous Tool Gateway is not available (not entitled or unreachable)")}
		}
		return key, base, nil
	}
	if key, base, ok := directOpenAICredentials(ctx, dictGetDefault(openaiCfg, "api_key", ""), dictGetDefault(openaiCfg, "base_url", "")); ok {
		return key, base, nil
	}
	if selected != "" {
		return "", "", &sttValueError{msg: tools.SelectionError("stt", selected, "neither stt.openai.api_key in config nor VOICE_TOOLS_OPENAI_KEY/OPENAI_API_KEY is set")}
	}
	key, base, ok, err := sttManagedOpenAIAudio(ctx)
	if err != nil {
		return "", "", err
	}
	if !ok {
		message := "Neither stt.openai.api_key in config nor VOICE_TOOLS_OPENAI_KEY/OPENAI_API_KEY is set"
		if sttManagedNousToolsEnabled() {
			message += ". " + sttNousGatewayUnavailableMessage("managed OpenAI audio for transcription")
		}
		return "", "", &sttValueError{msg: message}
	}
	return key, base, nil
}

// Plain transcript of a text, mapping or scalar response, a structured reply without text raising its error
func extractTranscriptText(transcription any) (string, error) {
	var text string
	switch v := transcription.(type) {
	case string:
		text = pystr.Strip(v)
	case map[string]any:
		if s, ok := v["text"].(string); ok {
			text = pystr.Strip(s)
		} else {
			if e := v["error"]; pyTruthy(e) {
				return "", &sttResponseError{msg: pyrepr.Str(e)}
			}
			return "", &sttResponseError{msg: "Transcription response contained no text"}
		}
	default:
		text = pystr.Strip(pyrepr.Str(v))
	}
	if m, _ := asrTextRe.FindStringMatch(text); m != nil {
		return pystr.Strip(m.GroupByName("text").String()), nil
	}
	return text, nil
}
