package nemorelay

import (
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

type (
	FlowError            = types.FlowError
	FlowErrorKind        = types.FlowErrorKind
	UpstreamFailure      = types.UpstreamFailure
	UpstreamFailureClass = types.UpstreamFailureClass
)

func alreadyExistsError(message string) error {
	return types.NewFlowError(types.FlowErrorAlreadyExists, message)
}

func notFoundError(message string) error {
	return types.NewFlowError(types.FlowErrorNotFound, message)
}

func invalidArgumentError(message string) error {
	return types.NewFlowError(types.FlowErrorInvalidArgument, message)
}

func guardrailRejectedError(message string) error {
	return types.NewFlowError(types.FlowErrorGuardrailRejected, message)
}

func internalError(message string) error {
	return types.NewFlowError(types.FlowErrorInternal, message)
}

// Returns the OpenTelemetry error type of a runtime error, internal for foreign errors
func otelErrorType(err error) string {
	if flow, ok := types.AsFlowError(err); ok {
		return flow.OTelErrorType()
	}
	return "internal_error"
}

// Returns the exception type a callback error carries
func errorExceptionType(err error) *string {
	if flow, ok := types.AsFlowError(err); ok {
		return flow.ExceptionType()
	}
	return nil
}
