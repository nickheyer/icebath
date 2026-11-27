package generation

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/httpx"
)

const (
	// Address of OpenAI's API, where an OpenAI client goes when given none
	OpenAIBase = "https://api.openai.com/v1"
	// Tries after the first a request gets, and the limit of each wait for the service
	openAIRetries = 2
	openAITimeout = 600 * time.Second
)

// Client of the images and videos APIs of OpenAI and of the services that copy them, such as DeepInfra and Meta
type OpenAI struct {
	BaseURL string
	Key     string
	// Headers every request carries, such as OpenAI-Organization
	Headers map[string]string
}

// One file of an images edit
type Upload struct {
	Name string
	Data []byte
}

// Draws pictures: POST images/generations with the JSON body
func (c OpenAI) Images(ctx context.Context, body map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, err := c.send(ctx, http.MethodPost, "images/generations", nil, func() (io.Reader, string) {
		return bytes.NewReader(encoded), "application/json"
	})
	if err != nil {
		return nil, err
	}
	return reply(raw), nil
}

// Edits pictures: POST images/edits as a form, one image as image and several as image[]
func (c OpenAI) Edit(ctx context.Context, fields map[string]any, images []Upload) (map[string]any, error) {
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	for name, value := range fields {
		if err := field(w, name, value); err != nil {
			return nil, err
		}
	}
	name := "image"
	if len(images) > 1 {
		name = "image[]"
	}
	for _, image := range images {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, name, strings.ReplaceAll(image.Name, `"`, "")))
		header.Set("Content-Type", cmp.Or(TypeOf(image.Name), "application/octet-stream"))
		part, err := w.CreatePart(header)
		if err != nil {
			return nil, err
		}
		part.Write(image.Data)
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	payload, kind := form.Bytes(), w.FormDataContentType()
	raw, err := c.send(ctx, http.MethodPost, "images/edits", nil, func() (io.Reader, string) { return bytes.NewReader(payload), kind })
	if err != nil {
		return nil, err
	}
	return reply(raw), nil
}

// Form fields of one value, lists as name[] and objects as name[key]
func field(w *multipart.Writer, name string, value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case []any:
		for _, item := range v {
			if err := field(w, name+"[]", item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range v {
			if err := field(w, name+"["+key+"]", item); err != nil {
				return err
			}
		}
		return nil
	}
	return w.WriteField(name, fmt.Sprint(value))
}

// Starts a video job: POST videos with the JSON body
func (c OpenAI) CreateVideo(ctx context.Context, body map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, err := c.send(ctx, http.MethodPost, "videos", nil, func() (io.Reader, string) { return bytes.NewReader(encoded), "application/json" })
	if err != nil {
		return nil, err
	}
	return reply(raw), nil
}

// State of a video job
func (c OpenAI) Video(ctx context.Context, id string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("Expected a non-empty value for `video_id` but received ''")
	}
	raw, err := c.send(ctx, http.MethodGet, "videos/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, err
	}
	return reply(raw), nil
}

// Bytes of the video a finished job made
func (c OpenAI) Content(ctx context.Context, id string) ([]byte, error) {
	if id == "" {
		return nil, fmt.Errorf("Expected a non-empty value for `video_id` but received ''")
	}
	return c.send(ctx, http.MethodGet, "videos/"+url.PathEscape(id)+"/content", http.Header{"Accept": {"application/binary"}}, nil)
}

// Object of a reply, empty when it is none
func reply(raw []byte) map[string]any {
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// Sends a request, tried again on a broken connection, a conflict, a rate limit or a failure of the service, as the service's x-should-retry
// header and Retry-After say
func (c OpenAI) send(ctx context.Context, method, path string, header http.Header, body func() (io.Reader, string)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		status, h, raw, err := c.attempt(ctx, method, path, header, attempt, body)
		left := openAIRetries - attempt
		if err == nil && status >= 200 && status < 300 {
			return raw, nil
		}
		if err == nil && (left <= 0 || !httpx.Retried(status, h)) {
			return nil, apiError(status, raw)
		}
		if err != nil && (left <= 0 || ctx.Err() != nil) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(httpx.Backoff(attempt, h)):
		}
	}
}

func (c OpenAI) attempt(ctx context.Context, method, path string, header http.Header, attempt int, body func() (io.Reader, string)) (int, http.Header, []byte, error) {
	var reader io.Reader
	var kind string
	if body != nil {
		reader, kind = body()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	base := cmp.Or(strings.TrimSpace(c.BaseURL), OpenAIBase)
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/"+path, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("Connection error: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", Agent)
	request.Header.Set("X-Stainless-Retry-Count", strconv.Itoa(attempt))
	if c.Key != "" {
		request.Header.Set("Authorization", "Bearer "+c.Key)
	}
	if kind != "" {
		request.Header.Set("Content-Type", kind)
	}
	for name, value := range c.Headers {
		request.Header.Set(name, value)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: openAITimeout, ForceAttemptHTTP2: true}}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, fmt.Errorf("Request timed out: %w", err)
		}
		return 0, nil, nil, fmt.Errorf("Connection error: %w", err)
	}
	defer response.Body.Close()
	raw, err := Idle(response.Body, openAITimeout, cancel)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("Connection error: %w", err)
	}
	return response.StatusCode, response.Header, raw, nil
}

// Error of a failed status: Error code and what the service said
func apiError(status int, raw []byte) error {
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		compact, _ := json.Marshal(parsed)
		return fmt.Errorf("Error code: %d - %s", status, compact)
	}
	if text := strings.TrimSpace(strings.ToValidUTF8(string(raw), "�")); text != "" {
		return errors.New(text)
	}
	return fmt.Errorf("Error code: %d", status)
}
