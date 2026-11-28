package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Message waiting its turn
type queued struct {
	// What the model is sent
	text string
	// What the user typed, which the session's title and transcript show in its place; the text when empty
	display string
	// Compression the user asked for, run in place of a turn; nil for a message
	compact *compactJob
	// Images the message hands the model, such as those a subagent's task must see
	media []*sessionsv1.Media
	// A notice the session was given, such as a background result, rather than a message the person sent
	notice bool
	// The person spoke the message in voice mode, so its turn runs on the voice chat model
	voice bool
}

// Notices appended once to the last tool result before the next model call
const (
	budgetWarning = "[SYSTEM NOTICE — iteration budget checkpoint] You have used %d of %d iterations. Checkpoint durable progress now, then " +
		"continue the task; do not stop solely because of this warning."
	runBudgetWrapup = "[SYSTEM NOTICE — run time budget nearly exhausted] Run time budget nearly exhausted. Stop new discovery/verification work " +
		"now. Produce the required final deliverable (answer/JSON/summary) from the state you already have, completing only mandatory writes."
	// Asked of the model once the step limit is reached, and given when it can't answer
	summaryRequest = "You've reached the maximum number of tool-calling iterations allowed. Please provide a final response summarizing what " +
		"you've found and accomplished so far, without calling any more tools."
	emptySummary = "I reached my step limit for this reply and couldn't sum up my work. Send “continue” to keep going."
	// Told the model with the problems a language server found in its edits after their results had returned, before it gives its answer
	lateVerdicts = "[System note: the language server reported problems in files you edited this turn, after those tool results had returned. " +
		"Fix them if they are real, then give your final answer.]"
)

const (
	// Times a turn goes on to answer late language server verdicts, and how long it waits for those still coming before it ends
	lateVerdictRounds = 3
	lateVerdictWait   = 5 * time.Second
	// Finish reason of an answer kept as interim while the turn goes on to check its work
	verificationRequired = "verification_required"
	// Finish reason of an answer kept as interim while a hook sends the turn back to check its work
	verifyHookContinue = "verify_hook_continue"
	// Times a turn is sent back to check edits that lack fresh passing evidence
	verifyStopAttempts = 2
)

// Blocks of thinking some models put before their answer
var thinkBlocks = regexp.MustCompile(`(?s)<think>.*?</think>`)

// One turn: the message stored, then model calls and their tool calls in turn until the model answers without tools, the step limit is
// reached, the guardrails stop a loop or the user stops the reply. The tools offered are pinned when the turn starts, so they stay the
// same for all of its calls
func (a *Agent) turn(ctx context.Context, r *run, q queued) (err error) {
	store, err := a.store()
	if err != nil {
		return err
	}
	session, err := store.Get(ctx, r.sessionID())
	if err != nil {
		return err
	}
	a.restore(r)
	settings := a.settings(r)
	surface := platformOf(session)
	// Whatever fires inside the turn names it; its hooks hear how it ended, however it ended
	ctx, th := a.beginTurn(ctx, r, settings, session, surface, q.text)
	defer func() { a.endTurn(ctx, r, settings, th, err) }()
	env := &tools.Env{Cwd: session.GetDirectory(), Config: settings, OneShot: r.oneshot, Client: r.shell(), Platform: surface}
	if a.terminal != nil {
		env.Visible = func(host string) string { return a.terminal.Visible(settings, host) }
	}
	finite := r.finite.Load()
	asking := approvals.Context{Session: r.sessionID(), Config: settings, Source: session.GetSource(), Finite: finite, Activity: func(text string) {
		if err := store.Update(a.ctx, r.sessionID(), sessions.Change{Activity: &text}); err != nil {
			slog.Debug("Could not record what the session waits on", "session", r.sessionID(), "error", err)
		}
	}}
	env.Clarify = func(ctx context.Context, questions []tools.Question) (tools.Reply, error) {
		return a.clarify(ctx, r, questions)
	}
	r.yield.Store(false)
	if !finite {
		// A turn its client waits for alone has nobody to hear of work that ends after it, which so stays inside it
		env.Notify = func(notice tools.Notice) error { return a.deliver(r, notice) }
	}
	env.Yield = func() bool { return r.yield.Swap(false) }
	// What the tools keep for one turn only, such as a browser, is let go however the turn ends, under the session it began on too when a
	// compression moved the conversation on
	began := r.sessionID()
	defer func() {
		a.tools.Ended(env, r.sessionID())
		if now := r.sessionID(); now != began {
			a.tools.Ended(env, began)
		}
	}()
	c := r.subagent()
	if c != nil {
		// A subagent's tools work for the session that delegated, whose surface they see; nobody answers its questions or prompts, and its
		// background news go to the parent as the settings allow
		env.Platform, env.Delegated, env.Clarify, env.Notify, env.Yield = c.platform, c.delegated(), nil, c.notice, nil
		asking.Answer = c.answer
	}
	bind := func(route provider.Route) {
		env.Aux = a.helpers(r, settings, route)
		asking.Aux = env.Aux
		a.approvals.Bind(env, asking)
	}
	// The message is stored whatever becomes of the turn, so it can be sent again once what stopped it is fixed; the note of skills that
	// changed goes with it when the prompt could be built
	stored := false
	keep := func(note string) error {
		if q.text == "" || stored {
			return nil
		}
		stored = true
		message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: cmp.Or(q.display, q.text), Media: q.media}
		if sent := strings.TrimSpace(q.text + "\n\n" + note); sent != message.Text {
			message.Sent = sent
		}
		return a.append(store, r, message)
	}
	instance, route, err := a.current(r)
	if err == nil && route.Model == "" {
		err = fmt.Errorf("No model is set for %s. Pick one with /model.", route.Provider)
	}
	if err != nil {
		return errors.Join(keep(""), err)
	}
	bind(route)
	th.model = route.Model
	// The prompt is built for the main model, a fallback taking its place named in it as each call goes
	primaryInstance, primary, err := a.route(r)
	if err != nil {
		primaryInstance, primary = instance, route
	}
	focus := agentprompt.Focused(settings, surface, env.Cwd)
	offered := a.offered(env, focus, c)
	definitions := tools.Definitions(offered, env)
	byName := map[string]tools.Def{}
	for _, def := range offered {
		byName[def.Name] = def
	}
	shape := shaping{surface: surface, focus: focus}
	built, penv, err := a.system(ctx, r, settings, session, primaryInstance, primary, definitions, shape)
	if err != nil {
		return errors.Join(keep(""), err)
	}
	note := a.skillChanges(ctx, r, store, penv, built.text)
	if q.text != "" {
		history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
		if err != nil {
			return errors.Join(keep(""), err)
		}
		block, extra := a.preLLM(ctx, r, settings, th, history, route.Model)
		if block != "" {
			// A message a hook refused is kept withheld, and the turn ends with the hook's reason
			stored = true
			if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: withheldPrompt}); err != nil {
				return err
			}
			th.exit = blockedExit
			return a.answer(ctx, store, r, settings, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: block})
		}
		// What the hooks give the turn, the newest change of the profile's Bot Desktop, what a language server found late in edits of an
		// earlier turn, then the skills that changed go with the message
		notes := append(append([]string{extra, r.screenNote()}, a.tools.Notes(ctx, r.sessionID(), time.Time{})...), note)
		note = strings.Join(slices.DeleteFunc(notes, func(n string) bool { return strings.TrimSpace(n) == "" }), "\n\n")
	}
	if err := keep(note); err != nil {
		return err
	}
	if q.text != "" && c == nil {
		a.title(ctx, r, store, env.Aux, route, cmp.Or(q.display, q.text))
	}
	// A turn the person spoke runs on the voice chat model and effort, the session's own route put back however it ends
	if q.voice {
		a.speakTurn(ctx, r, settings, env.Aux, route, func(voice provider.Route) int {
			history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
			if err != nil {
				return 0
			}
			return requestTokens(noted(built.messages(voice, primary), history), sent(history), definitions)
		})
		defer a.endVoice(r)
	}
	guard := newGuardrails(guardLimits(settings, surface), byName)
	batch, single := toolLimits(settings)
	limit := settings.GetInt(agentconfig.MaxTurns)
	ratio := cast.ToFloat64(settings.Get(agentconfig.BudgetWarning))
	budget := time.Duration(max(cast.ToFloat64(settings.Get(agentconfig.RunBudget)), 0) * float64(time.Second))
	started := time.Now()
	var deadline time.Time
	if budget > 0 {
		deadline = started.Add(budget)
	}
	// The first request of a turn the person started says so, the provider counting it as theirs
	personal := c == nil && !q.notice
	warned, wrapped := false, false
	// Times the turn went on to answer late language server verdicts, to check its edits and for the hooks that check them, and the answer
	// it held back meanwhile
	verdicts, nudges, hooked := 0, 0, 0
	interim := ""
	r.mu.Lock()
	r.edited = nil
	r.mu.Unlock()
	searching := a.searching(settings, byName)
	var strike vetted
	r.begin()
	a.sweep(settings)
	// How long the session lay idle before this turn, for the compaction of a session resumed after a pause
	idle := time.Duration(0)
	if updated := session.GetUpdated(); updated != nil {
		idle = time.Since(updated.AsTime())
	}
	fresh := true
	for step := 0; ; step++ {
		if limit > 0 && step >= limit {
			if interim != "" {
				// The answer held back while the turn checked its work stands as the reply
				th.final = cmp.Or(th.final, interim)
				return nil
			}
			if c != nil {
				c.mu.Lock()
				c.exit = "max_iterations"
				c.mu.Unlock()
			}
			return a.summarize(ctx, r, store, settings, built, definitions, step, limit, searching, deadline, session.GetSource())
		}
		history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
		if err != nil {
			return err
		}
		instance, route, err := a.current(r)
		if err != nil {
			return err
		}
		bind(route)
		th.model = route.Model
		// The session keeps its own route through a turn the voice chat model runs
		own, _ := a.own(r)
		a.recordRoute(ctx, r, own.Model, own.Provider, own.BaseURL)
		cenv := a.contextEnv(ctx, r, settings, instance, route, env.Aux)
		engine := a.engine(r, cenv)
		env.Window = cenv.Window
		if r.rebuilt() {
			if session, err = store.Get(ctx, r.sessionID()); err != nil {
				return err
			}
			if built, _, err = a.system(ctx, r, settings, session, primaryInstance, primary, definitions, shape); err != nil {
				return err
			}
		}
		messages := r.outgoing(sent(env.Aux.Shown(ctx, history, settings.GetInt(agentconfig.ReplayImages))))
		if engine != nil {
			messages = selected(r, engine, messages, history, cenv.Window)
		}
		head := noted(built.messages(route, primary), history)
		a.restoreAnchor(ctx, r, history)
		tokens, anchored := r.pressure(history, head, messages, definitions)
		if engine != nil {
			measure := func() int {
				current, err := store.Messages(ctx, r.sessionID(), sessions.Model)
				if err != nil {
					return tokens
				}
				return requestTokens(noted(built.messages(route, primary), current), sent(current), definitions)
			}
			var next phase
			reply := ""
			if fresh {
				fresh = false
				next, err = a.turnStart(ctx, r, settings, cenv, engine, idle, messages, tokens, anchored, measure)
			} else {
				next, reply, err = a.preAPI(ctx, r, settings, cenv, engine, messages, tokens, anchored)
			}
			switch {
			case err != nil:
				return err
			case next == finish:
				return a.ending(r, reply)
			case next == anew:
				step--
				continue
			}
		}
		a.settle(r, settings)
		response, route, err := a.ask(ctx, r, settings, instance, route, func(instance provider.Instance, route provider.Route) *provider.Request {
			return &provider.Request{
				Model:              route.Model,
				Messages:           append(noted(built.messages(route, primary), history), messages...),
				Tools:              definitions,
				MaxTokens:          r.outputCap(instance.Spec.DefaultMaxTokens),
				Reasoning:          provider.Reasoning{Effort: a.effort(r)},
				SessionID:          r.sessionID(),
				StaticSystemPrefix: built.static,
				WebSearch:          searching,
				Extra:              a.nativeFields(r, settings, route, engine),
				Config:             settings,
				Deadline:           deadline,
				UserTurn:           personal,
				Source:             session.GetSource(),
			}
		})
		personal = false
		if err != nil {
			if ctx.Err() != nil || engine == nil {
				return err
			}
			outcome, reply, rerr := a.recover(ctx, r, settings, cenv, engine, err, messages, tokens)
			switch {
			case rerr != nil:
				return rerr
			case outcome == retried:
				a.anchor(r, nil)
				step--
				continue
			case outcome == ended:
				return a.ending(r, reply)
			}
			return err
		}
		a.observe(r, engine, response.Usage, history)
		c.answered()
		reply := response.Message
		th.finish, th.model = response.FinishReason, route.Model
		switch response.FinishReason {
		case "length", "content_filter":
			if len(reply.GetToolCalls()) == 0 || response.FinishReason == "content_filter" {
				reply.ToolCalls = nil
				if reply.GetText() != "" || reply.GetReasoning() != "" {
					if err := a.append(store, r, reply); err != nil {
						return err
					}
				}
				if response.FinishReason == "length" {
					return errors.New("The model reached its length limit and ended the reply early. Send “continue” to have it go on.")
				}
				return errors.New("The provider's safety filter ended the reply early. Rephrase your message, or pick another model with /model.")
			}
		}
		if len(reply.GetToolCalls()) > 0 {
			reply.ToolCalls = capDelegations(deduplicate(reply.GetToolCalls()), maxChildren(settings))
			vetting, results, reason := a.vet(r, reply, history, byName, response.FinishReason, &strike)
			switch vetting {
			case again:
				r.restart()
				continue
			case halted:
				reply.ToolCalls = nil
				if reply.GetText() != "" || reply.GetReasoning() != "" {
					if err := a.append(store, r, reply); err != nil {
						return err
					}
				}
				return errors.New(reason)
			case answered:
				a.interim(ctx, settings, th, route, reply)
				if err := a.append(store, r, reply); err != nil {
					return err
				}
				for _, call := range reply.GetToolCalls() {
					if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: call.GetId(), ToolName: call.GetName(),
						Text: results[call.GetId()], Failed: true}); err != nil {
						return err
					}
				}
				continue
			}
		}
		if len(reply.GetToolCalls()) == 0 {
			if gate, reason := a.stopGate(ctx, r, env, settings, reply.GetText(), &verdicts, &nudges, &hooked); gate != "" {
				reply.FinishReason = reason
				if th.held {
					// A held turn shows no answer the hooks did not rewrite, the one it holds back meanwhile included
					a.transform(ctx, settings, reply)
				}
				if err := a.append(store, r, reply); err != nil {
					return err
				}
				if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: gate, Display: "hidden"}); err != nil {
					return err
				}
				interim = cmp.Or(strings.TrimSpace(reply.GetText()), interim)
				continue
			}
			if err := a.answer(ctx, store, r, settings, reply); err != nil {
				return err
			}
			if engine != nil {
				return a.finished(ctx, r, settings, cenv, engine)
			}
			return nil
		}
		a.interim(ctx, settings, th, route, reply)
		if err := a.append(store, r, reply); err != nil {
			return err
		}
		used := step + 1
		x := &executor{a: a, r: r, env: env, tools: byName, guard: guard, batch: batch, single: single,
			commit: func(message *sessionsv1.Message) error { return a.append(store, r, message) },
			tail: func() string {
				// What a language server found in the edits of the batch after their results returned goes first
				notes := a.tools.Notes(ctx, r.sessionID(), time.Time{})
				if budget > 0 && !wrapped && time.Since(started) >= budget*4/5 {
					wrapped = true
					slog.Info("Run budget wrap-up notice injected", "session", r.sessionID(), "budget", budget, "elapsed", time.Since(started).Round(time.Second))
					notes = append(notes, runBudgetWrapup)
				}
				if ratio > 0 && ratio < 1 && limit > 1 && !warned && float64(used) >= math.Min(ratio*float64(limit), float64(limit-1)) {
					warned = true
					notes = append(notes, fmt.Sprintf(budgetWarning, used, limit))
				}
				return strings.Join(notes, "\n\n")
			}}
		// Each project the calls change is snapshotted again before this reply's calls change it
		a.tools.Stepped(r.sessionID())
		// The calls name the model request that made them
		if err := x.run(th.bind(ctx), reply.GetToolCalls()); err != nil {
			return err
		}
		if err := a.steered(store, r); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if halt := guard.halt; halt != nil {
			a.tell(r, haltNotice(*halt))
			th.exit = "guardrail_halt"
			return a.answer(ctx, store, r, settings, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: haltReply(*halt)})
		}
		if engine != nil {
			current, err := store.Messages(ctx, r.sessionID(), sessions.Model)
			if err != nil {
				return err
			}
			engine.Prepare(a.ctx, cenv, current, "tool-batch")
			pressure, _ := r.pressure(current, noted(built.messages(route, primary), current), sent(current), definitions)
			if reply, err := a.postTool(ctx, r, settings, cenv, engine, pressure); err != nil {
				return err
			} else if reply != "" {
				return a.ending(r, reply)
			}
		}
	}
}

// Asks the model, retrying where that can help; a failure retries cannot fix, or that outlasts them, moves the session to the next model
// of its fallback chain and asks that one. Gives the reply and the route that gave it
func (a *Agent) ask(ctx context.Context, r *run, settings *config.Config, instance provider.Instance, route provider.Route,
	build func(provider.Instance, provider.Route) *provider.Request) (*provider.Response, provider.Route, error) {
	// A route refusing the reasoning asked for is asked once more without it
	reasoned := false
	for {
		client, err := a.transports.Client(route)
		if err != nil {
			return nil, route, err
		}
		started := time.Now()
		response, exhausted, err := a.complete(ctx, r, client, build(instance, route))
		if err == nil {
			a.record(r, route, "", response.Usage, time.Since(started))
			a.capture(r, route, response.Header)
			return response, route, nil
		}
		if ctx.Err() != nil {
			return nil, route, err
		}
		if !reasoned && a.refuseReasoning(r, route, err) {
			reasoned = true
			continue
		}
		reason := fallbackReason(err, exhausted)
		if reason == "" {
			return nil, route, err
		}
		if len(a.chain(settings)) > 0 {
			a.tell(r, cmp.Or(failureLabel(err, exhausted), "The provider failed")+". Trying a fallback model.")
		}
		if !a.failover(r, settings, route, err, reason) {
			if reason == billingReason {
				return nil, route, a.billing(route, err)
			}
			return nil, route, err
		}
		r.restart()
		if instance, route, err = a.current(r); err != nil {
			return nil, route, err
		}
	}
}

// What the turn says before it falls back: that retries ran out, or what refused the request
func failureLabel(err error, exhausted bool) string {
	failure, ok := errors.AsType[*provider.APIError](err)
	switch {
	case !ok:
		return ""
	case exhausted:
		return "The provider still failed after every retry"
	case failure.StatusCode > 0:
		return fmt.Sprintf("The provider refused the request with HTTP status %d", failure.StatusCode)
	}
	return "The provider refused the request"
}

// Streams one reply, retried after a failure retrying can fix while nothing of it streamed; whether the failure outlasted every retry. The
// request hooks hear each attempt, the stream observers each piece, and the plugins that classify failures each failure
func (a *Agent) complete(ctx context.Context, r *run, client provider.Client, request *provider.Request) (*provider.Response, bool, error) {
	settings := a.settings(r)
	retries := settings.GetInt(agentconfig.APIMaxRetries)
	route := client.Route()
	th := turningOf(ctx)
	th.calls++
	th.request = fmt.Sprintf("%s:api:%d", th.ID, th.calls)
	request.Classify = a.classifier(ctx, settings, route, request)
	observed := a.hooks.Has(settings, hooks.StreamStart) || a.hooks.Has(settings, hooks.StreamDelta) || a.hooks.Has(settings, hooks.StreamEnd)
	reasoning := a.hooks.StreamsReasoning(settings)
	base := th.stream(route, request.Model)
	for attempt := 0; ; attempt++ {
		streamed := false
		started := time.Now()
		var first *time.Time
		a.preRequest(ctx, settings, th, route, request, attempt, started)
		if observed {
			a.hooks.Stream(ctx, settings, hooks.Started{Stream: base})
		}
		th.calling = true
		response, err := client.Stream(ctx, request, func(event provider.StreamEvent) error {
			// Only part of the reply counts as streamed: a line about the request leaves it free to be sent again
			streamed = streamed || event.Delivers()
			if event.Delivers() && first == nil {
				now := time.Now()
				first = &now
			}
			switch event.Kind {
			case provider.EventTextDelta:
				if !th.held {
					r.delta(event.Text, "")
				}
				if observed {
					a.hooks.Stream(ctx, settings, hooks.Delta{Stream: base, Text: event.Text, Kind: "text"})
				}
			case provider.EventReasoningDelta:
				r.delta("", event.Text)
				if reasoning {
					a.hooks.Stream(ctx, settings, hooks.Delta{Stream: base, Text: event.Text, Kind: "reasoning"})
				}
			case provider.EventStatus:
				if text := strings.TrimSpace(event.Text); text != "" {
					a.tell(r, text)
				}
			case provider.EventWait:
				r.waiting(event.Text)
			case provider.EventActivity:
				r.touch(event.Text)
			}
			return nil
		})
		// A stop that cut the request short is one during it
		th.calling = err != nil && ctx.Err() != nil
		if err == nil {
			if observed {
				a.hooks.Stream(ctx, settings, hooks.Ended{Stream: base, Text: response.Message.GetText(), Finished: true})
			}
			a.postRequest(ctx, settings, th, route, request, response, started, first)
			return response, false, nil
		}
		if observed {
			a.hooks.Stream(ctx, settings, hooks.Ended{Stream: base, Error: hooks.Optional(err.Error())})
		}
		a.requestFailed(ctx, settings, th, route, request, err, attempt, started)
		failure, ok := errors.AsType[*provider.APIError](err)
		if !ok || !failure.Retryable() || streamed {
			return nil, false, err
		}
		if attempt >= retries {
			return nil, true, err
		}
		// The wait shows in place of the request's line while it lasts
		wait := retryWait(failure, attempt)
		reason := "The request to the provider failed."
		if failure.Kind == provider.ErrRateLimit {
			reason = "The provider is limiting requests."
		}
		r.waiting(fmt.Sprintf("%s Trying again in %s, retry %d of %d.", reason, strs.Span(max(wait.Round(time.Second), time.Second)), attempt+1, retries))
		slog.Warn(fmt.Sprintf("Retrying API call in %ss (attempt %d/%d) provider=%s model=%s error=%s", strconv.FormatFloat(wait.Seconds(), 'f', -1, 64),
			attempt+1, retries, route.Provider, request.Model, err))
		select {
		case <-ctx.Done():
			r.waiting("")
			return nil, false, ctx.Err()
		case <-time.After(wait):
		}
		r.waiting("")
	}
}

// Longest wait a provider's Retry-After is followed for
const retryAfterCap = 600 * time.Second

// Wait before the next try of a failed request: what the provider asked, within the cap, else two seconds doubling with each try up to a
// minute, with up to half of it again at random so retries of many sessions spread out
func retryWait(failure *provider.APIError, attempt int) time.Duration {
	if failure.RetryAfter > 0 {
		return min(failure.RetryAfter, retryAfterCap)
	}
	delay := min(2*time.Second<<min(attempt, 10), time.Minute)
	return delay + time.Duration(rand.Float64()*0.5*float64(delay))
}

// The model's last word at the step limit: asked to sum up what it did without calling more tools, its answer stored as the turn's reply
func (a *Agent) summarize(ctx context.Context, r *run, store sessions.Store, settings *config.Config, built *system, definitions []map[string]any,
	steps, limit int, searching string, deadline time.Time, source string) error {
	a.tell(r, fmt.Sprintf("The agent used all %s it may take for one reply, so it is asked to sum up its work.", strs.Count(limit, "step")))
	if th := turningOf(ctx); th != nil {
		th.exit = "budget_exhausted"
	}
	if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: summaryRequest, Display: "hidden"}); err != nil {
		return err
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return err
	}
	instance, route, err := a.current(r)
	if err != nil {
		return err
	}
	_, primary, err := a.route(r)
	if err != nil {
		primary = route
	}
	text := ""
	for range 2 {
		response, _, err := a.ask(ctx, r, settings, instance, route, func(instance provider.Instance, route provider.Route) *provider.Request {
			return &provider.Request{Model: route.Model, Messages: append(built.messages(route, primary), sent(history)...), Tools: definitions,
				MaxTokens: instance.Spec.DefaultMaxTokens, Reasoning: provider.Reasoning{Effort: a.effort(r)}, SessionID: r.sessionID(),
				StaticSystemPrefix: built.static, WebSearch: searching, Config: settings, Deadline: deadline, Source: source}
		})
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			slog.Warn("Failed to get summary response", "session", r.sessionID(), "error", err)
			text = fmt.Sprintf("I used all %s I may take for one reply before finishing, and couldn't sum up my work. Send “continue” to keep "+
				"going, or raise Max steps with /settings max steps.", strs.Count(limit, "step"))
			break
		}
		if text = strings.TrimSpace(thinkBlocks.ReplaceAllString(response.Message.GetText(), "")); text != "" {
			break
		}
	}
	return a.answer(ctx, store, r, settings, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: cmp.Or(text, emptySummary)})
}

// Messages as the model is sent them: a message's text in place of what it shows when the two differ
func sent(messages []*sessionsv1.Message) []*sessionsv1.Message {
	out := make([]*sessionsv1.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.GetSent() != "" && m.GetSent() != m.GetText() {
			copied := *m
			copied.Text = m.GetSent()
			out[i] = &copied
		}
	}
	return out
}

// Tools of the turn: every tool offered to the session, those that have nothing to do with code left out in the coding focus posture, and
// for a subagent those no subagent is offered
func (a *Agent) offered(env *tools.Env, focus bool, c *child) []tools.Def {
	var out []tools.Def
	for _, def := range a.tools.Offered(env) {
		if focus && def.Unfocused || c != nil && !c.offers(def) {
			continue
		}
		out = append(out, def)
	}
	return out
}

// Calls of a reply with those of delegate_task past the subagents one call may start dropped, the others kept in their order
func capDelegations(calls []*sessionsv1.ToolCall, limit int) []*sessionsv1.ToolCall {
	count := 0
	for _, call := range calls {
		if call.GetName() == "delegate_task" {
			count++
		}
	}
	if count <= limit {
		return calls
	}
	kept, out := 0, make([]*sessionsv1.ToolCall, 0, len(calls))
	for _, call := range calls {
		if call.GetName() == "delegate_task" {
			if kept >= limit {
				continue
			}
			kept++
		}
		out = append(out, call)
	}
	slog.Warn(fmt.Sprintf("Truncated %d excess delegate_task call(s) to enforce max_concurrent_children=%d limit", count-limit, limit))
	return out
}

// Delivers the guidance steered into the running turn as a message of its own right after the tool results it waited for
func (a *Agent) steered(store sessions.Store, r *run) error {
	r.mu.Lock()
	pending := r.steering
	r.steering = nil
	r.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	text := strings.Join(pending, "\n")
	slog.Info(fmt.Sprintf("Delivered /steer to agent after tool batch (%d chars) as new user message: %s", len([]rune(text)), preview(text)))
	return a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: text, Sent: steerMarker(text), Display: "steer"})
}

// Guidance as the model is sent it, inside the marks the prompt names as the only text mid turn that is the user's own
func steerMarker(text string) string {
	return agentprompt.SteerOpen + "\n" + text + "\n" + agentprompt.SteerClose
}

// What keeps the turn going when the model answers without calls, empty to end it, and the finish reason of the answer it holds back: the
// problems a language server found late in the turn's edits, waited for a few seconds, three times at most; else, the turn having changed
// code with no fresh passing check of it, the nudge to check it first, twice at most; else a hook's own reason to send the agent back, as
// often as the tools allow
func (a *Agent) stopGate(ctx context.Context, r *run, env *tools.Env, settings *config.Config, final string, verdicts, nudges, hooked *int) (string, string) {
	if *verdicts < lateVerdictRounds {
		if notes := a.tools.Notes(ctx, r.sessionID(), time.Now().Add(lateVerdictWait)); len(notes) > 0 {
			*verdicts++
			slog.Debug(fmt.Sprintf("late LSP verdicts delivered before the final answer (%d)", *verdicts), "session", r.sessionID())
			return lateVerdicts + "\n\n" + strings.Join(notes, "\n\n"), verificationRequired
		}
	}
	r.mu.Lock()
	edited := slices.Sorted(slices.Values(r.edited))
	r.mu.Unlock()
	if len(edited) == 0 {
		return "", ""
	}
	if *nudges < verifyStopAttempts {
		if nudge := a.tools.Unverified(env, r.sessionID(), edited); nudge != "" {
			*nudges++
			slog.Debug(fmt.Sprintf("verification stop-loop nudge issued (attempt %d)", *nudges), "session", r.sessionID())
			return nudge, verificationRequired
		}
	}
	_, route, _ := a.current(r)
	nudge := a.tools.Continue(ctx, env, tools.Ending{Session: r.sessionID(), Platform: env.Platform, Model: route.Model,
		Coding: agentprompt.Coding(settings, env.Platform, env.Cwd), Attempt: *hooked, Final: final, Changed: edited})
	if nudge == "" {
		return "", ""
	}
	*hooked++
	slog.Debug(fmt.Sprintf("pre_verify nudge issued (attempt %d)", *hooked), "session", r.sessionID())
	return nudge, verifyHookContinue
}

// Backend the turn's web_search tool searches with, for a provider whose own search can stand in for it; none when the turn offers no
// web_search
func (a *Agent) searching(settings *config.Config, offered map[string]tools.Def) string {
	if _, ok := offered["web_search"]; !ok || a.web == nil {
		return ""
	}
	backend, ok := a.web.Active(settings, web.Searching)
	if !ok {
		return ""
	}
	return backend.Name
}

// Surface the session is shown on, whose hints its prompt carries: where it was started, the terminal UI when nothing says
func platformOf(session *sessionsv1.Session) string {
	return cmp.Or(strings.ToLower(strings.TrimSpace(session.GetSource())), "tui")
}

// Note for the turn about the skills that changed since the session's prompt was built, when the model was not told yet
func (a *Agent) skillChanges(ctx context.Context, r *run, store sessions.Store, env *prompt.Env, system string) string {
	if a.skills == nil {
		return ""
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return ""
	}
	var said []string
	for _, m := range history {
		if m.GetRole() == sessionsv1.Role_ROLE_USER {
			said = append(said, cmp.Or(m.GetSent(), m.GetText()))
		}
	}
	return a.skills.Changes(ctx, env, system, said)
}
