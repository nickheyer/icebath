package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What one session's model calls took since it was opened here, for /status
type spent struct {
	provider.Usage
	calls int
	// Prompt of the newest call, the size of the context as the model last saw it
	prompt int64
	// Seconds the newest calls took, newest last
	latency []time.Duration
	// Rate limits the newest answer that stated them gave, nil before one did
	limits *provider.RateLimits
	// Answers OpenRouter served from its response cache
	cacheHits int
}

// Calls whose time /status averages
const latencies = 10

// Records the tokens a call of the session took and what they cost, by the route it ran on and the task it ran for, none for the main
// model's own calls
func (a *Agent) record(r *run, route provider.Route, task string, usage *provider.Usage, took time.Duration) {
	calls := int64(1)
	// A call of the session's own turn the voice chat model runs is kept under that task, while it counts as the session's
	tag := task
	if tag == "" {
		tag = r.routeTask()
	}
	if usage == nil {
		slog.Info(fmt.Sprintf("API call: model=%s provider=%s task=%s in=? out=? usage=unavailable", route.Model, route.Provider, cmp.Or(tag, "main")))
		usage = &provider.Usage{}
	} else {
		cache := ""
		if prompt := usage.Prompt(); usage.CacheRead != 0 && prompt != 0 {
			cache = fmt.Sprintf(" cache=%d/%d (%.0f%%)", usage.CacheRead, prompt, 100*float64(usage.CacheRead)/float64(prompt))
		}
		if usage.CacheWrite != 0 {
			cache += fmt.Sprintf(" write=%d", usage.CacheWrite)
		}
		slog.Info(fmt.Sprintf("API call: model=%s provider=%s task=%s in=%d out=%d total=%d%s", route.Model, route.Provider, cmp.Or(tag, "main"),
			usage.Prompt(), usage.Output, usage.Prompt()+usage.Output, cache))
	}
	if task == "" {
		r.mu.Lock()
		r.spent.Usage = r.spent.Add(*usage)
		r.spent.calls++
		if prompt := usage.Prompt(); prompt > 0 {
			r.spent.prompt = prompt
		}
		slog.Info("API call latency", "session", r.sessionID(), "seconds", took.Seconds())
		r.spent.latency = append(r.spent.latency, took)
		if len(r.spent.latency) > latencies {
			r.spent.latency = r.spent.latency[len(r.spent.latency)-latencies:]
		}
		r.mu.Unlock()
	}
	// A helper call that reported no tokens leaves no row
	if task != "" && *usage == (provider.Usage{}) {
		return
	}
	store, err := a.store()
	if err != nil {
		return
	}
	row := &sessionsv1.Usage{Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite, Reasoning: usage.Reasoning,
		ApiCalls: calls}
	if *usage != (provider.Usage{}) {
		cost := a.models.Host().Cost(a.ctx, a.settings(r), route, *usage)
		row.EstimatedCost, row.CostStatus, row.CostSource, row.PricingVersion = cost.Amount(), cost.Status, cost.Source, cost.Version
	} else {
		row.CostStatus, row.CostSource = "unknown", "none"
	}
	if err := store.RecordUsage(context.WithoutCancel(a.ctx), r.sessionID(), route.Model, tag, row); err != nil {
		slog.Debug("Token persistence failed", "session", r.sessionID(), "error", err)
	}
}

// Keeps the rate limits an answer of the main model states, and counts the answers OpenRouter served from its cache
func (a *Agent) capture(r *run, route provider.Route, header http.Header) {
	if len(header) == 0 {
		return
	}
	limits := provider.ParseRateLimits(header, route.Provider, time.Now())
	status := strings.ToUpper(strings.TrimSpace(header.Get("X-OpenRouter-Cache-Status")))
	r.mu.Lock()
	if limits != nil {
		r.spent.limits = limits
	}
	if status == "HIT" {
		r.spent.cacheHits++
	}
	hits := r.spent.cacheHits
	r.mu.Unlock()
	switch {
	case status == "HIT":
		slog.Info(fmt.Sprintf("OpenRouter response cache HIT (total: %d)", hits))
	case status != "":
		slog.Debug("OpenRouter response cache " + status)
	}
}

// Why a session falls back when its provider wants paying
const billingReason = "the account is out of credit or usage"

// Failure of a route whose provider wants paying, saying where its account adds credit; for a subscription signed in with OAuth, where its
// usage is seen, hedged when only the error's words said so, as a content filter's refusal can read the same
func (a *Agent) billing(route provider.Route, err error) error {
	failure, _ := errors.AsType[*provider.APIError](err)
	link := a.providers.Billing(route)
	label, model := cmp.Or(link.Label, cmp.Or(route.Provider, "The provider")), cmp.Or(route.Model, "the model")
	elsewhere := "You can also switch to an API key or another provider with /model <model> --provider <name>."
	var lines []string
	switch {
	case route.Auth == provider.AuthOAuth && link.Plan != "" && (failure == nil || failure.StatusCode != 402):
		lines = []string{
			label + " says your plan's usage for " + model + " may be used up, extra usage included, but this error does not prove it.",
			"If " + link.Plan + " still shows usage left, this is probably not about billing. With a subscription sign-in, the provider sends " +
				"this same message when its content filter refuses part of the request, often a phrase in the system prompt.",
			"If your usage is used up, wait for it to reset, or add extra usage at " + link.Plan + ".",
			elsewhere,
		}
	case route.Auth == provider.AuthOAuth && link.Plan != "":
		lines = []string{
			label + " says your plan's usage for " + model + " is used up, extra usage included.",
			"Wait for it to reset, or add extra usage at " + link.Plan + ".",
			elsewhere,
		}
	default:
		lines = []string{label + " says the account has run out of credit, or is not allowed to use " + model + ".",
			"Add credit or update billing with " + label + ", then send your message again."}
		if link.URL != "" {
			lines = append(lines, "Billing for "+label+": "+link.URL)
		}
		lines = append(lines, "You can switch providers for now with /model <model> --provider <name>.")
	}
	return fmt.Errorf("%w\n\n%s", err, strings.Join(lines, "\n"))
}

// Lines of /status about what the session's calls took: here, and as the store counts them over its life
func (a *Agent) usageLines(r *run, session *sessionsv1.Session) string {
	r.mu.Lock()
	s := r.spent
	r.mu.Unlock()
	var out strings.Builder
	if total := session.GetUsage(); total != nil && total.GetApiCalls() > 0 {
		fmt.Fprintf(&out, "- **Tokens** %s in, %s out", strs.Thousands(total.GetInput()+total.GetCacheRead()+total.GetCacheWrite()), strs.Thousands(total.GetOutput()))
		if total.GetCacheRead() > 0 {
			fmt.Fprintf(&out, ", %s read from cache", strs.Thousands(total.GetCacheRead()))
		}
		if total.GetReasoning() > 0 {
			fmt.Fprintf(&out, ", %s reasoning", strs.Thousands(total.GetReasoning()))
		}
		fmt.Fprintf(&out, " over %s\n", strs.Count(int(total.GetApiCalls()), "model call"))
		if cost := costLabel(total); cost != "" {
			fmt.Fprintf(&out, "- **Cost** %s\n", cost)
		}
	}
	if s.prompt > 0 {
		line := fmt.Sprintf("- **Context** %s tokens at the last call", strs.Thousands(s.prompt))
		if _, route, err := a.current(r); err == nil {
			if window := int64(a.window(a.ctx, a.settings(r), route)); window > 0 {
				line = fmt.Sprintf("- **Context** %s of %s tokens at the last call, %.0f%% full", strs.Thousands(s.prompt), strs.Thousands(window),
					100*float64(s.prompt)/float64(window))
			}
		}
		out.WriteString(line + "\n")
	}
	if len(s.latency) > 0 {
		var sum time.Duration
		for _, took := range s.latency {
			sum += took
		}
		fmt.Fprintf(&out, "- **Model latency** %s on average over the last %s\n", strs.Span((sum / time.Duration(len(s.latency))).Round(100*time.Millisecond)),
			strs.Count(len(s.latency), "call"))
	}
	return out.String()
}

// What the session's calls cost as the store counts them: an estimate, or included in a subscription; empty when not known
func costLabel(total *sessionsv1.Usage) string {
	switch {
	case total.GetCostStatus() == "included" && total.GetEstimatedCost() == 0:
		return "Included in your plan"
	case total.GetEstimatedCost() > 0:
		prefix := ""
		if total.GetActualCost() == 0 {
			prefix = "about "
		}
		amount := total.GetEstimatedCost()
		if total.GetActualCost() > 0 {
			amount = total.GetActualCost()
		}
		if amount < 0.01 {
			return fmt.Sprintf("%s$%.4f", prefix, amount)
		}
		cents := int64(math.Round(amount * 100))
		return fmt.Sprintf("%s$%s.%02d", prefix, strs.Thousands(cents/100), cents%100)
	}
	return ""
}

// Token usage of the session, its cost and the rate limits the provider stated, then what the provider says the account may still spend;
// reset spends a banked reset of the account's limits where its kind has them
func (a *Agent) Usage(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	words := strings.Fields(strings.ToLower(argument))
	if len(words) > 0 && words[0] != "reset" {
		return "", errors.New("Usage: /usage [reset [--force]]")
	}
	settings := a.settings(r)
	_, route, routeErr := a.current(r)
	if len(words) > 0 {
		spec, _ := a.providers.Spec(route.Spec)
		if routeErr != nil || spec.ResetUsage == nil {
			return "", errors.New("Only ChatGPT or Codex subscriptions have banked usage resets. Switch to one with /model first.")
		}
		return spec.ResetUsage(ctx, route, slices.Contains(words[1:], "--force")), nil
	}
	var account []string
	if routeErr == nil {
		if reading := a.models.Host().Usage(ctx, settings, route); reading != nil {
			account = reading.Lines(true)
		}
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	total := session.GetUsage()
	r.mu.Lock()
	last, limits := r.spent.prompt, r.spent.limits
	r.mu.Unlock()
	var lines []string
	if limited := limits.Lines(time.Now()); len(limited) > 0 {
		lines = append(append(lines, limited...), "")
	}
	if total.GetApiCalls() > 0 {
		lines = append(lines, "**Tokens used in this session**", "Model: `"+cmp.Or(route.Model, session.GetModel())+"`", "Input tokens: "+strs.Thousands(total.GetInput()))
		if total.GetCacheRead() > 0 {
			lines = append(lines, "Cache read tokens: "+strs.Thousands(total.GetCacheRead()))
		}
		if total.GetCacheWrite() > 0 {
			lines = append(lines, "Cache write tokens: "+strs.Thousands(total.GetCacheWrite()))
		}
		all := total.GetInput() + total.GetCacheRead() + total.GetCacheWrite() + total.GetOutput()
		lines = append(lines, "Output tokens: "+strs.Thousands(total.GetOutput()), "Total tokens: "+strs.Thousands(all), "Model calls: "+strs.Thousands(total.GetApiCalls()))
		if cost := costLabel(total); cost != "" {
			lines = append(lines, "Cost: "+cost)
		}
		if routeErr == nil && last > 0 {
			if window := int64(a.window(ctx, settings, route)); window > 0 {
				// The provider's count of the newest request with what came since, as the context dial reads it
				used, mark := last, ""
				if history, err := store.Messages(ctx, id, sessions.Model); err == nil && len(history) > 0 {
					occupied, estimated := a.occupancy(ctx, r, history)
					used = int64(occupied)
					if estimated {
						mark = "about "
					}
				}
				lines = append(lines, fmt.Sprintf("Context: %s%s of %s tokens, %.0f%% full", mark, strs.Thousands(used), strs.Thousands(window), 100*float64(used)/float64(window)))
			}
		}
	}
	if len(account) > 0 {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, account...)
	}
	if len(lines) == 0 {
		return "This session has used no tokens yet. Send a message, then look again.", nil
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n"), nil
}

// Lines after a model switch of what is known of the model: its window, its most output and what it can do
func (a *Agent) modelInfo(settings *config.Config, route provider.Route) string {
	if route.Model == "" {
		return ""
	}
	var out strings.Builder
	info, known := a.models.Host().Info(settings, route)
	if window := a.window(a.ctx, settings, route); window > 0 {
		fmt.Fprintf(&out, "\nContext window: %s tokens", strs.Thousands(int64(window)))
		if pinned := settings.GetInt(agentconfig.ContextLength); pinned > 0 {
			out.WriteString(", as you set it in /settings under Agent › Context window")
		}
	}
	if known && info.MaxOutput > 0 {
		fmt.Fprintf(&out, "\nMax output: %s tokens", strs.Thousands(int64(info.MaxOutput)))
	}
	if known {
		out.WriteString("\nCan do: " + info.Abilities())
	}
	if route.Mode == provider.ModeAnthropicMessages || provider.BaseHostMatches(route.BaseURL, "openrouter.ai") && strings.Contains(strings.ToLower(route.Model), "claude") {
		out.WriteString("\nPrompt caching: On")
	}
	return out.String()
}

// Fetches the online model catalog again and forgets the model lists kept for the session's provider, so the next listing reads them anew
func (a *Agent) RefreshModels(ctx context.Context, id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	settings := a.settings(r)
	host := a.models.Host()
	host.RefreshCatalogs(settings)
	if _, route, err := a.current(r); err == nil {
		host.Catalog(ctx, settings, route, true)
	}
	return nil
}
