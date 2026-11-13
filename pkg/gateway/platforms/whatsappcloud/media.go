package whatsappcloud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var httpPrefixes = []string{"http://", "https://"}

// Whether a media source is an http or https URL rather than a local path
func isHTTPSource(source string) bool {
	for _, p := range httpPrefixes {
		if strings.HasPrefix(source, p) {
			return true
		}
	}
	return false
}

// Inbound media cache directory under the profile home, the legacy whatsapp_cloud/media when populated
func inboundMediaCache(ctx context.Context) string {
	return home.HermesDir(home.Dir(ctx), "platforms/whatsapp_cloud/media", "whatsapp_cloud/media")
}

// Mime of a path from its extension, the per kind default when it guesses none
func guessMime(path, kind string) string {
	if typ, _, err := pymimetypes.GuessType(path, true); err == nil && typ != "" {
		return typ
	}
	if m := defaultMime[kind]; m != "" {
		return m
	}
	return "application/octet-stream"
}

// Uploads a local file to Graph media, the media id or the error text
func (a *WhatsAppCloudAdapter) uploadMedia(ctx context.Context, filePath, mediaKind, mimeType string) (any, string, error) {
	client := a.client()
	if client == nil {
		return nil, "Not connected", nil
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, "File not found: " + filePath, nil
	}
	limit, ok := mediaSizeLimits[mediaKind]
	if !ok {
		limit = mediaSizeLimits["document"]
	}
	if size := info.Size(); size > limit {
		return nil, fmt.Sprintf("File %s is %d bytes; Cloud API %s cap is %d bytes", pypath.Basename(filePath), size, mediaKind, limit), nil
	}
	if mimeType == "" {
		mimeType = guessMime(filePath, mediaKind)
	}
	resp, err := func() (*pyhttpx.Response, error) {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, &pyErr{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
		}
		body, contentType, err := gateway.EncodeHTTPXMultipart([]gateway.MultipartPart{
			{Name: "file", Filename: pypath.Basename(filePath), ContentType: mimeType, Data: data},
			{Name: "messaging_product", Data: []byte("whatsapp")},
			{Name: "type", Data: []byte(mimeType)},
		})
		if err != nil {
			return nil, err
		}
		headers := a.authHeaders(false)
		headers["Content-Type"] = contentType
		return doRequest(ctx, client, http.MethodPost, a.graphURL("media"), headers, body)
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		slog.Error("[whatsapp_cloud] media upload failed: " + err.Error())
		return nil, pyrepr.ExceptionMessage(err), nil
	}
	if resp.StatusCode != http.StatusOK {
		msg, err := responseError(resp)
		return nil, msg, err
	}
	var mediaID any
	if decoded, err := resp.JSON(); err == nil {
		if m, ok := pyjson.Dict(decoded); ok {
			mediaID = m["id"]
		}
	}
	if !pyrepr.Truthy(mediaID) {
		return nil, "Upload response missing 'id'", nil
	}
	return mediaID, "", nil
}

// Media message referencing exactly one of an uploaded id or a public link
func (a *WhatsAppCloudAdapter) sendMedia(ctx context.Context, chatID, mediaKind string, mediaID any, mediaLink, caption, filename, replyTo string) (gateway.SendResult, error) {
	if a.client() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if pyrepr.Truthy(mediaID) == (mediaLink != "") {
		return gateway.SendResult{Success: false, Error: "Exactly one of media_id or media_link must be set"}, nil
	}
	block := pyjson.ObjectOf("link", mediaLink)
	if pyrepr.Truthy(mediaID) {
		block = pyjson.ObjectOf("id", mediaID)
	}
	if caption != "" && (mediaKind == "image" || mediaKind == "video" || mediaKind == "document") {
		block.Set("caption", caption)
	}
	if filename != "" && mediaKind == "document" {
		block.Set("filename", filename)
	}
	var quote any
	if replyTo != "" {
		quote = replyTo
	}
	return a.postMessageResult(ctx, outboundPayload(chatID, mediaKind, block, quote), postLogs{fail: "[whatsapp_cloud] media send failed",
		reject: "[whatsapp_cloud] media send rejected (status=%d, kind=%s): %s", rejectArgs: []any{mediaKind}})
}

// URL sources sent by link, local paths uploaded and sent by id then indexed for later quotes
func (a *WhatsAppCloudAdapter) sendMediaFromPathOrLink(ctx context.Context, chatID, source, mediaKind, caption, filename, replyTo, mimeType string) (gateway.SendResult, error) {
	var mediaID any
	link := source
	if !isHTTPSource(source) {
		id, errMsg, err := a.uploadMedia(ctx, source, mediaKind, mimeType)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if errMsg != "" {
			return gateway.SendResult{Success: false, Error: errMsg}, nil
		}
		mediaID, link = id, ""
	}
	result, err := a.sendMedia(ctx, chatID, mediaKind, mediaID, link, caption, filename, replyTo)
	if err != nil {
		return result, err
	}
	if result.Success && pyrepr.Truthy(result.MessageID) && link == "" {
		mime := mimeType
		if mime == "" {
			mime = guessMime(source, mediaKind)
		}
		gateway.RichSentRecordMedia(ctx, chatID, result.MessageID, [][2]string{{source, mime}})
		if caption != "" {
			gateway.RichSentRecord(ctx, chatID, result.MessageID, caption)
		}
	}
	return result, nil
}

// Image URL or path sent as an image message
func (a *WhatsAppCloudAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendMediaFromPathOrLink(ctx, chatID, imageURL, "image", caption, "", replyTo, "")
}

// Local image sent as an image message
func (a *WhatsAppCloudAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaFromPathOrLink(ctx, chatID, imagePath, "image", caption, "", replyTo, "")
}

// Local video sent as a video message
func (a *WhatsAppCloudAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaFromPathOrLink(ctx, chatID, videoPath, "video", caption, "", replyTo, "")
}

// Local file sent as a document under its given name or its basename
func (a *WhatsAppCloudAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if fileName == "" {
		fileName = pypath.Basename(filePath)
	}
	return a.sendMediaFromPathOrLink(ctx, chatID, filePath, "document", caption, fileName, replyTo, "")
}

// Audio as a voice bubble, a local MP3 converted to Ogg Opus first unless is_voice is exactly false
func (a *WhatsAppCloudAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	mimeType := ""
	isVoice, isBool := extra["is_voice"].(bool)
	isMP3 := strings.HasSuffix(pystr.Lower(audioPath), ".mp3")
	if isBool && !isVoice && isMP3 {
		mimeType = "audio/mpeg"
	} else if !isHTTPSource(audioPath) && isMP3 && pathExists(audioPath) {
		opusPath, err := a.convertToOpus(ctx, audioPath)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if opusPath != "" {
			defer os.Remove(opusPath)
			return a.sendMediaFromPathOrLink(ctx, chatID, opusPath, "audio", caption, "", replyTo, "audio/ogg; codecs=opus")
		}
		mimeType = "audio/mpeg"
	}
	return a.sendMediaFromPathOrLink(ctx, chatID, audioPath, "audio", caption, "", replyTo, mimeType)
}

// Whether a path exists, directories included
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// MP3 transcoded to a sibling Ogg Opus file, empty when ffmpeg is missing with a one time warning
func (a *WhatsAppCloudAdapter) convertToOpus(ctx context.Context, mp3Path string) (string, error) {
	if ffmpegPath == "" {
		a.mu.Lock()
		warn := !a.warnedNoFFmpeg
		a.warnedNoFFmpeg = true
		a.mu.Unlock()
		if warn {
			slog.Warn("[whatsapp_cloud] ffmpeg not found on PATH — voice messages will be delivered as MP3 audio " +
				"attachments instead of native voice notes (green waveform bubble). Install ffmpeg to enable: " +
				"Windows `winget install Gyan.FFmpeg`, macOS `brew install ffmpeg`, Linux package manager.")
		}
		return "", nil
	}
	output := mp3Path
	if i := strings.LastIndex(mp3Path, "."); i >= 0 {
		output = mp3Path[:i]
	}
	out, err := gateway.TranscodeToOggOpus(ctx, mp3Path, "32k", 60*time.Second, output+".ogg")
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out, nil
}

// Authenticated Graph GET logged uniformly on failure, nil on a transport error or non 200 status
func (a *WhatsAppCloudAdapter) graphGet(ctx context.Context, client *http.Client, url, what, mediaID string) (*pyhttpx.Response, error) {
	resp, err := doRequest(ctx, client, http.MethodGet, url, a.authHeaders(false), nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[whatsapp_cloud] media %s fetch raised (id=%s): %s", what, mediaID, err.Error()))
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		slog.Warn(fmt.Sprintf("[whatsapp_cloud] media %s fetch failed (id=%s, status=%d)", what, mediaID, resp.StatusCode))
		return nil, nil
	}
	return resp, nil
}

// Two step Graph download of a media id into the inbound cache, its path and mime or empty on failure
func (a *WhatsAppCloudAdapter) downloadMediaToCache(ctx context.Context, mediaID, extHint string) (string, string, error) {
	client := a.client()
	if client == nil {
		return "", "", nil
	}
	mediaID = pystr.Strip(mediaID)
	if ok, _ := mediaIDRE.MatchString(mediaID); !ok {
		short := []rune(mediaID)
		if len(short) > 64 {
			short = short[:64]
		}
		slog.Warn("[whatsapp_cloud] refusing malformed media id " + pyrepr.Repr(string(short)))
		return "", "", nil
	}
	metaResp, err := a.graphGet(ctx, client, fmt.Sprintf("%s/%s/%s", graphAPIBase, a.apiVersion, mediaID), "metadata", mediaID)
	if err != nil || metaResp == nil {
		return "", "", err
	}
	meta, err := metaResp.JSON()
	if err != nil {
		meta = map[string]any{}
	}
	mm, err := dictOf(meta)
	if err != nil {
		return "", "", err
	}
	tempURL := mm["url"]
	mime := pyrepr.Or(mm["mime_type"], "")
	if !pyrepr.Truthy(tempURL) {
		return "", "", nil
	}
	blobResp, err := a.graphGet(ctx, client, pyrepr.Str(tempURL), "bytes", mediaID)
	if err != nil || blobResp == nil {
		return "", "", err
	}
	mimeText := ""
	if pyrepr.Truthy(mime) {
		mimeText = pyrepr.Str(mime)
	}
	cache := inboundMediaCache(ctx)
	ext := extHint
	if ext == "" {
		ext = extForMime(mimeText)
	}
	if ext == "" {
		ext = ".bin"
	}
	outPath := filepath.Join(cache, mediaID+ext)
	if err := os.MkdirAll(cache, 0o777); err != nil {
		return "", "", &pyErr{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
	}
	if err := os.WriteFile(outPath, blobResp.Content, 0o666); err != nil {
		slog.Error(fmt.Sprintf("[whatsapp_cloud] failed to write cached media (id=%s): %s", mediaID, pypath.OSErrorText(err)))
		return "", "", nil
	}
	return outPath, mimeText, nil
}
