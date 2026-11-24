package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Request body cap of the API server, sized for long agent conversations with tool calls
	APIMaxRequestBytes = 10_000_000
	// Cap of normalized content text
	apiMaxNormalizedTextLength = 65_536
	// Most items read from an array content value
	apiMaxContentListSize = 1_000
	// Messages a Responses request keeps when it asks for automatic truncation
	APIResponsesAutoTruncationHistoryLimit = 100
	// Bytes a file input fetched by file_url may carry, the document reader's ceiling
	apiMaxFileURLBytes = 50 * 1024 * 1024
)

var (
	apiTextPartTypes  = map[string]bool{"text": true, "input_text": true, "output_text": true}
	apiImagePartTypes = map[string]bool{"image_url": true, "input_image": true}
	apiFilePartTypes  = map[string]bool{"file": true, "input_file": true}
)

// Text cut to the normalized length cap in code points
func apiCapText(text string) string {
	return pySliceRunes(text, apiMaxNormalizedTextLength)
}

// Items cut to the content list size cap
func apiCapList(items []any) []any {
	if len(items) > apiMaxContentListSize {
		return items[:apiMaxContentListSize]
	}
	return items
}

// Lowercased stripped part type of a content part, empty when unset
func apiPartType(part *jsonutil.Object) string {
	raw := part.ValueOr("type", nil)
	if !anyutil.IsTrue(raw) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(anyutil.String(raw)))
}

// OpenAI chat content flattened to one plain string, non text parts skipped and depth, size and length bounded
func apiNormalizeChatContent(content any) string {
	return apiNormalizeChatContentDepth(content, 10, 0)
}

// Chat content flattening at a nesting depth
func apiNormalizeChatContentDepth(content any, maxDepth, depth int) string {
	if depth > maxDepth || content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return apiCapText(v)
	case []any:
		var parts []string
		total := 0
		for _, item := range apiCapList(v) {
			part := ""
			switch it := item.(type) {
			case string:
				part = it
			case *jsonutil.Object:
				if apiTextPartTypes[apiPartType(it)] {
					if text := it.ValueOr("text", ""); anyutil.IsTrue(text) {
						part = anyutil.String(text)
					}
				}
			case []any:
				part = apiNormalizeChatContentDepth(it, maxDepth, depth+1)
			}
			if part != "" {
				part = apiCapText(part)
				parts = append(parts, part)
				total += runeLen(part)
			}
			if total >= apiMaxNormalizedTextLength {
				break
			}
		}
		return apiCapText(strings.Join(parts, "\n"))
	}
	return apiCapText(anyutil.String(content))
}

// Canonical vision part of one image part, the coded ValueError of a malformed one
func apiNormalizeImagePart(part *jsonutil.Object) (*jsonutil.Object, error) {
	detail := part.ValueOr("detail", nil)
	imageRef := part.ValueOr("image_url", nil)
	urlValue := imageRef
	if ref, isDict := imageRef.(*jsonutil.Object); isDict {
		urlValue = ref.ValueOr("url", nil)
		detail = ref.ValueOr("detail", detail)
	}
	text, ok := urlValue.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_image_url:Image parts must include a non-empty image URL."}
	}
	text = strings.TrimSpace(text)
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "data:") {
		if !strings.HasPrefix(lowered, "data:image/") || !strings.Contains(text, ",") {
			return nil, &PyError{Kind: "ValueError", Msg: "unsupported_content_type:Only image data URLs are supported. " +
				"Non-image data payloads are not supported."}
		}
	} else if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_image_url:Image inputs must use http(s) URLs or data:image/... URLs."}
	}
	imageURL := jsonutil.ObjectOf("url", text)
	if detail != nil {
		detailText, isText := detail.(string)
		if !isText || strings.TrimSpace(detailText) == "" {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid_content_part:Image detail must be a non-empty string when provided."}
		}
		imageURL.Set("detail", strings.TrimSpace(detailText))
	}
	return jsonutil.ObjectOf("type", "image_url", "image_url", imageURL), nil
}

// Coded ValueError of a malformed file part
func apiFileInputError(code, msg string) error {
	return &PyError{Kind: "ValueError", Msg: code + ":" + msg}
}

// Bytes and media type of a file_data value, a data: URL or bare base64
func apiDecodeFileData(raw string) ([]byte, string, error) {
	text := strings.TrimSpace(raw)
	mediaType := ""
	if strings.HasPrefix(strings.ToLower(text), "data:") {
		header, payload, found := strings.Cut(text[len("data:"):], ",")
		if !found {
			return nil, "", apiFileInputError("invalid_file_data", "file_data data: URLs need a comma before the payload.")
		}
		params := strings.Split(header, ";")
		mediaType = strings.ToLower(strings.TrimSpace(params[0]))
		isBase64 := false
		for _, p := range params[1:] {
			if strings.EqualFold(strings.TrimSpace(p), "base64") {
				isBase64 = true
			}
		}
		if !isBase64 {
			decoded, err := url.PathUnescape(payload)
			if err != nil {
				return nil, "", apiFileInputError("invalid_file_data", "file_data is a data: URL whose payload does not percent-decode.")
			}
			return []byte(decoded), mediaType, nil
		}
		text = payload
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, text)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := enc.DecodeString(compact); err == nil {
			return data, mediaType, nil
		}
	}
	return nil, "", apiFileInputError("invalid_file_data", "file_data must be a data: URL or base64 text.")
}

// Bytes and media type of a file_url, fetched through the SSRF guarded client within the size ceiling
func apiFetchFileURL(ctx context.Context, raw string) ([]byte, string, error) {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return nil, "", apiFileInputError("invalid_file_url", "file_url must be an http(s) URL.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(raw), nil)
	if err != nil {
		return nil, "", apiFileInputError("invalid_file_url", "file_url is not a valid URL: "+err.Error())
	}
	resp, err := tools.NewSSRFSafeClient(nil, 60*time.Second, true).Do(req)
	if err != nil {
		return nil, "", apiFileInputError("file_url_unreachable", "file_url could not be fetched: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiFileInputError("file_url_unreachable", fmt.Sprintf("file_url answered HTTP %d.", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxFileURLBytes+1))
	if err != nil {
		return nil, "", apiFileInputError("file_url_unreachable", "file_url stopped mid download: "+err.Error())
	}
	if len(data) > apiMaxFileURLBytes {
		return nil, "", apiFileInputError("file_too_large", fmt.Sprintf("file_url is larger than %d MB.", apiMaxFileURLBytes>>20))
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return data, strings.ToLower(mediaType), nil
}

// Text of an uploaded file, UTF-8 text taken as is and every other document through the document reader
func apiFileText(ctx context.Context, data []byte, name, mediaType string) (string, error) {
	if !tools.IsExtractableDocument(name) && utf8.Valid(data) && (strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		strings.HasPrefix(http.DetectContentType(data), "text/")) {
		return string(data), nil
	}
	return tools.ExtractDocumentBytes(ctx, nil, data, name)
}

// One file part read into a text part naming where its bytes are kept, an image file becoming a vision part
func apiNormalizeFilePart(ctx context.Context, part *jsonutil.Object) (*jsonutil.Object, error) {
	fields := part
	if inner, ok := part.ValueOr("file", nil).(*jsonutil.Object); ok {
		fields = inner
	}
	field := func(key string) string {
		if v, ok := fields.ValueOr(key, nil).(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	var data []byte
	var mediaType string
	var err error
	switch {
	case field("file_data") != "":
		data, mediaType, err = apiDecodeFileData(field("file_data"))
	case field("file_url") != "":
		data, mediaType, err = apiFetchFileURL(ctx, field("file_url"))
	case field("file_id") != "":
		return nil, apiFileInputError("invalid_content_part", "file_id names a Files API upload, and this server keeps no Files API store. "+
			"Send the file inline as file_data (a data: URL or base64) or as file_url.")
	default:
		return nil, apiFileInputError("invalid_content_part", "File parts need file_data (a data: URL or base64) or file_url.")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, apiFileInputError("invalid_file_data", "The file is empty.")
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(data))
	}
	name := filepath.Base(field("filename"))
	if name == "." || name == "/" || name == "" {
		name = "document"
		if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			name += exts[0]
		}
	}
	if strings.HasPrefix(mediaType, "image/") {
		return jsonutil.ObjectOf("type", "image_url", "image_url", jsonutil.ObjectOf("url",
			"data:"+mediaType+";base64,"+base64.StdEncoding.EncodeToString(data))), nil
	}
	saved, err := mediacache.CacheDocumentFromBytes(ctx, home.Dir(ctx), data, name)
	if err != nil {
		return nil, err
	}
	text, readErr := apiFileText(ctx, data, name, mediaType)
	if readErr != nil {
		return jsonutil.ObjectOf("type", "text", "text", fmt.Sprintf("[The user sent a document: '%s'. It is saved at: %s. "+
			"Its text could not be read here (%s); read the saved file with read_file before answering.]", name, saved, readErr)), nil
	}
	note := fmt.Sprintf("[The user sent a document: '%s'. It is saved at: %s. Its text follows.]\n", name, saved)
	body := apiCapText(text)
	if runeLen(body) < runeLen(text) {
		body += "\n[The text continues past this point; read the saved file with read_file for the rest.]"
	}
	return jsonutil.ObjectOf("type", "text", "text", note+body), nil
}

// Validated multimodal content, a plain string when text only, else canonical text and image_url parts
func apiNormalizeMultimodalContent(ctx context.Context, content any) (any, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return apiCapText(v), nil
	case []any:
		var normalized []*jsonutil.Object
		for _, item := range apiCapList(v) {
			if text, isText := item.(string); isText {
				if text != "" {
					normalized = append(normalized, jsonutil.ObjectOf("type", "text", "text", apiCapText(text)))
				}
				continue
			}
			part, isDict := item.(*jsonutil.Object)
			if !isDict {
				continue
			}
			partType := apiPartType(part)
			switch {
			case apiTextPartTypes[partType]:
				if text := part.ValueOr("text", nil); text != nil && anyutil.String(text) != "" {
					normalized = append(normalized, jsonutil.ObjectOf("type", "text", "text", apiCapText(anyutil.String(text))))
				}
			case apiImagePartTypes[partType]:
				image, err := apiNormalizeImagePart(part)
				if err != nil {
					return nil, err
				}
				normalized = append(normalized, image)
			case apiFilePartTypes[partType]:
				file, err := apiNormalizeFilePart(ctx, part)
				if err != nil {
					return nil, err
				}
				normalized = append(normalized, file)
			default:
				return nil, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("unsupported_content_type:Unsupported content part type %s. "+
					"Content parts are text/input_text, image_url/input_image and file/input_file.", anyutil.Quote(part.ValueOr("type", nil)))}
			}
		}
		if len(normalized) == 0 {
			return "", nil
		}
		textOnly := true
		for _, part := range normalized {
			if part.ValueOr("type", nil) != "text" {
				textOnly = false
				break
			}
		}
		if textOnly {
			var texts []string
			for _, part := range normalized {
				if text := part.ValueOr("text", nil); anyutil.IsTrue(text) {
					texts = append(texts, text.(string))
				}
			}
			return strings.Join(texts, "\n"), nil
		}
		out := make([]any, len(normalized))
		for i, part := range normalized {
			out[i] = part
		}
		return out, nil
	}
	return apiNormalizeChatContent(content), nil
}

// Whether content carries any text, image or file attachment, empty turns being rejected
func apiContentHasVisiblePayload(content any) bool {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, item := range v {
			part, isDict := item.(*jsonutil.Object)
			if !isDict {
				continue
			}
			partType := apiPartType(part)
			text := part.ValueOr("text", nil)
			if apiImagePartTypes[partType] || apiFilePartTypes[partType] || (apiTextPartTypes[partType] && anyutil.IsTrue(text) && strings.TrimSpace(anyutil.String(text)) != "") {
				return true
			}
		}
	}
	return false
}
