package provider

import (
	"errors"
	"net/http"
	"strings"
)

// AuxSDKError is the OpenAI SDK APIError shape an auxiliary SDK client raises, status zero when status less
type AuxSDKError struct {
	StatusCode int
	Body       any
	Headers    http.Header
	Message    string
	Code       string
	Type       string
	Class      string
	Err        error
}

func (e *AuxSDKError) Error() string { return e.Message }

func (e *AuxSDKError) Unwrap() error { return e.Err }

// AuxRuntimeError is a Python RuntimeError raised by the auxiliary client, carrying an HTTP 200 provider error object
type AuxRuntimeError struct {
	Message       string
	ProviderError map[string]any
}

func (e *AuxRuntimeError) Error() string { return e.Message }

// Marks the error as a Python RuntimeError for callers that branch on the exception class
func (e *AuxRuntimeError) PyRuntimeError() {}

// HTTP status and error code of a Bedrock service error, zero and empty for any other error
func auxBotocoreError(err error) (int, string) {
	var ae *AWSServiceError
	if errors.As(err, &ae) {
		return ae.StatusCode, ae.Code
	}
	return 0, ""
}

// HTTP status on the error itself, its response, or a Bedrock error's response metadata, zero when absent
func auxExcHTTPStatus(err error) int {
	var sdk *AuxSDKError
	if errors.As(err, &sdk) && sdk.StatusCode != 0 {
		return sdk.StatusCode
	}
	var api *APIError
	if errors.As(err, &api) && api.StatusCode != 0 {
		return api.StatusCode
	}
	var gem *GeminiAPIError
	if errors.As(err, &gem) && gem.StatusCode != 0 {
		return gem.StatusCode
	}
	var cc *ccHTTPError
	if errors.As(err, &cc) && cc.Status != 0 {
		return cc.Status
	}
	var hs *HTTPStatusError
	if errors.As(err, &hs) && hs.Status != 0 {
		return hs.Status
	}
	status, _ := auxBotocoreError(err)
	return status
}

// HTTP status carried on the error itself or on its response, zero when absent
func ExcStatusCode(err error) int { return auxExcHTTPStatus(err) }

// Parsed SDK body of an error, a decoded JSON object or the raw text, nil when the error has none
func auxErrBody(err error) any {
	var sdk *AuxSDKError
	if errors.As(err, &sdk) {
		return sdk.Body
	}
	var api *APIError
	if errors.As(err, &api) && api.Body != "" {
		if m := decodeJSONObject(api.Body); m != nil {
			return m
		}
		return api.Body
	}
	return nil
}

// True for an HTTP 200 empty or malformed chat completion, routed like model incompatibility
func auxIsInvalidAuxResponseError(err error) bool {
	var re *AuxRuntimeError
	if !errors.As(err, &re) {
		return false
	}
	msg := strings.ToLower(re.Message)
	return strings.Contains(msg, "auxiliary ") && strings.Contains(msg, "llm returned invalid response") &&
		strings.Contains(msg, "choices[0].message")
}

// True for a status less structured provider error such as an SSE error event after a committed 200
func auxIsStatuslessStructuredProviderError(err error) bool {
	if auxExcHTTPStatus(err) != 0 {
		return false
	}
	body := auxErrBody(err)
	inner := body
	if m, ok := body.(map[string]any); ok {
		if v, has := m["error"]; has {
			inner = v
		}
	}
	switch e := inner.(type) {
	case string:
		return strings.TrimSpace(e) != ""
	case map[string]any:
		return auxTruthy(e["type"]) || auxTruthy(e["code"]) || auxTruthy(e["message"])
	}
	return false
}

// Python truthiness of a decoded JSON value
func auxTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
