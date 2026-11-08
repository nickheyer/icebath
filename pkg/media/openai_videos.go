package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Video job object the SDK constructs from a videos response, attributes looked up like getattr
type openAIVideo struct {
	fields *pyjson.Object
	text   string
}

// Attribute of the job and whether the response carried it
func (v *openAIVideo) attr(key string) (any, bool) {
	if v == nil || v.fields == nil {
		return nil, false
	}
	return v.fields.Get(key)
}

// Plain attribute value, nil for a missing attribute
func (v *openAIVideo) get(key string) any {
	val, _ := v.attr(key)
	return pyjson.Plain(val)
}

// Python class name the job attribute errors report
func (v *openAIVideo) typeName() string {
	if v.fields == nil {
		return "str"
	}
	return "Video"
}

// Job id or the AttributeError text the SDK raises reading a missing id
func (v *openAIVideo) id() (string, error) {
	raw, ok := v.attr("id")
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'id'", v.typeName())
	}
	return pyrepr.Str(pyjson.Plain(raw)), nil
}

// Job error as str() renders the constructed VideoCreateError model, a plain value as itself
func (v *openAIVideo) errorText() (string, bool) {
	raw, ok := v.attr("error")
	if !ok || raw == nil {
		return "", false
	}
	obj, isObj := pyjson.AsObject(raw)
	if !isObj {
		s := pyrepr.Str(pyjson.Plain(raw))
		return s, pyTruthy(pyjson.Plain(raw))
	}
	var parts []string
	seen := map[string]bool{}
	for _, k := range []string{"code", "message"} {
		if val, ok := obj.Get(k); ok {
			parts = append(parts, k+"="+pyrepr.Repr(val))
			seen[k] = true
		}
	}
	for _, k := range obj.Keys {
		if !seen[k] {
			val, _ := obj.Get(k)
			parts = append(parts, k+"="+pyrepr.Repr(val))
		}
	}
	return strings.Join(parts, " "), true
}

// Video model of a 2xx body, the text kept when the body is not a JSON object
func parseOpenAIVideo(raw []byte) *openAIVideo {
	text, _ := pyjson.DecodeBytes(raw)
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return &openAIVideo{text: text}
	}
	if obj, ok := pyjson.AsObject(parsed); ok {
		return &openAIVideo{fields: obj}
	}
	return &openAIVideo{text: text}
}

var openAIVideoCreateParams = []string{"prompt", "input_reference", "model", "seconds", "size"}

// videos.create posting prompt, model, seconds and size as JSON with extra_body merged after them
func (c *openAIClient) VideosCreate(ctx context.Context, req map[string]any, extraBody map[string]any, extraOrder []string) (*openAIVideo, error) {
	if _, ok := req["prompt"]; !ok {
		return nil, &openAIError{Class: "TypeError", Message: "Videos.create() missing 1 required keyword-only argument: 'prompt'"}
	}
	for k := range req {
		if !containsString(openAIVideoCreateParams, k) {
			return nil, &openAIError{Class: "TypeError", Message: fmt.Sprintf("Videos.create() got an unexpected keyword argument '%s'", k)}
		}
	}
	obj := pyjson.NewObject()
	for _, k := range openAIVideoCreateParams {
		if v, ok := req[k]; ok {
			obj.Set(k, v)
		}
	}
	for _, k := range extraOrder {
		if v, ok := extraBody[k]; ok {
			obj.Set(k, v)
		}
	}
	body, err := pyjson.Dumps(obj, pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	_, raw, err := c.send(ctx, http.MethodPost, "/videos", nil, func() (io.Reader, string, error) {
		return strings.NewReader(body), "application/json", nil
	})
	if err != nil {
		return nil, err
	}
	return parseOpenAIVideo(raw), nil
}

// Path segment of a video id, the SDK ValueError for an empty id
func openAIVideoPath(videoID, suffix string) (string, error) {
	if videoID == "" {
		return "", &openAIError{Class: "ValueError", Message: fmt.Sprintf("Expected a non-empty value for `video_id` but received %s", pyrepr.Repr(videoID))}
	}
	return "/videos/" + url.PathEscape(videoID) + suffix, nil
}

// videos.retrieve reading the current job state
func (c *openAIClient) VideosRetrieve(ctx context.Context, videoID string) (*openAIVideo, error) {
	path, err := openAIVideoPath(videoID, "")
	if err != nil {
		return nil, err
	}
	_, raw, err := c.send(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return parseOpenAIVideo(raw), nil
}

// videos.download_content returning the rendered video bytes
func (c *openAIClient) VideosDownloadContent(ctx context.Context, videoID string) ([]byte, error) {
	path, err := openAIVideoPath(videoID, "/content")
	if err != nil {
		return nil, err
	}
	_, raw, err := c.send(ctx, http.MethodGet, path, http.Header{"Accept": {"application/binary"}}, nil)
	return raw, err
}
