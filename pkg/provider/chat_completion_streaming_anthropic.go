package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Opened Anthropic SDK MessageStream yielding events, its HTTP response, socket and message snapshot
type anthMessageStream interface {
	Next(ctx context.Context) (any, error)
	Close() error
	HTTPResponse() *http.Response
	AttemptConn() net.Conn
	Snapshot() map[string]any
}

// Client with the Anthropic SDK messages.stream entry point
type anthMessagesStreamer interface {
	MessagesStream(ctx context.Context, kwargs map[string]any) (anthMessageStream, error)
}

// SDK MessageStream over one streamed Messages response, accumulating the snapshot and firing the derived events
type anthSDKMessageStream struct {
	mu           sync.Mutex
	resp         *http.Response
	conn         net.Conn
	sse          sseFrameSource
	snap         map[string]any
	jsonBufs     map[int]string
	pending      []any
	outputTokens any
	done         bool
}

// Opens messages.stream with usage null normalization, a request that never got a response raised as APIConnectionError
func (c *anthropicSDKClient) MessagesStream(ctx context.Context, kwargs map[string]any) (anthMessageStream, error) {
	s := &anthSDKMessageStream{jsonBufs: map[int]string{}, outputTokens: 0}
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		s.mu.Lock()
		s.conn = info.Conn
		s.mu.Unlock()
	}})
	resp, err := c.post(traced, kwargs, true)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
			return nil, err
		}
		return nil, &AnthropicConnectionError{Err: err}
	}
	s.resp, s.sse = resp, c.inner.sseFrames(resp)
	return s, nil
}

func (s *anthSDKMessageStream) HTTPResponse() *http.Response { return s.resp }

// Socket of the stream while its body is still open
func (s *anthSDKMessageStream) AttemptConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	return s.conn
}

func (s *anthSDKMessageStream) Close() error {
	s.mu.Lock()
	already := s.done
	s.done = true
	s.mu.Unlock()
	if already {
		return nil
	}
	return s.resp.Body.Close()
}

// Message the stream accumulated so far, nil before message_start
func (s *anthSDKMessageStream) Snapshot() map[string]any { return s.snap }

// Next SDK event, io.EOF at the end of the body, an error event raised as a status error
func (s *anthSDKMessageStream) Next(ctx context.Context) (any, error) {
	for len(s.pending) == 0 {
		f, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = s.Close()
				return nil, io.EOF
			}
			return nil, sdkStreamReadError(err, s.resp)
		}
		switch f.Event {
		case "completion", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop":
		case "error":
			return nil, s.errorEvent(f.Data)
		default:
			continue
		}
		decoded, err := jsonutil.Decode(f.Data)
		if err != nil {
			return nil, &AuxTransportError{Class: "JSONDecodeError", Message: err.Error(), Err: err, Doc: f.Data}
		}
		event, ok := jsonutil.Plain(decoded).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("an Anthropic stream event is still %s after decoding it twice, expected a mapping", errchain.Kind(decoded))
		}
		if _, has := event["type"]; !has && f.Event != "completion" {
			event["type"] = f.Event
		}
		s.normalizeUsage(event)
		if err := s.accumulate(event); err != nil {
			return nil, err
		}
		s.pending = s.buildEvents(event)
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

// Status error of an SSE error event carrying the parsed body or the raw text
func (s *anthSDKMessageStream) errorEvent(data string) error {
	var body any = data
	message := data
	if parsed, err := jsonutil.Decode(data); err == nil {
		body, message = jsonutil.Plain(parsed), anyutil.String(parsed)
		if parsed == nil {
			message = "null"
		}
	} else if data == "" {
		message = fmt.Sprintf("Error code: %d", s.resp.StatusCode)
	}
	return &AuxSDKError{Class: auxSDKStatusClass(s.resp.StatusCode), StatusCode: s.resp.StatusCode, Message: message, Body: body, Headers: s.resp.Header.Clone()}
}

// Transport failure reading the body as the httpx error class and text the reader raises, carrying the request
func sdkStreamReadError(err error, resp *http.Response) error {
	out := &AuxTransportError{Class: "RemoteProtocolError", Message: err.Error(), Err: err}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		out.Class, out.Message = "ReadTimeout", "timed out"
	} else if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		text := errchain.OSText(err)
		out.Class, out.Message = "ReadError", text
		out.Err = &AuxTransportError{Class: "ReadError", Message: text, Err: newOSConnectionError(err, text)}
	}
	out.Request = failedRequestOfResponse(resp)
	return out
}

// OS level socket error at the root of the httpx and httpcore ReadError chain, the dropped socket's errno class
type osConnectionError struct {
	class, text string
	errno       syscall.Errno
}

func newOSConnectionError(err error, text string) *osConnectionError {
	out := &osConnectionError{class: "ConnectionError", text: text}
	for errno, class := range osConnectionErrorClasses {
		if errors.Is(err, errno) {
			out.class, out.errno = class, errno
		}
	}
	return out
}

// Kind label of the socket error behind each dropped socket errno
var osConnectionErrorClasses = map[syscall.Errno]string{
	syscall.ECONNRESET: "ConnectionResetError", syscall.EPIPE: "BrokenPipeError", syscall.ECONNABORTED: "ConnectionAbortedError",
}

func (e *osConnectionError) Error() string        { return e.text }
func (e *osConnectionError) ErrorKind() string    { return e.class }
func (e *osConnectionError) Is(target error) bool { return e.errno != 0 && target == e.errno }
func (e *osConnectionError) Timeout() bool        { return false }
func (e *osConnectionError) Temporary() bool      { return false }

// Buffered request behind a response, nil without one
func failedRequestOfResponse(resp *http.Response) *FailedStreamRequest {
	if resp == nil || resp.Request == nil {
		return nil
	}
	return failedStreamRequestOf(resp.Request)
}

// Fills a null usage on message_start and message_delta before the snapshot accumulates it
func (s *anthSDKMessageStream) normalizeUsage(event map[string]any) {
	switch event["type"] {
	case "message_start":
		message, ok := event["message"].(map[string]any)
		if !ok {
			return
		}
		if usage := message["usage"]; usage == nil {
			message["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		} else if u, ok := usage.(map[string]any); ok {
			s.outputTokens = u["output_tokens"]
			if !anyutil.IsTrue(s.outputTokens) {
				s.outputTokens = 0
			}
		}
	case "message_delta":
		if event["usage"] == nil {
			event["usage"] = map[string]any{"output_tokens": s.outputTokens}
		}
	}
}

func (s *anthSDKMessageStream) contentAt(event map[string]any) map[string]any {
	index, ok := anyInt(event["index"])
	content, _ := s.snap["content"].([]any)
	if !ok || index < 0 || index >= len(content) {
		return nil
	}
	block, _ := content[index].(map[string]any)
	return block
}

// Folds one raw event into the message snapshot like the SDK accumulate_event
func (s *anthSDKMessageStream) accumulate(event map[string]any) error {
	typ := asStr(event["type"])
	if s.snap == nil {
		if typ != "message_start" {
			return &StreamEventOrderError{Type: typ}
		}
		message, _ := event["message"].(map[string]any)
		s.snap = relayCopyMap(message)
		content, _ := s.snap["content"].([]any)
		s.snap["content"] = append([]any{}, content...)
		return nil
	}
	switch typ {
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		content, _ := s.snap["content"].([]any)
		s.snap["content"] = append(content, relayCopyMap(block))
	case "content_block_delta":
		block := s.contentAt(event)
		delta, _ := event["delta"].(map[string]any)
		if block == nil || delta == nil {
			return nil
		}
		blockType := asStr(block["type"])
		switch asStr(delta["type"]) {
		case "text_delta":
			if blockType == "text" {
				block["text"] = asStr(block["text"]) + asStr(delta["text"])
			}
		case "input_json_delta":
			if blockType == "tool_use" || blockType == "server_tool_use" {
				index, _ := anyInt(event["index"])
				buf := s.jsonBufs[index] + asStr(delta["partial_json"])
				if buf != "" {
					input, err := jsonutil.DecodePartial(buf)
					if err != nil {
						return err
					}
					block["input"] = jsonutil.Plain(input)
				}
				s.jsonBufs[index] = buf
			}
		case "citations_delta":
			if blockType == "text" {
				citations, _ := block["citations"].([]any)
				block["citations"] = append(citations, delta["citation"])
			}
		case "thinking_delta":
			if blockType == "thinking" {
				block["thinking"] = asStr(block["thinking"]) + asStr(delta["thinking"])
			}
		case "signature_delta":
			if blockType == "thinking" {
				block["signature"] = delta["signature"]
			}
		}
	case "message_delta":
		delta, _ := event["delta"].(map[string]any)
		s.snap["stop_reason"], s.snap["stop_sequence"] = delta["stop_reason"], delta["stop_sequence"]
		usage, _ := s.snap["usage"].(map[string]any)
		usage = relayCopyMap(usage)
		incoming, _ := event["usage"].(map[string]any)
		usage["output_tokens"] = incoming["output_tokens"]
		for _, k := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "server_tool_use"} {
			if v := incoming[k]; v != nil {
				usage[k] = v
			}
		}
		s.snap["usage"] = usage
	}
	return nil
}

// Raw event followed by the SDK derived text, thinking, input_json, citation and signature events
func (s *anthSDKMessageStream) buildEvents(event map[string]any) []any {
	switch asStr(event["type"]) {
	case "message_start", "message_delta", "content_block_start":
		return []any{event}
	case "message_stop":
		return []any{map[string]any{"type": "message_stop", "message": s.snap}}
	case "content_block_stop":
		return []any{map[string]any{"type": "content_block_stop", "index": event["index"], "content_block": s.contentAt(event)}}
	case "content_block_delta":
		out := []any{event}
		block := s.contentAt(event)
		delta, _ := event["delta"].(map[string]any)
		if block == nil || delta == nil {
			return out
		}
		blockType := asStr(block["type"])
		switch asStr(delta["type"]) {
		case "text_delta":
			if blockType == "text" {
				out = append(out, map[string]any{"type": "text", "text": delta["text"], "snapshot": block["text"]})
			}
		case "input_json_delta":
			if blockType == "tool_use" || blockType == "server_tool_use" {
				out = append(out, map[string]any{"type": "input_json", "partial_json": delta["partial_json"], "snapshot": block["input"]})
			}
		case "citations_delta":
			if blockType == "text" {
				citations, _ := block["citations"].([]any)
				out = append(out, map[string]any{"type": "citation", "citation": delta["citation"], "snapshot": citations})
			}
		case "thinking_delta":
			if blockType == "thinking" {
				out = append(out, map[string]any{"type": "thinking", "thinking": delta["thinking"], "snapshot": block["thinking"]})
			}
		case "signature_delta":
			if blockType == "thinking" {
				out = append(out, map[string]any{"type": "signature", "signature": block["signature"]})
			}
		}
		return out
	}
	return nil
}

// Streams one Messages attempt on the request local client, firing deltas and returning the final message
func streamingCallAnthropicWire(c *streamingCall, client any) (any, error) {
	hasToolUse, sawEvent, sawStop := false, false, false
	c.touchChunkClock()
	diag := c.newDiag()
	c.mu.Lock()
	c.writerToken, c.attemptResponse, c.attemptConn, c.attemptClient = 0, nil, nil, client
	c.mu.Unlock()
	var opened anthMessageStream
	var baseFinal map[string]any
	textWatch, thinkingWatch := NewRunawayStreamWatch(), NewRunawayStreamWatch()
	runaway := ""
	accumulator := NewAnthropicStreamAccumulator()
	factory := func(ctx context.Context, next map[string]any) (any, error) {
		finalKwargs := relayCopyMap(next)
		anthSanitizeKwargs(finalKwargs, c.agent.LogPrefix())
		streamer, ok := client.(anthMessagesStreamer)
		if !ok {
			return nil, fmt.Errorf("the Anthropic client (%T) has no messages API", client)
		}
		raw, err := streamer.MessagesStream(c.ctx, finalKwargs)
		if err != nil {
			return nil, err
		}
		opened = raw
		return raw, nil
	}
	identity := agentStreamRelayIdentity(c.agent, "anthropic")
	stream, err := NewManagedLlmStream(c.ctx, c.apiKwargs, factory, ManagedLlmStreamOptions{
		SessionID: identity.SessionID, Name: identity.Name, ModelName: identity.ModelName,
		Finalizer: func(context.Context) (any, error) { return accumulator.Finalize(), nil },
		OnStreamCreated: func(_ context.Context, raw any) error {
			ms, _ := raw.(anthMessageStream)
			var resp *http.Response
			c.mu.Lock()
			if ms != nil {
				resp = ms.HTTPResponse()
				c.attemptConn = ms.AttemptConn
			}
			c.attemptResponse = resp
			c.mu.Unlock()
			if resp != nil {
				quietCall(func() {
					c.mu.Lock()
					defer c.mu.Unlock()
					StreamDiagCaptureResponse(diag, resp)
				})
			}
			token := ClaimStreamWriter(c.agent)
			c.mu.Lock()
			c.writerToken = token
			c.mu.Unlock()
			c.reabortIfCancelled(resp)
			return nil
		},
		OnChunk:      func(_ context.Context, chunk any) error { return accumulator.Observe(chunk) },
		ChunkAdapter: func(chunk any) any { return chunk },
		AcceptChunk:  func(any) bool { return c.writerStillCurrent("Anthropic streaming") },
		Metadata:     identity.metadata("anthropic_messages"), DeferLogicalCompletion: true,
	})
	if err != nil {
		return nil, err
	}
	c.setManagedStream(stream)
	err = func() (loopErr error) {
		defer func() {
			c.closeManagedStream()
			if opened != nil {
				_ = opened.Close()
			}
		}()
		for {
			raw, err := stream.Next(c.ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			sawEvent = true
			c.countChunk(diag, raw)
			if c.agent.InterruptRequested() {
				break
			}
			event, _ := raw.(map[string]any)
			switch asStr(event["type"]) {
			case "message_stop":
				sawStop = true
			case "message_delta":
				delta, _ := event["delta"].(map[string]any)
				reason, _ := delta["stop_reason"].(string)
				c.markFinishSeen(diag, reason)
			case "content_block_start":
				block, _ := event["content_block"].(map[string]any)
				if anyutil.IsTrue(block) && asStr(block["type"]) == "tool_use" {
					hasToolUse = true
					if name := asStr(block["name"]); name != "" {
						c.emitToolStarted(name)
						c.mu.Lock()
						c.partialToolNames = append(c.partialToolNames, name)
						c.mu.Unlock()
					}
				}
			case "content_block_delta":
				delta, _ := event["delta"].(map[string]any)
				switch asStr(delta["type"]) {
				case "text_delta":
					text := asStr(delta["text"])
					if text != "" && !hasToolUse {
						c.emitText(text)
					}
					if textWatch.Feed(text) {
						runaway = "content"
					}
				case "thinking_delta":
					if thinking := asStr(delta["thinking"]); thinking != "" {
						c.emitReasoning(thinking)
						if thinkingWatch.Feed(thinking) {
							runaway = "reasoning"
						}
					}
				}
			}
			if runaway != "" {
				break
			}
		}
		if !c.agent.InterruptRequested() && opened != nil && runaway == "" {
			if !sawStop {
				if sawEvent {
					return &EmptyStreamError{Message: "Anthropic Messages stream ended before message_stop (possible upstream stream drop)."}
				}
				return &EmptyStreamError{Message: "Provider returned an empty stream with no events (possible upstream error or malformed event stream)."}
			}
			baseFinal = opened.Snapshot()
			if details := accumulator.Finalize()["stop_details"]; details != nil && baseFinal["stop_details"] == nil {
				baseFinal["stop_details"] = details
			}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if c.agent.InterruptRequested() {
		return nil, nil
	}
	if runaway != "" {
		c.logRunawayCut(runaway)
		var snapshot map[string]any
		if opened != nil {
			snapshot = opened.Snapshot()
		}
		message := relayJSONableDict(accumulator.Response(snapshot))
		message["stop_reason"] = "max_tokens"
		message[ccRawRunaway] = true
		return message, nil
	}
	if baseFinal != nil {
		if err := anthCheckMessage(baseFinal, false); err != nil {
			return nil, err
		}
		if !stream.OutputModified {
			if err := anthCheckMessage(baseFinal, true); err != nil {
				return nil, err
			}
			return baseFinal, nil
		}
	}
	message := relayJSONableDict(accumulator.Response(baseFinal))
	if err := anthCheckMessage(message, true); err != nil {
		return nil, err
	}
	return message, nil
}
