package transport

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials/aws"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/transport"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Carrier of the replies of this API in a stored message
const carrier = string(provider.ModeBedrockConverse)

// Line shown once IAM refuses ConverseStream and the session goes on without streaming
const deniedNotice = "\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream — falling back to non-streaming InvokeModel.\n   Grant that action to " +
	"restore streaming output.\n"

// Stop reasons as finish reasons
var finishes = map[string]string{"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length",
	"content_filtered": "content_filter", "guardrail_intervened": "content_filter"}

type registrant struct {
	config    *config.Config
	providers *provider.Registry
	models    *models.Models
	pool      transport.Pool
	profiles  aws.Profiles
	// Sections whose cachePoint each model refused, by the model and the section; routes IAM refused streaming to
	markers, unstreamed sync.Map
}

func Register(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) {
	registry.Register(&registrant{config: cfg, providers: providers, models: m})
}

func (r *registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeBedrockConverse: func(route provider.Route) (provider.Client, error) {
			if strings.TrimSpace(route.BaseURL) == "" {
				return nil, fmt.Errorf("%s has no Bedrock runtime address for %s.", route.Provider, route.Model)
			}
			return &client{r: r, route: route}, nil
		},
	}
}

type client struct {
	r     *registrant
	route provider.Route
}

func (c *client) Route() provider.Route {
	return c.route
}

func (c *client) key() string {
	return c.route.Provider + "\x00" + c.route.Model + "\x00" + strings.TrimRight(c.route.BaseURL, "/")
}

// One request shaped for Converse
type prepared struct {
	request  *provider.Request
	call     *provider.Call
	spec     provider.Spec
	settings config.View
	model    string
	// Model the request is served by, an application inference profile's own
	foundation string
	cache      bool
	body       map[string]any
	tokens     int
	limits     transport.Limits
	refused    transport.Refused
	aliases    map[string]string
}

// Kind of the route as its provider has it, else as its plugin registers it
func (c *client) kind(cfg *config.Config) provider.Spec {
	if instance, ok := c.r.providers.Instance(cfg, c.route.Provider); ok && instance.ID == c.route.Provider && instance.Spec.Name == c.route.Spec {
		return instance.Spec
	}
	spec, _ := c.r.providers.Spec(c.route.Spec)
	return spec
}

// Model id a reasoning model's floor is known by: a Bedrock id without its region and vendor
func family(model string) string {
	m := strings.ToLower(model)
	parts := strings.Split(m, ".")
	if len(parts) > 2 && len(parts[0]) <= 4 {
		parts = parts[1:]
	}
	if len(parts) > 1 {
		parts = parts[1:]
	}
	return strings.Join(parts, ".")
}

func (c *client) prepare(ctx context.Context, request *provider.Request) (*prepared, error) {
	cfg := cmp.Or(request.Config, c.r.config)
	spec := c.kind(cfg)
	settings := cfg.View(providerconfig.Key).View(c.route.Provider)
	model := cmp.Or(strings.TrimSpace(request.Model), c.route.Model)
	p := &prepared{request: request, spec: spec, settings: settings, model: model}
	region := cmp.Or(c.route.Region, aws.EndpointRegion(c.route.BaseURL))
	bearer := ""
	if c.route.Sign == nil {
		bearer = strings.TrimSpace(c.route.APIKey)
	}
	p.foundation = c.r.profiles.Resolve(ctx, c.r.pool.Client(settings, transport.Seconds(30), true), model, cmp.Or(region, "us-east-1"),
		c.route.Sign, bearer)
	p.call = &provider.Call{Context: ctx, Route: c.route, Model: model, Settings: settings, Config: cfg, SessionID: request.SessionID, Source: request.Source,
		Scope: transport.Scope(request.SessionID), Ask: request.Reasoning.Ask(), UserTurn: request.UserTurn, Kind: c.r.providers.Spec}
	p.call.Thinking, _ = c.r.models.Thinking(ctx, cfg, c.route)
	p.cache = cfg.GetString(transportconfig.PromptCacheTTL) != transportconfig.CacheOff
	tools, aliases := transport.Reserved(request.Tools, spec.ReservedTools)
	p.aliases = aliases
	p.body = c.body(p, tools)
	if spec.Shape != nil {
		extra, top := spec.Shape(p.call)
		maps.Copy(p.body, extra)
		maps.Copy(p.body, top)
		delete(p.body, "extra_headers")
	}
	for key, value := range request.Extra {
		p.body[key] = value
	}
	p.tokens = transport.Tokens(p.body)
	p.limits = transport.Limits{Config: cfg, Settings: settings, Model: model, Local: c.route.Local, Tokens: p.tokens, Deadline: request.Deadline,
		Family: family(p.foundation)}
	p.refused = transport.Refused{Route: c.route, Model: model, Spec: spec, Classify: request.Classify}
	return p, nil
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	p, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	resent := map[string]bool{}
	for {
		var response *provider.Response
		delivered := false
		if _, off := c.r.unstreamed.Load(c.key()); off {
			response, err = c.complete(ctx, p, fn)
		} else {
			response, delivered, err = c.stream(ctx, p, fn)
		}
		if err == nil || delivered || ctx.Err() != nil {
			return response, err
		}
		failure, _ := errors.AsType[*provider.APIError](err)
		text := ""
		if failure != nil {
			text = failure.Message + " " + fmt.Sprint(failure.Err)
		}
		if placement := refusedPlacement(text); placement != "" && !resent[placement] {
			if body, changed := stripMarkers(p.body, placement); changed {
				resent[placement] = true
				c.r.markers.Store(strings.ToLower(p.model)+"\x00"+placement, true)
				slog.Warn(fmt.Sprintf("bedrock: %s rejected a cachePoint block in %s — dropping that cache marker for this model and retrying. Prompt "+
					"caching stays active for the remaining sections.", p.model, placement))
				p.body = body
				continue
			}
		}
		if redactedRefusal.MatchString(text) && !resent["redacted"] {
			if body, changed := stripRedacted(p.body); changed {
				resent["redacted"] = true
				slog.Warn(fmt.Sprintf("bedrock: %s rejected replayed redacted reasoning (encrypted content is sealed to the issuing model/flow) - "+
					"stripping redacted blocks and resending once.", p.model))
				p.body = body
				continue
			}
		}
		if _, off := c.r.unstreamed.Load(c.key()); !off && denied(text) {
			c.r.unstreamed.Store(c.key(), true)
			slog.Info("bedrock: converse_stream denied by IAM — using non-streaming converse() for this session.")
			transport.Say(fn, provider.EventStatus, deniedNotice)
			continue
		}
		return nil, err
	}
}

// Whether IAM refused the streaming call, which the session then stops making
func denied(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "invokemodelwithresponsestream") && provider.Contains(lower, "accessdeniedexception", "unauthorizedexception",
		"not authorized", "accessdenied")
}

// HTTP request of the body at the model's operation, signed when the route signs, else with its Bedrock API key as the bearer
func (c *client) request(ctx context.Context, p *prepared, operation string) (*http.Request, []byte, error) {
	payload, err := json.Marshal(p.body)
	if err != nil {
		return nil, nil, fmt.Errorf("The Converse request couldn't be encoded: %w", err)
	}
	address := strings.TrimRight(c.route.BaseURL, "/") + aws.ModelPath(p.model, operation)
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("User-Agent", provider.UserAgent("IcebathAgent"))
	for name, value := range c.route.Headers {
		call.Header.Set(name, value)
	}
	if p.spec.RequestHeaders != nil {
		for name, value := range p.spec.RequestHeaders(p.call) {
			call.Header.Set(name, value)
		}
	}
	transport.Extra(call.Header, p.call.Config, p.settings, p.request.SessionID, false)
	if c.route.Sign != nil {
		if err := c.route.Sign(call, payload); err != nil {
			return nil, nil, fmt.Errorf("The request to %s couldn't be signed: %w", c.route.Provider, err)
		}
	} else if key := strings.TrimSpace(c.route.APIKey); key != "" {
		call.Header.Set("Authorization", "Bearer "+key)
	}
	return call, payload, nil
}

// Error of a refused call, its AWS error code and message read into the shape the classifier reads
func refusal(p *prepared, response *http.Response, data []byte) *provider.APIError {
	code, message := aws.ErrorOf(response.Header, response.StatusCode, data)
	body, _ := json.Marshal(transport.ErrorBody(map[string]any{"code": code, "message": cmp.Or(message, string(data))}, response.StatusCode))
	failure := p.refused.HTTP(response.StatusCode, body, response.Header)
	if message != "" && !strings.Contains(failure.Message, code) {
		failure.Message = code + ": " + failure.Message
	}
	return failure
}

// One streamed attempt: the reply, whether any of it reached the person, and the error that ended it
func (c *client) stream(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, bool, error) {
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: timeouts, Model: p.model, Tokens: p.tokens, Emit: fn}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	call, payload, err := c.request(attempt, p, "converse-stream")
	if err != nil {
		return nil, false, err
	}
	stop := watch.Headers(cancel)
	response, err := c.r.pool.Client(p.settings, timeouts.Connect, false).Do(call)
	stop()
	sent := func(err error) error {
		return &transport.SentError{URL: call.URL.String(), Bytes: len(payload), Err: err}
	}
	if err != nil {
		return nil, false, p.refused.Transport(ctx, sent(watch.Failed(ctx, err)))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, transport.ErrorBodyLimit))
		return nil, false, refusal(p, response, data)
	}
	s := newStream(p, fn, response.Header)
	events := transport.NewEventStream(response.Body)
	next := func() (*transport.Frame, error) {
		for {
			m, err := events.Next()
			if err != nil {
				return nil, err
			}
			if failure := m.Exception(response.Header); failure != nil {
				return nil, failure
			}
			if event := m.Header(":event-type"); event != "" {
				return &transport.Frame{Event: event, Data: string(m.Payload), Fields: map[string]string{}}, nil
			}
		}
	}
	if err := watch.Run(ctx, next, s.frame); err != nil {
		if stop, ok := errors.AsType[*stopped](err); ok {
			return nil, s.delivered, stop.err
		}
		if failure, ok := errors.AsType[*transport.StreamError](err); ok {
			return nil, s.delivered, p.refused.Stream(failure)
		}
		if _, ok := errors.AsType[*provider.APIError](err); ok {
			return nil, s.delivered, err
		}
		return nil, s.delivered, p.refused.Transport(ctx, sent(err))
	}
	if !s.stopped {
		return nil, s.delivered, p.refused.Empty("Bedrock Converse stream ended before messageStop; response is incomplete", nil)
	}
	reply := s.parts.build(p, s.ordered(), s.usage, s.reason)
	reply.Header = response.Header
	return reply, s.delivered, nil
}

// One Converse call that isn't streamed, waited on under the silent request limit
func (c *client) complete(ctx context.Context, p *prepared, fn provider.StreamFunc) (*provider.Response, error) {
	timeouts := p.limits.Stream(ctx)
	watch := &transport.Watch{Timeouts: transport.Timeouts{Stale: p.limits.Call(ctx, p.call.Ask.Requested(), 1)}, Model: p.model, Tokens: p.tokens,
		Emit: fn}
	response, err := transport.Await(ctx, watch, func(ctx context.Context) (*provider.Response, error) {
		bounded, cancel := context.WithTimeout(ctx, timeouts.Request)
		defer cancel()
		call, _, err := c.request(bounded, p, "converse")
		if err != nil {
			return nil, err
		}
		response, err := c.r.pool.Client(p.settings, timeouts.Connect, false).Do(call)
		if err != nil {
			return nil, p.refused.Transport(ctx, transport.Timed(err, transport.PhaseConnect))
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, p.refused.Transport(ctx, transport.Timed(err, transport.PhaseRead))
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, refusal(p, response, data)
		}
		var answer map[string]any
		if json.Unmarshal(data, &answer) != nil {
			return nil, p.refused.Stream(transport.FrameError(string(data), response.Header))
		}
		if _, ok := answer["output"]; !ok {
			return nil, p.refused.Empty("Bedrock Converse response carried no output", nil)
		}
		reply := c.normalize(p, answer)
		reply.Header = response.Header
		return reply, nil
	})
	if err != nil {
		return nil, err
	}
	return response, transport.Replay(response, fn)
}

// Reply of a whole Converse answer
func (c *client) normalize(p *prepared, answer map[string]any) *provider.Response {
	parts := &parts{}
	var ordered []any
	output, _ := answer["output"].(map[string]any)
	message, _ := output["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, item := range content {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case b["text"] != nil:
			piece := fmt.Sprint(b["text"])
			parts.texts = append(parts.texts, piece)
			ordered = append(ordered, map[string]any{"text": piece})
		case b["reasoningContent"] != nil:
			kept := map[string]any{}
			parts.reasoning(b["reasoningContent"], kept)
			if len(kept) > 0 {
				ordered = append(ordered, map[string]any{"reasoningContent": kept})
			}
		case b["toolUse"] != nil:
			tu, _ := b["toolUse"].(map[string]any)
			input := tu["input"]
			if input == nil {
				input = map[string]any{}
			}
			ordered = append(ordered, toolUse(transport.Text(tu["toolUseId"]), transport.Text(tu["name"]), input))
			parts.call(transport.Text(tu["toolUseId"]), transport.Text(tu["name"]), input)
		}
	}
	usage, _ := answer["usage"].(map[string]any)
	return parts.build(p, ordered, usage, cmp.Or(transport.Text(answer["stopReason"]), "end_turn"))
}

// What a reply is put together from
type parts struct {
	texts []string
	calls []*sessionsv1.ToolCall
	// Redacted reasoning of the reply, base64 encoded
	redacted []any
}

func (r *parts) call(id, name string, input any) {
	args, err := json.Marshal(input)
	if err != nil {
		args = []byte("{}")
	}
	r.calls = append(r.calls, &sessionsv1.ToolCall{Id: id, Name: name, Arguments: string(args)})
}

// Folds a reasoning payload into the reply and the block it came in, nested or flat; the text it added
func (r *parts) reasoning(raw any, kept map[string]any) string {
	reasoning, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if nested, ok := reasoning["reasoningText"].(map[string]any); ok {
		reasoning = maps.Clone(reasoning)
		maps.Copy(reasoning, nested)
	}
	piece := transport.Text(reasoning["text"])
	if piece != "" {
		kept["text"] = transport.Text(kept["text"]) + piece
	}
	if signature := transport.Text(reasoning["signature"]); signature != "" {
		kept["signature"] = transport.Text(kept["signature"]) + signature
	}
	if data := transport.Text(reasoning["redactedContent"]); data != "" {
		r.redacted = append(r.redacted, map[string]any{"type": "redacted_thinking", "data": data})
		kept["redactedContentBase64"] = data
	}
	return piece
}

// Reply of the ordered blocks: its text, its reasoning, its tool calls, the details replayed with it stamped with the model that made it
func (r *parts) build(p *prepared, ordered []any, usage map[string]any, stop string) *provider.Response {
	var details []any
	var thoughts []string
	for _, item := range ordered {
		b, _ := item.(map[string]any)
		reasoning, ok := b["reasoningContent"].(map[string]any)
		if !ok {
			continue
		}
		if piece := transport.Text(reasoning["text"]); piece != "" {
			thoughts = append(thoughts, piece)
			detail := map[string]any{"type": "thinking", "thinking": piece}
			if signature := transport.Text(reasoning["signature"]); signature != "" {
				detail["signature"] = signature
			}
			details = append(details, detail)
		}
		if data := transport.Text(reasoning["redactedContentBase64"]); data != "" {
			details = append(details, map[string]any{"type": "redacted_thinking", "data": data})
		}
	}
	if len(details) == 0 {
		details = r.redacted
	}
	if len(details) > 0 {
		details = append(details, map[string]any{"type": issuerRecord, "model": p.foundation})
	}
	finish, ok := finishes[stop]
	if !ok {
		finish = "stop"
	}
	if len(r.calls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	for _, call := range r.calls {
		if original, ok := p.aliases[call.Name]; ok {
			call.Name = original
		}
	}
	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: strings.Join(r.texts, "\n"), Reasoning: strings.Join(thoughts, "\n\n"),
		ToolCalls: r.calls, FinishReason: finish}
	if len(details) > 0 || len(ordered) > 0 {
		if raw, err := json.Marshal(carried{Details: details, Blocks: ordered}); err == nil {
			message.Carriers = map[string][]byte{carrier: raw}
		}
	}
	count := func(key string) int64 { n, _ := usage[key].(float64); return int64(n) }
	reply := &provider.Response{Message: message, FinishReason: finish}
	if len(usage) > 0 {
		reply.Usage = &provider.Usage{Input: count("inputTokens"), CacheRead: count("cacheReadInputTokens"), CacheWrite: count("cacheWriteInputTokens"),
			Output: count("outputTokens")}
	}
	return reply
}

// Live state of one ConverseStream reply
type stream struct {
	p      *prepared
	fn     provider.StreamFunc
	header http.Header
	parts  *parts
	blocks map[int]map[string]any
	// Block a delta without an index goes to, and whether one is open
	current int
	open    bool
	tool    map[string]string
	text    []string
	// A tool call began, so text after it isn't shown
	calling   bool
	stopped   bool
	reason    string
	usage     map[string]any
	delivered bool
}

func newStream(p *prepared, fn provider.StreamFunc, header http.Header) *stream {
	return &stream{p: p, fn: fn, header: header, parts: &parts{}, blocks: map[int]map[string]any{}}
}

// Error the consumer of the stream's events returned, which ends the stream as it is
type stopped struct{ err error }

func (e *stopped) Error() string { return e.err.Error() }

func (e *stopped) Unwrap() error { return e.err }

func (s *stream) emit(kind provider.EventKind, text string) error {
	if s.fn == nil {
		return nil
	}
	if err := s.fn(provider.StreamEvent{Kind: kind, Text: text}); err != nil {
		return &stopped{err}
	}
	return nil
}

// Block index of an event, a new one for a block that names none
func (s *stream) index(payload map[string]any, start bool) int {
	if n, ok := payload["contentBlockIndex"].(float64); ok {
		return int(n)
	}
	if start || !s.open {
		return len(s.blocks)
	}
	return s.current
}

func (s *stream) flush() {
	if len(s.text) > 0 {
		s.parts.texts = append(s.parts.texts, strings.Join(s.text, ""))
		s.text = nil
	}
}

// One event of the stream: whether it was output, whether reading stops, and the error it carries
func (s *stream) frame(f *transport.Frame) (bool, bool, error) {
	var payload map[string]any
	if json.Unmarshal([]byte(f.Data), &payload) != nil {
		return false, false, transport.FrameError(f.Data, s.header)
	}
	if err := s.emit(provider.EventActivity, "receiving stream response"); err != nil {
		return true, true, err
	}
	switch f.Event {
	case "contentBlockStart":
		i := s.index(payload, true)
		s.current, s.open = i, true
		start, _ := payload["start"].(map[string]any)
		if tu, ok := start["toolUse"].(map[string]any); ok {
			s.calling = true
			s.flush()
			s.tool = map[string]string{"id": transport.Text(tu["toolUseId"]), "name": transport.Text(tu["name"])}
			s.blocks[i] = toolUse(s.tool["id"], s.tool["name"], map[string]any{})
			name := s.tool["name"]
			if original, ok := s.p.aliases[name]; ok {
				name = original
			}
			if err := s.emit(provider.EventToolStart, name); err != nil {
				return true, true, err
			}
		}
	case "contentBlockDelta":
		i := s.index(payload, false)
		s.current, s.open = i, true
		delta, _ := payload["delta"].(map[string]any)
		switch {
		case delta["text"] != nil:
			piece := fmt.Sprint(delta["text"])
			b := s.blocks[i]
			if b == nil {
				b = map[string]any{"text": ""}
				s.blocks[i] = b
			}
			b["text"] = transport.Text(b["text"]) + piece
			s.text = append(s.text, piece)
			if !s.calling && piece != "" {
				s.delivered = true
				if err := s.emit(provider.EventTextDelta, piece); err != nil {
					return true, true, err
				}
			}
		case delta["toolUse"] != nil && s.tool != nil:
			tu, _ := delta["toolUse"].(map[string]any)
			s.tool["input"] += transport.Text(tu["input"])
		case delta["reasoningContent"] != nil:
			b := s.blocks[i]
			if b == nil {
				b = map[string]any{"reasoningContent": map[string]any{}}
				s.blocks[i] = b
			}
			kept, _ := b["reasoningContent"].(map[string]any)
			if piece := s.parts.reasoning(delta["reasoningContent"], kept); piece != "" {
				s.delivered = true
				if err := s.emit(provider.EventReasoningDelta, piece); err != nil {
					return true, true, err
				}
			}
		}
	case "contentBlockStop":
		i := s.index(payload, false)
		s.open = false
		if s.tool != nil {
			input := arguments(s.tool["input"])
			if strings.TrimSpace(s.tool["input"]) == "" {
				input = map[string]any{}
			}
			s.parts.call(s.tool["id"], s.tool["name"], input)
			if b, ok := s.blocks[i]; ok {
				if tu, ok := b["toolUse"].(map[string]any); ok {
					tu["input"] = input
				}
			}
			s.tool = nil
		} else {
			s.flush()
		}
	case "messageStop":
		s.stopped = true
		s.reason = cmp.Or(transport.Text(payload["stopReason"]), "end_turn")
	case "metadata":
		usage, _ := payload["usage"].(map[string]any)
		s.usage = map[string]any{}
		for _, k := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens"} {
			s.usage[k] = usage[k]
		}
	}
	return true, false, nil
}

// Blocks of the reply in their order
func (s *stream) ordered() []any {
	s.flush()
	indexes := slices.Sorted(maps.Keys(s.blocks))
	out := make([]any, 0, len(indexes))
	for _, i := range indexes {
		if b := s.blocks[i]; len(b) > 0 {
			if reasoning, ok := b["reasoningContent"].(map[string]any); ok && len(reasoning) == 0 {
				continue
			}
			out = append(out, b)
		}
	}
	return out
}
