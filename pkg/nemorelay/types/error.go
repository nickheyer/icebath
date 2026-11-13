package types

import (
	"errors"
	"fmt"
	"strings"
)

// UpstreamFailureClass classifies a provider failure
type UpstreamFailureClass uint8

const (
	UpstreamFailureClassConnection UpstreamFailureClass = iota + 1
	UpstreamFailureClassTimeout
	UpstreamFailureClassRetryableStatus
	UpstreamFailureClassContextWindow
	UpstreamFailureClassModelUnavailable
	UpstreamFailureClassAuthentication
	UpstreamFailureClassInvalidRequest
	UpstreamFailureClassOther
)

var upstreamFailureClassNames = []string{"connection", "timeout", "retryable_status", "context_window", "model_unavailable", "authentication", "invalid_request", "other"}

var upstreamFailureClassDebug = []string{"Connection", "Timeout", "RetryableStatus", "ContextWindow", "ModelUnavailable", "Authentication", "InvalidRequest", "Other"}

// String returns the snake_case wire name
func (c UpstreamFailureClass) String() string {
	if c < 1 || int(c) > len(upstreamFailureClassNames) {
		return ""
	}
	return upstreamFailureClassNames[c-1]
}

func (c UpstreamFailureClass) rustDebug(b *strings.Builder) {
	if c >= 1 && int(c) <= len(upstreamFailureClassDebug) {
		b.WriteString(upstreamFailureClassDebug[c-1])
	}
}

func (c UpstreamFailureClass) serdeSerialize() (any, error) { return c.String(), nil }

func (c *UpstreamFailureClass) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, upstreamFailureClassNames)
	if err != nil {
		return err
	}
	*c = UpstreamFailureClass(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (c UpstreamFailureClass) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *UpstreamFailureClass) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// UpstreamFailure describes a failed provider call
type UpstreamFailure struct {
	Status  *uint16              `json:"status" serde:"option"`
	Body    string               `json:"body"`
	Headers map[string]string    `json:"headers"`
	Class   UpstreamFailureClass `json:"class"`
}

// IsRetryable reports whether the failure class allows a retry
func (f UpstreamFailure) IsRetryable() bool {
	switch f.Class {
	case UpstreamFailureClassConnection, UpstreamFailureClassTimeout, UpstreamFailureClassRetryableStatus,
		UpstreamFailureClassContextWindow, UpstreamFailureClassModelUnavailable:
		return true
	}
	return false
}

// String renders the failure exactly as the Rust Display does
func (f UpstreamFailure) String() string {
	var class strings.Builder
	f.Class.rustDebug(&class)
	if f.Status != nil {
		return fmt.Sprintf("upstream provider returned HTTP %d (%s): %s", *f.Status, class.String(), f.Body)
	}
	return fmt.Sprintf("upstream provider transport failure (%s): %s", class.String(), f.Body)
}

// MarshalJSON writes the serde form
func (f UpstreamFailure) MarshalJSON() ([]byte, error) { return EncodeJSON(f) }

// UnmarshalJSON reads the serde form
func (f *UpstreamFailure) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, f) }

// FlowErrorKind selects the runtime error variant
type FlowErrorKind uint8

const (
	FlowErrorAlreadyExists FlowErrorKind = iota + 1
	FlowErrorNotFound
	FlowErrorInvalidArgument
	FlowErrorScopeStackEmpty
	FlowErrorGuardrailRejected
	FlowErrorUpstream
	FlowErrorInternal
	FlowErrorCallbackException
)

// FlowError is the runtime error with one variant per Rust FlowError case
type FlowError struct {
	Kind              FlowErrorKind
	Message           string
	Upstream          *UpstreamFailure
	ExceptionTypeName string
	Source            error
}

// NewFlowError builds a message-carrying runtime error of the given kind
func NewFlowError(kind FlowErrorKind, message string) *FlowError {
	return &FlowError{Kind: kind, Message: message}
}

// InvalidArgumentf builds an invalid argument error from a format
func InvalidArgumentf(format string, args ...any) *FlowError {
	return &FlowError{Kind: FlowErrorInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

// Internalf builds an internal error from a format
func Internalf(format string, args ...any) *FlowError {
	return &FlowError{Kind: FlowErrorInternal, Message: fmt.Sprintf(format, args...)}
}

// NotFoundf builds a not found error from a format
func NotFoundf(format string, args ...any) *FlowError {
	return &FlowError{Kind: FlowErrorNotFound, Message: fmt.Sprintf(format, args...)}
}

// AlreadyExistsf builds an already exists error from a format
func AlreadyExistsf(format string, args ...any) *FlowError {
	return &FlowError{Kind: FlowErrorAlreadyExists, Message: fmt.Sprintf(format, args...)}
}

// GuardrailRejectedf builds a guardrail rejection error from a format
func GuardrailRejectedf(format string, args ...any) *FlowError {
	return &FlowError{Kind: FlowErrorGuardrailRejected, Message: fmt.Sprintf(format, args...)}
}

// NewScopeStackEmptyError builds the empty scope stack error
func NewScopeStackEmptyError() *FlowError {
	return &FlowError{Kind: FlowErrorScopeStackEmpty}
}

// NewUpstreamFlowError wraps a provider failure
func NewUpstreamFlowError(failure UpstreamFailure) *FlowError {
	return &FlowError{Kind: FlowErrorUpstream, Upstream: &failure}
}

// NewCallbackExceptionFlowError wraps a failure raised by a host callback
func NewCallbackExceptionFlowError(message, exceptionType string, source error) *FlowError {
	return &FlowError{Kind: FlowErrorCallbackException, Message: message, ExceptionTypeName: exceptionType, Source: source}
}

// Error renders the variant exactly as the Rust Display does
func (e *FlowError) Error() string {
	switch e.Kind {
	case FlowErrorAlreadyExists:
		return "already exists: " + e.Message
	case FlowErrorNotFound:
		return "not found: " + e.Message
	case FlowErrorInvalidArgument:
		return "invalid argument: " + e.Message
	case FlowErrorScopeStackEmpty:
		return "scope stack empty"
	case FlowErrorGuardrailRejected:
		return "guardrail rejected: " + e.Message
	case FlowErrorUpstream:
		if e.Upstream == nil {
			return ""
		}
		return e.Upstream.String()
	}
	return "internal error: " + e.Message
}

// Unwrap exposes the callback source error
func (e *FlowError) Unwrap() error {
	return e.Source
}

// OTelErrorType returns the error.type attribute value for the variant
func (e *FlowError) OTelErrorType() string {
	switch e.Kind {
	case FlowErrorAlreadyExists:
		return "already_exists"
	case FlowErrorNotFound:
		return "not_found"
	case FlowErrorInvalidArgument:
		return "invalid_argument"
	case FlowErrorScopeStackEmpty:
		return "scope_stack_empty"
	case FlowErrorGuardrailRejected:
		return "guardrail_rejected"
	case FlowErrorUpstream:
		class := UpstreamFailureClassOther
		if e.Upstream != nil {
			class = e.Upstream.Class
		}
		switch class {
		case UpstreamFailureClassConnection:
			return "connection_error"
		case UpstreamFailureClassTimeout:
			return "timeout"
		case UpstreamFailureClassRetryableStatus:
			return "retryable_status"
		case UpstreamFailureClassContextWindow:
			return "context_window"
		case UpstreamFailureClassModelUnavailable:
			return "model_unavailable"
		case UpstreamFailureClassAuthentication:
			return "authentication"
		case UpstreamFailureClassInvalidRequest:
			return "invalid_request"
		}
		return "upstream_error"
	}
	return "internal_error"
}

// ExceptionType returns the callback exception type name, nil for other variants
func (e *FlowError) ExceptionType() *string {
	if e.Kind != FlowErrorCallbackException {
		return nil
	}
	name := e.ExceptionTypeName
	return &name
}

// AsFlowError finds a FlowError in an error chain
func AsFlowError(err error) (*FlowError, bool) {
	var fe *FlowError
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// IsFlowErrorKind reports whether an error chain holds a FlowError of the kind
func IsFlowErrorKind(err error, kind FlowErrorKind) bool {
	fe, ok := AsFlowError(err)
	return ok && fe.Kind == kind
}
