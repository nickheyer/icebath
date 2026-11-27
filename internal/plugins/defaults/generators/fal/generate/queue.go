package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	falconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/config"
)

const (
	// Wait between two looks at a request in the queue
	pollEvery = 2 * time.Second
	// Limit of the submit
	submitTimeout = 120 * time.Second
	// Tries after the first a request gets, and the bounds of the wait before one
	maxRetries = 10
	baseDelay  = 0.1
	maxDelay   = 30.0
	userAgent  = "icebath-fal"
)

// Key FAL calls send: the FAL key, else its id and secret joined; empty when neither is set
func key(cfg *config.Config) string {
	if key := strings.TrimSpace(cfg.GetString(falconfig.APIKey)); key != "" {
		return key
	}
	id, secret := strings.TrimSpace(cfg.GetString(falconfig.KeyID)), strings.TrimSpace(cfg.GetString(falconfig.KeySecret))
	if id != "" && secret != "" {
		return id + ":" + secret
	}
	return ""
}

// Whether a FAL key is set
func ready(cfg *config.Config) bool {
	return key(cfg) != ""
}

// Address of FAL's queue
func queue(cfg *config.Config) string {
	host := strings.TrimSpace(cfg.GetString(falconfig.RunHost))
	if host == "" {
		host = falconfig.QueueRun
	}
	return "https://queue." + host + "/"
}

// Failed status FAL answered, carrying what it said
type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

// Stop of the call while FAL still works on it
var errInterrupted = errors.New("interrupted")

// A request in FAL's queue
type handle struct {
	key                              string
	id, response, status, cancelling string
}

// Puts the request for the application in FAL's queue
func submit(ctx context.Context, cfg *config.Config, application string, arguments map[string]any) (*handle, error) {
	k := key(cfg)
	if k == "" {
		return nil, errors.New("Please set the FAL_KEY environment variable to your API key.")
	}
	body, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	raw, err := send(ctx, k, http.MethodPost, queue(cfg)+application, body, submitTimeout, map[string]string{"x-idempotency-key": uuid.NewString()})
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	h := &handle{key: k}
	for field, into := range map[string]*string{"request_id": &h.id, "response_url": &h.response, "status_url": &h.status, "cancel_url": &h.cancelling} {
		text, ok := reply[field].(string)
		if !ok {
			return nil, fmt.Errorf("the fal.ai queue reply has no %s", field)
		}
		*into = text
	}
	return h, nil
}

// Waits for the request to complete, looking every two seconds, then reads its result; a stopped call leaves the job cancelled
func (h *handle) wait(ctx context.Context) (map[string]any, error) {
	for {
		raw, err := send(ctx, h.key, http.MethodGet, h.status+"?logs=0", nil, 0, nil)
		if err != nil {
			return nil, h.stopped(ctx, err)
		}
		var state struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		switch state.Status {
		case "COMPLETED":
			raw, err := send(ctx, h.key, http.MethodGet, h.response, nil, 0, nil)
			if err != nil {
				return nil, h.stopped(ctx, err)
			}
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			return result, nil
		case "IN_QUEUE", "IN_PROGRESS":
		default:
			return nil, fmt.Errorf("Unknown status: %s", state.Status)
		}
		select {
		case <-ctx.Done():
			return nil, h.stopped(ctx, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// The error of a wait, the interruption once the call was stopped, the job cancelled at FAL so it is not billed for nothing
func (h *handle) stopped(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	cancel, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	send(cancel, h.key, http.MethodPut, h.cancelling, nil, 0, nil)
	return errInterrupted
}

// Sends one request with the key, tried again on a broken connection, a conflict, a rate limit, or a gateway failure that is no JSON
func send(ctx context.Context, key, method, address string, body []byte, timeout time.Duration, headers map[string]string) ([]byte, error) {
	for retry := 0; ; retry++ {
		status, kind, raw, err := attempt(ctx, key, method, address, body, timeout, headers)
		again := err != nil && ctx.Err() == nil || err == nil && (status == 408 || status == 409 || status == 429 ||
			(status == 502 || status == 503 || status == 504) && !strings.Contains(kind, "application/json"))
		if !again || retry >= maxRetries {
			if err != nil {
				return nil, err
			}
			if status < 200 || status > 299 {
				return nil, failure(status, raw)
			}
			return raw, nil
		}
		delay := math.Min(baseDelay*math.Pow(2, float64(retry)), maxDelay) * (0.5 + rand.Float64())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(math.Min(delay, maxDelay) * float64(time.Second))):
		}
	}
}

func attempt(ctx context.Context, key, method, address string, body []byte, timeout time.Duration, headers map[string]string) (int, string, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return 0, "", nil, err
	}
	request.Header.Set("Authorization", "Key "+key)
	request.Header.Set("User-Agent", userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, "", nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", nil, err
	}
	return response.StatusCode, strings.ToLower(response.Header.Get("Content-Type")), raw, nil
}

// Error of a failed status with what FAL said: the detail of its JSON, else the body
func failure(status int, raw []byte) error {
	message := strings.TrimSpace(strings.ToValidUTF8(string(raw), "�"))
	var reply map[string]any
	if json.Unmarshal(raw, &reply) == nil {
		if detail, ok := reply["detail"]; ok {
			if text, ok := detail.(string); ok {
				message = text
			} else if encoded, err := json.Marshal(detail); err == nil {
				message = string(encoded)
			}
		}
	}
	if message == "" {
		message = fmt.Sprintf("FAL answered %d %s", status, http.StatusText(status))
	}
	return &statusError{Status: status, Message: message}
}
