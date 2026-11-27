package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	codexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/transports/codexresponses/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const carrier = string(provider.ModeCodexResponses)

type registrant struct {
	config    *config.Config
	providers *provider.Registry
	models    *models.Models
	pool      transport.Pool
	// Sessions that stopped replaying encrypted reasoning after it was refused, models that refused every turn's reasoning, models warned of
	// as taking no reasoning off
	replayOff, allTurnsOff, warned sync.Map
}

func Register(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) {
	registry.Register(&registrant{config: cfg, providers: providers, models: m})
}

func (r *registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeCodexResponses: func(route provider.Route) (provider.Client, error) {
			if strings.TrimSpace(route.BaseURL) == "" {
				return nil, fmt.Errorf("%s has no address for %s. Set its Base URL in /providers.", route.Provider, route.Model)
			}
			return &client{r: r, route: route}, nil
		},
	}
}

// Whether the session replays no encrypted reasoning to the model any more
func (r *registrant) off(session, model string) bool {
	_, ok := r.replayOff.Load(session + "\x00" + model)
	return ok
}

func (r *registrant) rejectsAllTurns(model string) bool {
	_, ok := r.allTurnsOff.Load(model)
	return ok
}

type client struct {
	r     *registrant
	route provider.Route
}

func (c *client) Route() provider.Route {
	return c.route
}

// Physical retries of one call whose stream broke on its way
const physicalRetries = 1

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	clocks := &clocks{call: time.Now()}
	recovered := map[string]bool{}
	for {
		p, err := c.prepare(ctx, request)
		if err != nil {
			return nil, err
		}
		w := c.watchdogs(ctx, p)
		response, err := c.attempts(ctx, p, w, clocks, fn)
		if err == nil || ctx.Err() != nil {
			return response, err
		}
		failure, ok := errors.AsType[*provider.APIError](err)
		if !ok || failure.StatusCode != 0 && failure.StatusCode != 400 {
			return nil, err
		}
		text := strings.ToLower(failure.Message + " " + fmt.Sprint(failure.Err))
		switch {
		case p.allTurns && !recovered["all_turns"] && (strings.Contains(text, "reasoning.context") || strings.Contains(text, "all_turns")):
			recovered["all_turns"] = true
			c.r.allTurnsOff.Store(p.model, true)
			slog.Warn(fmt.Sprintf("reasoning.context rejected by %s; omitting it for this session", p.model))
		case !recovered["replay"] && !c.r.off(request.SessionID, p.model) && encryptedRefusal(text):
			recovered["replay"] = true
			c.r.replayOff.Store(request.SessionID+"\x00"+p.model, true)
			slog.Warn(fmt.Sprintf("%s refused the replayed encrypted reasoning; this session sends it no more", p.model))
		default:
			return nil, err
		}
	}
}

// Whether a refusal is of replayed encrypted reasoning the endpoint can't vouch for
func encryptedRefusal(text string) bool {
	return strings.Contains(text, "invalid_encrypted_content") || strings.Contains(text, "encrypted content for item") && strings.Contains(text, "could not be verified") ||
		strings.Contains(text, "could not decrypt the provided encrypted_content") || strings.Contains(text, "encrypted content could not be decrypted or parsed") ||
		strings.Contains(text, "encrypted_content") && strings.Contains(text, "was not issued to this caller")
}

// Physical attempts of one call: a stream that broke on its way before any event asked once more, its oversized tool outputs saved to
// files first
func (c *client) attempts(ctx context.Context, p *prepared, w watchdogs, clocks *clocks, fn provider.StreamFunc) (*provider.Response, error) {
	for attempt := 0; ; attempt++ {
		response, events, err := c.once(ctx, p, w, attempt, clocks, fn)
		if err == nil || ctx.Err() != nil {
			return response, err
		}
		if attempt >= physicalRetries || !broken(err) {
			return nil, c.classify(ctx, p, err)
		}
		slog.Debug(fmt.Sprintf("Codex Responses stream transport failed (attempt %d/%d); retrying. model=%s error=%v", attempt+1, physicalRetries+1,
			p.model, err))
		if events == 0 {
			spill(p, attempt+1)
		}
	}
}

// Attempt the watch over the call cut short, which no physical retry asks again: the retry loops of the caller reconnect
type killed struct{ err error }

func (e *killed) Error() string { return e.err.Error() }

func (e *killed) Unwrap() error { return e.err }

// Whether an attempt broke on its way rather than on the provider's answer, the watch over it or the consumer of its events
func broken(err error) bool {
	if _, ok := errors.AsType[*killed](err); ok {
		return false
	}
	if _, ok := errors.AsType[*stopped](err); ok {
		return false
	}
	if _, ok := errors.AsType[*provider.APIError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*eventError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*transport.StreamError](err); ok {
		return false
	}
	if errors.Is(err, errNoTerminal) {
		return false
	}
	_, network := errors.AsType[net.Error](err)
	_, timed := errors.AsType[*transport.TimeoutError](err)
	return network || timed || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// Error of a failed attempt as its caller reads it
func (c *client) classify(ctx context.Context, p *prepared, err error) error {
	if _, ok := errors.AsType[*provider.APIError](err); ok {
		return err
	}
	if stop, ok := errors.AsType[*stopped](err); ok {
		return stop.err
	}
	if cut, ok := errors.AsType[*killed](err); ok {
		return p.refused.Transport(ctx, cut.err)
	}
	if event, ok := errors.AsType[*eventError](err); ok {
		failure := p.refused.Stream(&transport.StreamError{Body: event.body(), Text: event.message})
		if failure.Message == "" {
			failure.Message = event.message
		}
		return failure
	}
	if stream, ok := errors.AsType[*transport.StreamError](err); ok {
		return p.refused.Stream(stream)
	}
	if errors.Is(err, errNoTerminal) {
		return p.refused.Empty(errNoTerminal.Error(), err)
	}
	return p.refused.Transport(ctx, transport.Timed(err, transport.PhaseConnect))
}

// Saves the oversized tool outputs of the request's input to files, for a stream that broke before any event when its input is past the
// budget of one turn
func spill(p *prepared, attempt int) {
	items, _ := p.body["input"].([]any)
	raw, _ := json.Marshal(items)
	budget := tools.BudgetFor(p.cfg, 0)
	if len(raw) <= budget.Turn {
		return
	}
	env := &tools.Env{Config: p.cfg}
	spilled := 0
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
		entry, ok := item.(map[string]any)
		output, isText := entry["output"].(string)
		if !ok || entry["type"] != "function_call_output" || !isText {
			continue
		}
		id, _ := entry["call_id"].(string)
		if replaced := tools.Spill(env, output, "codex_zero_event_retry", strs.Filled(id, "call"), budget.Result, budget.Preview); replaced != output {
			copied := map[string]any{}
			for k, v := range entry {
				copied[k] = v
			}
			copied["output"] = replaced
			out[i] = copied
			spilled++
		}
	}
	if spilled == 0 {
		slog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): no prunable tool output; resending payload unchanged (serialized_input_bytes=%d, "+
			"model=%s)", attempt, physicalRetries+1, len(raw), p.model))
		return
	}
	after, _ := json.Marshal(out)
	slog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): spilled %d oversized tool output(s) before reconnect, serialized_input_bytes=%d -> %d "+
		"(model=%s)", attempt, physicalRetries+1, spilled, len(raw), len(after), p.model))
	p.body["input"] = out
}

// Clocks of one call across its physical attempts
type clocks struct {
	call, attempt, event, progress, retry time.Time
}

// Thresholds of the watch over one call: its whole call silence limit, the first event, the first progress and the silence between events
type watchdogs struct {
	stale               time.Duration
	firstEvent          time.Duration
	firstEventOn        bool
	idle                time.Duration
	idleOn              bool
	afterProgress       bool
	progress            time.Duration
	tokens              int
	local, codexWatched bool
}

// First progress grace of a large request to the Codex backend
const firstProgress = 300 * time.Second

// Least silence limit of a request to the Codex backend by the size of its context
func codexFloor(tokens int) time.Duration {
	switch {
	case tokens > 100_000:
		return 1200 * time.Second
	case tokens > 50_000:
		return 900 * time.Second
	case tokens > 10_000:
		return 600 * time.Second
	}
	return 0
}

// Whether a setting was given in the file or the environment rather than left at its default
func set(cfg *config.Config, key string) bool {
	_, setting, ok := cfg.Setting(key)
	if !ok {
		return false
	}
	source, _ := cfg.Source(setting)
	return source != config.FromDefault
}

func (c *client) watchdogs(ctx context.Context, p *prepared) watchdogs {
	cfg := p.cfg
	effort := p.call.Ask.Requested()
	w := watchdogs{tokens: p.tokens, local: c.route.Local || models.Local(c.route.BaseURL), codexWatched: p.codex}
	w.stale = p.limits.Call(ctx, effort, 1)
	effortFloor := transport.EffortFloor(effort)
	floor := time.Duration(0)
	if !w.local {
		floor = codexFloor(p.tokens)
		if floor > 0 && w.stale < floor {
			w.stale = floor
		}
		if hard := transport.Seconds(cfgFloat(cfg, codexconfig.HardTimeout)); hard > 0 && w.stale > hard {
			w.stale = hard
		}
	}
	idle := 12 * time.Second
	switch {
	case p.tokens > 100_000:
		idle = 180 * time.Second
	case p.tokens > 50_000:
		idle = 120 * time.Second
	case p.tokens > 10_000:
		idle = 60 * time.Second
	}
	idle = max(idle, effortFloor)
	explicit := set(cfg, codexconfig.FirstEventTimeout)
	w.firstEvent = transport.Seconds(cfgFloat(cfg, codexconfig.FirstEventTimeout))
	w.firstEventOn = w.firstEvent > 0
	switch {
	case !w.firstEventOn:
	case !w.local:
		above := cfgFloat(cfg, codexconfig.FirstEventLargeAbove)
		if !cfg.GetBool(codexconfig.FirstEventStrict) && above > 0 && float64(p.tokens) >= above && w.firstEvent < idle {
			slog.Info(fmt.Sprintf("Scaling codex-responses no-event TTFB watchdog from %.0fs to %.0fs for large request (context=~%s tokens >= %.0f). "+
				"Set %s to keep the smaller cutoff.", w.firstEvent.Seconds(), idle.Seconds(), strs.Thousands(p.tokens), above, codexconfig.FirstEventStrict))
			w.firstEvent = idle
		}
		if limit := transport.Seconds(cfgFloat(cfg, codexconfig.FirstEventMax)); limit > 0 && w.firstEvent > limit {
			slog.Info(fmt.Sprintf("Capping codex-responses no-event TTFB timeout from %.0fs to %.0fs (context=~%s tokens) per %s.", w.firstEvent.Seconds(),
				limit.Seconds(), strs.Thousands(p.tokens), codexconfig.FirstEventMax))
			w.firstEvent = limit
		}
	case !explicit:
		if ceiling := transport.Seconds(cfgFloat(cfg, transportconfig.LocalStaleTimeout)); ceiling > w.firstEvent {
			slog.Info(fmt.Sprintf("Local provider detected (%s) — no-event TTFB watchdog raised from %.0fs to %.0fs (%s); set %s for an explicit cutoff.",
				c.route.BaseURL, w.firstEvent.Seconds(), ceiling.Seconds(), transportconfig.LocalStaleTimeout, codexconfig.FirstEventTimeout))
			w.firstEvent = ceiling
		}
	}
	if w.firstEventOn && !explicit {
		w.firstEvent = max(w.firstEvent, effortFloor)
	}
	idleSet := strings.TrimSpace(cfg.GetString(codexconfig.EventTimeout)) != ""
	w.idle = idle
	if idleSet {
		w.idle = transport.Seconds(cfgFloat(cfg, codexconfig.EventTimeout))
	}
	w.idleOn = w.idle > 0
	if p.codex && floor > 0 && !idleSet {
		w.afterProgress, w.progress = true, firstProgress
	}
	return w
}

func cfgFloat(cfg *config.Config, key string) float64 {
	var f float64
	switch v := cfg.Get(key).(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case string:
		fmt.Sscanf(strings.TrimSpace(v), "%g", &f)
	}
	return f
}

var silentReject = regexp.MustCompile(`(?:^|[/\-_])gpt-5\.5(?:$|[\-_])`)

// What to try when the Codex backend shows its known silent rejection of a model: no events and no error
func hint(p *prepared) string {
	if !p.codex || !silentReject.MatchString(strings.ToLower(p.model)) {
		return ""
	}
	return fmt.Sprintf("OpenAI seems to be quietly refusing %s for this ChatGPT account, which has happened to some ChatGPT Plus accounts. "+
		"Switch with `/model gpt-5.4`, or add a fallback model in /settings under Agent, because each reply otherwise waits the whole silence "+
		"limit first. Some accounts can't use gpt-5.4-codex.", p.model)
}

// Error the consumer of the stream's events returned, which ends the stream as it is
type stopped struct{ err error }

func (e *stopped) Error() string { return e.err.Error() }

func (e *stopped) Unwrap() error { return e.err }

func emit(fn provider.StreamFunc, kind provider.EventKind, text string) error {
	if fn == nil {
		return nil
	}
	if err := fn(provider.StreamEvent{Kind: kind, Text: text}); err != nil {
		return &stopped{err}
	}
	return nil
}

// Request of one attempt, its body compressed for the official Codex backend and signed when the route signs
func (c *client) request(ctx context.Context, p *prepared) (*http.Request, []byte, error) {
	streamed := maps.Clone(p.body)
	streamed["stream"] = true
	payload, err := json.Marshal(streamed)
	if err != nil {
		return nil, nil, fmt.Errorf("The request to the provider could not be built: %w", err)
	}
	body := payload
	header := p.header.Clone()
	if p.compress && header.Get("Content-Encoding") == "" {
		if encoder, err := zstd.NewWriter(nil); err == nil {
			body = encoder.EncodeAll(payload, make([]byte, 0, len(payload)))
			encoder.Close()
			header.Set("Content-Encoding", "zstd")
		}
	}
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	call.Header = header
	if c.route.Sign != nil {
		if err := c.route.Sign(call, body); err != nil {
			return nil, nil, fmt.Errorf("The request to %s could not be signed: %w", c.route.Provider, err)
		}
	}
	return call, payload, nil
}

type frame struct {
	frame *transport.Frame
	err   error
}

// One physical attempt under the watch: the reply, the events it parsed, and the error that ended it
func (c *client) once(ctx context.Context, p *prepared, w watchdogs, attempt int, clocks *clocks, fn provider.StreamFunc) (*provider.Response, int,
	error) {
	timeouts := p.limits.Stream(ctx)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeouts.Configured {
		var stop context.CancelFunc
		attemptCtx, stop = context.WithTimeout(attemptCtx, timeouts.Request)
		defer stop()
	}
	start := time.Now()
	if attempt == 0 || w.afterProgress {
		clocks.attempt, clocks.event, clocks.progress = start, time.Time{}, time.Time{}
	}
	if attempt > 0 && w.afterProgress {
		clocks.retry = start
		slog.Info(fmt.Sprintf("Codex physical stream retry at %.3f (attempt=%d/%d, model=%s)", float64(start.UnixNano())/1e9, attempt+1,
			physicalRetries+1, p.model))
	}
	call, payload, err := c.request(attemptCtx, p)
	if err != nil {
		return nil, 0, err
	}
	_ = emit(fn, provider.EventActivity, "waiting for the provider to answer")
	type opened struct {
		response *http.Response
		err      error
	}
	result := make(chan opened, 1)
	go func() {
		response, err := c.r.pool.Client(p.settings, timeouts.Connect, false).Do(call)
		result <- opened{response, err}
	}()
	frames := make(chan frame, 32)
	var response *http.Response
	h := hint(p)
	with := func(message, separator string) string {
		if h != "" {
			return message + separator + h
		}
		return message
	}
	kill := func(status, message string) error {
		_ = emit(fn, provider.EventStatus, status)
		cancel()
		return &killed{&transport.TimeoutError{Phase: transport.PhaseRead, Err: errors.New(message)}}
	}
	a := newAssembler(p.model)
	a.text = func(text string) error { return emit(fn, provider.EventTextDelta, text) }
	a.reasoning = func(text string) error { return emit(fn, provider.EventReasoningDelta, text) }
	a.toolStart = func(name string) error {
		if original, ok := p.aliases[name]; ok {
			name = original
		}
		return emit(fn, provider.EventToolStart, name)
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	events := 0
	notice := &waiting{}
	for {
		select {
		case <-ctx.Done():
			return nil, events, ctx.Err()
		case <-attemptCtx.Done():
			if ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
				return nil, events, &killed{&transport.TimeoutError{Phase: transport.PhaseRead, Err: fmt.Errorf("The request took longer than its time limit of %s.", span(timeouts.Request))}}
			}
			return nil, events, attemptCtx.Err()
		case o := <-result:
			if o.err != nil {
				return nil, 0, &transport.SentError{URL: p.url, Bytes: len(payload), Err: transport.Timed(o.err, transport.PhaseConnect)}
			}
			response = o.response
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				data, _ := io.ReadAll(io.LimitReader(response.Body, transport.ErrorBodyLimit))
				return nil, 0, p.refused.HTTP(response.StatusCode, data, response.Header)
			}
			kind := strings.ToLower(response.Header.Get("Content-Type"))
			if strings.Contains(kind, "application/json") && !strings.Contains(kind, "event-stream") {
				data, err := io.ReadAll(response.Body)
				if err != nil {
					return nil, 0, err
				}
				var whole map[string]any
				if json.Unmarshal(data, &whole) != nil {
					return nil, 0, transport.FrameError(string(data), response.Header)
				}
				if object, ok := whole["error"].(map[string]any); ok && len(object) > 0 || whole["error"] != nil && !ok {
					return nil, 0, p.refused.HTTP(response.StatusCode, data, response.Header)
				}
				reply, err := c.finish(p, whole, response.Header)
				if err == nil {
					err = transport.Replay(reply, fn)
				}
				return reply, 1, err
			}
			go read(transport.NewEvents(response.Body), frames, attemptCtx)
		case now := <-ticker.C:
			if err := notice.poll(now, clocks, w, p, fn, attempt); err != nil {
				return nil, events, err
			}
			elapsed, attemptElapsed := now.Sub(clocks.call), now.Sub(clocks.attempt)
			switch {
			case w.firstEventOn && clocks.event.IsZero() && attemptElapsed > w.firstEvent:
				slog.Warn(fmt.Sprintf("Codex stream produced no parsed stream event within TTFB cutoff (%.0fs > %.0fs, model=%s). Backend accepted the "+
					"connection but sent no stream events. Killing connection so the retry loop can reconnect.", attemptElapsed.Seconds(),
					w.firstEvent.Seconds(), p.model))
				return nil, events, kill(with(fmt.Sprintf("%s sent nothing in its first %s. Connecting again.", p.model, span(attemptElapsed)),
					"\n"), with(fmt.Sprintf("%s sent nothing in its first %s, past the first output wait of %s. Raise First output wait in "+
					"/settings under Connections › OpenAI responses API if it needs longer.", p.model, span(attemptElapsed), span(w.firstEvent)), "\n"))
			case w.progress > 0 && !clocks.event.IsZero() && clocks.progress.IsZero() && attemptElapsed > w.progress:
				slog.Warn(fmt.Sprintf("Codex stream produced lifecycle events but no substantive model progress for %.0fs (threshold %.0fs, model=%s, "+
					"context=~%s tokens). Reconnecting.", attemptElapsed.Seconds(), w.progress.Seconds(), p.model, strs.Thousands(w.tokens)))
				return nil, events, kill(fmt.Sprintf("The reply from %s opened but made no progress for %s. Connecting again.", p.model,
					span(attemptElapsed)), fmt.Sprintf("The reply from %s made no progress for %s, past the limit of %s.", p.model, span(attemptElapsed),
					span(w.progress)))
			case w.idleOn && !clocks.event.IsZero() && (!w.afterProgress || !clocks.progress.IsZero()) && now.Sub(clocks.event) > w.idle:
				idle := now.Sub(clocks.event)
				point := "it began"
				if w.afterProgress {
					point = "the model began writing"
				}
				slog.Warn(fmt.Sprintf("Codex stream produced no SSE events for %.0fs after %s (threshold %.0fs, model=%s, context=~%s tokens). Killing "+
					"connection so the retry loop can reconnect.", idle.Seconds(), point, w.idle.Seconds(), p.model, strs.Thousands(w.tokens)))
				return nil, events, kill(fmt.Sprintf("The reply from %s went quiet for %s after %s. Connecting again.", p.model, span(idle), point),
					fmt.Sprintf("The reply from %s went quiet for %s after %s, past the silence limit of %s. Raise Silence limit in /settings under "+
						"Connections › OpenAI responses API if it needs longer.", p.model, span(idle), point, span(w.idle)))
			case w.stale != transport.Forever && elapsed > w.stale:
				slog.Warn(fmt.Sprintf("Non-streaming API call stale for %.0fs (threshold %.0fs). model=%s context=~%s tokens. Killing connection.",
					elapsed.Seconds(), w.stale.Seconds(), p.model, strs.Thousands(w.tokens)))
				status := "The request is stopped."
				if h != "" {
					status = h
				}
				_ = emit(fn, provider.EventStatus, fmt.Sprintf("%s sent nothing for %s. %s", p.model, span(elapsed), status))
				cancel()
				return nil, events, &killed{&transport.TimeoutError{Phase: transport.PhaseRead, Err: &transport.StallError{Silence: elapsed,
					Model: p.model, Limit: w.stale}}}
			}
		case f := <-frames:
			if f.err != nil {
				if !errors.Is(f.err, io.EOF) {
					return nil, events, f.err
				}
				assembled, err := a.result()
				if err != nil {
					return nil, events, err
				}
				reply, err := c.finish(p, assembled, response.Header)
				return reply, events, err
			}
			data := strings.TrimSpace(f.frame.Data)
			if data == "" {
				if f.frame.Event != "" {
					continue
				}
				return nil, events, transport.FrameError("", response.Header)
			}
			if data == "[DONE]" {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(data), &event) != nil || event == nil {
				return nil, events, transport.FrameError(data, response.Header)
			}
			if _, ok := event["type"]; !ok && f.frame.Event != "" {
				event["type"] = f.frame.Event
			}
			now := time.Now()
			if clocks.event.IsZero() {
				slog.Info(fmt.Sprintf("Codex stream first parsed event at %.3f (attempt=%d/%d, model=%s)", float64(now.UnixNano())/1e9, attempt+1,
					physicalRetries+1, p.model))
			}
			clocks.event = now
			events++
			if progress(event) {
				if clocks.progress.IsZero() {
					slog.Info(fmt.Sprintf("Codex stream first substantive progress at %.3f (attempt=%d/%d, model=%s)", float64(now.UnixNano())/1e9,
						attempt+1, physicalRetries+1, p.model))
				}
				clocks.progress = now
				if w.afterProgress {
					clocks.retry = time.Time{}
				}
			}
			_ = emit(fn, provider.EventActivity, "receiving the reply")
			done, err := a.feed(event)
			if err != nil {
				return nil, events, err
			}
			if done {
				drain(frames, p, c.r.config)
				cancel()
				assembled, err := a.result()
				if err != nil {
					return nil, events, err
				}
				if state, _ := assembled["status"].(string); state == "incomplete" || state == "failed" {
					slog.Warn(fmt.Sprintf("Codex Responses stream terminal status=%s (incomplete_details=%v, error=%v, streamed_chars=%d).", state,
						assembled["incomplete_details"], assembled["error"], len(fmt.Sprint(assembled["output_text"]))))
				}
				reply, err := c.finish(p, assembled, response.Header)
				return reply, events, err
			}
		}
	}
}

// Reads a stream's frames until it ends or the attempt is cut
func read(events *transport.Events, frames chan<- frame, ctx context.Context) {
	for {
		f, err := events.Next()
		select {
		case frames <- frame{f, err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// Waits for a stream that gave its final answer to close, as long as transports.stream_drain_timeout allows, so a relay that holds it open
// can't wedge the turn
func drain(frames <-chan frame, p *prepared, fallback *config.Config) {
	cfg := p.cfg
	if cfg == nil {
		cfg = fallback
	}
	budget := transport.Seconds(math.Max(cfgFloat(cfg, transportconfig.StreamDrainTimeout), 0))
	if budget <= 0 {
		return
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		select {
		case f := <-frames:
			if f.err != nil {
				return
			}
		case <-timer.C:
			slog.Warn(fmt.Sprintf("Codex Responses stream remained open %.1fs after a terminal response (%s); shut down its socket. model=%s",
				budget.Seconds(), transportconfig.StreamDrainTimeout, p.model))
			return
		}
	}
}

// What the person was last shown of a silent call
type waiting struct {
	beat         time.Time
	phase, label string
	labelled     bool
	near, shown  bool
}

// Liveness and wait lines of a silent call: a heartbeat every thirty seconds, a line naming what it waits on after a minute, cleared once
// the model makes progress
func (n *waiting) poll(now time.Time, clocks *clocks, w watchdogs, p *prepared, fn provider.StreamFunc, attempt int) error {
	last := clocks.attempt
	if !clocks.event.IsZero() {
		last = clocks.event
	}
	if n.shown && !clocks.progress.IsZero() && clocks.progress.After(n.beat) {
		n.shown = false
		if err := emit(fn, provider.EventWait, ""); err != nil {
			return err
		}
	}
	if !n.beat.IsZero() && now.Sub(n.beat) < 30*time.Second {
		return nil
	}
	n.beat = now
	silence := now.Sub(last)
	if silence < time.Minute {
		return emit(fn, provider.EventActivity, fmt.Sprintf("waiting for the provider to answer, %s so far", span(silence)))
	}
	phase := "post_event"
	switch {
	case clocks.event.IsZero() && attempt > 0:
		phase = "reconnect"
	case clocks.event.IsZero():
		phase = "first_event"
	case clocks.progress.IsZero():
		phase = "pre_progress"
	}
	var deadline *transport.Deadline
	if w.stale != transport.Forever {
		deadline = &transport.Deadline{Label: "wall-clock stale", Remaining: w.stale - now.Sub(clocks.call)}
	}
	near := deadline != nil && deadline.Remaining <= 15*time.Second
	label := ""
	if deadline != nil {
		label = deadline.Label
	}
	if n.phase == phase && n.labelled == (deadline != nil) && n.label == label && !(near && !n.near) {
		return emit(fn, provider.EventActivity, fmt.Sprintf("waiting for the provider to answer, %s so far", span(silence)))
	}
	n.phase, n.label, n.labelled, n.shown = phase, label, deadline != nil, true
	n.near = n.near || near
	return emit(fn, provider.EventWait, transport.WaitText(p.model, silence, phase, deadline))
}

// Reply of a Responses object, its tool calls sent on as they end when nothing streamed them
func (c *client) finish(p *prepared, response map[string]any, header http.Header) (*provider.Response, error) {
	r, err := normalize(p, response, p.issuer, p.model)
	if err != nil {
		return nil, p.refused.Empty(err.Error(), err)
	}
	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: r.text, Reasoning: r.reasoning, ToolCalls: r.calls, FinishReason: r.finish}
	if len(r.kept.Reasoning) > 0 || len(r.kept.Messages) > 0 || r.kept.Phase != "" {
		if raw, err := json.Marshal(r.kept); err == nil {
			message.Carriers = map[string][]byte{carrier: raw}
		}
	}
	reply := &provider.Response{Message: message, FinishReason: r.finish, Header: header}
	if usage, ok := response["usage"].(map[string]any); ok {
		reply.Usage = usageOf(usage, response["service_tier"])
	}
	return reply, nil
}

// Tokens of a Responses usage object, the cached and written input taken out of the input
func usageOf(u map[string]any, tier any) *provider.Usage {
	count := func(paths ...[]string) int64 {
		for _, path := range paths {
			var v any = u
			for _, key := range path {
				m, _ := v.(map[string]any)
				v = m[key]
			}
			if n, ok := v.(float64); ok && n > 0 {
				return int64(n)
			}
		}
		return 0
	}
	input := count([]string{"input_tokens"})
	read := count([]string{"input_tokens_details", "cached_tokens"})
	write := count([]string{"input_tokens_details", "cache_write_tokens"}, []string{"input_tokens_details", "cache_creation_tokens"})
	tierName, _ := tier.(string)
	return &provider.Usage{Input: max(input-read-write, 0), CacheRead: read, CacheWrite: write, Output: count([]string{"output_tokens"}),
		Reasoning: count([]string{"output_tokens_details", "reasoning_tokens"}, []string{"completion_tokens_details", "reasoning_tokens"}),
		Tier:      strings.ToLower(tierName)}
}

// Length of time in words, rounded to whole seconds
func span(d time.Duration) string {
	return strs.Span(max(d.Round(time.Second), time.Second))
}
