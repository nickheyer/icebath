package provider

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Python module of the SDK exception classes
func (e *AuxSDKError) PyModule() string { return "openai" }

// Base class of each httpx and openai SDK exception class the auxiliary clients raise
var auxSDKExceptionParents = map[string]string{
	"HTTPError": "Exception", "RequestError": "HTTPError", "TransportError": "RequestError",
	"TimeoutException": "TransportError", "ConnectTimeout": "TimeoutException", "ReadTimeout": "TimeoutException",
	"WriteTimeout": "TimeoutException", "PoolTimeout": "TimeoutException", "NetworkError": "TransportError",
	"ConnectError": "NetworkError", "ReadError": "NetworkError", "WriteError": "NetworkError", "CloseError": "NetworkError",
	"ProtocolError": "TransportError", "LocalProtocolError": "ProtocolError", "RemoteProtocolError": "ProtocolError",
	"ProxyError": "TransportError", "UnsupportedProtocol": "TransportError", "DecodingError": "RequestError",
	"TooManyRedirects": "RequestError", "HTTPStatusError": "HTTPError", "InvalidURL": "Exception",
	"CookieConflict": "Exception", "StreamError": "RuntimeError", "StreamConsumed": "StreamError",
	"StreamClosed": "StreamError", "ResponseNotRead": "StreamError", "RequestNotRead": "StreamError",
	"OpenAIError": "Exception", "APIError": "OpenAIError", "APIResponseValidationError": "APIError",
	"APIStatusError": "APIError", "BadRequestError": "APIStatusError", "AuthenticationError": "APIStatusError",
	"PermissionDeniedError": "APIStatusError", "NotFoundError": "APIStatusError", "ConflictError": "APIStatusError",
	"UnprocessableEntityError": "APIStatusError", "RateLimitError": "APIStatusError",
	"InternalServerError": "APIStatusError", "APIConnectionError": "APIError", "APITimeoutError": "APIConnectionError",
	"LengthFinishReasonError": "OpenAIError", "ContentFilterFinishReasonError": "OpenAIError",
}

// Class names of the SDK exception through BaseException as type(exc).__mro__ lists them
func (e *AuxSDKError) PyExceptionMRO() []string {
	return pyrepr.ExceptionMROFrom(e.PyTypeName(), auxSDKExceptionParents)
}

// Class names of the httpx transport exception through BaseException as type(exc).__mro__ lists them
func (e *AuxTransportError) PyExceptionMRO() []string {
	return pyrepr.ExceptionMROFrom(e.Class, auxSDKExceptionParents)
}

var (
	_ interface{ PyExceptionMRO() []string } = (*AuxSDKError)(nil)
	_ interface{ PyExceptionMRO() []string } = (*AuxTransportError)(nil)
	_ interface{ PyModule() string }         = (*AuxSDKError)(nil)
	_ interface{ PyModule() string }         = (*AuxTransportError)(nil)
	_ interface{ PyModule() string }         = (*AWSServiceError)(nil)
	_ interface{ PyModule() string }         = (*AWSReadTimeoutError)(nil)
	_ interface{ PyModule() string }         = (*AWSCredentialError)(nil)
	_ interface{ PyModule() string }         = (*lmHTTPStatusError)(nil)
)

// Python exception class the SDK raised, APIError when unclassified
func (e *AuxSDKError) PyTypeName() string {
	if e.Class == "" {
		return "APIError"
	}
	return e.Class
}

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
