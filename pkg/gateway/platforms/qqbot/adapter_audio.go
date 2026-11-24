package qqbot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var voiceExtensions = []string{".silk", ".amr", ".mp3", ".wav", ".ogg", ".m4a", ".aac", ".speex", ".flac"}

// Direct REST endpoint and usual model of each voice to text service qqbot.stt.provider offers
var (
	sttProviderBaseURLs = map[string]string{"zai": "https://open.bigmodel.cn/api/coding/paas/v4", "openai": "https://api.openai.com/v1"}
	sttProviderModels   = map[string]string{"zai": "glm-asr", "openai": "whisper-1"}
)

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

// Whether the error is the failure of an HTTP exchange: a refused or broken connection, a timeout or a body that did not arrive
func isTransportError(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}

// Whether the error is the status error of a response outside the success range
func isHTTPStatusError(err error) bool {
	var se *httputil.StatusError
	return errors.As(err, &se)
}

// Downloads a URL and caches it as an image, a converted voice file or a document, empty when the download fails
func (a *QQAdapter) downloadAndCache(ctx context.Context, rawURL, contentType, originalName string) (string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		return "", errors.New("Blocked unsafe URL: " + textutil.First(rawURL, 80))
	}
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return "", nil
	}
	resp, err := httpRequest(ctx, client, http.MethodGet, rawURL, a.qqMediaHeaders(), nil, 30.0)
	if err == nil {
		err = resp.CheckStatus()
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pkgLog().Debug(fmt.Sprintf("[%s] Download failed for %s: %v", a.logTag(), textutil.First(rawURL, 80), err))
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
		return a.convertAudioToWAV(ctx, data, rawURL)
	}
	filename := originalName
	if filename == "" {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return "", err
		}
		if filename = pathutil.Name(parsed.Path); filename == "" {
			filename = "qq_attachment"
		}
	}
	return cacheDocumentFromBytes(ctx, data, filename)
}

// Whether an attachment is voice by its content type, else by its file name unless it is an explicit file upload
func isVoiceContentType(contentType, filename string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "voice" || strings.HasPrefix(ct, "audio/") {
		return true
	}
	if ct == "file" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(filename))
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
func (a *QQAdapter) sttVoiceAttachment(ctx context.Context, rawURL, contentType, filename, asrReferText, voiceWAVURL string) (string, error) {
	if asrReferText != "" {
		pkgLog().Debug(fmt.Sprintf("[%s] STT: using QQ asr_refer_text: %s", a.logTag(), strconv.Quote(textutil.First(asrReferText, 100))))
		return asrReferText, nil
	}
	isPreWAV := voiceWAVURL != ""
	downloadURL := rawURL
	if isPreWAV {
		downloadURL = voiceWAVURL
		if strings.HasPrefix(voiceWAVURL, "//") {
			downloadURL = "https:" + voiceWAVURL
		}
		pkgLog().Debug(fmt.Sprintf("[%s] STT: using voice_wav_url (pre-converted WAV)", a.logTag()))
	}
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), downloadURL) {
		pkgLog().Warn(fmt.Sprintf("[QQ] STT blocked unsafe URL: %s", textutil.First(downloadURL, 80)))
		return "", nil
	}
	transcript, err := a.transcribeVoiceURL(ctx, downloadURL, filename, isPreWAV)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errorutil.IsSystem(err) || isHTTPStatusError(err) || isTransportError(err) {
			pkgLog().Warn(fmt.Sprintf("[%s] STT failed for voice attachment: %v", a.logTag(), err))
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
		pkgLog().Warn(fmt.Sprintf("[%s] STT: no HTTP client", a.logTag()))
		return "", nil
	}
	downloadHeaders := a.qqMediaHeaders()
	pkgLog().Debug(fmt.Sprintf("[%s] STT: downloading voice from %s (pre_wav=%s, headers=%s)", a.logTag(), textutil.First(downloadURL, 80),
		strconv.FormatBool(isPreWAV), strconv.FormatBool(len(downloadHeaders) > 0)))
	resp, err := httpRequest(ctx, client, http.MethodGet, downloadURL, downloadHeaders, nil, 30.0)
	if err != nil {
		return "", err
	}
	if err := resp.CheckStatus(); err != nil {
		return "", err
	}
	audioData := resp.Content
	contentType := resp.Header.Get("content-type")
	if contentType == "" {
		contentType = "unknown"
	}
	pkgLog().Debug(fmt.Sprintf("[%s] STT: downloaded %d bytes, content_type=%s", a.logTag(), len(audioData), contentType))
	if len(audioData) < 10 {
		pkgLog().Warn(fmt.Sprintf("[%s] STT: downloaded data too small (%d bytes), skipping", a.logTag(), len(audioData)))
		return "", nil
	}
	var wavPath string
	if isPreWAV {
		if wavPath, err = writeTemp(audioData, ".wav"); err != nil {
			return "", err
		}
		pkgLog().Debug(fmt.Sprintf("[%s] STT: using pre-converted WAV directly (%d bytes)", a.logTag(), len(audioData)))
	} else {
		pkgLog().Debug(fmt.Sprintf("[%s] STT: converting to wav, filename=%s", a.logTag(), strconv.Quote(filename)))
		if wavPath, err = a.convertAudioToWAVFile(ctx, audioData, filename); err != nil {
			return "", err
		}
		if _, statErr := os.Stat(wavPath); wavPath == "" || statErr != nil {
			pkgLog().Warn(fmt.Sprintf("[%s] STT: ffmpeg conversion produced no output", a.logTag()))
			return "", nil
		}
	}
	pkgLog().Debug(fmt.Sprintf("[%s] STT: calling ASR on %s", a.logTag(), wavPath))
	transcript, err := a.callSTT(ctx, wavPath)
	unlinkQuiet(wavPath)
	if err != nil {
		return "", err
	}
	if transcript != "" {
		pkgLog().Debug(fmt.Sprintf("[%s] STT success: %s", a.logTag(), strconv.Quote(textutil.First(transcript, 100))))
	} else {
		pkgLog().Warn(fmt.Sprintf("[%s] STT: ASR returned empty transcript", a.logTag()))
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
	ext := strings.ToLower(pathutil.Suffix(filename))
	if ext == "" {
		ext = guessExtFromData(audioData)
	}
	pkgLog().Info(fmt.Sprintf("[%s] STT: audio_data size=%d, ext=%s, first_20_bytes=%s", a.logTag(), len(audioData), strconv.Quote(ext),
		strconv.Quote(string(audioData[:min(20, len(audioData))]))))
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
				err = fmt.Errorf("%q and %q are the same file", srcPath, silkPath)
			} else {
				err = fsutil.CopyFile(srcPath, silkPath)
			}
		}
		if err == nil {
			err = silkToWAV(ctx, attempt.path, wavPath, 16000)
		}
		if errors.Is(err, media.ErrPilkMissing) {
			pkgLog().Warn(fmt.Sprintf("[%s] pilk not installed — cannot decode SILK audio. Run: %s", a.logTag(), pm.InstallHint("silk")))
			return ""
		}
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("[%s] pilk %s conversion failed: %v", a.logTag(), attempt.how, err))
			continue
		}
		if wavOK(wavPath) {
			info, _ := os.Stat(wavPath)
			pkgLog().Debug(fmt.Sprintf("[%s] pilk converted %s%s to wav (%d bytes)", a.logTag(), pathutil.Name(srcPath), attempt.label, info.Size()))
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
		pkgLog().Debug(fmt.Sprintf("[%s] raw PCM fallback failed: %v", a.logTag(), err))
		return ""
	}
	return wavPath
}

// Converts audio to 16 kHz mono WAV with ffmpeg, empty when ffmpeg is missing, fails, times out or writes nothing
func (a *QQAdapter) convertFFmpegToWAV(ctx context.Context, srcPath, wavPath string) string {
	proc, err := executil.Run(ctx, []string{"ffmpeg", "-y", "-i", srcPath, "-ar", "16000", "-ac", "1", wavPath},
		executil.Options{Timeout: 30 * time.Second})
	switch {
	case ctx.Err() != nil:
		return ""
	case err != nil:
		pkgLog().Warn(fmt.Sprintf("[%s] ffmpeg conversion error: %v", a.logTag(), err))
		return ""
	}
	if proc.Returncode != 0 {
		pkgLog().Warn(fmt.Sprintf("[%s] ffmpeg failed for %s: %s", a.logTag(), pathutil.Name(srcPath),
			strings.ToValidUTF8(string(proc.Stderr[:min(200, len(proc.Stderr))]), "\uFFFD")))
		return ""
	}
	if !wavOK(wavPath) {
		pkgLog().Warn(fmt.Sprintf("[%s] ffmpeg produced no/small output for %s", a.logTag(), pathutil.Name(srcPath)))
		return ""
	}
	info, _ := os.Stat(wavPath)
	pkgLog().Debug(fmt.Sprintf("[%s] ffmpeg converted %s to wav (%d bytes)", a.logTag(), pathutil.Name(srcPath), info.Size()))
	return wavPath
}

// Resolved STT backend of the adapter
type sttConfig struct {
	baseURL, apiKey, model string
	timeout                float64
}

// STT backend from the qqbot.stt settings and the QQ_STT_API_KEY credential, nil when voice to text is off or no key is set
func (a *QQAdapter) resolveSTTConfig(ctx context.Context) (*sttConfig, error) {
	sctx := gateway.SettingsContext(ctx)
	if !settings.QqbotSttEnabled.Get(sctx) {
		return nil, nil
	}
	key, err := gateway.GetScopedSecret(ctx, "QQ_STT_API_KEY", "", false)
	if err != nil || key == "" {
		return nil, err
	}
	provider := settings.QqbotSttProvider.Get(sctx)
	baseURL := strings.TrimSpace(settings.QqbotSttBaseURL.Get(sctx))
	if baseURL == "" {
		baseURL = sttProviderBaseURLs[provider]
	}
	model := strings.TrimSpace(settings.QqbotSttModel.Get(sctx))
	if model == "" {
		model = sttProviderModels[provider]
	}
	return &sttConfig{strings.TrimRight(baseURL, "/"), key, model, settings.QqbotSttTimeout.Get(sctx).Seconds()}, nil
}

// Transcribes a WAV through an OpenAI compatible STT API, empty when STT is unconfigured or the call fails
func (a *QQAdapter) callSTT(ctx context.Context, wavPath string) (string, error) {
	cfg, err := a.resolveSTTConfig(ctx)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		pkgLog().Warn(fmt.Sprintf("[%s] STT not configured (voice to text is off or QQ_STT_API_KEY is not set)", a.logTag()))
		return "", nil
	}
	transcript, err := a.postTranscription(ctx, cfg, wavPath)
	if err != nil && ctx.Err() == nil && (errorutil.IsSystem(err) || isHTTPStatusError(err)) {
		pkgLog().Warn(fmt.Sprintf("[%s] STT API call failed (model=%s, base=%s): %v", a.logTag(), cfg.model, textutil.First(cfg.baseURL, 50), err))
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
		return "", errors.New("HTTP client not initialized")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", cfg.model); err != nil {
		return "", err
	}
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, pathutil.Name(wavPath)))
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
	if err := resp.CheckStatus(); err != nil {
		return "", err
	}
	decoded, err := resp.JSON()
	if err != nil {
		return "", err
	}
	result, ok := jsonutil.AsMap(decoded)
	if !ok {
		return "", notDictErr(decoded)
	}
	content := ""
	if choices := getOr(result, "choices", []any{}); anyutil.IsTrue(choices) {
		list, isList := choices.([]any)
		if !isList || len(list) == 0 {
			return "", fmt.Errorf("transcription choices: expected a non-empty array, got %s", anyutil.Kind(choices))
		}
		first, isDict := jsonutil.AsMap(list[0])
		if !isDict {
			return "", notDictErr(list[0])
		}
		message, isDict := jsonutil.AsMap(getOr(first, "message", map[string]any{}))
		if !isDict {
			return "", notDictErr(first["message"])
		}
		text, isStr := getOr(message, "content", "").(string)
		if !isStr {
			return "", fmt.Errorf("transcription message content: expected a string, got %s", anyutil.Kind(message["content"]))
		}
		content = text
	}
	if stripped := strings.TrimSpace(content); stripped != "" {
		return stripped, nil
	}
	text, isStr := getOr(result, "text", "").(string)
	if !isStr {
		return "", fmt.Errorf("transcription text: expected a string, got %s", anyutil.Kind(result["text"]))
	}
	return strings.TrimSpace(text), nil
}

// Converts voice bytes to WAV and caches the result, the original bytes cached under their extension when conversion fails
func (a *QQAdapter) convertAudioToWAV(ctx context.Context, audioData []byte, sourceURL string) (string, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(pathutil.Suffix(parsed.Path))
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
		pkgLog().Warn(fmt.Sprintf("[%s] audio conversion failed for %s (format=%s)", a.logTag(), textutil.First(sourceURL, 60), ext))
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
	pkgLog().Debug(fmt.Sprintf("[%s] Failed to read converted wav: %v", a.logTag(), err))
	return "", nil
}
