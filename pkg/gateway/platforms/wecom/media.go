package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Size caps of WeCom native media and the chunked upload protocol
const (
	imageMaxBytes    = 10 * 1024 * 1024
	videoMaxBytes    = 10 * 1024 * 1024
	voiceMaxBytes    = 2 * 1024 * 1024
	fileMaxBytes     = 20 * 1024 * 1024
	uploadChunkSize  = 512 * 1024
	maxUploadChunks  = 100
	mediaIdleTimeout = 30 * time.Second
	octetStream      = "application/octet-stream"
)

// Native type caps with the catalog key of the kind label and the cap shown in the downgrade notice
var typeLimits = map[string]struct {
	maxBytes int
	labelKey string
	cap      string
}{
	"image": {imageMaxBytes, "platform.wecom.media.kind_image", "10MB"},
	"video": {videoMaxBytes, "platform.wecom.media.kind_video", "10MB"},
	"voice": {voiceMaxBytes, "platform.wecom.media.kind_voice", "2MB"},
}

// Mime types of the common attachment extensions, the system table covering the rest
var extMimes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff", ".svg": "image/svg+xml",
	".ico": "image/vnd.microsoft.icon", ".heic": "image/heic", ".heif": "image/heif", ".avif": "image/avif",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".avi": "video/x-msvideo", ".mpeg": "video/mpeg",
	".mpg": "video/mpeg", ".webm": "video/webm", ".mkv": "video/x-matroska", ".3gp": "video/3gpp",
	".mp3": "audio/mpeg", ".wav": "audio/x-wav", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/opus",
	".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac", ".amr": "audio/amr",
	".pdf": "application/pdf", ".doc": "application/msword", ".xls": "application/vnd.ms-excel", ".ppt": "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".txt":  "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".html": "text/html", ".htm": "text/html",
	".json": "application/json", ".xml": "text/xml", ".zip": "application/zip", ".gz": "application/gzip",
	".tar": "application/x-tar", ".7z": "application/x-7z-compressed", ".rar": "application/vnd.rar",
}

var (
	placeholderRE = regexp.MustCompile(`^<[^>\n]+>$`)
	dispositionRE = regexp.MustCompile(`filename="?([^";]+)"?`)
)

// Mime type of a file name by extension, empty when unknown
func mimeForName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	if m, ok := extMimes[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return strings.TrimSpace(strings.SplitN(m, ";", 2)[0])
	}
	return ""
}

// Mime type of an extension, the fallback when unknown
func mimeForExt(ext, fallback string) string {
	if m := mimeForName("x" + ext); m != "" {
		return m
	}
	return fallback
}

// Extension of a mime type, empty for unknown types and the generic octet stream
func extForMime(contentType string) string {
	ext := gateway.ExtForMime(contentType, nil, false, true, "")
	if ext == ".bin" {
		return ""
	}
	return ext
}

// Image extension from the magic bytes, jpg when unrecognized
func detectImageExt(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return ".jpg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return ".gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return ".webp"
	}
	return ".jpg"
}

// Path part of a URL, empty when it does not parse
func urlPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// Extension from the content type, then the URL path, then the fallback, never the generic bin
func guessExtension(rawURL, contentType, fallback string) string {
	if ext := extForMime(contentType); ext != "" {
		return ext
	}
	if ext := path.Ext(urlPath(rawURL)); ext != "" {
		return ext
	}
	return fallback
}

// File name from Content-Disposition, else the URL path, given an extension from the content type when it has none
func guessFilename(rawURL, disposition, contentType string) string {
	if m := dispositionRE.FindStringSubmatch(disposition); m != nil {
		return m[1]
	}
	name := path.Base(strings.TrimRight(urlPath(rawURL), "/"))
	if name == "." || name == "/" || name == "" {
		name = "document"
	}
	if strings.Contains(name, ".") {
		return name
	}
	ext := gateway.ExtForMime(contentType, nil, false, true, "")
	if ext == "" {
		ext = ".bin"
	}
	return name + ext
}

// Mime type of a file name, amr voice and the octet stream as fallbacks
func guessMimeType(filename string) string {
	if m := mimeForName(filename); m != "" {
		return m
	}
	if strings.ToLower(filepath.Ext(filename)) == ".amr" {
		return "audio/amr"
	}
	return octetStream
}

// Lowercased mime type, falling back to the file name when it is blank or a generic label
func normalizeContentType(contentType, filename string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if normalized != "" && normalized != octetStream && normalized != "text/plain" {
		return normalized
	}
	return guessMimeType(filename)
}

// Native WeCom media kind of a mime type
func detectMediaType(contentType string) string {
	m := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case m == "application/ogg":
		return "voice"
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "audio/"):
		return "voice"
	}
	return "file"
}

// Native type a file goes out as, a rejection above the absolute cap and a downgrade to file past a type cap
type sizeVerdict struct {
	finalType     string
	rejected      bool
	rejectReason  string
	downgraded    bool
	downgradeNote string
}

// Applies the absolute, voice format and per type size rules to an outbound file
func applyFileSizeLimits(ctx context.Context, size int, detectedType, contentType string) sizeVerdict {
	sizeMB := fmt.Sprintf("%.2f", float64(size)/(1024*1024))
	kind := strings.ToLower(detectedType)
	if kind == "" {
		kind = "file"
	}
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if size > fileMaxBytes {
		return sizeVerdict{finalType: kind, rejected: true,
			rejectReason: i18n.T(ctx, "platform.wecom.media.file_too_large", "", map[string]any{"size_mb": sizeMB})}
	}
	if kind == "voice" && ct != "" && ct != "audio/amr" {
		return sizeVerdict{finalType: "file", downgraded: true,
			downgradeNote: i18n.T(ctx, "platform.wecom.media.voice_format_downgraded", "", map[string]any{"content_type": ct})}
	}
	if limit, ok := typeLimits[kind]; ok && size > limit.maxBytes {
		return sizeVerdict{finalType: "file", downgraded: true, downgradeNote: i18n.T(ctx, "platform.wecom.media.size_downgraded", "",
			map[string]any{"label": i18n.T(ctx, limit.labelKey, "", nil), "size_mb": sizeMB, "cap": limit.cap})}
	}
	return sizeVerdict{finalType: kind}
}

// Bytes of an inline base64 attachment, data URL prefixes and line breaks tolerated
func decodeBase64(data string) ([]byte, error) {
	if _, after, found := strings.Cut(data, ","); found {
		data = after
	}
	data = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, data)
	if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(data, "="))
}

// Plaintext of an AES-256-CBC media download keyed by its unpadded base64 aeskey
func decryptFileBytes(encrypted []byte, aesKey string) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, errors.New("encrypted_data is empty")
	}
	if aesKey == "" {
		return nil, errors.New("aes_key is required")
	}
	key, err := base64.StdEncoding.DecodeString(aesKey + strings.Repeat("=", (4-len(aesKey)%4)%4))
	if err != nil {
		return nil, fmt.Errorf("invalid WeCom AES key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("Invalid WeCom AES key length: expected 32 bytes, got %d", len(key))
	}
	if len(encrypted)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted media is not a whole number of AES blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, encrypted)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain) {
		return nil, fmt.Errorf("Invalid PKCS#7 padding value: %d", pad)
	}
	if !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("Invalid PKCS#7 padding: padding bytes mismatch")
	}
	return plain[:len(plain)-pad], nil
}

// Media HTTP client of the connection, or a fresh SSRF guarded one and its release
func (a *WeComAdapter) mediaClient(ctx context.Context) (*http.Client, func()) {
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client != nil {
		return client, func() {}
	}
	client = newMediaHTTPClient(ctx)
	return client, client.CloseIdleConnections
}

// SSRF guarded client without a total deadline, downloads bounded by an idle timeout instead
func newMediaHTTPClient(ctx context.Context) *http.Client {
	noTotal := time.Duration(0)
	return gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{Timeout: &noTotal})
}

// Body and lowercased headers of an SSRF checked GET capped at maxBytes and cut off after an idle stall
func (a *WeComAdapter) downloadRemoteBytes(ctx context.Context, rawURL string, maxBytes int) ([]byte, map[string]string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		return nil, nil, errors.New("Blocked unsafe URL (SSRF protection): " + gateway.SafeURLForLog(rawURL, 80))
	}
	client, release := a.mediaClient(ctx)
	defer release()
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(mediaIdleTimeout, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "icebath-agent/1.0")
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, gateway.SafeURLForLog(rawURL, 80))
	}
	headers := map[string]string{}
	for k := range resp.Header {
		headers[strings.ToLower(k)] = resp.Header.Get(k)
	}
	if n, err := strconv.Atoi(headers["content-length"]); err == nil && n > maxBytes {
		return nil, nil, fmt.Errorf("Remote media exceeds WeCom limit: %d bytes > %d bytes", n, maxBytes)
	}
	var out bytes.Buffer
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(mediaIdleTimeout)
			out.Write(buf[:n])
			if out.Len() > maxBytes {
				return nil, nil, fmt.Errorf("Remote media exceeds WeCom limit while downloading: %d bytes > %d bytes", out.Len(), maxBytes)
			}
		}
		if rerr == io.EOF {
			return out.Bytes(), headers, nil
		}
		if rerr != nil {
			if ctx.Err() == nil && reqCtx.Err() != nil {
				return nil, nil, fmt.Errorf("media download stalled for %s", mediaIdleTimeout)
			}
			return nil, nil, rerr
		}
	}
}

// One attachment reference of a callback, its kind and its fields
type mediaRef struct {
	kind  string
	media map[string]any
}

// Attachments of a callback body cached locally, their paths and mime types in order
func (a *WeComAdapter) extractMedia(ctx context.Context, body map[string]any) ([]string, []string) {
	var refs []mediaRef
	ref := func(kind string, container map[string]any) bool {
		m, ok := container[kind].(map[string]any)
		if ok {
			refs = append(refs, mediaRef{kind, m})
		}
		return ok
	}
	msgtype := strings.ToLower(textOf(body["msgtype"]))
	if msgtype == "mixed" {
		items, _ := objAt(body, "mixed")["msg_item"].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && strings.ToLower(textOf(m["msgtype"])) == "image" {
				ref("image", m)
			}
		}
	} else {
		ref("image", body)
		if msgtype == "file" {
			ref("file", body)
		}
		if appmsg, ok := body["appmsg"].(map[string]any); ok && msgtype == "appmsg" {
			if !ref("file", appmsg) {
				ref("image", appmsg)
			}
		}
	}
	quote := objAt(body, "quote")
	if qt := strings.ToLower(textOf(quote["msgtype"])); qt == "image" || qt == "file" {
		ref(qt, quote)
	}
	var urls, types []string
	for _, r := range refs {
		if p, m, ok := a.cacheMedia(ctx, r.kind, r.media); ok {
			urls, types = append(urls, p), append(types, m)
		}
	}
	return urls, types
}

// Caches one inbound image or file from inline base64 or its download URL, decrypting by its aeskey
func (a *WeComAdapter) cacheMedia(ctx context.Context, kind string, media map[string]any) (string, string, bool) {
	if inline := textOf(media["base64"]); inline != "" {
		raw, err := decodeBase64(inline)
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] Failed to decode %s base64 media: %v", a.Name(), kind, err))
			return "", "", false
		}
		filename := textOf(media["filename"])
		if filename == "" {
			filename = textOf(media["name"])
		}
		if filename == "" {
			filename = "wecom_file"
		}
		docMime := mimeForName(filename)
		if docMime == "" {
			docMime = octetStream
		}
		return a.storeMedia(ctx, kind, raw, detectImageExt(raw), "", filename, docMime, "")
	}
	rawURL := strAt(media, "url")
	if rawURL == "" {
		return "", "", false
	}
	aesKey := strAt(media, "aeskey")
	if unescaped, err := url.PathUnescape(aesKey); err == nil {
		aesKey = unescaped
	}
	raw, headers, err := a.downloadRemoteBytes(ctx, rawURL, fileMaxBytes)
	step := "download"
	if err == nil && aesKey != "" {
		step = "decrypt"
		raw, err = decryptFileBytes(raw, aesKey)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to %s %s from %s: %v", a.Name(), step, kind, gateway.SafeURLForLog(rawURL, 80), err))
		return "", "", false
	}
	contentType := strings.TrimSpace(strings.SplitN(headers["content-type"], ";", 2)[0])
	if contentType == "" {
		contentType = octetStream
	}
	ext := guessExtension(rawURL, contentType, detectImageExt(raw))
	imageMime := ""
	if strings.HasPrefix(contentType, "image/") {
		imageMime = contentType
	}
	return a.storeMedia(ctx, kind, raw, ext, imageMime, guessFilename(rawURL, headers["content-disposition"], contentType), contentType,
		" from "+gateway.SafeURLForLog(rawURL, 80))
}

// Caches bytes as an image or a document, the path and mime type, non image bytes for an image refused
func (a *WeComAdapter) storeMedia(ctx context.Context, kind string, raw []byte, ext, imageMime, filename, docMime, origin string) (string, string, bool) {
	if kind != "image" {
		p, err := gateway.CacheDocumentFromBytesAsync(ctx, raw, filename)
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Failed to cache document%s: %v", a.Name(), origin, err))
			return "", "", false
		}
		return p, docMime, true
	}
	p, err := gateway.CacheImageFromBytesAsync(ctx, raw, ext)
	if err != nil {
		var valueErr *mediacache.ValueError
		if errors.As(err, &valueErr) {
			slog.Warn(fmt.Sprintf("[%s] Rejected non-image bytes%s: %v", a.Name(), origin, err))
		} else {
			slog.Warn(fmt.Sprintf("[%s] Failed to cache image%s: %v", a.Name(), origin, err))
		}
		return "", "", false
	}
	if imageMime == "" {
		imageMime = mimeForExt(ext, "image/jpeg")
	}
	return p, imageMime, true
}

// Local media path that does not exist
type mediaNotFoundError struct{ path string }

func (e *mediaNotFoundError) Error() string { return "Media file not found: " + e.path }

// Whether the source is an http or https URL
func looksLikeURL(source string) bool {
	u, err := url.Parse(strings.TrimSpace(source))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// Absolute local path of a plain or file URL source, a home relative tilde expanded
func localMediaPath(source string) (string, error) {
	p := source
	if u, err := url.Parse(source); err == nil && u.Scheme == "file" {
		p = u.Path
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// Bytes, mime type and file name of an outbound URL or local file
func (a *WeComAdapter) loadOutboundMedia(ctx context.Context, source, fileName string) ([]byte, string, string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, "", "", errors.New("media source is required")
	}
	if placeholderRE.MatchString(source) {
		return nil, "", "", errors.New("Media placeholder was not replaced with a real file path: " + source)
	}
	if looksLikeURL(source) {
		data, headers, err := a.downloadRemoteBytes(ctx, source, fileMaxBytes)
		if err != nil {
			return nil, "", "", err
		}
		name := fileName
		if name == "" {
			name = guessFilename(source, headers["content-disposition"], headers["content-type"])
		}
		return data, normalizeContentType(headers["content-type"], name), name, nil
	}
	local, err := localMediaPath(source)
	if err != nil {
		return nil, "", "", err
	}
	if info, err := os.Stat(local); err != nil || !info.Mode().IsRegular() {
		return nil, "", "", &mediaNotFoundError{local}
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return nil, "", "", err
	}
	name := fileName
	if name == "" {
		name = filepath.Base(local)
	}
	return data, normalizeContentType("", name), name, nil
}

// Outbound media with its detected and final native type
type preparedMedia struct {
	data         []byte
	contentType  string
	fileName     string
	detectedType string
	sizeVerdict
}

// Loads outbound media and applies the WeCom size and format rules to it
func (a *WeComAdapter) prepareOutboundMedia(ctx context.Context, source, fileName string) (*preparedMedia, error) {
	data, contentType, name, err := a.loadOutboundMedia(ctx, source, fileName)
	if err != nil {
		return nil, err
	}
	detected := detectMediaType(contentType)
	return &preparedMedia{data: data, contentType: contentType, fileName: name, detectedType: detected,
		sizeVerdict: applyFileSizeLimits(ctx, len(data), detected, contentType)}, nil
}

// Uploads bytes through the init, chunk and finish commands and returns the finish answer with its media_id
func (a *WeComAdapter) uploadMediaBytes(ctx context.Context, data []byte, mediaType, filename string) (map[string]any, error) {
	if len(data) == 0 {
		return nil, errors.New("Cannot upload empty media")
	}
	totalChunks := (len(data) + uploadChunkSize - 1) / uploadChunkSize
	if totalChunks > maxUploadChunks {
		return nil, fmt.Errorf("File too large: %d chunks exceeds maximum of %d chunks", totalChunks, maxUploadChunks)
	}
	sum := md5.Sum(data)
	initResponse, err := a.checkedRequest(ctx, cmdUploadInit, map[string]any{"type": mediaType, "filename": filename,
		"total_size": len(data), "total_chunks": totalChunks, "md5": hex.EncodeToString(sum[:])}, "media upload init")
	if err != nil {
		return nil, err
	}
	uploadID := strAt(objAt(initResponse, "body"), "upload_id")
	if uploadID == "" {
		return nil, fmt.Errorf("media upload init failed: missing upload_id in response %v", initResponse)
	}
	for index := 0; index < totalChunks; index++ {
		chunk := data[index*uploadChunkSize : min((index+1)*uploadChunkSize, len(data))]
		if _, err := a.checkedRequest(ctx, cmdUploadChunk, map[string]any{"upload_id": uploadID, "chunk_index": index,
			"base64_data": base64.StdEncoding.EncodeToString(chunk)}, fmt.Sprintf("media upload chunk %d", index)); err != nil {
			return nil, err
		}
	}
	finishResponse, err := a.checkedRequest(ctx, cmdUploadFinish, map[string]any{"upload_id": uploadID}, "media upload finish")
	if err != nil {
		return nil, err
	}
	finish := objAt(finishResponse, "body")
	mediaID := strAt(finish, "media_id")
	if mediaID == "" {
		return nil, fmt.Errorf("media upload finish failed: missing media_id in response %v", finishResponse)
	}
	uploadedType := textOf(finish["type"])
	if uploadedType == "" {
		uploadedType = mediaType
	}
	return map[string]any{"type": uploadedType, "media_id": mediaID, "created_at": finish["created_at"]}, nil
}

// Native media message body of an uploaded media_id
func mediaBody(mediaType, mediaID string) map[string]any {
	return map[string]any{"msgtype": mediaType, mediaType: map[string]any{"media_id": mediaID}}
}

// Markdown follow up after a media send, a failure logged and returned
func (a *WeComAdapter) sendFollowupMarkdown(ctx context.Context, chatID, content, replyTo string) gateway.SendResult {
	result, err := a.Send(ctx, chatID, content, replyTo, nil)
	if err != nil {
		result = gateway.SendResult{Success: false, Error: err.Error()}
	}
	if !result.Success {
		slog.Warn(fmt.Sprintf("[%s] Follow-up markdown send failed: %v", a.Name(), result.Error))
	}
	return result
}

// Uploads and sends one file natively, passive while a req_id is usable, with the caption and any downgrade notice following
func (a *WeComAdapter) sendMediaSource(ctx context.Context, chatID, source, caption, fileName, replyTo string) (gateway.SendResult, error) {
	if chatID == "" {
		return gateway.SendResult{Success: false, Error: "chat_id is required"}, nil
	}
	prepared, err := a.prepareOutboundMedia(ctx, source, fileName)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		var notFound *mediaNotFoundError
		if !errors.As(err, &notFound) {
			slog.Error(fmt.Sprintf("[%s] Failed to prepare outbound media %s: %v", a.Name(), source, err))
		}
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if prepared.rejected {
		if text := a.WarningText(ctx, "⚠️ "+prepared.rejectReason, &caption, "", chatID, nil); text != nil && *text != "" {
			a.sendFollowupMarkdown(ctx, chatID, *text, replyTo)
		}
		return gateway.SendResult{Success: false, Error: prepared.rejectReason}, nil
	}
	replyReqID := a.cachedReplyReqID(chatID, replyTo)
	if a.Streamer.hasActiveTurn(chatID) || a.Streamer.isExpired(chatID) {
		replyReqID = ""
	}
	upload, err := a.uploadMediaBytes(ctx, prepared.data, prepared.finalType, prepared.fileName)
	var mediaResponse map[string]any
	if err == nil {
		mediaID := textOf(upload["media_id"])
		slog.Info(fmt.Sprintf("[%s] upload_media_bytes OK: media_id=%s type=%s", a.Name(), mediaID, prepared.finalType))
		body := mediaBody(prepared.finalType, mediaID)
		op := "send_media_message"
		if replyReqID != "" {
			op = "send_reply_media"
			if mediaResponse, err = a.sendReplyRequest(ctx, replyReqID, body); err == nil {
				err = raiseForWeComError(mediaResponse, "send reply media message")
			}
		} else {
			body["chatid"] = chatID
			mediaResponse, err = a.checkedRequest(ctx, cmdSend, body, "send media message")
		}
		if err == nil {
			slog.Info(fmt.Sprintf("[%s] %s OK: %v", a.Name(), op, mediaResponse))
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		if errors.Is(err, errRequestTimeout) {
			slog.Error(fmt.Sprintf("[%s] TIMEOUT sending media %s", a.Name(), source))
			return gateway.SendResult{Success: false, Error: "Timeout sending media to WeCom"}, nil
		}
		slog.Error(fmt.Sprintf("[%s] Failed to send media %s: %v", a.Name(), source, err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	raw := map[string]any{"upload": upload, "media": mediaResponse}
	downgrade := ""
	if prepared.downgraded && prepared.downgradeNote != "" {
		empty := ""
		if text := a.WarningText(ctx, "ℹ️ "+prepared.downgradeNote, &empty, "", chatID, nil); text != nil {
			downgrade = *text
		}
	}
	for _, followup := range []struct{ key, text string }{{"caption", caption}, {"downgrade", downgrade}} {
		raw[followup.key], raw[followup.key+"_error"] = nil, nil
		if followup.text == "" {
			continue
		}
		result := a.sendFollowupMarkdown(ctx, chatID, followup.text, replyTo)
		raw[followup.key] = result.RawResponse
		if !result.Success {
			raw[followup.key+"_error"] = result.Error
		}
	}
	id := payloadReqID(mediaResponse)
	if id == "" {
		id = randHex(6)
	}
	return gateway.SendResult{Success: true, MessageID: id, RawResponse: raw}, nil
}

// Image by URL or path sent natively, a URL that fails falling back to a text link
func (a *WeComAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	result, err := a.sendMediaSource(ctx, chatID, imageURL, caption, "", replyTo)
	if err != nil || result.Success || !looksLikeURL(imageURL) {
		return result, err
	}
	slog.Warn(fmt.Sprintf("[%s] Falling back to text send for image URL %s: %v", a.Name(), gateway.SafeURLForLog(imageURL, 80), result.Error))
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	return a.Send(ctx, chatID, text, replyTo, nil)
}

// Local image sent natively
func (a *WeComAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, imagePath, caption, "", replyTo)
}

// Local document sent natively under its given file name
func (a *WeComAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	slog.Info(fmt.Sprintf("[%s] send_document called: chat=%s file=%s", a.Name(), chatID, filePath))
	return a.sendMediaSource(ctx, chatID, filePath, caption, fileName, replyTo)
}

// Audio sent as a voice message when AMR, otherwise as a file
func (a *WeComAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, audioPath, caption, "", replyTo)
}

// Local video sent natively, oversized ones as a file
func (a *WeComAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, videoPath, caption, "", replyTo)
}
