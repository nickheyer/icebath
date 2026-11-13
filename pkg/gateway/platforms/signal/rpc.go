package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultRPCTimeout = 30 * time.Second

// JSON-RPC 2.0 client of a signal-cli daemon running in HTTP mode
type rpcClient struct {
	baseURL string
	account string
	http    *http.Client
}

// JSON-RPC error object signal-cli answers a failed method with
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("signal-cli error %d: %s", e.Code, e.Message) }

// Error object of a raw JSON-RPC error member, a bare value becoming its message
func parseRPCError(raw json.RawMessage) *rpcError {
	var e rpcError
	if err := json.Unmarshal(raw, &e); err == nil {
		return &e
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return &rpcError{Message: text}
	}
	return &rpcError{Message: string(raw)}
}

// Whether a raw JSON member carries a value
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// Calls one method and returns its raw result, an *rpcError for a JSON-RPC error answer
func (c *rpcClient) call(ctx context.Context, method string, params map[string]any, id string, timeout time.Duration) (json.RawMessage, error) {
	if id == "" {
		id = fmt.Sprintf("%s_%d", method, time.Now().UnixMilli())
	}
	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &out)
	if decodeErr == nil && present(out.Error) {
		return nil, parseRPCError(out.Error)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("signal-cli answered HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("signal-cli sent an invalid JSON-RPC answer: %w", decodeErr)
	}
	return out.Result, nil
}

// GET of the daemon liveness endpoint, nil only for a 200 answer
func (c *rpcClient) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/check", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &checkStatusError{status: resp.StatusCode}
	}
	return nil
}

// Liveness answer other than 200
type checkStatusError struct{ status int }

func (e *checkStatusError) Error() string { return fmt.Sprintf("status %d", e.status) }

// Per recipient outcome of a send
type recipientResult struct {
	Type              string      `json:"type"`
	Success           *bool       `json:"success"`
	Failure           string      `json:"failure"`
	RetryAfterSeconds flexSeconds `json:"retryAfterSeconds"`
}

// Timestamp and per recipient outcomes of a send
type sendResponse struct {
	Timestamp int64             `json:"timestamp"`
	Results   []recipientResult `json:"results"`
}

// Decoded send result with its raw form kept for the send result
type sendOutcome struct {
	response sendResponse
	raw      map[string]any
}

// Decodes a send result, tolerating fields of an unexpected type
func decodeSendOutcome(raw json.RawMessage) *sendOutcome {
	out := &sendOutcome{}
	if !present(raw) {
		return out
	}
	if err := json.Unmarshal(raw, &out.response); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			out.response = sendResponse{}
		}
	}
	_ = json.Unmarshal(raw, &out.raw)
	return out
}

// Delivery verdict, delivered when any recipient succeeded, a rate limit when only rate limits failed
func (o *sendOutcome) verdict() (delivered bool, failure string, rateLimit *rateLimitError) {
	if len(o.response.Results) == 0 {
		return true, "", nil
	}
	var retry float64
	onlyRateLimited := true
	for _, r := range o.response.Results {
		failed := ""
		switch {
		case r.Type != "" && r.Type != "SUCCESS":
			failed = r.Type
		case r.Success != nil && !*r.Success:
			failed = r.Failure
			if failed == "" {
				failed = "Recipient delivery failed"
			}
		default:
			delivered = true
			continue
		}
		if failure == "" {
			failure = failed
		}
		if r.Type == "RATE_LIMIT_FAILURE" {
			retry = max(retry, float64(r.RetryAfterSeconds))
		} else {
			onlyRateLimited = false
		}
	}
	if !delivered && onlyRateLimited {
		rateLimit = &rateLimitError{message: "Rate limit exceeded for recipient", retryAfter: retry}
	}
	return delivered, failure, rateLimit
}

// Rate limit hint of recipients that failed alongside delivered ones, nil when none was rate limited
func (o *sendOutcome) partialRateLimit() *rateLimitError {
	var retry float64
	hit := false
	for _, r := range o.response.Results {
		if r.Type == "RATE_LIMIT_FAILURE" {
			hit = true
			retry = max(retry, float64(r.RetryAfterSeconds))
		}
	}
	if !hit {
		return nil
	}
	return &rateLimitError{message: "Rate limit exceeded for some recipients", retryAfter: retry}
}

// Routing params of a chat, groupId for group chats and a one element recipient list otherwise
func targetParams(params map[string]any, chatID, recipient string) map[string]any {
	if gid, ok := strings.CutPrefix(chatID, "group:"); ok {
		params["groupId"] = gid
	} else {
		params["recipient"] = []string{recipient}
	}
	return params
}
