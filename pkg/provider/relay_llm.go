package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	relayProviderMessageExtensionKeys = []string{"reasoning_content", "reasoning_details"}
	relayInternalProviderHeaders      = map[string]bool{"x-dynamo-parent-session-id": true, "x-dynamo-session-id": true}
	relayAcloseTimeout                = 10.0
	relayProtocolByAPIMode            = map[string][2]string{
		"chat_completions":   {"openai.chat_completions", "OpenAIChatCodec"},
		"codex_responses":    {"openai.responses", "OpenAIResponsesCodec"},
		"anthropic_messages": {"anthropic.messages", "AnthropicMessagesCodec"},
	}
	relayCodecFn = relayCodec
)

const relaySyncLoopError = "Synchronous Relay LLM execution cannot run on an event-loop thread"

// Python str of a metadata value or empty when the value is falsy
func relayTruthyStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

func relayAPIMode(metadata map[string]any) string { return relayTruthyStr(metadata["api_mode"]) }

// Relay canonical operation name for a known API mode, the provider name otherwise
func relayOperationName(providerName string, metadata map[string]any) string {
	if p, ok := relayProtocolByAPIMode[relayAPIMode(metadata)]; ok {
		return p[0]
	}
	return providerName
}

// JSON metadata keeping the physical provider when the operation name is canonical
func relayMetadata(providerName string, metadata map[string]any) map[string]any {
	out := relayJSONableDict(metadata)
	if _, ok := out["icebath.provider"]; !ok {
		out["icebath.provider"] = providerName
	}
	return out
}

type relayLogicalCall struct {
	turn      *tools.RelayTurnContext
	handle    any
	requestID string
}

type relayManagedAttempt struct {
	runtime       *tools.RelayRuntime
	session       *tools.RelaySession
	request       map[string]any
	metadata      map[string]any
	logical       *relayLogicalCall
	parent        any
	body          map[string]any
	relayRequest  any
	codecBaseline map[string]any
	operation     string
	relayKwargs   map[string]any
	context       *pyctx.Context
	rawMu         sync.Mutex
	hasValue      bool
	rawValue      any
	rawJSON       any
	rawErr        error
}

// Managed attempt for the session, the inherited turn's when nil, or nil to run unmanaged
func resolveRelayManagedAttempt(ctx context.Context, sessionID *string, request, metadata map[string]any, name, modelName string) (*relayManagedAttempt, error) {
	var sid string
	if sessionID == nil {
		s, ok := relayCurrentSessionID(ctx)
		if !ok {
			return nil, nil
		}
		sid = s
	} else {
		sid = *sessionID
	}
	if sid == "" {
		return nil, nil
	}
	rt, session, parent, err := tools.ResolveRelayExecutionContext(ctx, sid)
	if err != nil {
		return nil, err
	}
	if rt == nil || session == nil || !rt.ManagedExecutionEnabled() {
		return nil, nil
	}
	return newRelayManagedAttempt(ctx, rt, session, parent, request, metadata, name, modelName)
}

func relayCodecValue(c *tools.NemoRelayCodec) any {
	if c == nil {
		return nil
	}
	return c
}

// Builds the Relay request, codec baseline and call kwargs shared by every adapter
func newRelayManagedAttempt(ctx context.Context, rt *tools.RelayRuntime, session *tools.RelaySession, parent any, request, metadata map[string]any, name, modelName string) (*relayManagedAttempt, error) {
	a := &relayManagedAttempt{runtime: rt, session: session, request: request, metadata: metadata}
	logical, err := relayLogicalParent(ctx, rt, session, parent, metadata)
	if err != nil {
		return nil, err
	}
	a.logical, a.parent = logical, parent
	if logical != nil {
		a.parent = logical.handle
	}
	a.body = relayRequestBody(request, metadata)
	relay := rt.Relay
	if relay.LLMRequest == nil {
		return nil, tools.RelayAttributeError("NemoRelay", "LLMRequest")
	}
	if a.relayRequest, err = relay.LLMRequest(map[string]any{}, a.body); err != nil {
		return nil, err
	}
	if a.codecBaseline, err = relayCodecRoundTripRequestBody(ctx, relay, a.relayRequest, a.body, metadata); err != nil {
		return nil, err
	}
	a.operation = relayOperationName(name, metadata)
	codec, err := relayCodecFn(relay, metadata)
	if err != nil {
		return nil, err
	}
	responseCodec, err := relayCodecFn(relay, metadata)
	if err != nil {
		return nil, err
	}
	a.relayKwargs = map[string]any{
		"handle": a.parent, "metadata": relayMetadata(name, metadata), "model_name": modelName,
		"codec": relayCodecValue(codec), "response_codec": relayCodecValue(responseCodec),
	}
	a.context = pyctx.CopyContext(ctx)
	return a, nil
}

func (a *relayManagedAttempt) providerRequest(next any) map[string]any {
	return relayProviderRequest(a.request, next, a.body, a.codecBaseline, a.metadata)
}

// Runs an icebath callback in a fresh copy of the captured context where nested relay calls run unmanaged
func (a *relayManagedAttempt) runCallback(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	return fn(tools.RelayManagedCallbackGuard(pyctx.With(ctx, a.context.Copy())))
}

func (a *relayManagedAttempt) record(raw any) any {
	a.rawMu.Lock()
	defer a.rawMu.Unlock()
	a.hasValue, a.rawValue, a.rawJSON = true, raw, tools.RelayJSONable(raw)
	return a.rawJSON
}

// Provider callback handed to Relay running callback on its possibly rewritten request
func (a *relayManagedAttempt) invoke(callback func(ctx context.Context, request map[string]any) (any, error), async bool) tools.RelayProviderCallback {
	return func(ctx context.Context, next any) (any, error) {
		final := a.providerRequest(next)
		raw, err := a.runCallback(ctx, func(c context.Context) (any, error) {
			v, err := callback(c, final)
			if aw, ok := v.(tools.RelayAwaitable); ok && async && err == nil {
				return aw(c)
			}
			return v, err
		})
		if err != nil {
			a.rawMu.Lock()
			a.rawErr = err
			a.rawMu.Unlock()
			return nil, err
		}
		return a.record(raw), nil
	}
}

// Runs the Relay call inside the session context and awaits it
func (a *relayManagedAttempt) runManaged(ctx context.Context, relayCall func(ctx context.Context) (any, error)) (any, error) {
	return a.runtime.RunInSessionAsync(ctx, a.session, relayCall, false)
}

// Raises the provider's own error or recovers a completed provider result
func (a *relayManagedAttempt) resolveFailure(ctx context.Context, err error, deferLogical bool) (any, error) {
	a.rawMu.Lock()
	callbackErr, hasValue, value := a.rawErr, a.hasValue, a.rawValue
	a.rawMu.Unlock()
	if callbackErr != nil && tools.IsRelayWrappedCallbackError(err, callbackErr) {
		return nil, callbackErr
	}
	if auxIsBaseException(err) || callbackErr != nil || !hasValue {
		return nil, err
	}
	slog.WarnContext(ctx, "NeMo Relay LLM post-processing failed after provider success; returning the provider response", "error", err)
	if cerr := a.complete(ctx, deferLogical); cerr != nil {
		return nil, cerr
	}
	return value, nil
}

func (a *relayManagedAttempt) result(ctx context.Context, managed any, deferLogical bool) (any, error) {
	if err := a.complete(ctx, deferLogical); err != nil {
		return nil, err
	}
	a.rawMu.Lock()
	hasValue, value, raw := a.hasValue, a.rawValue, a.rawJSON
	a.rawMu.Unlock()
	if hasValue && relayJSONEqual(managed, raw) {
		return value, nil
	}
	return relayNamespace(managed), nil
}

func (a *relayManagedAttempt) complete(ctx context.Context, deferLogical bool) error {
	if deferLogical {
		return nil
	}
	return relayCompleteLogical(ctx, a.logical, "success", nil, nil, nil, nil, nil)
}

// Session id of the inherited icebath turn, false outside a live turn
func relayCurrentSessionID(ctx context.Context) (string, bool) {
	turn := tools.ActiveRelayTurn(ctx, nil)
	if turn == nil {
		return "", false
	}
	return turn.Lease.SessionID, true
}

// Options of one physical provider attempt through Relay
type RelayExecuteOptions struct {
	Name                   string
	ModelName              string
	SessionID              *string
	Metadata               map[string]any
	DeferLogicalCompletion bool
}

// Runs one non streaming provider attempt through Relay under the session or the inherited turn
func RelayExecute(ctx context.Context, request map[string]any, callback func(ctx context.Context, request map[string]any) (any, error), o RelayExecuteOptions) (any, error) {
	return relayExecute(ctx, request, callback, o, false)
}

// Async twin of RelayExecute awaiting an awaitable callback result on the caller's loop
func RelayExecuteAsync(ctx context.Context, request map[string]any, callback func(ctx context.Context, request map[string]any) (any, error), o RelayExecuteOptions) (any, error) {
	return relayExecute(ctx, request, callback, o, true)
}

func relayExecute(ctx context.Context, request map[string]any, callback func(ctx context.Context, request map[string]any) (any, error), o RelayExecuteOptions, async bool) (any, error) {
	attempt, err := resolveRelayManagedAttempt(ctx, o.SessionID, request, o.Metadata, o.Name, o.ModelName)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		v, err := callback(ctx, request)
		if aw, ok := v.(tools.RelayAwaitable); ok && async && err == nil {
			return aw(ctx)
		}
		return v, err
	}
	call := func(c context.Context) (any, error) {
		llm := attempt.runtime.Relay.LLM
		if llm == nil {
			return nil, tools.RelayAttributeError("NemoRelay", "llm")
		}
		if llm.Execute == nil {
			return nil, tools.RelayAttributeError("NemoRelayLLM", "execute")
		}
		return llm.Execute(c, attempt.operation, attempt.relayRequest, attempt.invoke(callback, async), attempt.relayKwargs)
	}
	var managed any
	if !async && tools.RelayHasRunningEventLoop(ctx) {
		err = errors.New(relaySyncLoopError)
	} else {
		managed, err = attempt.runManaged(tools.RelayWithRunningLoop(ctx), call)
	}
	if err != nil {
		return attempt.resolveFailure(ctx, err, o.DeferLogicalCompletion)
	}
	return attempt.result(ctx, managed, o.DeferLogicalCompletion)
}

// Runs one attempt through Relay under the inherited icebath turn
func relayExecuteCurrent(ctx context.Context, request map[string]any, callback func(ctx context.Context, request map[string]any) (any, error), name, modelName string, metadata map[string]any, deferLogicalCompletion bool) (any, error) {
	return RelayExecute(ctx, request, callback, RelayExecuteOptions{Name: name, ModelName: modelName, Metadata: metadata, DeferLogicalCompletion: deferLogicalCompletion})
}

// Async attempt through Relay under the inherited icebath turn
func relayExecuteCurrentAsync(ctx context.Context, request map[string]any, callback func(ctx context.Context, request map[string]any) (any, error), name, modelName string, metadata map[string]any, deferLogicalCompletion bool) (any, error) {
	return RelayExecuteAsync(ctx, request, callback, RelayExecuteOptions{Name: name, ModelName: modelName, Metadata: metadata, DeferLogicalCompletion: deferLogicalCompletion})
}

// Options of a provider stream run under the inherited turn
type RelayStreamOptions struct {
	Name                       string
	ModelName                  string
	Finalizer                  func(ctx context.Context) (any, error)
	Metadata                   map[string]any
	DeferLogicalCompletion     bool
	CompletedResponsePredicate func(value any) bool
}

// Streams under the inherited turn, returning a completed response a stream ignoring factory produced as is
func RelayStreamCurrent(ctx context.Context, request map[string]any, streamFactory func(ctx context.Context, request map[string]any) (any, error), o RelayStreamOptions) (any, error) {
	sessionID, ok := relayCurrentSessionID(ctx)
	if !ok || tools.RelayHasRunningEventLoop(ctx) {
		return streamFactory(ctx, request)
	}
	managed, err := relayStreamFn(ctx, request, streamFactory, ManagedLlmStreamOptions{
		SessionID: sessionID, Name: o.Name, ModelName: o.ModelName, Finalizer: o.Finalizer, Metadata: o.Metadata,
		DeferLogicalCompletion: o.DeferLogicalCompletion, CompletedResponsePredicate: o.CompletedResponsePredicate,
	})
	if err != nil {
		return nil, err
	}
	if o.CompletedResponsePredicate != nil {
		if err := managed.primeCompletedResponse(ctx); err != nil {
			return nil, err
		}
	}
	if final := managed.finalResponse(); final != nil {
		if err := managed.finishLogical(ctx, "success", nil); err != nil {
			return nil, err
		}
		if err := managed.closeStream(ctx, "cancelled", nil); err != nil {
			return nil, err
		}
		return final, nil
	}
	return managed, nil
}

// Stream surface the inherited turn entry drives while deciding the response shape
type relayStreamHandle interface {
	primeCompletedResponse(ctx context.Context) error
	finishLogical(ctx context.Context, outcome string, err error) error
	closeStream(ctx context.Context, logicalOutcome string, cause error) error
	finalResponse() any
}

var relayStreamFn = func(ctx context.Context, request map[string]any, streamFactory func(ctx context.Context, request map[string]any) (any, error), o ManagedLlmStreamOptions) (relayStreamHandle, error) {
	s, err := NewManagedLlmStream(ctx, request, streamFactory, o)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ManagedLlmStream) finalResponse() any { return s.FinalResponse }

// Shim facing stream entry the auxiliary client calls with a plain finalizer
func relayStreamCurrent(ctx context.Context, request map[string]any, streamFactory func(ctx context.Context, request map[string]any) (any, error), name, modelName string, finalizer func() any, metadata map[string]any, deferLogicalCompletion bool, completedResponsePredicate func(value any) bool) (any, error) {
	return RelayStreamCurrent(ctx, request, streamFactory, RelayStreamOptions{
		Name: name, ModelName: modelName, Metadata: metadata, DeferLogicalCompletion: deferLogicalCompletion,
		CompletedResponsePredicate: completedResponsePredicate,
		Finalizer:                  func(context.Context) (any, error) { return finalizer(), nil },
	})
}

// Awaits the Relay stream's aclose bounded by the timeout, false when the attempt was abandoned
func relayAcloseOnLoop(ctx context.Context, stream any) (bool, error) {
	it, ok := stream.(*tools.RelayAsyncIterator)
	if !ok || it == nil || it.Aclose == nil {
		return true, nil
	}
	timeout := relayAcloseTimeout
	loopCtx := tools.RelayWithRunningLoop(ctx)
	_, err := tools.RelayRunOnDaemonThread(func() (any, error) { return nil, it.Aclose(loopCtx) },
		"relay-llm-stream-aclose", &timeout, "Relay stream aclose did not finish; abandoning the close attempt")
	var te *tools.RelayTimeoutError
	if errors.As(err, &te) {
		slog.WarnContext(ctx, fmt.Sprintf("Relay stream aclose exceeded %ss; abandoning the close attempt and its private loop", pyrepr.Repr(relayAcloseTimeout)))
		return false, nil
	}
	return true, err
}

// Provider chunk iterator, self is the raw stream when it iterates itself
type relayChunkIter struct {
	next func(ctx context.Context) (any, bool, error)
	self any
}

type relayAnyChunkSource interface {
	Next(ctx context.Context) (any, error)
}

// Python iter of a provider stream value, TypeError when it is not iterable
func relayIter(raw any) (*relayChunkIter, error) {
	switch v := raw.(type) {
	case *relayChunkIter:
		return v, nil
	case AuxChatStream:
		return &relayChunkIter{self: raw, next: func(ctx context.Context) (any, bool, error) {
			m, err := v.Next(ctx)
			if errors.Is(err, io.EOF) {
				return nil, true, nil
			}
			return m, false, err
		}}, nil
	case relayAnyChunkSource:
		return &relayChunkIter{self: raw, next: func(ctx context.Context) (any, bool, error) {
			m, err := v.Next(ctx)
			if errors.Is(err, io.EOF) {
				return nil, true, nil
			}
			return m, false, err
		}}, nil
	}
	rv := reflect.ValueOf(raw)
	if raw != nil && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		i := 0
		return &relayChunkIter{next: func(context.Context) (any, bool, error) {
			if i >= rv.Len() {
				return nil, true, nil
			}
			i++
			return rv.Index(i - 1).Interface(), false, nil
		}}, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
}

func relaySliceIter(items []any) *relayChunkIter {
	it, _ := relayIter(items)
	return it
}

type relayRawChunk struct {
	encoded any
	raw     any
}

// Options of one Relay managed provider stream
type ManagedLlmStreamOptions struct {
	SessionID                  string
	Name                       string
	ModelName                  string
	Finalizer                  func(ctx context.Context) (any, error)
	OnStreamCreated            func(ctx context.Context, raw any) error
	OnChunk                    func(ctx context.Context, chunk any) error
	ChunkAdapter               func(chunk any) any
	AcceptChunk                func(chunk any) bool
	CompletedResponsePredicate func(value any) bool
	Metadata                   map[string]any
	DeferLogicalCompletion     bool
}

// Synchronous view of one Relay managed provider stream, yielding io.EOF when exhausted
type ManagedLlmStream struct {
	FinalResponse            any
	OutputModified           bool
	ctx                      context.Context
	closed                   bool
	providerCompleted        bool
	deliveredUnmatched       bool
	loop                     bool
	stream                   any
	rawStreamResource        any
	runtimeLease             *tools.RelayOperationLease
	closeErr                 error
	callbackErr              error
	logical                  *relayLogicalCall
	logicalResponseModelName *string
	logicalModelName         *string
	logicalProviderName      *string
	deferLogical             bool
	factory                  func(ctx context.Context, request map[string]any) (any, error)
	o                        ManagedLlmStreamOptions
	acceptChunk              func(chunk any) bool
	rawChunks                []relayRawChunk
	prefetched               []any
}

// Opens a provider stream managed by Relay when the session runs managed execution
func NewManagedLlmStream(ctx context.Context, request map[string]any, streamFactory func(ctx context.Context, request map[string]any) (any, error), o ManagedLlmStreamOptions) (*ManagedLlmStream, error) {
	s := &ManagedLlmStream{ctx: ctx, deferLogical: o.DeferLogicalCompletion, factory: streamFactory, o: o, acceptChunk: o.AcceptChunk}
	if strings.HasPrefix(relayTruthyStr(o.Metadata["call_role"]), "auxiliary:") {
		model, name := o.ModelName, o.Name
		s.logicalModelName, s.logicalProviderName = &model, &name
	}
	if s.o.ChunkAdapter == nil {
		s.o.ChunkAdapter = relayNamespace
	}
	sessionID := o.SessionID
	attempt, err := resolveRelayManagedAttempt(ctx, &sessionID, request, o.Metadata, o.Name, o.ModelName)
	if err == nil {
		if attempt == nil {
			err = s.startUnmanaged(ctx, request)
		} else {
			s.logical = attempt.logical
			err = s.startManaged(ctx, attempt)
		}
	}
	if err != nil {
		_ = s.closeStream(ctx, "cancelled", nil)
		return nil, err
	}
	runtime.SetFinalizer(s, func(s *ManagedLlmStream) { _ = s.closeStream(s.ctx, "cancelled", nil) })
	return s, nil
}

func (s *ManagedLlmStream) startUnmanaged(ctx context.Context, request map[string]any) error {
	raw, err := s.factory(ctx, request)
	if err != nil {
		return err
	}
	if p := s.o.CompletedResponsePredicate; p != nil && p(raw) {
		s.FinalResponse, s.stream = raw, relaySliceIter(nil)
		return nil
	}
	s.rawStreamResource = raw
	if s.o.OnStreamCreated != nil {
		if err := s.o.OnStreamCreated(ctx, raw); err != nil {
			return err
		}
	}
	it, err := relayIter(raw)
	if err != nil {
		return err
	}
	s.stream = it
	return nil
}

const (
	relayGenFresh = iota
	relayGenSuspended
	relayGenDone
)

type relayGeneratorExit struct{}

func (relayGeneratorExit) Error() string { return "GeneratorExit" }

func (relayGeneratorExit) auxBaseException() {}

// Relay's provider callback state, an async generator yielding JSON encoded chunks
type relayProviderGen struct {
	s       *ManagedLlmStream
	a       *relayManagedAttempt
	request any
	state   int
	raw     any
	hasRaw  bool
	it      *relayChunkIter
}

func (s *ManagedLlmStream) providerStream(a *relayManagedAttempt) tools.RelayProviderStreamCallback {
	return func(_ context.Context, request any) (*tools.RelayAsyncIterator, error) {
		g := &relayProviderGen{s: s, a: a, request: request}
		return &tools.RelayAsyncIterator{Next: g.anext, Aclose: g.aclose}, nil
	}
}

func (g *relayProviderGen) anext(ctx context.Context) (any, error) {
	if g.state == relayGenDone {
		return nil, io.EOF
	}
	if g.state == relayGenFresh {
		g.state = relayGenSuspended
		done, err := g.open(ctx)
		if err != nil {
			return nil, g.fail(ctx, err)
		}
		if done {
			return nil, g.finish(ctx)
		}
	}
	chunk, exhausted, err := g.pull(ctx)
	if err != nil {
		return nil, g.fail(ctx, err)
	}
	if exhausted {
		g.s.providerCompleted = true
		return nil, g.finish(ctx)
	}
	return chunk, nil
}

// Runs the factory, returning true when it produced a completed response instead of a stream
func (g *relayProviderGen) open(ctx context.Context) (bool, error) {
	s, a := g.s, g.a
	final := a.providerRequest(g.request)
	raw, err := a.runCallback(ctx, func(c context.Context) (any, error) { return s.factory(c, final) })
	if err != nil {
		return false, err
	}
	g.raw, g.hasRaw = raw, true
	if p := s.o.CompletedResponsePredicate; p != nil {
		hit, _ := a.runCallback(ctx, func(context.Context) (any, error) { return p(raw), nil })
		if hit == true {
			s.FinalResponse, s.providerCompleted = raw, true
			return true, nil
		}
	}
	if s.o.OnStreamCreated != nil {
		if _, err := a.runCallback(ctx, func(c context.Context) (any, error) { return nil, s.o.OnStreamCreated(c, raw) }); err != nil {
			return false, err
		}
	}
	it, err := a.runCallback(ctx, func(context.Context) (any, error) { return relayIter(raw) })
	if err != nil {
		return false, err
	}
	g.it = it.(*relayChunkIter)
	return false, nil
}

// Reads one provider chunk off the loop so a blocking read never withholds the previous chunk
func (g *relayProviderGen) pull(ctx context.Context) (any, bool, error) {
	s, a := g.s, g.a
	chunk, exhausted, err := relayNextProviderChunk(ctx, a, g.it)
	if err != nil || exhausted {
		return nil, exhausted, err
	}
	if s.acceptChunk != nil {
		ok, _ := a.runCallback(ctx, func(context.Context) (any, error) { return s.acceptChunk(chunk), nil })
		if ok != true {
			s.providerCompleted = true
			return nil, true, nil
		}
	}
	encoded := tools.RelayJSONable(chunk)
	s.rawChunks = append(s.rawChunks, relayRawChunk{encoded: encoded, raw: chunk})
	return encoded, false, nil
}

func relayNextProviderChunk(ctx context.Context, a *relayManagedAttempt, it *relayChunkIter) (any, bool, error) {
	type result struct {
		v    any
		done bool
		err  error
	}
	ch := make(chan result, 1)
	threadCtx := tools.RelayWithoutRunningLoop(ctx)
	go func() {
		var r result
		r.v, r.err = a.runCallback(threadCtx, func(c context.Context) (any, error) {
			v, done, err := it.next(c)
			if done {
				return nil, io.EOF
			}
			return v, err
		})
		if errors.Is(r.err, io.EOF) {
			r = result{done: true}
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		return r.v, r.done, r.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (g *relayProviderGen) closeRaw(ctx context.Context) error {
	closer, ok := g.raw.(io.Closer)
	if !g.hasRaw || !ok {
		return nil
	}
	_, err := g.a.runCallback(ctx, func(context.Context) (any, error) { return nil, closer.Close() })
	return err
}

// Generator end after a return, closing the raw stream whose failure replaces StopAsyncIteration
func (g *relayProviderGen) finish(ctx context.Context) error {
	g.state = relayGenDone
	if err := g.closeRaw(ctx); err != nil {
		g.s.closeErr = err
		return err
	}
	return io.EOF
}

func (g *relayProviderGen) fail(ctx context.Context, err error) error {
	g.state = relayGenDone
	g.s.callbackErr = err
	if cerr := g.closeRaw(ctx); cerr != nil {
		g.s.closeErr = cerr
		return cerr
	}
	return err
}

// Throws GeneratorExit into a suspended generator, closing its raw stream
func (g *relayProviderGen) aclose(ctx context.Context) error {
	if g.state != relayGenSuspended {
		g.state = relayGenDone
		return nil
	}
	g.state = relayGenDone
	g.s.callbackErr = relayGeneratorExit{}
	if err := g.closeRaw(ctx); err != nil {
		g.s.closeErr = err
		return err
	}
	return nil
}

// Terminal response for Relay keeping the original error while a provider failure unwinds
func (s *ManagedLlmStream) relayFinalizer(a *relayManagedAttempt) func(ctx context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		if s.callbackErr != nil {
			return nil, nil
		}
		response := s.FinalResponse
		if response == nil {
			var err error
			response, err = a.runCallback(ctx, func(c context.Context) (any, error) {
				if s.o.Finalizer == nil {
					return nil, errors.New("'NoneType' object is not callable")
				}
				return s.o.Finalizer(c)
			})
			if err != nil {
				s.callbackErr = err
				return nil, err
			}
		}
		if s.logicalModelName != nil {
			s.logicalResponseModelName = relayResponseModelName(response)
		}
		return tools.RelayJSONable(response), nil
	}
}

// Opens Relay's stream on the private loop this iterator owns
func (s *ManagedLlmStream) startManaged(ctx context.Context, a *relayManagedAttempt) error {
	observe := func(ctx context.Context, chunk any) error {
		if s.o.OnChunk == nil {
			return nil
		}
		_, err := a.runCallback(ctx, func(c context.Context) (any, error) { return nil, s.o.OnChunk(c, tools.RelayJSONable(chunk)) })
		return err
	}
	lease, err := a.runtime.AcquireOperationLease()
	if err != nil {
		return err
	}
	s.runtimeLease, s.loop = lease, true
	stream, err := a.runManaged(tools.RelayWithRunningLoop(ctx), func(c context.Context) (any, error) {
		llm := a.runtime.Relay.LLM
		if llm == nil {
			return nil, tools.RelayAttributeError("NemoRelay", "llm")
		}
		if llm.StreamExecute == nil {
			return nil, tools.RelayAttributeError("NemoRelayLLM", "stream_execute")
		}
		return llm.StreamExecute(c, a.operation, a.relayRequest, s.providerStream(a), observe, s.relayFinalizer(a), a.relayKwargs)
	})
	if err == nil {
		s.stream = stream
		return nil
	}
	if s.loop && s.recoverableRelayFailure(ctx, err) {
		return s.preservePendingProviderChunks(ctx)
	}
	defer func() {
		s.loop = false
		s.releaseRuntimeLease()
	}()
	if s.loop {
		outcome := "failed"
		if relayIsCancellation(err) {
			outcome = "cancelled"
		}
		if ferr := s.finishLogical(ctx, outcome, err); ferr != nil {
			return ferr
		}
	}
	return err
}

// Advances once while preserving a genuine first chunk
func (s *ManagedLlmStream) primeCompletedResponse(ctx context.Context) error {
	if s.closed || len(s.prefetched) > 0 {
		return nil
	}
	chunk, err := s.Next(ctx)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	s.prefetched = append(s.prefetched, chunk)
	return nil
}

// Whether Relay post processing failed after the provider already succeeded and no transformed output went out
func (s *ManagedLlmStream) recoverableRelayFailure(ctx context.Context, err error) bool {
	recoverable := !auxIsBaseException(err) && s.providerCompleted && s.callbackErr == nil
	if recoverable && s.deliveredUnmatched {
		slog.WarnContext(ctx, "NeMo Relay stream post-processing failed after transformed output; propagating rather than replaying provider chunks", "error", err)
		return false
	}
	if recoverable {
		slog.WarnContext(ctx, "NeMo Relay stream post-processing failed after provider success; preserving the provider result", "error", err)
	}
	return recoverable
}

// Completes the logical scope unless deferred, reporting an auxiliary stream's error class
func (s *ManagedLlmStream) finishLogical(ctx context.Context, outcome string, err error) error {
	if s.deferLogical {
		return nil
	}
	var errorClass *string
	if err != nil && s.logicalModelName != nil {
		if auxIsCancellation(err) {
			outcome = "cancelled"
		} else {
			provider := ""
			if s.logicalProviderName != nil {
				provider = *s.logicalProviderName
			}
			ec := auxErrorClass(err, provider, *s.logicalModelName)
			errorClass = &ec
		}
	}
	if ferr := relayCompleteLogical(ctx, s.logical, outcome, s.logicalModelName, s.logicalProviderName, s.logicalResponseModelName, s.runtimeLease, errorClass); ferr != nil {
		return ferr
	}
	s.logical = nil
	return nil
}

// Next provider or Relay chunk, io.EOF once the stream is exhausted or closed
func (s *ManagedLlmStream) Next(ctx context.Context) (any, error) {
	if s.closed {
		return nil, io.EOF
	}
	if n := len(s.prefetched); n > 0 {
		chunk := s.prefetched[n-1]
		s.prefetched = s.prefetched[:n-1]
		return chunk, nil
	}
	if !s.loop {
		it, _ := s.stream.(*relayChunkIter)
		var chunk any
		done := it == nil
		if it != nil {
			var err error
			if chunk, done, err = it.next(ctx); err != nil {
				return nil, err
			}
		}
		if done || (s.acceptChunk != nil && !s.acceptChunk(chunk)) {
			if err := s.closeStream(ctx, "cancelled", nil); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		return chunk, nil
	}
	var chunk any
	var err error
	if it, ok := s.stream.(*tools.RelayAsyncIterator); ok && it != nil && it.Next != nil {
		chunk, err = it.Next(tools.RelayWithRunningLoop(ctx))
	} else {
		err = fmt.Errorf("'%s' object is not an async iterator", pyrepr.TypeName(s.stream))
	}
	if errors.Is(err, io.EOF) {
		if len(s.rawChunks) > 0 {
			s.OutputModified = true
		}
		if ferr := s.finishLogical(ctx, "success", nil); ferr != nil {
			return nil, ferr
		}
		if cerr := s.closeStream(ctx, "cancelled", nil); cerr != nil {
			return nil, cerr
		}
		return nil, io.EOF
	}
	if err != nil {
		if cb := s.callbackErr; cb != nil && tools.IsRelayWrappedCallbackError(err, cb) {
			if cerr := s.closeStream(ctx, "failed", cb); cerr != nil {
				return nil, cerr
			}
			return nil, cb
		}
		if s.recoverableRelayFailure(ctx, err) {
			if perr := s.preservePendingProviderChunks(ctx); perr != nil {
				return nil, perr
			}
			return s.Next(ctx)
		}
		outcome := "failed"
		if relayIsCancellation(err) {
			outcome = "cancelled"
		}
		if cerr := s.closeStream(ctx, outcome, err); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	for i, rc := range s.rawChunks {
		if relayJSONEqual(chunk, rc.encoded) {
			if i > 0 {
				s.OutputModified = true
			}
			s.rawChunks = s.rawChunks[i+1:]
			return rc.raw, nil
		}
	}
	s.OutputModified, s.deliveredUnmatched = true, true
	return s.o.ChunkAdapter(chunk), nil
}

// Closes an explicitly abandoned stream, cancelling its logical call and surfacing the first close failure
func (s *ManagedLlmStream) Close() error {
	if err := s.closeStream(s.ctx, "cancelled", nil); err != nil {
		return err
	}
	closeErr := s.closeErr
	s.closeErr = nil
	return closeErr
}

// Switches a failed Relay stream to its undelivered provider chunks
func (s *ManagedLlmStream) preservePendingProviderChunks(ctx context.Context) error {
	pending := make([]any, len(s.rawChunks))
	for i, rc := range s.rawChunks {
		pending[i] = rc.raw
	}
	s.rawChunks = nil
	loop, relayStream := s.loop, s.stream
	s.loop, s.stream, s.rawStreamResource, s.acceptChunk = false, relaySliceIter(pending), nil, nil
	defer s.releaseRuntimeLease()
	if loop {
		if _, err := relayAcloseOnLoop(ctx, relayStream); err != nil {
			if auxIsBaseException(err) {
				return err
			}
			slog.DebugContext(ctx, "Relay stream cleanup failed during provider fallback", "error", err)
		}
	}
	return s.finishLogical(ctx, "success", nil)
}

func (s *ManagedLlmStream) keepFirstCloseError(err error) {
	if s.closeErr == nil {
		s.closeErr = err
	}
}

// Closes the unmanaged provider stream and resource once each
func (s *ManagedLlmStream) closeProviderResources(ctx context.Context) error {
	var resources []any
	add := func(r any) {
		if r == nil {
			return
		}
		for _, seen := range resources {
			if tools.RelayIdentical(seen, r) {
				return
			}
		}
		resources = append(resources, r)
	}
	if it, ok := s.stream.(*relayChunkIter); ok && it != nil {
		add(it.self)
	} else {
		add(s.stream)
	}
	add(s.rawStreamResource)
	s.stream, s.rawStreamResource = nil, nil
	for _, r := range resources {
		closer, ok := r.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			if auxIsBaseException(err) {
				return err
			}
			s.keepFirstCloseError(err)
			slog.DebugContext(ctx, "Provider stream cleanup failed", "error", err)
		}
	}
	return nil
}

func (s *ManagedLlmStream) closeStream(ctx context.Context, logicalOutcome string, cause error) error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.prefetched = nil
	defer s.releaseRuntimeLease()
	loop := s.loop
	s.loop = false
	if !loop {
		if err := s.closeProviderResources(ctx); err != nil {
			return err
		}
	} else if _, err := relayAcloseOnLoop(ctx, s.stream); err != nil {
		if auxIsBaseException(err) {
			return err
		}
		s.keepFirstCloseError(err)
	}
	return s.finishLogical(ctx, logicalOutcome, cause)
}

func (s *ManagedLlmStream) releaseRuntimeLease() {
	lease := s.runtimeLease
	s.runtimeLease = nil
	if lease != nil {
		lease.Release()
	}
}

var relayAnthropicAppendDeltas = map[string]string{"text_delta": "text", "thinking_delta": "thinking", "signature_delta": "signature"}

// Rebuilds an Anthropic Message from post intercept SSE events
type AnthropicStreamAccumulator struct {
	message map[string]any
	blocks  map[int]map[string]any
}

func NewAnthropicStreamAccumulator() *AnthropicStreamAccumulator {
	return &AnthropicStreamAccumulator{message: map[string]any{}, blocks: map[int]map[string]any{}}
}

// Python int value of a JSON index, decoded floats with integral value and bools included
func relayPyInt(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int(n), true
		}
		return 0, false
	case float32:
		return relayPyInt(float64(n))
	}
	if i, ok := pyrepr.Int(v); ok && i.IsInt64() {
		return int(i.Int64()), true
	}
	return 0, false
}

// Feeds one stream event into the message being rebuilt
func (a *AnthropicStreamAccumulator) Observe(event any) error {
	payload, ok := tools.RelayJSONable(event).(map[string]any)
	if !ok {
		return nil
	}
	switch payload["type"] {
	case "message_start":
		if message, ok := payload["message"].(map[string]any); ok {
			for _, k := range []string{"id", "type", "role", "model", "usage"} {
				if v, has := message[k]; has {
					a.message[k] = v
				}
			}
		}
	case "content_block_start":
		index, iok := relayPyInt(payload["index"])
		if block, ok := payload["content_block"].(map[string]any); ok && iok {
			a.blocks[index] = relayCopyMap(block)
		}
	case "content_block_delta":
		return a.onContentBlockDelta(payload)
	case "message_delta":
		if delta, ok := payload["delta"].(map[string]any); ok {
			for _, k := range []string{"stop_reason", "stop_sequence", "stop_details"} {
				if v, has := delta[k]; has {
					a.message[k] = v
				}
			}
		}
		if usage, has := payload["usage"]; has {
			current, cok := a.message["usage"].(map[string]any)
			if update, uok := usage.(map[string]any); cok && uok {
				merged := relayCopyMap(current)
				for k, v := range update {
					if v != nil {
						merged[k] = v
					}
				}
				usage = merged
			}
			a.message["usage"] = usage
		}
	}
	return nil
}

func (a *AnthropicStreamAccumulator) onContentBlockDelta(payload map[string]any) error {
	index, iok := relayPyInt(payload["index"])
	delta, dok := payload["delta"].(map[string]any)
	if !iok || !dok {
		return nil
	}
	block, ok := a.blocks[index]
	if !ok {
		block = map[string]any{}
		a.blocks[index] = block
	}
	deltaType, _ := delta["type"].(string)
	if field, ok := relayAnthropicAppendDeltas[deltaType]; ok {
		block[field] = relayTruthyStr(block[field]) + relayTruthyStr(delta[field])
		return nil
	}
	switch deltaType {
	case "input_json_delta":
		prior := ""
		if v, has := block["_partial_json"]; has {
			prior = pyrepr.Str(v)
			delete(block, "_partial_json")
		}
		block["_partial_json"] = prior + relayTruthyStr(delta["partial_json"])
	case "citations_delta":
		citation, has := delta["citation"]
		if !has {
			return nil
		}
		existing, present := block["citations"]
		if !present {
			block["citations"] = []any{citation}
			return nil
		}
		list, ok := existing.([]any)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'append'", pyrepr.TypeName(existing))
		}
		block["citations"] = append(list, citation)
	}
	return nil
}

// Message dict with content blocks in index order and partial tool JSON parsed into input
func (a *AnthropicStreamAccumulator) Finalize() map[string]any {
	indices := make([]int, 0, len(a.blocks))
	for i := range a.blocks {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	blocks := make([]any, 0, len(indices))
	for _, i := range indices {
		block := relayCopyMap(a.blocks[i])
		if partial, has := block["_partial_json"]; has {
			delete(block, "_partial_json")
			if text, ok := partial.(string); ok {
				if parsed, err := pyjson.Loads(text); err == nil {
					partial = pyjson.Plain(parsed)
				}
			}
			block["input"] = partial
		}
		blocks = append(blocks, block)
	}
	out := relayCopyMap(a.message)
	out["content"] = blocks
	return out
}

// Attribute shaped response merged over base
func (a *AnthropicStreamAccumulator) Response(base any) any {
	assembled := a.Finalize()
	content := assembled["content"].([]any)
	delete(assembled, "content")
	merged := relayJSONableDict(base)
	for k, v := range assembled {
		merged[k] = v
	}
	if _, has := merged["content"]; len(content) > 0 || !has {
		merged["content"] = content
	}
	return relayNamespace(merged)
}

func relayCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Turn, handle and request id of the turn's logical LLM scope, pushing it once
func relayLogicalParent(ctx context.Context, rt *tools.RelayRuntime, session *tools.RelaySession, parent any, metadata map[string]any) (*relayLogicalCall, error) {
	sid := session.SessionID
	turn := tools.ActiveRelayTurn(ctx, &sid)
	requestID := relayTruthyStr(metadata["api_request_id"])
	if turn == nil || requestID == "" {
		return nil, nil
	}
	if host, ok := turn.Lease.Host.(*tools.RelayRuntime); !ok || host != rt {
		return nil, nil
	}
	fl := turn.FinalizeLock()
	fl.Lock()
	defer fl.Unlock()
	if turn.Closed() {
		return nil, nil
	}
	turn.LogicalLLMMu.Lock()
	defer turn.LogicalLLMMu.Unlock()
	handle, _ := turn.LogicalLLMCalls.Get(requestID)
	if handle == nil {
		role := relayTruthyStr(metadata["call_role"])
		if role == "" {
			role = "primary"
		}
		if rt.Relay.Scope == nil || rt.Relay.Scope.Push == nil {
			return nil, tools.RelayAttributeError("NemoRelayScope", "push")
		}
		pushed, err := rt.RunInSession(ctx, session, func(c context.Context) (any, error) {
			return rt.Relay.Scope.Push(c, tools.RelayLogicalLLMScope, rt.Relay.ScopeType.Function, map[string]any{
				"handle": parent, "input": map[string]any{},
				"metadata": tools.RelayRuntimeMetadata(rt.RuntimeID, map[string]any{"icebath.call_role": role}),
			})
		}, false, nil)
		if err != nil {
			return nil, err
		}
		handle = pushed
		turn.LogicalLLMCalls.Set(requestID, handle)
	}
	return &relayLogicalCall{turn: turn, handle: handle, requestID: requestID}, nil
}

// Pops the logical scope through the top guard, deferring its output when a sibling scope sits above it
func relayCompleteLogical(ctx context.Context, logical *relayLogicalCall, outcome string, modelName, providerName, responseModelName *string, operationLease *tools.RelayOperationLease, errorClass *string) error {
	if logical == nil {
		return nil
	}
	turn := logical.turn
	lease := turn.Lease
	host, ok := lease.Host.(*tools.RelayRuntime)
	if !ok {
		return nil
	}
	output := map[string]any{"outcome": outcome}
	if modelName != nil && providerName != nil {
		output["model"], output["provider"] = *modelName, *providerName
		if responseModelName != nil {
			output["response_model"] = *responseModelName
		}
		if errorClass != nil {
			output["error_class"] = *errorClass
		}
	}
	fl := turn.FinalizeLock()
	fl.Lock()
	defer fl.Unlock()
	turn.LogicalLLMMu.Lock()
	current, _ := turn.LogicalLLMCalls.Get(logical.requestID)
	turn.LogicalLLMMu.Unlock()
	if !tools.RelayIdentical(current, logical.handle) || lease.Session == nil {
		return nil
	}
	pop := func(c context.Context) (any, error) {
		return tools.PopRelayScopeIfTop(c, host.Relay, logical.handle, output, tools.RelayRuntimeMetadata(host.RuntimeID, nil))
	}
	var popped any
	var err error
	if operationLease != nil {
		popped, err = operationLease.RunInSession(ctx, lease.Session, pop, false, nil)
	} else {
		popped, err = host.RunInSession(ctx, lease.Session, pop, false, nil)
	}
	if err != nil {
		if auxIsBaseException(err) {
			return err
		}
		slog.WarnContext(ctx, "Icebath Relay logical LLM finalization failed", "error", err)
		return nil
	}
	if popped == false {
		host.DeferScopeOutput(logical.handle, output)
		slog.DebugContext(ctx, fmt.Sprintf("Left logical LLM scope %s under a concurrent scope; the drain closes it with its result", logical.requestID))
	}
	turn.LogicalLLMMu.Lock()
	if current, _ := turn.LogicalLLMCalls.Get(logical.requestID); tools.RelayIdentical(current, logical.handle) {
		turn.LogicalLLMCalls.Delete(logical.requestID)
	}
	turn.LogicalLLMMu.Unlock()
	return nil
}

// Route report of a logical call, each nil field an absent keyword
type RelayLogicalCompletion struct {
	Outcome           string
	ModelName         *string
	ProviderName      *string
	ResponseModelName *string
	ErrorClass        *string
}

// Completes the active turn's logical LLM call after caller validation
func RelayCompleteLogicalCall(ctx context.Context, apiRequestID string, o RelayLogicalCompletion) error {
	turn := tools.ActiveRelayTurn(ctx, nil)
	if turn == nil || apiRequestID == "" {
		return nil
	}
	turn.LogicalLLMMu.Lock()
	handle, _ := turn.LogicalLLMCalls.Get(apiRequestID)
	turn.LogicalLLMMu.Unlock()
	if handle == nil {
		return nil
	}
	return relayCompleteLogical(ctx, &relayLogicalCall{turn: turn, handle: handle, requestID: apiRequestID}, o.Outcome,
		o.ModelName, o.ProviderName, o.ResponseModelName, nil, o.ErrorClass)
}

func relayOptionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Shim facing completion where an empty string is an absent keyword
func relayCompleteLogicalCall(ctx context.Context, apiRequestID, outcome, modelName, providerName, responseModelName, errorClass string) error {
	return RelayCompleteLogicalCall(ctx, apiRequestID, RelayLogicalCompletion{
		Outcome: outcome, ModelName: relayOptionalString(modelName), ProviderName: relayOptionalString(providerName),
		ResponseModelName: relayOptionalString(responseModelName), ErrorClass: relayOptionalString(errorClass),
	})
}

// Provider reported model name when it is a non blank string
func relayResponseModelName(response any) *string {
	m, ok := response.(map[string]any)
	if !ok {
		m, _ = tools.RelayJSONable(response).(map[string]any)
	}
	if v, ok := m["model"].(string); ok && strings.TrimSpace(v) != "" {
		return &v
	}
	return nil
}

func relayDictCopy(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return relayCopyMap(m), true
}

// Provider request overlaying only the fields Relay intercepts actually changed, with filtered headers
func relayProviderRequest(original map[string]any, request any, relayRequestBody, codecBaselineBody, metadata map[string]any) map[string]any {
	content := request
	var headers any
	if r, ok := request.(*tools.RelayLLMRequest); ok && r != nil {
		content, headers = r.Content, r.Headers
	}
	body, ok := content.(map[string]any)
	if !ok {
		body = relayRequestBody
	}
	final := relayCopyMap(original)
	if codecBaselineBody != nil && !relayJSONEqual(body, relayRequestBody) {
		baseline := codecBaselineBody
		intercepted := relayProviderRequestBody(body, metadata)
		keys := map[string]bool{}
		for k := range baseline {
			keys[k] = true
		}
		for k := range intercepted {
			keys[k] = true
		}
		for key := range keys {
			iv, inInter := intercepted[key]
			bv, inBase := baseline[key]
			if !inInter {
				delete(final, key)
			} else if !inBase || !relayJSONEqual(iv, bv) {
				final[key] = iv
			}
		}
		relayRestoreProviderMessageExtensions(original, final, baseline, intercepted)
	}
	hm, isDict := headers.(map[string]any)
	if !isDict {
		return final
	}
	filtered := map[string]any{}
	for k, v := range hm {
		if !relayInternalProviderHeaders[strings.ToLower(k)] {
			filtered[k] = v
		}
	}
	_, known := relayProtocolByAPIMode[relayAPIMode(metadata)]
	_, hasExtra := original["extra_headers"]
	if len(filtered) > 0 && !known && !hasExtra {
		for k := range filtered {
			if strings.ToLower(k) == "traceparent" {
				delete(filtered, k)
			}
		}
	}
	if len(filtered) > 0 {
		merged := map[string]any{}
		switch existing := final["extra_headers"].(type) {
		case map[string]any:
			for k, v := range existing {
				merged[k] = v
			}
		case map[string]string:
			for k, v := range existing {
				merged[k] = v
			}
		}
		for k, v := range filtered {
			merged[k] = v
		}
		final["extra_headers"] = merged
	}
	return final
}

// Rewrites each dict tool that matches in the body's tools list
func relayRewriteTools(body map[string]any, match func(map[string]any) bool, rewrite func(map[string]any) map[string]any) {
	var list []any
	switch t := body["tools"].(type) {
	case []any:
		list = t
	case []map[string]any:
		list = make([]any, len(t))
		for i, m := range t {
			list[i] = m
		}
	default:
		return
	}
	out := make([]any, len(list))
	for i, t := range list {
		if m, ok := t.(map[string]any); ok && match(m) {
			out[i] = rewrite(m)
		} else {
			out[i] = t
		}
	}
	body["tools"] = out
}

func relayCodexCodecTools(body map[string]any) {
	if body["tools"] == nil {
		delete(body, "tools")
	}
	relayRewriteTools(body, func(t map[string]any) bool {
		_, hasFn := t["function"]
		return t["type"] == "function" && !hasFn
	}, func(t map[string]any) map[string]any {
		fn := map[string]any{}
		for k, v := range t {
			if k != "type" {
				fn[k] = v
			}
		}
		return map[string]any{"type": "function", "function": fn}
	})
}

func relayChatCodecTools(body map[string]any) {
	relayRewriteTools(body, func(t map[string]any) bool {
		_, hasFn := t["function"]
		_, hasType := t["type"]
		return hasFn && !hasType
	}, func(t map[string]any) map[string]any {
		out := map[string]any{"type": "function"}
		for k, v := range t {
			out[k] = v
		}
		return out
	})
}

var relayCodecToolNormalizers = map[string]func(map[string]any){"codex_responses": relayCodexCodecTools, "chat_completions": relayChatCodecTools}

// Codec facing JSON body without the client only timeout
func relayRequestBody(request, metadata map[string]any) map[string]any {
	body := relayJSONableDict(request)
	delete(body, "timeout")
	if normalize, ok := relayCodecToolNormalizers[relayAPIMode(metadata)]; ok {
		normalize(body)
	}
	return body
}

func relayAnyList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// Restores provider wire fields the typed codec cannot represent
func relayRestoreProviderMessageExtensions(original, final, baseline, intercepted map[string]any) {
	var lists [4][]any
	for i, body := range []map[string]any{original, final, baseline, intercepted} {
		list, ok := relayAnyList(body["messages"])
		if !ok {
			return
		}
		lists[i] = list
	}
	n := len(lists[0])
	for _, l := range lists[1:] {
		if len(l) != n {
			return
		}
	}
	for i := 0; i < n; i++ {
		var msgs [4]map[string]any
		allDicts := true
		for j := range lists {
			m, ok := lists[j][i].(map[string]any)
			if !ok {
				allDicts = false
				break
			}
			msgs[j] = m
		}
		if !allDicts {
			continue
		}
		for _, key := range relayProviderMessageExtensionKeys {
			v, inOriginal := msgs[0][key]
			if !inOriginal {
				continue
			}
			_, inBase := msgs[2][key]
			_, inInter := msgs[3][key]
			_, inFinal := msgs[1][key]
			if !inBase && !inInter && !inFinal {
				msgs[1][key] = v
			}
		}
	}
}

// Codec only request shape used to identify real rewrites, nil when the baseline fails
func relayCodecRoundTripRequestBody(ctx context.Context, relay *tools.NemoRelay, relayRequest any, relayRequestBody, metadata map[string]any) (map[string]any, error) {
	codec, err := relayCodecFn(relay, metadata)
	if err != nil {
		return nil, err
	}
	if codec == nil {
		return relayProviderRequestBody(relayRequestBody, metadata), nil
	}
	encoded, err := func() (any, error) {
		if codec.Decode == nil {
			return nil, tools.RelayAttributeError("NemoRelayCodec", "decode")
		}
		decoded, err := codec.Decode(ctx, relayRequest)
		if err != nil {
			return nil, err
		}
		if codec.Encode == nil {
			return nil, tools.RelayAttributeError("NemoRelayCodec", "encode")
		}
		return codec.Encode(ctx, decoded, relayRequest)
	}()
	if err != nil {
		if auxIsBaseException(err) {
			return nil, err
		}
		slog.WarnContext(ctx, "NeMo Relay request codec baseline failed; ignoring request rewrites", "error", err)
		return nil, nil
	}
	content := encoded
	if r, ok := encoded.(*tools.RelayLLMRequest); ok && r != nil {
		content = r.Content
	}
	if m, ok := content.(map[string]any); ok {
		return relayProviderRequestBody(m, metadata), nil
	}
	slog.WarnContext(ctx, "NeMo Relay request codec returned an unsupported baseline; ignoring request rewrites")
	return nil, nil
}

// Provider shaped body, flattening codex function tools back to the Responses wire form
func relayProviderRequestBody(content, metadata map[string]any) map[string]any {
	body := relayCopyMap(content)
	if relayAPIMode(metadata) == "codex_responses" {
		relayRewriteTools(body, func(t map[string]any) bool {
			_, ok := t["function"].(map[string]any)
			return t["type"] == "function" && ok
		}, func(t map[string]any) map[string]any {
			out := map[string]any{"type": "function"}
			fn, _ := relayDictCopy(t["function"])
			for k, v := range fn {
				out[k] = v
			}
			return out
		})
	}
	return body
}

// Codec instance of the API mode's protocol, nil when the binding lacks it
func relayCodec(relay *tools.NemoRelay, metadata map[string]any) (*tools.NemoRelayCodec, error) {
	p, ok := relayProtocolByAPIMode[relayAPIMode(metadata)]
	if !ok || relay == nil || relay.Codecs == nil {
		return nil, nil
	}
	var ctor func() (*tools.NemoRelayCodec, error)
	switch p[1] {
	case "OpenAIChatCodec":
		ctor = relay.Codecs.OpenAIChatCodec
	case "OpenAIResponsesCodec":
		ctor = relay.Codecs.OpenAIResponsesCodec
	case "AnthropicMessagesCodec":
		ctor = relay.Codecs.AnthropicMessagesCodec
	}
	if ctor == nil {
		return nil, nil
	}
	return ctor()
}

// JSON object form of a value, empty for anything that is not an object
func relayJSONableDict(value any) map[string]any {
	if m, ok := tools.RelayJSONable(value).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Attribute shaped copy of a JSON value
func relayNamespace(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = relayNamespace(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = relayNamespace(item)
		}
		return out
	}
	return value
}

// Equality of the canonical JSON forms, false when either cannot encode
func relayJSONEqual(left, right any) bool {
	l, err := tools.RelayCanonicalJSON(left, nil)
	if err != nil {
		return false
	}
	r, err := tools.RelayCanonicalJSON(right, nil)
	return err == nil && l == r
}

// Whether the error is a task cancel, an interrupted call or a keyboard interrupt
func relayIsCancellation(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrSecretPromptInterrupted) {
		return true
	}
	var ie auxInterruptedError
	return errors.As(err, &ie)
}
