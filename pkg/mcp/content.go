package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Hard ceiling in characters for one MCP text payload, far above the spillover threshold so only floods are lossy
const hardResultCapChars = 2_000_000

// Decoded resource bytes allowed per block and the base64 length rejected before any decode
const (
	resourceMaxBytes    = 50 * 1024 * 1024
	resourceMaxB64Chars = resourceMaxBytes*4/3 + 4
)

// Head and tail of pathological MCP text around a truncation notice
func truncateTextResult(text string) string {
	return tools.TruncateHeadTail(text, hardResultCapChars, "MCP RESULT")
}

// Reports whether a _meta key uses a protocol reserved modelcontextprotocol or mcp label followed by another label
func isReservedMetaKey(key string) bool {
	slash := strings.Index(key, "/")
	if slash <= 0 {
		return false
	}
	labels := strings.Split(key[:slash], ".")
	for i, label := range labels {
		if (label == "modelcontextprotocol" || label == "mcp") && i < len(labels)-1 {
			return true
		}
	}
	return false
}

// Model facing _meta with protocol reserved keys dropped, nil when nothing remains
func stripReservedMetaKeys(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range meta {
		if !isReservedMetaKey(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Preferred file extension Python's default mimetypes table guesses for a MIME type
var mimeExtensions = map[string]string{
	"application/epub+zip": ".epub", "application/gzip": ".gz", "application/json": ".json", "application/manifest+json": ".webmanifest", "application/msword": ".doc", "application/n-quads": ".nq", "application/n-triples": ".nt", "application/octet-stream": ".bin", "application/oda": ".oda", "application/ogg": ".ogx", "application/pdf": ".pdf", "application/pkcs7-mime": ".p7c", "application/postscript": ".ps", "application/trig": ".trig", "application/vnd.apple.mpegurl": ".m3u", "application/vnd.ms-excel": ".xls", "application/vnd.ms-fontobject": ".eot", "application/vnd.ms-powerpoint": ".ppt", "application/vnd.oasis.opendocument.graphics": ".odg", "application/vnd.oasis.opendocument.presentation": ".odp", "application/vnd.oasis.opendocument.spreadsheet": ".ods", "application/vnd.oasis.opendocument.text": ".odt", "application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx", "application/vnd.rar": ".rar", "application/wasm": ".wasm", "application/x-7z-compressed": ".7z", "application/x-bcpio": ".bcpio", "application/x-cpio": ".cpio", "application/x-csh": ".csh", "application/x-debian-package": ".deb", "application/x-dvi": ".dvi", "application/x-gtar": ".gtar", "application/x-hdf": ".hdf", "application/x-hdf5": ".h5", "application/x-httpd-php": ".php", "application/x-latex": ".latex", "application/x-mif": ".mif", "application/x-netcdf": ".cdf", "application/x-pkcs12": ".p12", "application/x-pn-realaudio": ".ram", "application/x-python-code": ".pyc", "application/x-rpm": ".rpm", "application/x-sh": ".sh", "application/x-shar": ".shar", "application/x-shockwave-flash": ".swf", "application/x-sv4cpio": ".sv4cpio", "application/x-sv4crc": ".sv4crc", "application/x-tar": ".tar", "application/x-tcl": ".tcl", "application/x-tex": ".tex", "application/x-texinfo": ".texi", "application/x-troff": ".roff", "application/x-troff-man": ".man", "application/x-troff-me": ".me", "application/x-troff-ms": ".ms", "application/x-ustar": ".ustar", "application/x-wais-source": ".src", "application/xml": ".xsl", "application/yaml": ".yaml", "application/zip": ".zip", "audio/3gpp": ".3gp", "audio/3gpp2": ".3g2", "audio/aac": ".aac", "audio/basic": ".au", "audio/flac": ".flac", "audio/matroska": ".mka", "audio/mp4": ".m4a", "audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/opus": ".opus", "audio/vnd.wave": ".wav", "audio/webm": ".weba", "audio/x-aiff": ".aif", "audio/x-pn-realaudio": ".ra", "font/otf": ".otf", "font/ttf": ".ttf", "font/woff": ".woff", "font/woff2": ".woff2", "image/avif": ".avif", "image/bmp": ".bmp", "image/emf": ".emf", "image/fits": ".fits", "image/g3fax": ".g3", "image/gif": ".gif", "image/heic": ".heic", "image/heif": ".heif", "image/ief": ".ief", "image/jp2": ".jp2", "image/jpeg": ".jpg", "image/jpm": ".jpm", "image/jpx": ".jpx", "image/png": ".png", "image/svg+xml": ".svg", "image/t38": ".t38", "image/tiff": ".tiff", "image/tiff-fx": ".tfx", "image/vnd.microsoft.icon": ".ico", "image/webp": ".webp", "image/wmf": ".wmf", "image/x-cmu-raster": ".ras", "image/x-portable-anymap": ".pnm", "image/x-portable-bitmap": ".pbm", "image/x-portable-graymap": ".pgm", "image/x-portable-pixmap": ".ppm", "image/x-rgb": ".rgb", "image/x-xbitmap": ".xbm", "image/x-xpixmap": ".xpm", "image/x-xwindowdump": ".xwd", "message/rfc822": ".eml", "model/gltf+json": ".gltf", "model/gltf-binary": ".glb", "model/stl": ".stl", "text/css": ".css", "text/csv": ".csv", "text/html": ".html", "text/javascript": ".js", "text/markdown": ".md", "text/n3": ".n3", "text/plain": ".txt", "text/richtext": ".rtx", "text/rtf": ".rtf", "text/tab-separated-values": ".tsv", "text/vtt": ".vtt", "text/x-python": ".py", "text/x-rst": ".rst", "text/x-setext": ".etx", "text/x-sgml": ".sgm", "text/x-vcard": ".vcf", "text/xml": ".xml", "video/matroska": ".mkv", "video/matroska-3d": ".mk3d", "video/mp4": ".mp4", "video/mpeg": ".mpeg", "video/ogg": ".ogv", "video/quicktime": ".mov", "video/vnd.avi": ".avi", "video/webm": ".webm", "video/x-m4v": ".m4v", "video/x-ms-wmv": ".wmv", "video/x-sgi-movie": ".movie",
}

// Extension mimetypes guesses for a MIME type, empty when it knows none
func guessExtension(mime string) string { return mimeExtensions[mime] }

// File extension for an image MIME type, png when unknown
func imageExtensionForMime(mime string) string {
	normalized := baseMime(mime)
	if normalized == "image/jpeg" || normalized == "image/jpg" {
		return ".jpg"
	}
	if ext := guessExtension(normalized); ext != "" {
		return ext
	}
	return ".png"
}

// Characters outside the base64 alphabet a non validating decode discards
var nonBase64Re = regexp.MustCompile(`[^A-Za-z0-9+/=]`)

// Base64 payload of one block, the inline marker instead when it is too large or malformed
func decodeBlockB64(data, what, label, capWhat, capSuffix, decodeFail string) ([]byte, string) {
	if capWhat != "" && len([]rune(data)) > resourceMaxB64Chars {
		return nil, fmt.Sprintf("[MCP %s too large to cache: ~%d bytes%s]", capWhat, len([]rune(data))*3/4, capSuffix)
	}
	raw, err := base64.StdEncoding.DecodeString(nonBase64Re.ReplaceAllString(data, ""))
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP %s decode failed (%s): %v", what, label, err))
		return nil, decodeFail
	}
	if capWhat != "" && len(raw) > resourceMaxBytes {
		return nil, fmt.Sprintf("[MCP %s too large to cache: %d bytes%s]", capWhat, len(raw), capSuffix)
	}
	return raw, ""
}

// Writes block bytes through a media cache writer, the failed marker with a warning when it errors
func writeBlockCache(write func() (string, error), what, failed string) (string, string) {
	p, err := write()
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP %s cache failed: %v", what, err))
		return "", failed
	}
	return p, ""
}

// Caches an image or audio block as a MEDIA tag, empty when it is not that media or fails
func cacheMediaBlock(env *tools.Env, block *ContentBlock, kind string, writer func(*tools.Env, []byte, string) (string, error), extFor func(string) string, capWhat string) string {
	mime := baseMime(block.MimeType)
	if block.Data == "" && block.Type != kind || !strings.HasPrefix(mime, kind+"/") {
		return ""
	}
	raw, marker := decodeBlockB64(block.Data, kind+" block", mime, capWhat, "", "")
	if raw == nil {
		return marker
	}
	p, marker := writeBlockCache(func() (string, error) { return writer(env, raw, extFor(mime)) }, kind+" block", "")
	if p == "" {
		return marker
	}
	return "MEDIA:" + p
}

// Caches an image block and returns its MEDIA tag, empty on any failure
func cacheImageBlock(env *tools.Env, block *ContentBlock) string {
	return cacheMediaBlock(env, block, "image", cacheImageFromBytes, imageExtensionForMime, "")
}

// WAV spellings mapped to the wav extension ahead of the mimetypes guess
var wavMimeExt = map[string]string{"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/wave": ".wav"}

// Caches an audio block and returns its MEDIA tag, empty on any failure
func cacheAudioBlock(env *tools.Env, block *ContentBlock) string {
	return cacheMediaBlock(env, block, "audio", cacheAudioFromBytes, func(mime string) string {
		if ext := wavMimeExt[mime]; ext != "" {
			return ext
		}
		if ext := guessExtension(mime); ext != "" {
			return ext
		}
		return ".ogg"
	}, "audio resource")
}

// Images attached natively from one result and the most prepared per result so skips can refill slots
const (
	nativeImageMax        = 4
	nativeImageCandidates = 16
)

// One prepared native image part and its scale note
type nativeImage struct {
	part map[string]any
	note string
}

// Image part and scale note for one cached image sized like every native embed, nil when it cannot embed safely
func nativeImagePart(env *tools.Env, src string) *nativeImage {
	data, err := os.ReadFile(src)
	if err != nil {
		return nil
	}
	mime := tools.DetectImageMimeTypeFromBytes(data)
	if mime == "" {
		return nil
	}
	normalized, mime, errText := tools.NormalizeToSupportedImage(env, src, mime)
	if errText != "" || normalized == "" {
		return nil
	}
	defer func() {
		if normalized != src {
			os.Remove(normalized)
		}
	}()
	target := min(tools.ResolveEmbedTargetBytes(env), tools.VisionMaxBase64Bytes)
	if tools.ValidateRasterImageDecodable(normalized) != "" {
		return nil
	}
	f, err := os.Open(normalized)
	if err != nil {
		return nil
	}
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		return nil
	}
	scale := &tools.ScaleInfo{}
	dataURL, err := tools.ResizeImageForVision(env, normalized, mime, tools.ResizeOptions{
		MaxBase64Bytes: target, MaxDimension: tools.VisionEmbedMaxDimension, ForceJPEG: true, Scale: scale})
	if err != nil {
		return nil
	}
	w, h := cfg.Width, cfg.Height
	if scale.Applied() {
		w, h = scale.NewWidth, scale.NewHeight
	}
	if max(w, h) > tools.VisionEmbedMaxDimension || len(dataURL) > target {
		return nil
	}
	var note *tools.ScaleInfo
	if scale.Applied() {
		note = scale
	}
	return &nativeImage{part: map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}, note: tools.BuildScaleNote(note, nil)}
}

// Text as is, or the multimodal envelope carrying the call's cached images when the route takes images in tool results
func resultWithNativeImages(ctx context.Context, env *tools.Env, sessionID, text string, imagePaths []string) any {
	if len(imagePaths) == 0 || !nativeVisionGate(ctx, env) {
		return text
	}
	var attached []any
	notes := ""
	candidates := imagePaths[:min(len(imagePaths), nativeImageCandidates)]
	for len(candidates) > 0 && len(attached) < nativeImageMax {
		n := min(nativeImageMax-len(attached), len(candidates))
		wave := candidates[:n]
		candidates = candidates[n:]
		results := make([]chan *nativeImage, len(wave))
		for i, p := range wave {
			results[i] = make(chan *nativeImage, 1)
			go func(p string, out chan<- *nativeImage) {
				defer func() {
					if r := recover(); r != nil {
						mcpLogger().Debug(fmt.Sprintf("MCP native image prep failed for %s", p), "panic", r)
						out <- nil
					}
				}()
				out <- tools.RunOnVisionCPU(env, func() *nativeImage { return nativeImagePart(env, p) })
			}(p, results[i])
		}
		for i, p := range wave {
			ready := <-results[i]
			if ready == nil {
				continue
			}
			imageURL := ready.part["image_url"].(map[string]any)["url"].(string)
			if tools.RepeatRefusal(env, sessionID, imageURL) != "" {
				notes += fmt.Sprintf("\n- MEDIA:%s: not attached; this image is already in context (vision.max_calls_per_image).", p)
				continue
			}
			attached = append(attached, ready.part)
			if ready.note != "" {
				notes += fmt.Sprintf("\n- MEDIA:%s: %s", p, ready.note)
			}
		}
	}
	if len(attached) == 0 {
		if notes == "" {
			return text
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil || payload == nil {
			return text + notes
		}
		prior := payload["result"]
		if !pyTruthy(prior) {
			prior = ""
		}
		payload["result"] = pyStr(prior) + notes
		return tools.JSON(payload)
	}
	header := "The image(s) from this call are attached \u2014 inspect them with your native vision."
	content := append([]any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "text", "text": header + notes}}, attached...)
	return map[string]any{"_multimodal": true, "content": content, "text_summary": text}
}

var controlCharsRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// Display filename hint from a resource URI's last path segment, control characters stripped and length capped
func resourceFilename(uri, mime string) string {
	name := ""
	if uri != "" {
		if u, err := url.Parse(uri); err == nil {
			if p, err := url.PathUnescape(u.EscapedPath()); err == nil {
				name = pathName(p)
			} else {
				name = pathName(u.Path)
			}
		}
	}
	name = pyStrip(controlCharsRe.ReplaceAllString(name, ""))
	if r := []rune(name); len(r) > 150 {
		stem, ext, dot := name, "", false
		if i := strings.LastIndex(name, "."); i >= 0 {
			stem, ext, dot = name[:i], name[i+1:], true
		}
		if extLen := len([]rune(ext)); dot && extLen > 0 && extLen <= 12 {
			sr := []rune(stem)
			name = string(sr[:max(0, min(len(sr), 150-extLen-1))]) + "." + ext
		} else {
			name = string(r[:150])
		}
	}
	if name == "" || name == "." || name == ".." {
		ext := guessExtension(baseMime(mime))
		if ext == "" {
			ext = ".bin"
		}
		name = "resource" + ext
	}
	return name
}

// Final component of a URL path the way PurePosixPath.name reads it
func pathName(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	base := path.Base(p)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

// Inline notice naming an unsupported block and whatever handles it exposes
func renderDroppedBlockNotice(block *ContentBlock, blockType string) string {
	details := []string{"type=" + blockType}
	if block.MimeType != "" {
		details = append(details, "mimeType="+block.MimeType)
	}
	uri := block.URI
	if uri == "" && block.Resource != nil {
		uri = block.Resource.URI
	}
	if uri != "" {
		details = append(details, "uri="+uri)
	}
	if block.Size != nil {
		details = append(details, fmt.Sprintf("size=%d", *block.Size))
	}
	if block.Name != "" {
		details = append(details, "name="+block.Name)
	}
	return "[MCP content dropped: unsupported block (" + strings.Join(details, ", ") + ")]"
}

// Resource link or embedded resource rendered as text, an embedded blob saved to the document cache
func renderResourceBlock(env *tools.Env, block *ContentBlock, serverName string) string {
	if block.Type == "resource_link" || (block.URI != "" && block.Resource == nil && block.Type != "text") {
		if block.URI == "" {
			return ""
		}
		details := "uri=" + block.URI
		if block.Name != "" {
			details += ", name=" + block.Name
		}
		if block.MimeType != "" {
			details += ", mimeType=" + block.MimeType
		}
		reader := "the MCP server's read_resource tool"
		if serverName != "" {
			reader = PrefixedToolName(serverName, "read_resource")
		}
		return fmt.Sprintf("[MCP resource link: %s \u2014 fetch it with %s]", details, reader)
	}
	res := block.Resource
	if res == nil {
		return ""
	}
	if res.Text != nil {
		return tools.StripUnicodeTags(*res.Text)
	}
	if res.Blob == nil {
		return ""
	}
	label := res.MimeType
	if label == "" {
		label = res.URI
	}
	raw, marker := decodeBlockB64(*res.Blob, "embedded resource", label, "embedded resource", ", uri="+res.URI,
		"[MCP embedded resource could not be decoded: "+label+"]")
	if raw == nil {
		return marker
	}
	kind := res.MimeType
	if kind == "" {
		kind = "unknown type"
	}
	p, marker := writeBlockCache(func() (string, error) {
		return cacheDocumentFromBytes(env, raw, resourceFilename(res.URI, res.MimeType))
	}, "embedded resource", "[MCP embedded resource could not be cached: "+label+"]")
	if p == "" {
		return marker
	}
	return fmt.Sprintf("[MCP resource saved to %s (%s, %d bytes) \u2014 read it with read_file or terminal tools]", p, kind, len(raw))
}

// Concatenated text of an error result's blocks, embedded resources carrying theirs under resource text
func errorResultText(result *CallToolResult) string {
	var b strings.Builder
	for _, blk := range result.Content {
		if blk == nil {
			continue
		}
		if blk.Text != "" {
			b.WriteString(blk.Text)
		} else if blk.Resource != nil && blk.Resource.Text != nil && *blk.Resource.Text != "" {
			b.WriteString(*blk.Resource.Text)
		}
	}
	return b.String()
}

// Rendered block text, the count of real rendered blocks and the files this call's images were cached to
func renderContentBlocks(env *tools.Env, result *CallToolResult, serverName string) (string, int, []string) {
	var parts []string
	usable := 0
	var images []string
	for _, blk := range result.Content {
		if blk == nil {
			continue
		}
		if blk.Text != "" {
			parts = append(parts, tools.StripUnicodeTags(blk.Text))
			if pyStrip(blk.Text) != "" {
				usable++
			}
			continue
		}
		img := cacheImageBlock(env, blk)
		if strings.HasPrefix(img, "MEDIA:") {
			images = append(images, img[len("MEDIA:"):])
		}
		rendered := img
		if rendered == "" {
			rendered = cacheAudioBlock(env, blk)
		}
		if rendered == "" {
			rendered = renderResourceBlock(env, blk, serverName)
		}
		if rendered != "" {
			parts = append(parts, rendered)
			usable++
			continue
		}
		blockType := blk.Type
		if blockType == "" {
			blockType = "ContentBlock"
		}
		switch blockType {
		case "text", "resource", "audio", "image":
			mcpLogger().Debug(fmt.Sprintf("MCP %s: content block type %s rendered empty", serverName, pyReprStr(blockType)))
		default:
			mcpLogger().Warn(fmt.Sprintf("MCP %s: dropping unsupported content block type %s", serverName, pyReprStr(blockType)))
			parts = append(parts, renderDroppedBlockNotice(blk, blockType))
		}
	}
	return truncateTextResult(strings.Join(parts, "\n")), usable, images
}

// Structured content, or its head and tail truncated JSON text when the serialized form is over the hard cap
func cappedStructuredContent(result *CallToolResult) any {
	if result.StructuredContent == nil {
		return nil
	}
	asJSON := tools.JSON(result.StructuredContent)
	if len([]rune(asJSON)) > hardResultCapChars {
		return truncateTextResult(asJSON)
	}
	return result.StructuredContent
}

// Reports whether some text block is the structured content serialized as JSON, compared as parsed values
func contentDualEmitsStructured(result *CallToolResult, structured any) bool {
	for _, blk := range result.Content {
		if blk == nil || blk.Text == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(blk.Text), &parsed); err != nil {
			continue
		}
		if reflect.DeepEqual(parsed, structured) {
			return true
		}
	}
	return false
}

// JSON object whose keys keep their insertion order
type orderedPayload struct {
	keys []string
	vals map[string]any
}

// Sets a key, appending it on first use
func (o *orderedPayload) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Encodes the object with its keys in insertion order
func (o *orderedPayload) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(tools.JSON(k))
		b.WriteByte(':')
		b.WriteString(tools.JSON(o.vals[k]))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// Reports whether a value encodes as JSON
func isJSONEncodable(v any) bool {
	_, err := json.Marshal(v)
	return err == nil
}

// Handler JSON for a tool result, content and structured content both forwarded unless a text block dual emits it
func RenderCallToolResult(env *tools.Env, result *CallToolResult, serverName string, imagePaths *[]string) string {
	if result.IsError {
		text := errorResultText(result)
		if text == "" {
			text = "MCP tool returned an error"
		}
		return tools.ErrorResult(fmt.Errorf("%s", sanitizeError(truncateTextResult(text))))
	}
	text, usable, cached := renderContentBlocks(env, result, serverName)
	if imagePaths != nil {
		*imagePaths = append(*imagePaths, cached...)
	}
	structured := cappedStructuredContent(result)
	meta := stripReservedMetaKeys(result.Meta)
	if structured != nil && usable > 0 {
		if _, isStr := structured.(string); isStr || contentDualEmitsStructured(result, structured) {
			structured = nil
		}
	}
	if structured == nil && meta == nil {
		return tools.JSON(map[string]any{"result": text})
	}
	payload := &orderedPayload{}
	if text != "" {
		payload.set("result", text)
	}
	if structured != nil {
		if text != "" {
			payload.set("structuredContent", structured)
		} else {
			payload.set("result", structured)
		}
	}
	if meta != nil {
		payload.set("_meta", meta)
	}
	if _, ok := payload.vals["result"]; !ok {
		payload.set("result", text)
	}
	if !isJSONEncodable(payload.vals) {
		return tools.JSON(map[string]any{"result": text})
	}
	return tools.JSON(payload)
}

// Native image gate of the session's main route, a failing gate keeping the MEDIA paths
func nativeVisionGate(ctx context.Context, env *tools.Env) (native bool) {
	defer func() {
		if r := recover(); r != nil {
			mcpLogger().Debug("MCP native image gate failed, keeping MEDIA: paths", "panic", r)
			native = false
		}
	}()
	return tools.ShouldUseNativeVisionFastPath(ctx, env)
}
