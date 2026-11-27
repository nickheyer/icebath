package auxiliary

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Why a failed call falls back to the next route
const (
	reasonAuth       = "auth error"
	reasonPayment    = "payment error"
	reasonRateLimit  = "rate limit"
	reasonModel      = "model incompatible with route"
	reasonInvalid    = "invalid provider response"
	reasonServer     = "server error"
	reasonTimeout    = "request timed out"
	reasonConnection = "connection error"
)

// Reply a router sends in place of a completion when its upstream timed out
const shim = "Connect timeout, please try again later."

// Request fields that carry reasoning, all dropped when a route refuses them
var reasoningFields = []string{"reasoning", "reasoning_effort", "thinking", "thinking_config", "thinkingconfig", "thinking_budget", "thinkingbudget",
	"enable_thinking", "think", "verbosity"}

// Words a provider refuses a part of a request with
var refusals = []string{"unavailable", "does not support", "doesn't support", "not supported", "unsupported parameter", "unsupported_parameter",
	"unknown parameter", "unrecognized request argument", "unrecognized parameter", "extra inputs are not permitted", "unsupported value", "unsupported"}

var billing = []string{"credits", "insufficient funds", "insufficient_quota", "can only afford", "billing", "quota exceeded", "daily limit",
	"resource exhausted", "weekly limit", "payment required"}

var afford = regexp.MustCompile(`can only afford\s+(\d+)`)

// Blocks of thinking some models put before their answer
var thinking = regexp.MustCompile(`(?s)<think>.*?</think>|<thinking>.*?</thinking>|<reasoning>.*?</reasoning>|<thought>.*?</thought>|` +
	`<REASONING_SCRATCHPAD>.*?</REASONING_SCRATCHPAD>|<思考>.*?</思考>|<反思>.*?</反思>|<推理>.*?</推理>|<推敲>.*?</推敲>`)

// Call that ran out of time, all of it when it never sent anything
type timeout struct {
	text string
	full bool
}

func (t *timeout) Error() string { return t.text }

// A call stopped for its silence is one that timed out, as the failures that are tried again later count it
func (t *timeout) Timeout() bool { return true }

// One route's answer to the request, retried on the route while that can help: after a brief network error, once at a rate limit, and once
// without each part of the request the route refuses
func (c *Client) attempt(ctx context.Context, task Task, s settings, next candidate, request Request) (Answer, error) {
	client, err := c.Transports.Client(next.route)
	if err != nil {
		return Answer{}, err
	}
	spec, _ := c.Providers.Spec(next.route.Spec)
	model := next.route.Model
	bare := strings.ToLower(model[strings.LastIndex(model, "/")+1:])
	learned := c.Config.Profile() + "/" + next.key() + "/" + bare + "/"
	call := &provider.Request{
		Model:       model,
		Messages:    request.Messages,
		Temperature: spec.Temperature(model, request.Temperature),
		Format:      request.Format,
		Reasoning:   provider.Reasoning{Effort: s.effort},
		Extra:       maps.Clone(s.extra),
		Config:      c.Config,
	}
	if request.Reasoning != nil {
		call.Reasoning.Effort = *request.Reasoning
	}
	if call.Extra == nil {
		call.Extra = map[string]any{}
	}
	maps.Copy(call.Extra, request.Extra)
	if forwards(task, next.route) {
		call.MaxTokens = request.MaxTokens
	}
	if c.State.refuses(learned + "temperature") {
		call.Temperature = nil
	}
	if call.Format != nil && (slices.Contains(spec.UnsupportedResponseFormats, "json_schema") || c.State.refuses(learned+"format")) {
		call.Format = nil
	}
	if call.Reasoning.Effort == "none" && c.State.refuses(learned+"floor") {
		call.Reasoning.Effort = "low"
	}
	window := request.Timeout
	if window <= 0 {
		window = time.Duration(max(s.timeout, task.MinTimeout)) * time.Second
	}
	stall := min(time.Duration(cmp.Or(s.stall, 60))*time.Second, window)
	retries := min(max(c.Config.GetInt(auxiliaryconfig.TransientRetries), 0), 6)
	done := map[string]bool{}
	once := func(rung string) bool {
		first := !done[rung]
		done[rung] = true
		return first
	}
	for transient := 0; ; {
		watched := c.preCall(ctx, task, next.route, call)
		response, err := c.stream(ctx, client, call, window, stall, request.Progress)
		c.postCall(ctx, watched, call, response, err)
		if err == nil {
			if response.Usage != nil && c.Used != nil {
				c.Used(task.Name, next.route, *response.Usage)
			}
			text, err := answer(task, response)
			return Answer{Text: text, Finish: response.FinishReason, Route: next.route}, err
		}
		if ctx.Err() != nil {
			return Answer{}, ctx.Err()
		}
		var failure *provider.APIError
		errors.As(err, &failure)
		text := strings.ToLower(err.Error())
		var late *timeout
		switch {
		case transientError(err) && transient < retries && !(task.Critical && errors.As(err, &late) && late.full):
			transient++
			if err := sleep(ctx, min(time.Second<<(transient-1), 8*time.Second)); err != nil {
				return Answer{}, err
			}
		case paymentError(err) && afford.MatchString(text) && once("afford"):
			match := afford.FindStringSubmatch(text)
			limit, _ := strconv.Atoi(match[1])
			if limit -= 64; limit < 512 || call.MaxTokens > 0 && call.MaxTokens <= limit {
				return Answer{}, err
			}
			call.MaxTokens = limit
		case failure != nil && failure.Kind == provider.ErrRateLimit && !paymentError(err) && once("rate"):
			wait := failure.RetryAfter
			if wait > 5*time.Second {
				return Answer{}, err
			}
			if wait <= 0 {
				wait = time.Second + time.Duration(rand.Float64()*float64(time.Second/2))
			}
			if err := sleep(ctx, min(wait, 4*time.Second)); err != nil {
				return Answer{}, err
			}
		case call.Temperature != nil && rejected(failure, text, "temperature") && once("temperature"):
			call.Temperature = nil
			c.State.refuse(learned + "temperature")
		case call.Format != nil && formatRefused(failure, text) && once("format"):
			call.Format = nil
			if capability(text) {
				c.State.refuse(learned + "format")
			}
		case reasoningRequired(failure, text) && once("floor"):
			call.Reasoning.Effort = "low"
			for _, field := range reasoningFields {
				delete(call.Extra, field)
			}
			c.State.refuse(learned + "floor")
		case (call.Reasoning.Effort != "" || hasReasoning(call.Extra)) && reasoningRefused(failure, text) && once("reasoning"):
			call.Reasoning.Effort = ""
			for _, field := range reasoningFields {
				delete(call.Extra, field)
			}
		case call.MaxTokens > 0 && (strings.Contains(text, "max_tokens") || strings.Contains(text, "max_completion_tokens") || strings.Contains(text, "unsupported_parameter") ||
			strings.Contains(text, "1210") && strings.Contains(next.route.BaseURL, "bigmodel")) && once("max"):
			call.MaxTokens = 0
		default:
			return Answer{}, err
		}
	}
}

// Text of a reply: the answer without blocks of thinking, else the reasoning when that is all the model sent
func answer(task Task, response *provider.Response) (string, error) {
	message := response.Message
	if strings.TrimSpace(message.GetText()) == shim && len(message.GetToolCalls()) == 0 {
		return "", &provider.APIError{Kind: provider.ErrEmptyResponse, Message: fmt.Sprintf("A router in front of the provider gave up waiting "+
			"for the helper job %s and sent a stand-in instead of an answer.", cmp.Or(task.Title, task.Name)), Own: true}
	}
	if text := strings.TrimSpace(thinking.ReplaceAllString(message.GetText(), "")); text != "" {
		return text, nil
	}
	return strings.TrimSpace(message.GetReasoning()), nil
}

// Streams the reply, stopping it when nothing comes for the window, when it stalls for the stall time once it started, or at the ceiling
func (c *Client) stream(ctx context.Context, client provider.Client, call *provider.Request, window, stall time.Duration, progress func()) (*provider.Response, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	start := time.Now()
	ceiling := max(10*time.Minute, 4*window)
	hard := time.AfterFunc(ceiling, func() {
		cancel(&timeout{text: fmt.Sprintf("The helper model was still answering after %s, past its time limit, so it was stopped.", strs.Span(ceiling))})
	})
	defer hard.Stop()
	var mu sync.Mutex
	started := false
	idle := time.AfterFunc(window, func() {
		mu.Lock()
		defer mu.Unlock()
		elapsed := time.Since(start).Seconds()
		if !started {
			cancel(&timeout{text: fmt.Sprintf("The helper model gave no output within %s.", strs.Span(max(window.Round(time.Second), time.Second))), full: true})
			return
		}
		cancel(&timeout{text: fmt.Sprintf("The helper model stopped giving output for %s, after %s, so it was stopped.",
			strs.Span(max(stall.Round(100*time.Millisecond), 100*time.Millisecond)), strs.Span(max(time.Duration(elapsed*float64(time.Second)).Round(100*time.Millisecond), 100*time.Millisecond)))})
	})
	defer idle.Stop()
	response, err := client.Stream(ctx, call, func(event provider.StreamEvent) error {
		// Only output restarts the stall clock, not what the wire says of its wait
		if !event.Delivers() {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		started = true
		idle.Reset(stall)
		if progress != nil {
			progress()
		}
		return nil
	})
	var late *timeout
	if cause := context.Cause(ctx); err != nil && errors.As(cause, &late) {
		return nil, late
	}
	return response, err
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Whether a route is sent the output cap the caller asks for. Elsewhere the provider's own cap holds, since a tight one cuts reasoning models off
func forwards(task Task, route provider.Route) bool {
	host := provider.BaseHost(route.BaseURL)
	return strings.HasPrefix(route.Spec, "minimax") || strings.Contains(strings.ToLower(route.BaseURL), "/anthropic") || route.Spec == "nvidia" ||
		host == "integrate.api.nvidia.com" || route.Spec == "openrouter" || host == "openrouter.ai" || route.Mode == provider.ModeGeminiNative ||
		task.Name == "moa_reference"
}

func transientError(err error) bool {
	var late *timeout
	if errors.As(err, &late) {
		return true
	}
	var failure *provider.APIError
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Kind == provider.ErrNetwork || failure.Kind == provider.ErrEmptyResponse || failure.StatusCode == 408 || failure.StatusCode >= 500
}

func paymentError(err error) bool {
	var failure *provider.APIError
	if !errors.As(err, &failure) {
		return false
	}
	if failure.StatusCode == 402 {
		return true
	}
	switch failure.StatusCode {
	case 0, 403, 404, 429:
		return contains(billing, strings.ToLower(failure.Message))
	}
	return false
}

// Why a failed call should try the next route, empty when no other route would do better
func fallbackReason(err error) string {
	var late *timeout
	if errors.As(err, &late) {
		return reasonTimeout
	}
	if paymentError(err) {
		return reasonPayment
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return reasonInvalid
	}
	var failure *provider.APIError
	if !errors.As(err, &failure) {
		return ""
	}
	text := strings.ToLower(failure.Message)
	switch failure.Kind {
	case provider.ErrAuth:
		return reasonAuth
	case provider.ErrRateLimit:
		return reasonRateLimit
	case provider.ErrNetwork:
		return reasonConnection
	case provider.ErrEmptyResponse:
		return reasonInvalid
	case provider.ErrServer:
		return reasonServer
	case provider.ErrInvalidRequest:
		if strings.Contains(text, "model") && (strings.Contains(text, "incompatible") || strings.Contains(text, "not supported") || strings.Contains(text, "does not support")) {
			return reasonModel
		}
	}
	return ""
}

// Whether a provider turned down the named field of the request
func rejected(failure *provider.APIError, text, field string) bool {
	return failure != nil && strings.Contains(text, field) && contains(refusals, text)
}

// Whether a provider turned down the reply format
func formatRefused(failure *provider.APIError, text string) bool {
	if failure == nil || failure.StatusCode != 0 && failure.StatusCode != 400 && failure.StatusCode != 422 {
		return false
	}
	format := strings.Contains(text, "response_format") || strings.Contains(text, "output_config") || strings.Contains(text, "text.format")
	return contains([]string{"guided_grammar", "xgrammar", "compile_grammar_error", "response mime type", "response_schema", "response_json_schema"}, text) ||
		format && (strings.Contains(text, "json_schema") || contains(refusals, text))
}

// Whether a refusal is one of capability, which the route will repeat, rather than of the schema sent
func capability(text string) bool {
	return contains(refusals, text) && !strings.Contains(text, "invalid schema")
}

var reasoningToken = regexp.MustCompile(`\b(reasoning_effort|thinking_config|thinking_budget|enable_thinking|thinkingconfig|thinkingbudget|reasoning|thinking|think)\b`)

// Whether a provider said in the failure that its model can't be told not to reason, or named the reasoning levels it takes
func ReasoningRequired(err error) bool {
	failure, _ := errors.AsType[*provider.APIError](err)
	return err != nil && reasoningRequired(failure, strings.ToLower(err.Error()))
}

// Whether a provider turned down the reasoning fields of the request in the failure
func ReasoningRefused(err error) bool {
	failure, _ := errors.AsType[*provider.APIError](err)
	return err != nil && reasoningRefused(failure, strings.ToLower(err.Error()))
}

// Whether a provider said its model can't be told not to reason
func reasoningRequired(failure *provider.APIError, text string) bool {
	if failure == nil || failure.StatusCode != 0 && failure.StatusCode != 400 && failure.StatusCode != 422 {
		return false
	}
	for _, at := range reasoningToken.FindAllStringIndex(text, -1) {
		near := text[max(at[0]-48, 0):min(at[1]+96, len(text))]
		if contains([]string{"mandatory", "cannot be disabled", "can't be disabled", "must be enabled", "is required", "always enabled", "cannot be turned off",
			"must be one of", "invalid value for"}, near) {
			return true
		}
	}
	return false
}

// Whether a provider turned down the reasoning fields
func reasoningRefused(failure *provider.APIError, text string) bool {
	if failure == nil {
		return false
	}
	if strings.Contains(text, "'param': 'reasoning") || strings.Contains(text, "invalid_reasoning_effort") {
		return true
	}
	for _, at := range reasoningToken.FindAllStringIndex(text, -1) {
		if strings.Contains(text[max(at[0]-32, 0):min(at[1]+32, len(text))], "unsupported") {
			return true
		}
	}
	return reasoningToken.MatchString(text) && contains(refusals, text)
}

func hasReasoning(extra map[string]any) bool {
	for _, field := range reasoningFields {
		if _, ok := extra[field]; ok {
			return true
		}
	}
	return false
}

// Whether the text holds any of the words
func contains(words []string, text string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}
