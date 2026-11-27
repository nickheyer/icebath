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
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	chatcompletions "github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions/transport"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Requests one long decode token may be sent with before a network that keeps dropping ends in an error
const tokenSends = 3

// Guidance a refusal of Google's API is given, by what went wrong
const (
	freeTier = "\n\nYour Google API key is on the free tier (a few hundred requests/day for Gemini Flash models). Icebath typically makes " +
		"3-10 API calls per user turn, so the free tier is exhausted in a handful of messages and cannot sustain an agent session. Enable " +
		"billing on your Google Cloud project and regenerate the key in a billing-enabled project: https://aistudio.google.com/apikey"
	standardKey = "\n\nGoogle Gemini rejected this API key's type — you do NOT need OAuth. Google began rejecting legacy 'Standard' Google " +
		"Cloud keys for the Gemini API on June 19, 2026, and all Standard keys stop working in September 2026. Open " +
		"https://aistudio.google.com/api-keys, check the key's type and status, and create a replacement Gemini API key (or, as a temporary " +
		"bridge, restrict the Standard key to generativelanguage.googleapis.com). Then update GEMINI_API_KEY / GOOGLE_API_KEY in the " +
		"profile's .env, delete any leftover Windows/system copy of those variables, and restart. A stale shell key can hide the .env value. " +
		"Details: https://ai.google.dev/gemini-api/docs/api-key"
	expressOnStudio = "\n\nGoogle issues 'AQ.' keys for both Google AI Studio and Vertex AI express mode, and each surface only accepts its " +
		"own keys: a Vertex express key always gets 403 PERMISSION_DENIED on generativelanguage.googleapis.com. If this key came from Google " +
		"Cloud / Vertex (express mode), point the provider at the express surface, e.g. set base_url / GEMINI_BASE_URL to " +
		"https://aiplatform.googleapis.com/v1beta1."
	studioOnExpress = "\n\nIf this 'AQ.' key came from Google AI Studio (https://aistudio.google.com/apikey), it only works on the default " +
		"generativelanguage.googleapis.com host — remove the explicit aiplatform base_url so the default surface is used."
	missingKey = "Gemini native client requires an API key, but none was provided. Set GOOGLE_API_KEY or GEMINI_API_KEY in your environment " +
		"or the profile's .env (get one at https://aistudio.google.com/app/apikey), or connect the Google provider with /providers."
)

type registrant struct {
	builder *chatcompletions.Builder
	pool    transport.Pool
}

func Register(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) {
	registry.Register(&registrant{builder: chatcompletions.NewBuilder(registry, cfg, providers, m)})
}

func (r *registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeGeminiNative: func(route provider.Route) (provider.Client, error) {
			if route.Auth != provider.AuthGCPOAuth && strings.TrimSpace(route.APIKey) == "" {
				return nil, errors.New(missingKey)
			}
			return &client{r: r, route: route, base: provider.GeminiBase(route.BaseURL)}, nil
		},
	}
}

type client struct {
	r     *registrant
	route provider.Route
	base  string
}

func (c *client) Route() provider.Route {
	return c.route
}

// Request the chat completions body of the request is translated into, with what it is read with
type prepared struct {
	built   *chatcompletions.Built
	model   string
	request map[string]any
	header  http.Header
}

func (c *client) prepare(ctx context.Context, request *provider.Request) (*prepared, error) {
	built, err := c.r.builder.Build(ctx, c.route, request)
	if err != nil {
		return nil, err
	}
	body := built.Body()
	model, _ := body["model"].(string)
	model = cmp.Or(model, request.Model)
	jsonSchema := strings.HasSuffix(strings.ToLower(c.base), "/v1beta")
	p := &prepared{built: built, model: bareModel(model), request: generateBody(body, model, jsonSchema)}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", provider.UserAgent("icebath-agent")+" (gemini-native)")
	h.Set("X-Goog-Api-Client", provider.UserAgent("icebath-agent"))
	for name, value := range c.route.Headers {
		h.Set(name, value)
	}
	transport.Extra(h, request.Config, built.Settings(), request.SessionID, false)
	if c.bearer() {
		h.Set("Authorization", "Bearer "+strings.TrimSpace(c.route.APIKey))
	} else {
		h.Set("x-goog-api-key", strings.TrimSpace(c.route.APIKey))
	}
	p.header = h
	return p, nil
}

// Whether the credential is an OAuth token, as Vertex's are, rather than an API key
func (c *client) bearer() bool {
	return c.route.Auth == provider.AuthGCPOAuth || c.route.Auth == provider.AuthOAuth
}

// Error of a refused request with the guidance its cause has: the free tier spent, a Standard key, a key and surface crossed
func (c *client) refusal(p *prepared, status int, header http.Header, data []byte) *provider.APIError {
	failure := p.built.Refused().HTTP(status, data, header)
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	object, _ := payload["error"].(map[string]any)
	state, message := strings.TrimSpace(transport.Text(object["status"])), strings.TrimSpace(transport.Text(object["message"]))
	reason := ""
	details, _ := object["details"].([]any)
	for _, d := range details {
		if detail, ok := d.(map[string]any); ok && strings.HasSuffix(transport.Text(detail["@type"]), "/google.rpc.ErrorInfo") && reason == "" {
			reason = transport.Text(detail["reason"])
		}
	}
	text := fmt.Sprintf("Gemini returned HTTP %d: %s", status, strings.ToValidUTF8(string(data), "�"))
	if message != "" {
		text = fmt.Sprintf("Gemini HTTP %d (%s): %s", status, cmpString(state, "error"), message)
	}
	probe := cmpString(message, string(data))
	key := strings.TrimSpace(c.route.APIKey)
	if !c.bearer() {
		lower := strings.ToLower(probe)
		switch {
		case status == 429 && strings.Contains(lower, "free_tier"):
			text += freeTier
		case status == 401 && (reason == "ACCESS_TOKEN_TYPE_UNSUPPORTED" || strings.Contains(lower, "expected oauth 2 access token")),
			status == 400 && strings.HasPrefix(key, "AIza") && (reason == "API_KEY_INVALID" || strings.Contains(lower, "api key not valid")):
			text += standardKey
		case status == 403 && strings.EqualFold(state, "PERMISSION_DENIED"):
			base := strings.ToLower(c.base)
			if strings.Contains(base, "aiplatform.googleapis.com") {
				text += studioOnExpress
			} else if strings.Contains(base, "generativelanguage.googleapis.com") && strings.HasPrefix(strings.ToUpper(key), "AQ.") {
				text += expressOnStudio
			}
		}
	}
	failure.Message = text
	return failure
}

func cmpString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	p, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	timeouts := p.built.Limits().Stream(ctx)
	watch := &transport.Watch{Timeouts: timeouts, Model: p.model, Tokens: p.built.Tokens(), Emit: fn}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	src := &source{c: c, p: p, ctx: attempt, connect: timeouts.Connect, slots: newSlots(), hops: maxHops(p.request), body: p.request}
	stop := watch.Headers(cancel)
	first, err := src.open()
	stop()
	if err != nil {
		if failure, ok := errors.AsType[*provider.APIError](err); ok {
			return nil, failure
		}
		return nil, p.built.Refused().Transport(ctx, watch.Failed(ctx, err))
	}
	src.response = first
	defer func() { src.close() }()
	chunks := p.built.Chunks(fn, first.Header, first.StatusCode)
	handle := func(f *transport.Frame) (bool, bool, error) {
		var event map[string]any
		if json.Unmarshal([]byte(f.Data), &event) != nil {
			return false, false, nil
		}
		for _, piece := range translate(event, p.model, src.slots) {
			stop, err := chunks.Feed(piece)
			if err != nil || stop {
				return true, true, err
			}
		}
		return true, false, nil
	}
	if err := watch.Run(ctx, src.next, handle); err != nil {
		if stream, ok := errors.AsType[*transport.StreamError](err); ok {
			return nil, p.built.Refused().Stream(stream)
		}
		if _, ok := errors.AsType[*provider.APIError](err); ok {
			return nil, err
		}
		if ctx.Err() == nil && !chunks.Delivered() {
			return nil, p.built.Refused().Transport(ctx, err)
		}
		return nil, chatcompletions.Unstopped(err)
	}
	return chunks.Finish()
}

// Events of every hop of one streamed turn, a dropped stream taken up again from the last token it gave
type source struct {
	c        *client
	p        *prepared
	ctx      context.Context
	connect  time.Duration
	slots    *slots
	hops     int
	body     map[string]any
	response *http.Response
	events   *transport.Events
	finished []map[string]any
	resume   string
	sends    int
	done     bool
}

func (s *source) close() {
	if s.response != nil {
		s.response.Body.Close()
	}
}

// Opens one streamGenerateContent request of the body
func (s *source) open() (*http.Response, error) {
	data, err := json.Marshal(s.body)
	if err != nil {
		return nil, fmt.Errorf("The Gemini request couldn't be encoded: %w", err)
	}
	call, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.c.base+"/models/"+s.p.model+":streamGenerateContent?alt=sse", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	call.Header = s.p.header.Clone()
	call.Header.Set("Accept", "text/event-stream")
	response, err := s.c.r.pool.Client(s.p.built.Settings(), s.connect, false).Do(call)
	if err != nil {
		return nil, transport.Timed(err, transport.PhaseConnect)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, transport.ErrorBodyLimit))
		return nil, s.c.refusal(s.p, response.StatusCode, response.Header, body)
	}
	s.events = transport.NewEvents(response.Body)
	return response, nil
}

// The next event of the turn as a frame, the long decode's next hop opened when one ends on a continuation token
func (s *source) next() (*transport.Frame, error) {
	for {
		if s.done {
			return nil, io.EOF
		}
		frame, err := s.events.Next()
		if err == nil {
			data := strings.TrimSpace(frame.Data)
			if data == "[DONE]" {
				err = io.EOF
			} else if data == "" {
				continue
			} else {
				var event map[string]any
				if json.Unmarshal([]byte(data), &event) != nil {
					slog.Debug("Non-JSON Gemini SSE line: " + strs.Head(strings.ToValidUTF8(data, ""), 200))
					continue
				}
				done := len(s.finished)
				event, token := split(event, &s.finished)
				if token != "" && len(s.finished) > done && len(s.finished) >= s.hops {
					cand := candidate(event)
					capped := map[string]any{}
					for k, v := range cand {
						capped[k] = v
					}
					capped["finishReason"] = "MAX_TOKENS"
					event["candidates"] = []any{capped}
					event["usageMetadata"] = mergeUsage(s.finished)
					token = ""
				}
				if token != "" {
					s.resume, s.sends = token, 0
				} else if transport.Text(candidate(event)["finishReason"]) != "" {
					s.resume = ""
				}
				raw, _ := json.Marshal(event)
				return &transport.Frame{Data: string(raw), Fields: map[string]string{}}, nil
			}
		}
		if !errors.Is(err, io.EOF) {
			if s.resume == "" || s.sends >= tokenSends {
				return nil, fmt.Errorf("Gemini streaming request failed: %w", err)
			}
			slog.Warn(fmt.Sprintf("Gemini stream dropped (%s); resuming from its last checkpoint", err))
		}
		if s.resume == "" || s.sends >= tokenSends {
			s.done = true
			return nil, io.EOF
		}
		s.close()
		s.body = withToken(s.p.request, s.resume)
		s.sends++
		response, err := s.open()
		if err != nil {
			return nil, err
		}
		s.response = response
	}
}

func withToken(request map[string]any, token string) map[string]any {
	out := map[string]any{}
	for k, v := range request {
		out[k] = v
	}
	out["continuationToken"] = token
	return out
}
