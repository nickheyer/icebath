package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var audioMimeExtensions = map[string]string{
	"audio/aac": ".aac", "audio/flac": ".flac", "audio/m4a": ".m4a", "audio/mp3": ".mp3",
	"audio/mp4": ".mp4", "audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/wav": ".wav",
	"audio/wave": ".wav", "audio/webm": ".webm", "audio/x-m4a": ".m4a", "audio/x-wav": ".wav",
	"video/webm": ".webm",
}

const maxTranscriptionUploadBytes = 25 * 1024 * 1024

var speakMimeByExt = map[string]string{
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".wav": "audio/wav", ".flac": "audio/flac",
}

const elevenLabsVoicesURL = "https://api.elevenlabs.io/v1/voices"

// Removes a file, ignoring any OS error
func unlinkQuietly(path string) { _ = os.Remove(path) }

// Runs fn under the config only profile scope of the requested profile
func runConfigScoped[T any](ctx context.Context, profile string, fn func(ctx context.Context) (T, error)) (T, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		var zero T
		return zero, err
	}
	return fn(sctx)
}

// Upload file extension of a recording mime type, webm for anything unknown
func audioExtensionForMime(mimeType string) string {
	head, _, _ := strings.Cut(mimeType, ";")
	if ext, ok := audioMimeExtensions[pystr.Lower(pystr.Strip(head))]; ok {
		return ext
	}
	return ".webm"
}

// Writes a decoded recording to a temp file and transcribes it under the profile scope
func transcribeUploadedAudio(ctx context.Context, audio []byte, mimeType, profile string) (map[string]any, error) {
	tmp, err := os.CreateTemp("", "icebath-desktop-voice-*"+audioExtensionForMime(mimeType))
	if err != nil {
		return nil, err
	}
	tempPath := tmp.Name()
	defer unlinkQuietly(tempPath)
	if _, err := tmp.Write(audio); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return runConfigScoped(ctx, profile, func(sctx context.Context) (map[string]any, error) {
		return media.TranscribeRecording(sctx, tempPath, "")
	})
}

// Transcribes a base64 data URL recording, an empty transcript standing for silence
func transcribeAudioUpload(ctx context.Context, payload *pyjson.Object, profile string) (*pyjson.Object, error) {
	dataURL, _ := payload.GetOr("data_url", "").(string)
	dataURL = pystr.Strip(dataURL)
	if !strings.HasPrefix(dataURL, "data:") || !strings.Contains(dataURL, ",") {
		return nil, NewHTTPException(400, "Invalid audio payload")
	}
	header, encoded, _ := strings.Cut(dataURL, ",")
	if !strings.Contains(header, ";base64") {
		return nil, NewHTTPException(400, "Audio payload must be base64 encoded")
	}
	mimeType, _ := payload.GetOr("mime_type", nil).(string)
	if mimeType == "" {
		mimeType, _, _ = strings.Cut(header[5:], ";")
	}
	if mimeType == "" {
		mimeType = "audio/webm"
	}
	mimeType = pystr.Strip(mimeType)
	normalized, _, _ := strings.Cut(mimeType, ";")
	normalized = pystr.Lower(normalized)
	if !strings.HasPrefix(normalized, "audio/") && normalized != "video/webm" {
		return nil, NewHTTPException(400, "Payload must be an audio recording")
	}
	audio, err := pybase64.B64DecodeValidate(encoded)
	if err != nil {
		return nil, NewHTTPException(400, "Audio payload is not valid base64")
	}
	if len(audio) == 0 {
		return nil, NewHTTPException(400, "Audio recording is empty")
	}
	if len(audio) > maxTranscriptionUploadBytes {
		return nil, NewHTTPException(413, "Audio recording is too large")
	}
	result, err := transcribeUploadedAudio(ctx, audio, mimeType, profile)
	if err := httpFailure(err, "Desktop voice transcription failed", 500, "Transcription failed", nil); err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["success"]) {
		message := "Transcription failed"
		if e := result["error"]; pyrepr.Truthy(e) {
			message = pyrepr.Str(e)
		}
		if strings.Contains(pystr.Lower(message), "empty transcript") {
			return pyjson.ObjectOf("ok", true, "transcript", "", "provider", result["provider"]), nil
		}
		return nil, NewHTTPException(400, message)
	}
	transcript := ""
	if t := result["transcript"]; pyrepr.Truthy(t) {
		transcript = pyrepr.Str(t)
	}
	return pyjson.ObjectOf("ok", true, "transcript", pystr.Strip(transcript), "provider", result["provider"]), nil
}

// Object whose keys follow ok then every key of the result in its order
func okMerged(result *pyjson.Object) *pyjson.Object {
	out := pyjson.ObjectOf("ok", true)
	for _, k := range result.Keys {
		out.Set(k, result.Vals[k])
	}
	return out
}

// Client voice config of the profile, a resolution failure falling back to the relay
func getClientVoiceConfig(ctx context.Context, profile string) (*pyjson.Object, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	result, failure := func() (result *pyjson.Object, failure any) {
		defer func() { failure = recover() }()
		return media.ResolveClientVoiceConfig(sctx), nil
	}()
	if failure != nil {
		routersLog().Error("Client voice-config resolution failed", "exc_info", fmt.Sprint(failure))
		fallback := func() *pyjson.Object { return pyjson.ObjectOf("mode", "relay", "reason", "resolution error") }
		return pyjson.ObjectOf("ok", true, "stt", fallback(), "tts", fallback()), nil
	}
	return okMerged(result), nil
}

// Selected voice chat mode of the profile and whether GPT-Live can start
func getVoiceLiveStatus(ctx context.Context, profile string) (*pyjson.Object, error) {
	result, err := runConfigScoped(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		return media.ResolveGPTLiveStatus(sctx), nil
	})
	if err := httpFailure(err, "GPT-Live status resolution failed", 500, "GPT-Live status failed", nil); err != nil {
		return nil, err
	}
	return okMerged(result), nil
}

// Exchanges the renderer's SDP offer for a GPT-Live session answer with the key kept on this host
func createVoiceLiveSession(ctx context.Context, payload *pyjson.Object, profile string) (*pyjson.Object, error) {
	sdp, _ := payload.GetOr("sdp", "").(string)
	if pystr.Strip(sdp) == "" {
		return nil, NewHTTPException(400, "An SDP offer is required")
	}
	history, _ := payload.GetOr("history", nil).([]any)
	result, err := runConfigScoped(ctx, profile, func(sctx context.Context) (any, error) {
		return media.CreateWebRTCSession(sctx, sdp, history)
	})
	var valueErr *media.VoiceLiveValueError
	var runtimeErr *media.VoiceLiveRuntimeError
	switch {
	case errors.As(err, &valueErr):
		return nil, NewHTTPException(503, valueErr.Error())
	case errors.As(err, &runtimeErr):
		return nil, NewHTTPException(502, runtimeErr.Error())
	case err != nil:
		return nil, err
	}
	answer, ok := result.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("TypeError: 'dict' object argument after ** must be a mapping, not %s", pyrepr.TypeName(result))
	}
	return okMerged(answer), nil
}

// Voice name with its category in parentheses when it has one
func elevenLabsVoiceLabel(voice *pyjson.Object) string {
	name := pyrepr.Or(voice.GetOr("name", nil), pyrepr.Or(voice.GetOr("voice_id", nil), "Voice"))
	label := pystr.Strip(pyrepr.Str(name))
	category := pystr.Strip(pyrepr.Str(pyrepr.Or(voice.GetOr("category", nil), "")))
	if category != "" {
		return label + " (" + category + ")"
	}
	return label
}

// ElevenLabs API key of the profile .env, else the scoped secret, else the process env when no scope is bound
func elevenLabsAPIKey(ctx context.Context, profile string) (string, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return "", err
	}
	if key := pystr.Strip(config.LoadEnv(home.Dir(sctx))["ELEVENLABS_API_KEY"]); key != "" {
		return key, nil
	}
	key, _, err := provider.GetSecret(ctx, "ELEVENLABS_API_KEY")
	var unscoped *provider.UnscopedSecretError
	if errors.As(err, &unscoped) {
		return pystr.Strip(os.Getenv("ELEVENLABS_API_KEY")), nil
	}
	if err != nil {
		return "", nil
	}
	return pystr.Strip(key), nil
}

// HTTP error of a voice list response, its status and reason phrase
type voiceListHTTPError struct {
	code   int
	reason string
}

func (e *voiceListHTTPError) Error() string {
	return fmt.Sprintf("HTTP Error %d: %s", e.code, e.reason)
}

// Decoded voice list of the ElevenLabs API under the dashboard response cap
func fetchElevenLabsVoices(ctx context.Context, apiKey string) (any, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, elevenLabsVoicesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("xi-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &voiceListHTTPError{resp.StatusCode, strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode))}
	}
	return readDashboardJSONResponse(resp.Body)
}

// ElevenLabs voices when an API key is configured, the key itself staying server side
func getElevenLabsVoices(ctx context.Context, profile string) (*pyjson.Object, error) {
	apiKey, err := elevenLabsAPIKey(ctx, profile)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return pyjson.ObjectOf("available", false, "voices", []any{}), nil
	}
	payload, err := fetchElevenLabsVoices(ctx, apiKey)
	if err != nil {
		var httpErr *voiceListHTTPError
		if errors.As(err, &httpErr) {
			signature := fmt.Sprintf("http-%d", httpErr.code)
			if httpErr.code == 401 || httpErr.code == 403 {
				if voiceListErrorLoggedOnce(&signature) {
					routersLog().Info(fmt.Sprintf("ElevenLabs voices unavailable: %s — check ELEVENLABS_API_KEY", httpErr))
				}
				return pyjson.ObjectOf("available", false, "voices", []any{}, "error", "unauthorized"), nil
			}
			if voiceListErrorLoggedOnce(&signature) {
				routersLog().Warn(fmt.Sprintf("ElevenLabs voice list failed: %s", httpErr))
			}
			return nil, NewHTTPException(502, "Could not load ElevenLabs voices")
		}
		signature := err.Error()
		if voiceListErrorLoggedOnce(&signature) {
			routersLog().Warn(fmt.Sprintf("ElevenLabs voice list failed: %s", err))
		}
		return nil, NewHTTPException(502, "Could not load ElevenLabs voices")
	}
	voiceListErrorLoggedOnce(nil)
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(payload))
	}
	rawVoices := obj.GetOr("voices", nil)
	if !pyrepr.Truthy(rawVoices) {
		rawVoices = []any{}
	}
	list, ok := rawVoices.([]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(rawVoices))
	}
	voices := []*pyjson.Object{}
	for _, v := range list {
		voice, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		voiceID := pystr.Strip(pyrepr.Str(pyrepr.Or(voice.GetOr("voice_id", nil), "")))
		if voiceID == "" {
			continue
		}
		voices = append(voices, pyjson.ObjectOf("voice_id", voiceID,
			"name", pyrepr.Str(pyrepr.Or(voice.GetOr("name", nil), voiceID)), "label", elevenLabsVoiceLabel(voice)))
	}
	sort.SliceStable(voices, func(i, j int) bool {
		return pystr.Lower(pyrepr.Str(voices[i].GetOr("label", ""))) < pystr.Lower(pyrepr.Str(voices[j].GetOr("label", "")))
	})
	return pyjson.ObjectOf("available", true, "voices", objectList(voices)), nil
}

// Synthesizes speech with the profile TTS chain and returns it as a base64 data URL
func speakText(ctx context.Context, payload *pyjson.Object, profile string) (*pyjson.Object, error) {
	text, _ := payload.GetOr("text", "").(string)
	text = pystr.Strip(text)
	if text == "" {
		return nil, NewHTTPException(400, "Text is required")
	}
	resultJSON, err := runConfigScoped(ctx, profile, func(sctx context.Context) (string, error) {
		return media.TextToSpeechTool(sctx, text, "")
	})
	if err := httpFailure(err, "Desktop voice TTS failed", 500, "Speech synthesis failed", nil); err != nil {
		return nil, err
	}
	parsed, err := pyjson.Loads(resultJSON)
	if err != nil {
		return nil, NewHTTPException(500, "Invalid TTS response")
	}
	result, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	if !pyrepr.Truthy(result.GetOr("success", nil)) {
		detail := "Speech synthesis failed"
		if e := result.GetOr("error", nil); pyrepr.Truthy(e) {
			detail = pyrepr.Str(e)
		}
		return nil, NewHTTPException(400, detail)
	}
	filePath, _ := result.GetOr("file_path", nil).(string)
	if st, err := os.Stat(filePath); filePath == "" || err != nil || !st.Mode().IsRegular() {
		return nil, NewHTTPException(500, "Audio file missing")
	}
	mimeType, ok := speakMimeByExt[strings.ToLower(filepath.Ext(filePath))]
	if !ok {
		mimeType = "audio/mpeg"
	}
	audio, err := func() ([]byte, error) {
		defer unlinkQuietly(filePath)
		return os.ReadFile(filePath)
	}()
	if err != nil {
		return nil, NewHTTPException(500, "Could not read audio: "+pypath.OSErrorText(err))
	}
	return pyjson.ObjectOf("ok", true, "data_url", "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(audio),
		"mime_type", mimeType, "provider", result.GetOr("provider", nil)), nil
}

// Lease result keys in the order the hermes warm and release dicts build them
var leaseResultKeys = []string{"provider", "warmed", "action", "error", "elapsed_ms", "leases", "released"}

// Lease response, ok lease and active then the lease result in its hermes key order
func leaseResponse(lease string, active bool, result map[string]any) *pyjson.Object {
	out := pyjson.ObjectOf("ok", true, "lease", lease, "active", active)
	for _, k := range leaseResultKeys {
		if v, ok := result[k]; ok {
			out.Set(k, v)
		}
	}
	return out
}

// Lease result of a failed warm up or release
func leaseErrorResult(failure error) map[string]any {
	return map[string]any{"leases": nil, "action": "error", "error": failure.Error()}
}

// Runs a lease step, a panic standing for the exception the hermes lease surfaces report in the body
func guardedLease(step func() (map[string]any, error)) (result map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return step()
}

// Applies a desktop TTS toggle as a warm up or release lease, a warm up failure reported in the body
func ttsLease(ctx context.Context, payload *pyjson.Object, profile string) (*pyjson.Object, error) {
	lease, _ := payload.GetOr("lease", "").(string)
	lease = pystr.Strip(lease)
	if lease == "" {
		return nil, NewHTTPException(400, "lease is required")
	}
	active, _ := payload.GetOr("active", true).(bool)
	result, err := guardedLease(func() (map[string]any, error) {
		if active {
			return runConfigScoped(ctx, profile, func(sctx context.Context) (map[string]any, error) {
				env := &tools.Env{Home: home.Dir(sctx), TerminalScope: tools.GetTerminalScope(sctx)}
				return media.AcquireTTSLease(sctx, env, lease, media.LoadTTSConfig(sctx, env)), nil
			})
		}
		released, err := runConfigScoped(ctx, profile, func(sctx context.Context) (map[string]any, error) {
			return media.ReleaseTTSLease(sctx, &tools.Env{Home: home.Dir(sctx), TerminalScope: tools.GetTerminalScope(sctx)}, lease), nil
		})
		if _, isHTTP := asHTTPException(err); isHTTP {
			return media.ReleaseTTSLease(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, lease), nil
		}
		return released, err
	})
	if err != nil {
		if h, isHTTP := asHTTPException(err); isHTTP {
			return nil, h
		}
		routersLog().Warn(fmt.Sprintf("TTS lease %s (%s) failed: %s", lease, pyrepr.Str(active), err))
		result = leaseErrorResult(err)
	}
	return leaseResponse(lease, active, result), nil
}

// Applies a voice input session as an STT warm up or release lease, warm up failures reported in the body
func sttLease(ctx context.Context, payload *pyjson.Object, profile string) (*pyjson.Object, error) {
	lease, _ := payload.GetOr("lease", "").(string)
	lease = pystr.Strip(lease)
	if lease == "" {
		return nil, NewHTTPException(400, "lease is required")
	}
	active, _ := payload.GetOr("active", true).(bool)
	result, err := guardedLease(func() (map[string]any, error) {
		var obj *pyjson.Object
		var err error
		if active {
			obj, err = runConfigScoped(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
				return media.AcquireSTTLease(sctx, lease, nil)
			})
		} else {
			obj = media.ReleaseSTTLease(lease)
		}
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, k := range obj.Keys {
			out[k] = obj.Vals[k]
		}
		return out, nil
	})
	if err != nil {
		if h, isHTTP := asHTTPException(err); isHTTP {
			return nil, h
		}
		routersLog().Warn(fmt.Sprintf("STT lease %s (%s) failed: %s", lease, pyrepr.Str(active), err))
		result = leaseErrorResult(err)
	}
	return leaseResponse(lease, active, result), nil
}

// Router of the desktop audio relay, voice config, leases and the live speech sockets
func audioRouter() *APIRouter {
	r := NewAPIRouter("")
	profileParam := QueryParam("profile", Optional(Str), nil)
	r.Post("/api/audio/transcribe", &Endpoint{
		Name:   "transcribe_audio_upload",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("payload", audioTranscriptionRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return transcribeAudioUpload(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Get("/api/audio/voice-config", &Endpoint{
		Name:   "get_client_voice_config",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getClientVoiceConfig(ctx, c.Str("profile")) },
	})
	r.Get("/api/audio/voice-live/status", &Endpoint{
		Name:   "get_voice_live_status",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getVoiceLiveStatus(ctx, c.Str("profile")) },
	})
	r.Post("/api/audio/voice-live/session", &Endpoint{
		Name:   "create_voice_live_session",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("payload", voiceLiveSessionRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createVoiceLiveSession(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Get("/api/audio/elevenlabs/voices", &Endpoint{
		Name:   "get_elevenlabs_voices",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getElevenLabsVoices(ctx, c.Str("profile")) },
	})
	r.Post("/api/audio/speak", &Endpoint{
		Name:   "speak_text",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("payload", ttsSpeakRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return speakText(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Post("/api/audio/tts-lease", &Endpoint{
		Name:   "tts_lease",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("payload", ttsLeaseRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return ttsLease(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.Post("/api/audio/stt-lease", &Endpoint{
		Name:   "stt_lease",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("payload", sttLeaseRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return sttLease(ctx, c.Object("payload"), c.Str("profile"))
		},
	})
	r.WebSocket("/api/audio/transcribe-stream", &WSEndpoint{
		Name:   "transcribe_stream_ws",
		Handle: func(ctx context.Context, c *Call) error { return transcribeStreamWS(ctx, c.WebSocket) },
	})
	r.WebSocket("/api/audio/speak-stream", &WSEndpoint{
		Name:   "speak_stream_ws",
		Handle: func(ctx context.Context, c *Call) error { return speakStreamWS(ctx, c.WebSocket) },
	})
	return r
}
