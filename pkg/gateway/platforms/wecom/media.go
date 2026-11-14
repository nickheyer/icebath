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
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	appCmdSend              = "aibot_send_msg"
	appCmdUploadMediaInit   = "aibot_upload_media_init"
	appCmdUploadMediaChunk  = "aibot_upload_media_chunk"
	appCmdUploadMediaFinish = "aibot_upload_media_finish"

	imageMaxBytes    = 10 * 1024 * 1024
	videoMaxBytes    = 10 * 1024 * 1024
	voiceMaxBytes    = 2 * 1024 * 1024
	fileMaxBytes     = 20 * 1024 * 1024
	absoluteMaxBytes = fileMaxBytes
	uploadChunkSize  = 512 * 1024
	maxUploadChunks  = 100
)

var voiceSupportedMimes = map[string]bool{"audio/amr": true}

// Image magic prefixes and the extension each names
var imageMagic = []struct {
	magics [][]byte
	ext    string
}{
	{[][]byte{[]byte("\x89PNG\r\n\x1a\n")}, ".png"},
	{[][]byte{[]byte("\xff\xd8\xff")}, ".jpg"},
	{[][]byte{[]byte("GIF87a"), []byte("GIF89a")}, ".gif"},
}

var mimePrefixKinds = []struct{ prefix, kind string }{{"image/", "image"}, {"video/", "video"}, {"audio/", "voice"}}

// Size cap, kind label catalog key and human cap per media type of the downgrade to file notice
var typeLimits = map[string]struct {
	maxBytes int
	labelKey string
	cap      string
}{
	"image": {imageMaxBytes, "platform.wecom.media.kind_image", "10MB"},
	"video": {videoMaxBytes, "platform.wecom.media.kind_video", "10MB"},
	"voice": {voiceMaxBytes, "platform.wecom.media.kind_voice", "2MB"},
}

var (
	dispositionFilenameRE = pyre.MustCompile(`filename="?([^";]+)"?`)
	mediaPlaceholderRE    = mustFullmatch(`<[^>\n]+>`)
)

// Python pattern that must span the whole subject, panicking on a pattern that does not compile
func mustFullmatch(pattern string) interface{ MatchString(string) (bool, error) } {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Media cache writers the adapter stores inbound media through, swappable like module globals
var (
	cacheImageFromBytes    = gateway.CacheImageFromBytesAsync
	cacheDocumentFromBytes = gateway.CacheDocumentFromBytesAsync
)

// Verdict of the size and format limits on one outbound media
type sizeVerdict struct {
	finalType     string
	rejected      bool
	rejectReason  string
	downgraded    bool
	downgradeNote string
}

// Message body naming an uploaded media id under its type
func mediaBody(mediaType, mediaID string) []any {
	return []any{"msgtype", mediaType, mediaType, obj("media_id", mediaID)}
}

// One inbound media reference with its kind
type mediaRef struct {
	kind  string
	media *pyjson.Object
}

// Caches every image and file the message or its quote carries, returning the cached paths and MIME types
func (a *WeComAdapter) extractMedia(ctx context.Context, body *pyjson.Object) ([]string, []string, error) {
	var refs []mediaRef
	ref := func(kind string, container *pyjson.Object) bool {
		media, ok := container.Vals[kind].(*pyjson.Object)
		if ok && media != nil {
			refs = append(refs, mediaRef{kind, media})
			return true
		}
		return false
	}
	msgtype := lowerOf(body.Vals["msgtype"])
	if msgtype == "mixed" {
		items, _ := dictOrEmpty(body, "mixed").Vals["msg_item"].([]any)
		for _, raw := range items {
			if item, ok := raw.(*pyjson.Object); ok && item != nil && lowerOf(item.Vals["msgtype"]) == "image" {
				ref("image", item)
			}
		}
	} else {
		ref("image", body)
		if msgtype == "file" {
			ref("file", body)
		}
		if appmsg, ok := body.Vals["appmsg"].(*pyjson.Object); msgtype == "appmsg" && ok && appmsg != nil {
			if !ref("file", appmsg) {
				ref("image", appmsg)
			}
		}
	}
	quote := dictOrEmpty(body, "quote")
	if quoteType := lowerOf(quote.Vals["msgtype"]); quoteType == "image" || quoteType == "file" {
		ref(quoteType, quote)
	}
	urls, types := []string{}, []string{}
	for _, r := range refs {
		path, mime, err := a.cacheMedia(ctx, r.kind, r.media)
		if err != nil {
			return nil, nil, err
		}
		if path != "" || mime != "" {
			urls, types = append(urls, path), append(types, mime)
		}
	}
	return urls, types, nil
}

// Caches one inbound image or file reference given inline as base64 or by URL, empty when it cannot be cached
func (a *WeComAdapter) cacheMedia(ctx context.Context, kind string, media *pyjson.Object) (string, string, error) {
	if inline := media.Vals["base64"]; pyrepr.Truthy(inline) {
		raw, err := decodeBase64(inline)
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] Failed to decode %s base64 media: %s", a.Name(), kind, errText(err)))
			return "", "", nil
		}
		filename := pyrepr.Str(pyrepr.Or(pyrepr.Or(media.Vals["filename"], media.Vals["name"]), "wecom_file"))
		docMime, _, err := pymimetypes.GuessType(filename, true)
		if err != nil {
			return "", "", err
		}
		if docMime == "" {
			docMime = "application/octet-stream"
		}
		return a.storeMedia(ctx, kind, raw, detectImageExt(raw), "", filename, docMime, "")
	}
	url := textOf(media.Vals["url"])
	if url == "" {
		return "", "", nil
	}
	aesKey := pyurl.Unquote(textOf(media.Vals["aeskey"]))
	step := "download"
	raw, headers, err := a.downloadRemoteBytes(ctx, url, absoluteMaxBytes)
	if err == nil && aesKey != "" {
		step = "decrypt"
		raw, err = decryptFileBytes(raw, aesKey)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[%s] Failed to %s %s from %s: %s", a.Name(), step, kind, url, errText(err)))
		return "", "", nil
	}
	contentType := pystr.Strip(strings.SplitN(headers["content-type"], ";", 2)[0])
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ext, err := guessExtension(url, contentType, detectImageExt(raw))
	if err != nil {
		return "", "", err
	}
	imageMime := ""
	if strings.HasPrefix(contentType, "image/") {
		imageMime = contentType
	}
	filename, err := guessFilename(url, headers["content-disposition"], contentType)
	if err != nil {
		return "", "", err
	}
	return a.storeMedia(ctx, kind, raw, ext, imageMime, filename, contentType, " from "+url)
}

// Caches bytes as an image or a document and returns the path and MIME type, empty for refused image bytes
func (a *WeComAdapter) storeMedia(ctx context.Context, kind string, raw []byte, ext, imageMime, filename, docMime, origin string) (string, string, error) {
	if kind != "image" {
		path, err := cacheDocumentFromBytes(ctx, raw, filename)
		return path, docMime, err
	}
	path, err := cacheImageFromBytes(ctx, raw, ext)
	var refused *mediacache.ValueError
	if errors.As(err, &refused) {
		slog.Warn(fmt.Sprintf("[%s] Rejected non-image bytes%s: %s", a.Name(), origin, errText(err)))
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if imageMime == "" {
		imageMime = mimeForExt(ext, "image/jpeg")
	}
	return path, imageMime, nil
}

// Bytes of a base64 text, dropping a data URL header before the comma
func decodeBase64(data any) ([]byte, error) {
	s, ok := data.(string)
	if !ok {
		return nil, &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'split'", pyrepr.TypeName(data))}
	}
	parts := strings.SplitN(s, ",", 2)
	return pybase64.B64Decode(pystr.Strip(parts[len(parts)-1]))
}

// Image extension from magic bytes, webp for a RIFF WEBP container and jpg otherwise
func detectImageExt(data []byte) string {
	for _, m := range imageMagic {
		for _, magic := range m.magics {
			if bytes.HasPrefix(data, magic) {
				return m.ext
			}
		}
	}
	if bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 12 && string(data[8:12]) == "WEBP" {
		return ".webp"
	}
	return ".jpg"
}

// MIME type the strict mimetypes table holds for an extension, else the fallback
func mimeForExt(ext, fallback string) string {
	if typ, ok := pymimetypes.TypesMapGet(pystr.Lower(ext)); ok {
		return typ
	}
	return fallback
}

// Extension from the content type unless it is the generic bin, then from the URL path, then the fallback
func guessExtension(url, contentType, fallback string) (string, error) {
	ext := ""
	if contentType != "" {
		ext = pymimetypes.GuessExtension(contentType, true)
	}
	if ext == ".bin" {
		ext = ""
	}
	if ext != "" {
		return ext, nil
	}
	parsed, err := pyurl.Parse(url)
	if err != nil {
		return "", err
	}
	if suffix := pypath.Suffix(parsed.Path); suffix != "" {
		return suffix, nil
	}
	return fallback, nil
}

// File name from the content disposition, else the URL path name with an extension guessed when it has none
func guessFilename(url, contentDisposition, contentType string) (string, error) {
	if m, _ := dispositionFilenameRE.FindStringMatch(contentDisposition); m != nil {
		return m.GroupByNumber(1).String(), nil
	}
	parsed, err := pyurl.Parse(url)
	if err != nil {
		return "", err
	}
	name := pypath.Name(parsed.Path)
	if name == "" {
		name = "document"
	}
	if strings.Contains(name, ".") {
		return name, nil
	}
	ext := pymimetypes.GuessExtension(contentType, true)
	if ext == "" {
		ext = ".bin"
	}
	return name + ext, nil
}

// AES-256-CBC decryption of a WeCom media file keyed by its unpadded base64 key, the padding checked
func decryptFileBytes(encryptedData []byte, aesKey string) ([]byte, error) {
	if len(encryptedData) == 0 {
		return nil, &pyErr{"ValueError", "encrypted_data is empty"}
	}
	if aesKey == "" {
		return nil, &pyErr{"ValueError", "aes_key is required"}
	}
	key, err := pybase64.B64Decode(aesKey + strings.Repeat("=", (4-pystr.Len(aesKey)%4)%4))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, &pyErr{"ValueError", fmt.Sprintf("Invalid WeCom AES key length: expected 32 bytes, got %d", len(key))}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(encryptedData)%aes.BlockSize != 0 {
		return nil, &pyErr{"ValueError", "The length of the provided data is not a multiple of the block length."}
	}
	decrypted := make([]byte, len(encryptedData))
	cipher.NewCBCDecrypter(block, key[:16]).CryptBlocks(decrypted, encryptedData)
	padLen := int(decrypted[len(decrypted)-1])
	if padLen < 1 || padLen > 32 || padLen > len(decrypted) {
		return nil, &pyErr{"ValueError", fmt.Sprintf("Invalid PKCS#7 padding value: %d", padLen)}
	}
	for _, b := range decrypted[len(decrypted)-padLen:] {
		if int(b) != padLen {
			return nil, &pyErr{"ValueError", "Invalid PKCS#7 padding: padding bytes mismatch"}
		}
	}
	return decrypted[:len(decrypted)-padLen], nil
}

// Downloads a remote media under the SSRF guard and the size cap, returning its bytes and lowercased headers
func (a *WeComAdapter) downloadRemoteBytes(ctx context.Context, url string, maxBytes int) ([]byte, map[string]string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), url) {
		return nil, nil, &pyErr{"ValueError", "Blocked unsafe URL (SSRF protection): " + pystr.Slice(url, 0, 80)}
	}
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		client = gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{DefaultLimits: true})
		defer client.CloseIdleConnections()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "icebath-agent/1.0")
	req.Header.Set("Accept", "*/*")
	stream, err := pyhttpx.Stream(client, req, 30*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, nil, &pyErr{"ReadTimeout", ""}
		}
		return nil, nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	defer stream.Body.Close()
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		return nil, nil, &pyErr{"HTTPStatusError", stream.StatusErrorText(url)}
	}
	headers := map[string]string{}
	for key, values := range stream.Header {
		headers[strings.ToLower(key)] = strings.Join(values, ", ")
	}
	if contentLength := headers["content-length"]; contentLength != "" && pystr.StrIsDigit(contentLength) {
		if n, ok := pystr.IntText(contentLength); ok && (!n.IsInt64() || n.Int64() > int64(maxBytes)) {
			return nil, nil, &pyErr{"ValueError", fmt.Sprintf("Remote media exceeds WeCom limit: %s bytes > %d bytes", n.String(), maxBytes)}
		}
	}
	var data []byte
	buf := make([]byte, 64*1024)
	for {
		n, rerr := stream.Body.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > maxBytes {
			return nil, nil, &pyErr{"ValueError", fmt.Sprintf("Remote media exceeds WeCom limit while downloading: %d bytes > %d bytes", len(data), maxBytes)}
		}
		if errors.Is(rerr, io.EOF) {
			return data, headers, nil
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, &pyErr{"ReadError", pyhttpx.ErrorText(rerr)}
		}
	}
}

// MIME type of a file name, audio/amr for an amr suffix the tables miss and octet stream otherwise
func guessMimeType(filename string) (string, error) {
	typ, _, err := pymimetypes.GuessType(filename, true)
	if err != nil {
		return "", err
	}
	if typ != "" {
		return typ, nil
	}
	if pystr.Lower(pypath.Suffix(filename)) == ".amr" {
		return "audio/amr", nil
	}
	return "application/octet-stream", nil
}

// Lowercased bare content type, guessed from the file name when missing or generic
func normalizeContentType(contentType, filename string) (string, error) {
	normalized := pystr.Lower(pystr.Strip(strings.SplitN(contentType, ";", 2)[0]))
	if normalized != "" && normalized != "application/octet-stream" && normalized != "text/plain" {
		return normalized, nil
	}
	return guessMimeType(filename)
}

// WeCom media type of a content type, ogg containers counting as voice and unknown types as file
func detectWeComMediaType(contentType string) string {
	mimeType := pystr.Lower(pystr.Strip(contentType))
	if mimeType == "application/ogg" {
		return "voice"
	}
	for _, p := range mimePrefixKinds {
		if strings.HasPrefix(mimeType, p.prefix) {
			return p.kind
		}
	}
	return "file"
}

// Rejects a file over the absolute cap and downgrades unsupported voice formats and oversized media to file
func applyFileSizeLimits(ctx context.Context, fileSize int, detectedType, contentType string) sizeVerdict {
	sizeMB := fmt.Sprintf("%.2f", float64(fileSize)/(1024*1024))
	normalizedType := pystr.Lower(detectedType)
	if normalizedType == "" {
		normalizedType = "file"
	}
	normalizedContentType := pystr.Lower(pystr.Strip(contentType))
	if fileSize > absoluteMaxBytes {
		return sizeVerdict{finalType: normalizedType, rejected: true,
			rejectReason: i18n.T(ctx, "platform.wecom.media.file_too_large", "", map[string]any{"size_mb": sizeMB})}
	}
	if normalizedType == "voice" && normalizedContentType != "" && !voiceSupportedMimes[normalizedContentType] {
		return sizeVerdict{finalType: "file", downgraded: true,
			downgradeNote: i18n.T(ctx, "platform.wecom.media.voice_format_downgraded", "", map[string]any{"content_type": normalizedContentType})}
	}
	if limit, ok := typeLimits[normalizedType]; ok && fileSize > limit.maxBytes {
		return sizeVerdict{finalType: "file", downgraded: true,
			downgradeNote: i18n.T(ctx, "platform.wecom.media.size_downgraded", "",
				map[string]any{"label": i18n.T(ctx, limit.labelKey, "", nil), "size_mb": sizeMB, "cap": limit.cap})}
	}
	return sizeVerdict{finalType: normalizedType}
}

// Whether a media source is an http or https URL
func looksLikeURL(mediaSource string) bool {
	parsed, err := pyurl.Parse(mediaSource)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// Bytes, content type and file name of an outbound media given as a URL or a local path
func (a *WeComAdapter) loadOutboundMedia(ctx context.Context, mediaSource, fileName string) ([]byte, string, string, error) {
	source := pystr.Strip(mediaSource)
	if source == "" {
		return nil, "", "", &pyErr{"ValueError", "media source is required"}
	}
	if placeholder, _ := mediaPlaceholderRE.MatchString(source); placeholder {
		return nil, "", "", &pyErr{"ValueError", "Media placeholder was not replaced with a real file path: " + source}
	}
	parsed, err := pyurl.Parse(source)
	if err != nil {
		return nil, "", "", err
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		data, headers, err := a.downloadRemoteBytes(ctx, source, absoluteMaxBytes)
		if err != nil {
			return nil, "", "", err
		}
		resolvedName := fileName
		if resolvedName == "" {
			if resolvedName, err = guessFilename(source, headers["content-disposition"], headers["content-type"]); err != nil {
				return nil, "", "", err
			}
		}
		contentType, err := normalizeContentType(headers["content-type"], resolvedName)
		return data, contentType, resolvedName, err
	}
	localPath := source
	if parsed.Scheme == "file" {
		localPath = pyurl.Unquote(parsed.Path)
	}
	localPath = pypath.Expanduser(localPath)
	if !pypath.IsAbs(localPath) {
		abs, err := filepath.Abs(localPath)
		if err != nil {
			return nil, "", "", err
		}
		localPath = abs
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			localPath = resolved
		}
	}
	if info, err := os.Stat(localPath); err != nil || !info.Mode().IsRegular() {
		return nil, "", "", &pyErr{"FileNotFoundError", "Media file not found: " + localPath}
	}
	resolvedName := fileName
	if resolvedName == "" {
		resolvedName = pypath.Name(localPath)
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", "", &pyErr{"FileNotFoundError", pypath.OSErrorText(err)}
		}
		return nil, "", "", err
	}
	contentType, err := normalizeContentType("", resolvedName)
	return data, contentType, resolvedName, err
}

// Loaded outbound media with its detected type and the verdict of the size limits
type preparedMedia struct {
	data         []byte
	contentType  string
	fileName     string
	detectedType string
	sizeVerdict
}

// Loads an outbound media and applies the size and format limits to it
func (a *WeComAdapter) prepareOutboundMedia(ctx context.Context, mediaSource, fileName string) (*preparedMedia, error) {
	data, contentType, resolvedName, err := a.loadOutboundMedia(ctx, mediaSource, fileName)
	if err != nil {
		return nil, err
	}
	detectedType := detectWeComMediaType(contentType)
	return &preparedMedia{data: data, contentType: contentType, fileName: resolvedName, detectedType: detectedType,
		sizeVerdict: applyFileSizeLimits(ctx, len(data), detectedType, contentType)}, nil
}

// Sends a proactive command and fails on a response that carries an error
func (a *WeComAdapter) checkedRequest(ctx context.Context, cmd string, body *pyjson.Object, operation string) (*pyjson.Object, error) {
	response, err := a.sendRequest(ctx, cmd, body, requestTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	if err := a.RaiseForWeComError(response, operation); err != nil {
		return nil, err
	}
	return response, nil
}

// Uploads media bytes in base64 chunks and returns the type, media id and creation time of the upload
func (a *WeComAdapter) uploadMediaBytes(ctx context.Context, data []byte, mediaType, filename string) (*pyjson.Object, error) {
	if len(data) == 0 {
		return nil, &pyErr{"ValueError", "Cannot upload empty media"}
	}
	totalSize, totalChunks := len(data), (len(data)+uploadChunkSize-1)/uploadChunkSize
	if totalChunks > maxUploadChunks {
		return nil, &pyErr{"ValueError", fmt.Sprintf("File too large: %d chunks exceeds maximum of %d chunks", totalChunks, maxUploadChunks)}
	}
	sum := md5.Sum(data)
	initResponse, err := a.checkedRequest(ctx, appCmdUploadMediaInit, obj("type", mediaType, "filename", filename, "total_size", int64(totalSize),
		"total_chunks", int64(totalChunks), "md5", hex.EncodeToString(sum[:])), "media upload init")
	if err != nil {
		return nil, err
	}
	uploadID := textOf(dictOrEmpty(initResponse, "body").Vals["upload_id"])
	if uploadID == "" {
		return nil, runtimeError("media upload init failed: missing upload_id in response " + pyrepr.Str(initResponse))
	}
	for chunkIndex, start := 0, 0; start < totalSize; chunkIndex, start = chunkIndex+1, start+uploadChunkSize {
		chunk := base64.StdEncoding.EncodeToString(data[start:min(start+uploadChunkSize, totalSize)])
		if _, err := a.checkedRequest(ctx, appCmdUploadMediaChunk, obj("upload_id", uploadID, "chunk_index", int64(chunkIndex), "base64_data", chunk),
			fmt.Sprintf("media upload chunk %d", chunkIndex)); err != nil {
			return nil, err
		}
	}
	finishResponse, err := a.checkedRequest(ctx, appCmdUploadMediaFinish, obj("upload_id", uploadID), "media upload finish")
	if err != nil {
		return nil, err
	}
	finishBody := dictOrEmpty(finishResponse, "body")
	mediaID := textOf(finishBody.Vals["media_id"])
	if mediaID == "" {
		return nil, runtimeError("media upload finish failed: missing media_id in response " + pyrepr.Str(finishResponse))
	}
	return obj("type", pyrepr.Str(pyrepr.Or(finishBody.Vals["type"], mediaType)), "media_id", mediaID, "created_at", finishBody.Vals["created_at"]), nil
}

// Sends an uploaded media proactively to a chat
func (a *WeComAdapter) sendMediaMessage(ctx context.Context, chatID, mediaType, mediaID string) (*pyjson.Object, error) {
	return a.checkedRequest(ctx, appCmdSend, obj(append([]any{"chatid", chatID}, mediaBody(mediaType, mediaID)...)...), "send media message")
}

// Sends follow up markdown after a media message, nil for empty content and a failed send only logged
func (a *WeComAdapter) sendFollowupMarkdown(ctx context.Context, chatID, content, replyTo string) (*gateway.SendResult, error) {
	if content == "" {
		return nil, nil
	}
	result, err := a.Self().Send(ctx, chatID, content, replyTo, nil)
	if err != nil {
		return nil, err
	}
	if !result.Success {
		slog.Warn(fmt.Sprintf("[%s] Follow-up markdown send failed: %s", a.Name(), pyrepr.Str(result.Error)))
	}
	return &result, nil
}

// Uploads a media source and sends it natively, with the caption and any downgrade notice as follow up markdown
func (a *WeComAdapter) sendMediaSource(ctx context.Context, chatID, mediaSource, caption, fileName, replyTo string) (gateway.SendResult, error) {
	if chatID == "" {
		return gateway.SendResult{Success: false, Error: "chat_id is required"}, nil
	}
	prepared, err := a.prepareOutboundMedia(ctx, mediaSource, fileName)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		var pe *pyErr
		if !errors.As(err, &pe) || pe.kind != "FileNotFoundError" {
			slog.Error(fmt.Sprintf("[%s] Failed to prepare outbound media %s: %s", a.Name(), mediaSource, errText(err)))
		}
		return gateway.SendResult{Success: false, Error: errText(err)}, nil
	}
	if prepared.rejected {
		if text := a.Self().WarningText(ctx, "⚠️ "+prepared.rejectReason, &caption, "", "", nil); text != nil && *text != "" {
			if _, err := a.sendFollowupMarkdown(ctx, chatID, *text, replyTo); err != nil {
				return gateway.SendResult{}, err
			}
		}
		return gateway.SendResult{Success: false, Error: prepared.rejectReason}, nil
	}
	replyReqID := a.cachedReplyReqID(chatID, replyTo)
	if a.stream.streamOwnsChat(chatID) {
		replyReqID = ""
	}
	uploadResult, mediaResponse, err := a.uploadAndSendMedia(ctx, chatID, prepared, replyReqID)
	switch {
	case err != nil && ctx.Err() != nil:
		return gateway.SendResult{}, ctx.Err()
	case isTimeout(err):
		slog.Error(fmt.Sprintf("[%s] TIMEOUT in _send_media_source for %s", a.Name(), mediaSource))
		return gateway.SendResult{Success: false, Error: "Timeout sending media to WeCom"}, nil
	case err != nil:
		slog.Error(fmt.Sprintf("[%s] Failed to send media %s: %s", a.Name(), mediaSource, errText(err)))
		return gateway.SendResult{Success: false, Error: errText(err)}, nil
	}
	raw := map[string]any{"upload": uploadResult, "media": mediaResponse}
	downgrade := ""
	if prepared.downgraded && prepared.downgradeNote != "" {
		if text := a.Self().WarningText(ctx, "ℹ️ "+prepared.downgradeNote, nil, "", "", nil); text != nil {
			downgrade = *text
		}
	}
	for _, followup := range []struct{ key, text string }{{"caption", caption}, {"downgrade", downgrade}} {
		raw[followup.key], raw[followup.key+"_error"] = nil, nil
		if followup.text == "" {
			continue
		}
		sent, err := a.sendFollowupMarkdown(ctx, chatID, followup.text, replyTo)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if sent != nil {
			raw[followup.key] = sent.RawResponse
			if !sent.Success {
				raw[followup.key+"_error"] = sent.Error
			}
		}
	}
	var messageID any = payloadReqID(mediaResponse)
	if messageID == "" {
		messageID = uuidHex()[:12]
	}
	return gateway.SendResult{Success: true, MessageID: messageID, RawResponse: raw}, nil
}

// Uploads the prepared media then sends it as a passive reply when a req id is usable and proactively otherwise
func (a *WeComAdapter) uploadAndSendMedia(ctx context.Context, chatID string, prepared *preparedMedia, replyReqID string) (*pyjson.Object, *pyjson.Object, error) {
	uploadResult, err := a.uploadMediaBytes(ctx, prepared.data, prepared.finalType, prepared.fileName)
	if err != nil {
		return nil, nil, err
	}
	mediaID := pyrepr.Str(uploadResult.Vals["media_id"])
	slog.Info(fmt.Sprintf("[%s] upload_media_bytes OK: media_id=%s type=%s", a.Name(), mediaID, prepared.finalType))
	var mediaResponse *pyjson.Object
	operation := "send_media_message"
	if replyReqID != "" {
		operation = "send_reply_media"
		if mediaResponse, err = a.sendReplyRequest(ctx, replyReqID, obj(mediaBody(prepared.finalType, mediaID)...)); err == nil {
			err = a.RaiseForWeComError(mediaResponse, "send reply media message")
		}
	} else {
		mediaResponse, err = a.sendMediaMessage(ctx, chatID, prepared.finalType, mediaID)
	}
	if err != nil {
		return nil, nil, err
	}
	slog.Info(fmt.Sprintf("[%s] %s OK: %s", a.Name(), operation, pyrepr.Str(mediaResponse)))
	return uploadResult, mediaResponse, nil
}

// Sends an image natively, an image URL that cannot be sent going out as text with its caption
func (a *WeComAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	result, err := a.sendMediaSource(ctx, chatID, imageURL, caption, "", replyTo)
	if err != nil || result.Success || !looksLikeURL(imageURL) {
		return result, err
	}
	slog.Warn(fmt.Sprintf("[%s] Falling back to text send for image URL %s: %s", a.Name(), imageURL, pyrepr.Str(result.Error)))
	content := imageURL
	if caption != "" {
		content = caption + "\n" + imageURL
	}
	return a.Self().Send(ctx, chatID, content, replyTo, nil)
}

// Sends a local image natively
func (a *WeComAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, imagePath, caption, "", replyTo)
}

// Sends a document natively under an optional file name
func (a *WeComAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	slog.Info(fmt.Sprintf("[%s] send_document called: chat=%s file=%s", a.Name(), chatID, filePath))
	return a.sendMediaSource(ctx, chatID, filePath, caption, fileName, replyTo)
}

// Sends audio natively, formats other than AMR going out as a file
func (a *WeComAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, audioPath, caption, "", replyTo)
}

// Sends a video natively
func (a *WeComAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaSource(ctx, chatID, videoPath, caption, "", replyTo)
}
