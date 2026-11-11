package gateway

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
func apiPartType(part *pyjson.Object) string {
	raw := part.GetOr("type", nil)
	if !pyrepr.Truthy(raw) {
		return ""
	}
	return strings.ToLower(pystr.Strip(pyrepr.Str(raw)))
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
			case *pyjson.Object:
				if apiTextPartTypes[apiPartType(it)] {
					if text := it.GetOr("text", ""); pyrepr.Truthy(text) {
						part = pyrepr.Str(text)
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
	return apiCapText(pyrepr.Str(content))
}

// Canonical vision part of one image part, the coded ValueError of a malformed one
func apiNormalizeImagePart(part *pyjson.Object) (*pyjson.Object, error) {
	detail := part.GetOr("detail", nil)
	imageRef := part.GetOr("image_url", nil)
	urlValue := imageRef
	if ref, isDict := imageRef.(*pyjson.Object); isDict {
		urlValue = ref.GetOr("url", nil)
		detail = ref.GetOr("detail", detail)
	}
	text, ok := urlValue.(string)
	if !ok || pystr.Strip(text) == "" {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_image_url:Image parts must include a non-empty image URL."}
	}
	text = pystr.Strip(text)
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "data:") {
		if !strings.HasPrefix(lowered, "data:image/") || !strings.Contains(text, ",") {
			return nil, &PyError{Kind: "ValueError", Msg: "unsupported_content_type:Only image data URLs are supported. " +
				"Non-image data payloads are not supported."}
		}
	} else if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_image_url:Image inputs must use http(s) URLs or data:image/... URLs."}
	}
	imageURL := pyjson.ObjectOf("url", text)
	if detail != nil {
		detailText, isText := detail.(string)
		if !isText || pystr.Strip(detailText) == "" {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid_content_part:Image detail must be a non-empty string when provided."}
		}
		imageURL.Set("detail", pystr.Strip(detailText))
	}
	return pyjson.ObjectOf("type", "image_url", "image_url", imageURL), nil
}

// Validated multimodal content, a plain string when text only, else canonical text and image_url parts
func apiNormalizeMultimodalContent(content any) (any, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return apiCapText(v), nil
	case []any:
		var normalized []*pyjson.Object
		for _, item := range apiCapList(v) {
			if text, isText := item.(string); isText {
				if text != "" {
					normalized = append(normalized, pyjson.ObjectOf("type", "text", "text", apiCapText(text)))
				}
				continue
			}
			part, isDict := item.(*pyjson.Object)
			if !isDict {
				continue
			}
			partType := apiPartType(part)
			switch {
			case apiTextPartTypes[partType]:
				if text := part.GetOr("text", nil); text != nil && pyrepr.Str(text) != "" {
					normalized = append(normalized, pyjson.ObjectOf("type", "text", "text", apiCapText(pyrepr.Str(text))))
				}
			case apiImagePartTypes[partType]:
				image, err := apiNormalizeImagePart(part)
				if err != nil {
					return nil, err
				}
				normalized = append(normalized, image)
			case apiFilePartTypes[partType]:
				return nil, &PyError{Kind: "ValueError", Msg: "unsupported_content_type:Inline image inputs are supported, " +
					"but uploaded files and document inputs are not supported on this endpoint."}
			default:
				return nil, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("unsupported_content_type:Unsupported content part type %s. "+
					"Only text and image_url/input_image parts are supported.", pyrepr.Repr(part.GetOr("type", nil)))}
			}
		}
		if len(normalized) == 0 {
			return "", nil
		}
		textOnly := true
		for _, part := range normalized {
			if part.GetOr("type", nil) != "text" {
				textOnly = false
				break
			}
		}
		if textOnly {
			var texts []string
			for _, part := range normalized {
				if text := part.GetOr("text", nil); pyrepr.Truthy(text) {
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

// Whether content carries any text or image attachment, empty turns being rejected
func apiContentHasVisiblePayload(content any) bool {
	switch v := content.(type) {
	case string:
		return pystr.Strip(v) != ""
	case []any:
		for _, item := range v {
			part, isDict := item.(*pyjson.Object)
			if !isDict {
				continue
			}
			partType := apiPartType(part)
			text := part.GetOr("text", nil)
			if apiImagePartTypes[partType] || (apiTextPartTypes[partType] && pyrepr.Truthy(text) && pystr.Strip(pyrepr.Str(text)) != "") {
				return true
			}
		}
	}
	return false
}
