package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Video job object the SDK constructs from a videos response, attributes looked up like getattr
type openAIVideo struct {
	fields *jsonutil.Object
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
	return jsonutil.Plain(val)
}

// Job id or the AttributeError text the SDK raises reading a missing id
func (v *openAIVideo) id() (string, error) {
	raw, ok := v.attr("id")
	if !ok {
		return "", errors.New("OpenAI video reply has no id")
	}
	return anyutil.String(jsonutil.Plain(raw)), nil
}

// Job error as str() renders the constructed VideoCreateError model, a plain value as itself
func (v *openAIVideo) errorText() (string, bool) {
	raw, ok := v.attr("error")
	if !ok || raw == nil {
		return "", false
	}
	obj, isObj := jsonutil.AsObject(raw)
	if !isObj {
		s := anyutil.String(jsonutil.Plain(raw))
		return s, pyTruthy(jsonutil.Plain(raw))
	}
	var parts []string
	seen := map[string]bool{}
	for _, k := range []string{"code", "message"} {
		if val, ok := obj.Get(k); ok {
			parts = append(parts, k+"="+errchain.Show(val))
			seen[k] = true
		}
	}
	for _, k := range obj.Keys() {
		if !seen[k] {
			val, _ := obj.Get(k)
			parts = append(parts, k+"="+errchain.Show(val))
		}
	}
	return strings.Join(parts, " "), true
}

// Video model of a 2xx body, the text kept when the body is not a JSON object
func parseOpenAIVideo(raw []byte) *openAIVideo {
	text, _ := jsonutil.Text(raw)
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		return &openAIVideo{text: text}
	}
	if obj, ok := jsonutil.AsObject(parsed); ok {
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
			return nil, &openAIError{Class: "TypeError", Message: fmt.Sprintf("video create request has an unknown option %s", k)}
		}
	}
	obj := jsonutil.NewObject()
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
	body, err := jsonutil.Encode(obj, jsonutil.Spaced(), jsonutil.ASCII())
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
		return "", &openAIError{Class: "ValueError", Message: fmt.Sprintf("Expected a non-empty value for `video_id` but received %s", errchain.Show(videoID))}
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
