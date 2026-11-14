package qqbot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const defaultSTTTimeout = 60.0

var voiceExtensions = []string{".silk", ".amr", ".mp3", ".wav", ".ogg", ".m4a", ".aac", ".speex", ".flac"}

// Direct REST endpoints of the STT provider names users write
var sttProviderBaseURLs = map[string]string{
	"zai":    "https://open.bigmodel.cn/api/coding/paas/v4",
	"openai": "https://api.openai.com/v1",
	"glm":    "https://open.bigmodel.cn/api/coding/paas/v4",
}

var audioURLExtensions = map[string]bool{".silk": true, ".amr": true, ".mp3": true, ".wav": true, ".ogg": true, ".m4a": true, ".aac": true, ".flac": true}

// Magic byte prefixes and the audio extension each names
var magicExts = []struct {
	magic []byte
	ext   string
}{
	{[]byte("#!SILK"), ".silk"}, {[]byte("\x02!"), ".silk"}, {[]byte("RIFF"), ".wav"}, {[]byte("fLaC"), ".flac"},
	{[]byte("\xff\xfb"), ".mp3"}, {[]byte("\xff\xf3"), ".mp3"}, {[]byte("\xff\xf2"), ".mp3"},
	{[]byte("\x30\x26\xb2\x75"), ".ogg"}, {[]byte("\x4f\x67\x67\x53"), ".ogg"},
}

// Media cache writers and the audio converters the adapter runs, swappable like module globals
var (
	cacheImageFromBytes    = gateway.CacheImageFromBytesAsync
	cacheDocumentFromBytes = gateway.CacheDocumentFromBytesAsync
	silkToWAV              = media.SilkToWAVAtRate
)

// Whether the error is an OSError of a file or process operation
func isOSError(err error) bool {
	var pathErr *fs.PathError
	var errno syscall.Errno
	var execErr *exec.Error
	return errors.As(err, &pathErr) || errors.As(err, &errno) || errors.As(err, &execErr)
}

// Whether the error is an httpx transport error
func isTransportError(err error) bool {
	var pe *pyErr
	return errors.As(err, &pe) && (pe.kind == "ConnectError" || pe.kind == "ReadTimeout" || pe.kind == "ReadError")
}

// Whether the error is the httpx error of a response outside the success range
func isHTTPStatusError(err error) bool {
	var pe *pyErr
	return errors.As(err, &pe) && pe.kind == "HTTPStatusError"
}

// Downloads a URL and caches it as an image, a converted voice file or a document, empty when the download fails
func (a *QQAdapter) downloadAndCache(ctx context.Context, url, contentType, originalName string) (string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), url) {
		return "", &pyErr{"ValueError", "Blocked unsafe URL: " + pystr.Slice(url, 0, 80)}
	}
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return "", nil
	}
	resp, err := httpRequest(ctx, client, http.MethodGet, url, a.qqMediaHeaders(), nil, 30.0)
	if err == nil {
		err = raiseForStatus(resp, url)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[%s] Download failed for %s: %s", a.logTag(), pystr.Slice(url, 0, 80), errText(err)))
		return "", nil
	}
	data := resp.Content
	if strings.HasPrefix(contentType, "image/") {
		ext := gateway.ExtForMime(contentType, nil, false, true, ".jpg")
		if ext == "" {
			ext = ".jpg"
		}
		return cacheImageFromBytes(ctx, data, ext)
	}
	if contentType == "voice" || strings.HasPrefix(contentType, "audio/") {
		return a.convertAudioToWAV(ctx, data, url)
	}
	filename := originalName
	if filename == "" {
		parsed, err := pyurl.Parse(url)
		if err != nil {
			return "", err
		}
		if filename = pypath.Name(parsed.Path); filename == "" {
			filename = "qq_attachment"
		}
	}
	return cacheDocumentFromBytes(ctx, data, filename)
}

// Whether an attachment is voice by its content type, else by its file name unless it is an explicit file upload
func isVoiceContentType(contentType, filename string) bool {
	ct := pystr.Lower(pystr.Strip(contentType))
	if ct == "voice" || strings.HasPrefix(ct, "audio/") {
		return true
	}
	if ct == "file" {
		return false
	}
	name := pystr.Lower(pystr.Strip(filename))
	for _, ext := range voiceExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// Authorization header the QQ multimedia CDN requires, empty without an access token
func (a *QQAdapter) qqMediaHeaders() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accessToken == "" {
		return map[string]string{}
	}
	return map[string]string{"Authorization": "QQBot " + a.accessToken}
}

// Transcript of a voice attachment from QQ's own text, else STT on the WAV URL, else STT on the converted original
func (a *QQAdapter) sttVoiceAttachment(ctx context.Context, url, contentType, filename, asrReferText, voiceWAVURL string) (string, error) {
	if asrReferText != "" {
		slog.Debug(fmt.Sprintf("[%s] STT: using QQ asr_refer_text: %s", a.logTag(), pyrepr.Repr(pystr.Slice(asrReferText, 0, 100))))
		return asrReferText, nil
	}
	isPreWAV := voiceWAVURL != ""
	downloadURL := url
	if isPreWAV {
		downloadURL = voiceWAVURL
		if strings.HasPrefix(voiceWAVURL, "//") {
			downloadURL = "https:" + voiceWAVURL
		}
		slog.Debug(fmt.Sprintf("[%s] STT: using voice_wav_url (pre-converted WAV)", a.logTag()))
	}
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), downloadURL) {
		slog.Warn(fmt.Sprintf("[QQ] STT blocked unsafe URL: %s", pystr.Slice(downloadURL, 0, 80)))
		return "", nil
	}
	transcript, err := a.transcribeVoiceURL(ctx, downloadURL, filename, isPreWAV)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if isOSError(err) || isHTTPStatusError(err) || isTransportError(err) {
			slog.Warn(fmt.Sprintf("[%s] STT failed for voice attachment: %s: %s", a.logTag(), pyrepr.ExceptionName(err), errText(err)))
			return "", nil
		}
		return "", err
	}
	return transcript, nil
}

// Downloads voice audio, converts it to WAV unless it already is one and runs STT on it
func (a *QQAdapter) transcribeVoiceURL(ctx context.Context, downloadURL, filename string, isPreWAV bool) (string, error) {
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		slog.Warn(fmt.Sprintf("[%s] STT: no HTTP client", a.logTag()))
		return "", nil
	}
	downloadHeaders := a.qqMediaHeaders()
	slog.Debug(fmt.Sprintf("[%s] STT: downloading voice from %s (pre_wav=%s, headers=%s)", a.logTag(), pystr.Slice(downloadURL, 0, 80),
		pyrepr.Str(isPreWAV), pyrepr.Str(len(downloadHeaders) > 0)))
	resp, err := httpRequest(ctx, client, http.MethodGet, downloadURL, downloadHeaders, nil, 30.0)
	if err != nil {
		return "", err
	}
	if err := raiseForStatus(resp, downloadURL); err != nil {
		return "", err
	}
	audioData := resp.Content
	contentType := resp.Header.Get("content-type")
	if contentType == "" {
		contentType = "unknown"
	}
	slog.Debug(fmt.Sprintf("[%s] STT: downloaded %d bytes, content_type=%s", a.logTag(), len(audioData), contentType))
	if len(audioData) < 10 {
		slog.Warn(fmt.Sprintf("[%s] STT: downloaded data too small (%d bytes), skipping", a.logTag(), len(audioData)))
		return "", nil
	}
	var wavPath string
	if isPreWAV {
		if wavPath, err = writeTemp(audioData, ".wav"); err != nil {
			return "", err
		}
		slog.Debug(fmt.Sprintf("[%s] STT: using pre-converted WAV directly (%d bytes)", a.logTag(), len(audioData)))
	} else {
		slog.Debug(fmt.Sprintf("[%s] STT: converting to wav, filename=%s", a.logTag(), pyrepr.Repr(filename)))
		if wavPath, err = a.convertAudioToWAVFile(ctx, audioData, filename); err != nil {
			return "", err
		}
		if _, statErr := os.Stat(wavPath); wavPath == "" || statErr != nil {
			slog.Warn(fmt.Sprintf("[%s] STT: ffmpeg conversion produced no output", a.logTag()))
			return "", nil
		}
	}
	slog.Debug(fmt.Sprintf("[%s] STT: calling ASR on %s", a.logTag(), wavPath))
	transcript, err := a.callSTT(ctx, wavPath)
	unlinkQuiet(wavPath)
	if err != nil {
		return "", err
	}
	if transcript != "" {
		slog.Debug(fmt.Sprintf("[%s] STT success: %s", a.logTag(), pyrepr.Repr(pystr.Slice(transcript, 0, 100))))
	} else {
		slog.Warn(fmt.Sprintf("[%s] STT: ASR returned empty transcript", a.logTag()))
	}
	return transcript, nil
}

// Writes data to a persistent temporary file with the suffix and returns its path
func writeTemp(data []byte, suffix string) (string, error) {
	tmp, err := os.CreateTemp("", "tmp*"+suffix)
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	return tmp.Name(), werr
}

func unlinkQuiet(path string) { _ = os.Remove(path) }

// Whether a WAV file exists and holds more than its bare 44 byte header
func wavOK(wavPath string) bool {
	info, err := os.Stat(wavPath)
	return err == nil && info.Size() > 44
}

// Path with everything after its last dot replaced by the extension
func siblingPath(path, ext string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i] + ext
	}
	return path + ext
}

// Writes audio to a temporary file with the extension and returns its path with the sibling WAV path
func tempPair(audioData []byte, ext string) (string, string, error) {
	srcPath, err := writeTemp(audioData, ext)
	return srcPath, siblingPath(srcPath, ".wav"), err
}

// Converts audio bytes to a temporary WAV through pilk, then ffmpeg, then the raw PCM wrap, empty when all fail
func (a *QQAdapter) convertAudioToWAVFile(ctx context.Context, audioData []byte, filename string) (string, error) {
	ext := pystr.Lower(pypath.Suffix(filename))
	if ext == "" {
		ext = guessExtFromData(audioData)
	}
	slog.Info(fmt.Sprintf("[%s] STT: audio_data size=%d, ext=%s, first_20_bytes=%s", a.logTag(), len(audioData), pyrepr.Repr(ext),
		pyrepr.BytesRepr(audioData[:min(20, len(audioData))])))
	srcPath, wavPath, err := tempPair(audioData, ext)
	if err != nil {
		return "", err
	}
	result := a.convertSilkToWAV(ctx, srcPath, wavPath)
	if result == "" {
		result = a.convertFFmpegToWAV(ctx, srcPath, wavPath)
	}
	if result == "" {
		result = a.convertRawToWAV(audioData, wavPath)
	}
	unlinkQuiet(srcPath)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return result, nil
}

// File extension from magic bytes, amr for unknown data since that is the most common QQ voice format
func guessExtFromData(data []byte) string {
	for _, m := range magicExts {
		if bytes.HasPrefix(data, m.magic) {
			return m.ext
		}
	}
	return ".amr"
}

func looksLikeSilk(data []byte) bool { return guessExtFromData(data) == ".silk" }

// Converts SILK to WAV with pilk, first from the source path and then from a copy named with the silk extension
func (a *QQAdapter) convertSilkToWAV(ctx context.Context, srcPath, wavPath string) string {
	silkPath := siblingPath(srcPath, ".silk")
	defer unlinkQuiet(silkPath)
	for _, attempt := range []struct{ path, label, how string }{{srcPath, "", "direct"}, {silkPath, " (as .silk)", ".silk"}} {
		var err error
		if attempt.path == silkPath {
			if srcPath == silkPath {
				err = &pyErr{"SameFileError", fmt.Sprintf("%s and %s are the same file", pyrepr.Repr(srcPath), pyrepr.Repr(silkPath))}
			} else {
				err = pyshutil.Copy2(srcPath, silkPath)
			}
		}
		if err == nil {
			err = silkToWAV(ctx, attempt.path, wavPath, 16000)
		}
		if errors.Is(err, media.ErrPilkMissing) {
			slog.Warn(fmt.Sprintf("[%s] pilk not installed — cannot decode SILK audio. Run: %s", a.logTag(), pm.InstallHint("silk")))
			return ""
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] pilk %s conversion failed: %s", a.logTag(), attempt.how, errText(err)))
			continue
		}
		if wavOK(wavPath) {
			info, _ := os.Stat(wavPath)
			slog.Debug(fmt.Sprintf("[%s] pilk converted %s%s to wav (%d bytes)", a.logTag(), pypath.Name(srcPath), attempt.label, info.Size()))
			return wavPath
		}
	}
	return ""
}

// Wraps bytes as 16 bit mono 16 kHz PCM in a WAV file, the last resort that keeps the ASR engine from failing
func (a *QQAdapter) convertRawToWAV(audioData []byte, wavPath string) string {
	var header bytes.Buffer
	header.WriteString("RIFF")
	binary.Write(&header, binary.LittleEndian, uint32(36+len(audioData)))
	header.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		binary.Write(&header, binary.LittleEndian, field)
	}
	header.WriteString("data")
	binary.Write(&header, binary.LittleEndian, uint32(len(audioData)))
	if err := os.WriteFile(wavPath, append(header.Bytes(), audioData...), 0o666); err != nil {
		slog.Debug(fmt.Sprintf("[%s] raw PCM fallback failed: %s", a.logTag(), errText(err)))
		return ""
	}
	return wavPath
}

// Converts audio to 16 kHz mono WAV with ffmpeg, empty when ffmpeg is missing, fails, times out or writes nothing
func (a *QQAdapter) convertFFmpegToWAV(ctx context.Context, srcPath, wavPath string) string {
	proc, err := pysubprocess.RunWith(ctx, []string{"ffmpeg", "-y", "-i", srcPath, "-ar", "16000", "-ac", "1", wavPath},
		pysubprocess.Options{Timeout: 30 * time.Second})
	var timeout *pysubprocess.TimeoutExpired
	switch {
	case ctx.Err() != nil:
		return ""
	case errors.As(err, &timeout):
		slog.Warn(fmt.Sprintf("[%s] ffmpeg conversion error: ", a.logTag()))
		return ""
	case err != nil:
		slog.Warn(fmt.Sprintf("[%s] ffmpeg conversion error: %s", a.logTag(), pypath.OSErrorText(err)))
		return ""
	}
	if proc.Returncode != 0 {
		slog.Warn(fmt.Sprintf("[%s] ffmpeg failed for %s: %s", a.logTag(), pypath.Name(srcPath),
			pystr.DecodeUTF8Replace(proc.Stderr[:min(200, len(proc.Stderr))])))
		return ""
	}
	if !wavOK(wavPath) {
		slog.Warn(fmt.Sprintf("[%s] ffmpeg produced no/small output for %s", a.logTag(), pypath.Name(srcPath)))
		return ""
	}
	info, _ := os.Stat(wavPath)
	slog.Debug(fmt.Sprintf("[%s] ffmpeg converted %s to wav (%d bytes)", a.logTag(), pypath.Name(srcPath), info.Size()))
	return wavPath
}

// Resolved STT backend of the adapter
type sttConfig struct {
	baseURL, apiKey, model string
	timeout                float64
}

// STT backend from the stt block of the extra, then from the QQ_STT env vars, nil when neither is configured
func (a *QQAdapter) resolveSTTConfig(ctx context.Context) (*sttConfig, error) {
	if cfg, ok := a.Config.Extra["stt"].(map[string]any); ok && cfg["enabled"] != false {
		baseURL := pyrepr.Or(cfg["baseUrl"], getOr(cfg, "base_url", ""))
		apiKey := pyrepr.Or(cfg["apiKey"], getOr(cfg, "api_key", ""))
		model := getOr(cfg, "model", "")
		timeout := defaultSTTTimeout
		if raw, present := cfg["timeout"]; present {
			if f, err := pyint.Float(raw); err == nil {
				timeout = f
			}
		}
		modelOr := func(def string) string {
			if pyrepr.Truthy(model) {
				return pyrepr.Str(model)
			}
			return def
		}
		if pyrepr.Truthy(baseURL) && pyrepr.Truthy(apiKey) {
			return &sttConfig{pystr.RstripChars(pyrepr.Str(baseURL), "/"), pyrepr.Str(apiKey), modelOr("whisper-1"), timeout}, nil
		}
		if pyrepr.Truthy(apiKey) {
			provider := pyrepr.Str(getOr(cfg, "provider", "zai"))
			if providerURL := sttProviderBaseURLs[provider]; providerURL != "" {
				defaultModel := "whisper-1"
				if provider == "zai" || provider == "glm" {
					defaultModel = "glm-asr"
				}
				return &sttConfig{providerURL, pyrepr.Str(apiKey), modelOr(defaultModel), timeout}, nil
			}
		}
	}
	key, err := gateway.GetScopedSecret(ctx, "QQ_STT_API_KEY", "", false)
	if err != nil || key == "" {
		return nil, err
	}
	baseURL, err := gateway.GetScopedSecret(ctx, "QQ_STT_BASE_URL", sttProviderBaseURLs["zai"], false)
	if err != nil {
		return nil, err
	}
	model, err := gateway.GetScopedSecret(ctx, "QQ_STT_MODEL", "glm-asr", false)
	if err != nil {
		return nil, err
	}
	return &sttConfig{pystr.RstripChars(baseURL, "/"), key, model, defaultSTTTimeout}, nil
}

// Transcribes a WAV through an OpenAI compatible STT API, empty when STT is unconfigured or the call fails
func (a *QQAdapter) callSTT(ctx context.Context, wavPath string) (string, error) {
	cfg, err := a.resolveSTTConfig(ctx)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		slog.Warn(fmt.Sprintf("[%s] STT not configured (no stt config or QQ_STT_API_KEY)", a.logTag()))
		return "", nil
	}
	transcript, err := a.postTranscription(ctx, cfg, wavPath)
	if err != nil && ctx.Err() == nil && (isOSError(err) || isHTTPStatusError(err)) {
		slog.Warn(fmt.Sprintf("[%s] STT API call failed (model=%s, base=%s): %s", a.logTag(), cfg.model, pystr.Slice(cfg.baseURL, 0, 50), errText(err)))
		return "", nil
	}
	return transcript, err
}

// Posts the WAV as a multipart transcription request and reads the transcript from either response shape
func (a *QQAdapter) postTranscription(ctx context.Context, cfg *sttConfig, wavPath string) (string, error) {
	audio, err := os.ReadFile(wavPath)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return "", &pyErr{"AttributeError", "'NoneType' object has no attribute 'post'"}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", cfg.model); err != nil {
		return "", err
	}
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, pypath.Name(wavPath)))
	partHeader.Set("Content-Type", "audio/wav")
	part, err := form.CreatePart(partHeader)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}
	rawURL := cfg.baseURL + "/audio/transcriptions"
	resp, err := httpRequest(ctx, client, http.MethodPost, rawURL,
		map[string]string{"Authorization": "Bearer " + cfg.apiKey, "Content-Type": form.FormDataContentType()}, body.Bytes(), cfg.timeout)
	if err != nil {
		return "", err
	}
	if err := raiseForStatus(resp, rawURL); err != nil {
		return "", err
	}
	decoded, err := resp.JSON()
	if err != nil {
		return "", err
	}
	result, ok := asDict(decoded)
	if !ok {
		return "", notDictErr(decoded)
	}
	content := ""
	if choices := getOr(result, "choices", []any{}); pyrepr.Truthy(choices) {
		list, isList := choices.([]any)
		if !isList || len(list) == 0 {
			return "", &pyErr{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(choices))}
		}
		first, isDict := asDict(list[0])
		if !isDict {
			return "", notDictErr(list[0])
		}
		message, isDict := asDict(getOr(first, "message", map[string]any{}))
		if !isDict {
			return "", notDictErr(first["message"])
		}
		text, isStr := getOr(message, "content", "").(string)
		if !isStr {
			return "", &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(message["content"]))}
		}
		content = text
	}
	if stripped := pystr.Strip(content); stripped != "" {
		return stripped, nil
	}
	text, isStr := getOr(result, "text", "").(string)
	if !isStr {
		return "", &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(result["text"]))}
	}
	return pystr.Strip(text), nil
}

// Converts voice bytes to WAV and caches the result, the original bytes cached under their extension when conversion fails
func (a *QQAdapter) convertAudioToWAV(ctx context.Context, audioData []byte, sourceURL string) (string, error) {
	parsed, err := pyurl.Parse(sourceURL)
	if err != nil {
		return "", err
	}
	ext := pystr.Lower(pypath.Suffix(parsed.Path))
	if !audioURLExtensions[ext] {
		ext = guessExtFromData(audioData)
	}
	srcPath, wavPath, err := tempPair(audioData, ext)
	if err != nil {
		return "", err
	}
	var converted string
	if ext == ".silk" || looksLikeSilk(audioData) {
		converted = a.convertSilkToWAV(ctx, srcPath, wavPath)
	} else {
		converted = a.convertFFmpegToWAV(ctx, srcPath, wavPath)
	}
	unlinkQuiet(srcPath)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if converted == "" {
		slog.Warn(fmt.Sprintf("[%s] audio conversion failed for %s (format=%s)", a.logTag(), pystr.Slice(sourceURL, 0, 60), ext))
		return cacheDocumentFromBytes(ctx, audioData, "qq_voice"+ext)
	}
	wavData, err := os.ReadFile(wavPath)
	if err == nil {
		if err = os.Remove(wavPath); err == nil {
			var cached string
			if cached, err = cacheDocumentFromBytes(ctx, wavData, "qq_voice.wav"); err == nil {
				return cached, nil
			}
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	slog.Debug(fmt.Sprintf("[%s] Failed to read converted wav: %s", a.logTag(), errText(err)))
	return "", nil
}
