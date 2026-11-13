package nemorelay

import (
	"context"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

type (
	EventSubscriberFn                func(event *types.Event)
	EventSanitizeFn                  func(ctx context.Context, event *types.Event, fields types.EventSanitizeFields) (types.EventSanitizeFields, error)
	EventMetadataInjectorFn          func(ctx context.Context, event *types.Event) (map[string]any, error)
	ConditionalMiddlewareGuardrailFn func(kinds []types.RuntimeRegistrationKind, registrationName string) *string
	ToolSanitizeFn                   func(ctx context.Context, toolName string, value any) (any, error)
	ToolConditionalFn                func(ctx context.Context, toolName string, args any) (*string, error)
	ToolInterceptFn                  func(ctx context.Context, toolName string, args any) (any, error)
	ToolExecutionNextFn              func(ctx context.Context, args any) (*types.ToolExecutionResult, error)
	ToolExecutionFn                  func(ctx context.Context, execution ToolExecutionContext, next ToolExecutionNextFn) (*types.ToolExecutionInterceptOutcome, error)
	LLMSanitizeRequestFn             func(ctx context.Context, request types.LLMRequest, sanitize LLMSanitizeRequestContext) (*types.LLMRequest, error)
	LLMSanitizeResponseFn            func(ctx context.Context, response any, sanitize LLMSanitizeResponseContext) (any, bool, error)
	LLMConditionalFn                 func(ctx context.Context, request types.LLMRequest) (*string, error)
	LLMRequestInterceptFn            func(ctx context.Context, name string, request types.LLMRequest, annotated *types.AnnotatedLLMRequest) (*types.LLMRequestInterceptOutcome, error)
	LLMExecutionNextFn               func(ctx context.Context, request types.LLMRequest) (any, error)
	LLMExecutionFn                   func(ctx context.Context, name string, request types.LLMRequest, execution *LLMExecutionContext, next LLMExecutionNextFn) (any, error)
	LLMStreamExecutionNextFn         func(ctx context.Context, request types.LLMRequest) (LLMJSONStream, error)
	LLMStreamExecutionFn             func(ctx context.Context, name string, request types.LLMRequest, execution *LLMExecutionContext, next LLMStreamExecutionNextFn) (LLMJSONStream, error)
	ToolExecuteFn                    = ToolExecutionNextFn
	LLMExecuteFn                     = LLMExecutionNextFn
)

type (
	LLMJSONStream         = types.LLMJSONStream
	LLMStreamTerminalizer = types.LLMStreamTerminalizer
)

// Tool name, arguments and call id handed to tool execution intercepts
type ToolExecutionContext struct {
	ToolName   string
	Args       any
	ToolCallID *string
}

// Request codec identity and capability visible to LLM request sanitizers
type LLMSanitizeRequestContext struct {
	Codec        types.LLMCodecIdentity
	RequestCodec types.LLMCodec
}

// Response codec identity and capability visible to LLM response sanitizers
type LLMSanitizeResponseContext struct {
	Codec         types.LLMCodecIdentity
	ResponseCodec types.LLMResponseCodec
}

// Builds a request context naming the codec, or the none identity without one
func LLMSanitizeRequestContextFor(codec types.LLMCodec) LLMSanitizeRequestContext {
	if codec == nil {
		return LLMSanitizeRequestContext{Codec: types.LLMCodecIdentityNone()}
	}
	return LLMSanitizeRequestContext{Codec: codec.CodecIdentity(), RequestCodec: codec}
}

// Builds a response context naming the codec, or the none identity without one
func LLMSanitizeResponseContextFor(codec types.LLMResponseCodec) LLMSanitizeResponseContext {
	if codec == nil {
		return LLMSanitizeResponseContext{Codec: types.LLMCodecIdentityNone()}
	}
	return LLMSanitizeResponseContext{Codec: codec.CodecIdentity(), ResponseCodec: codec}
}

// Codec capabilities available to LLM execution intercepts
type LLMExecutionContext struct {
	RequestCodec  LLMSanitizeRequestContext
	ResponseCodec *LLMSanitizeResponseContext
}

func NewLLMExecutionContext(request LLMSanitizeRequestContext, response *LLMSanitizeResponseContext) *LLMExecutionContext {
	return &LLMExecutionContext{RequestCodec: request, ResponseCodec: response}
}

func llmExecutionContextForNonStreaming(request types.LLMCodec, response types.LLMResponseCodec) *LLMExecutionContext {
	responseContext := LLMSanitizeResponseContextFor(response)
	return NewLLMExecutionContext(LLMSanitizeRequestContextFor(request), &responseContext)
}

func llmExecutionContextForStreaming(request types.LLMCodec) *LLMExecutionContext {
	return NewLLMExecutionContext(LLMSanitizeRequestContextFor(request), nil)
}

func inactiveExecutionCodecError() error {
	return invalidArgumentError("LLM execution codec capability is no longer active")
}

// Revocation gate shared by the codecs leased to one intercept invocation
type executionCodecGate struct {
	active atomic.Bool
}

func (g *executionCodecGate) ensureActive() error {
	if g.active.Load() {
		return nil
	}
	return inactiveExecutionCodecError()
}

// Leases codecs to one intercept invocation that stop working once it returns
func (c *LLMExecutionContext) lease() (*LLMExecutionContext, func()) {
	gate := &executionCodecGate{}
	gate.active.Store(true)
	request := LLMSanitizeRequestContext{Codec: c.RequestCodec.Codec}
	if c.RequestCodec.RequestCodec != nil {
		request.RequestCodec = &revocableRequestCodec{codec: c.RequestCodec.RequestCodec, identity: c.RequestCodec.Codec, gate: gate}
	}
	var response *LLMSanitizeResponseContext
	if c.ResponseCodec != nil {
		leased := LLMSanitizeResponseContext{Codec: c.ResponseCodec.Codec}
		if c.ResponseCodec.ResponseCodec != nil {
			leased.ResponseCodec = &revocableResponseCodec{codec: c.ResponseCodec.ResponseCodec, identity: c.ResponseCodec.Codec, gate: gate}
		}
		response = &leased
	}
	return NewLLMExecutionContext(request, response), func() { gate.active.Store(false) }
}

// Request codec that fails once its execution lease is revoked
type revocableRequestCodec struct {
	codec    types.LLMCodec
	identity types.LLMCodecIdentity
	gate     *executionCodecGate
}

func (c *revocableRequestCodec) CodecIdentity() types.LLMCodecIdentity {
	return c.identity
}

func (c *revocableRequestCodec) Decode(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
	if err := c.gate.ensureActive(); err != nil {
		return nil, err
	}
	return c.codec.Decode(request)
}

func (c *revocableRequestCodec) Encode(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest) (*types.LLMRequest, error) {
	if err := c.gate.ensureActive(); err != nil {
		return nil, err
	}
	return c.codec.Encode(annotated, original)
}

// Response codec that fails once its execution lease is revoked
type revocableResponseCodec struct {
	codec    types.LLMResponseCodec
	identity types.LLMCodecIdentity
	gate     *executionCodecGate
}

func (c *revocableResponseCodec) CodecIdentity() types.LLMCodecIdentity {
	return c.identity
}

func (c *revocableResponseCodec) DecodeResponse(response any) (*types.AnnotatedLLMResponse, error) {
	if err := c.gate.ensureActive(); err != nil {
		return nil, err
	}
	return c.codec.DecodeResponse(response)
}

func (c *revocableResponseCodec) AllowsEstimatedCost(response any) bool {
	if c.gate.ensureActive() != nil {
		return false
	}
	return types.AllowsEstimatedCost(c.codec, response)
}
