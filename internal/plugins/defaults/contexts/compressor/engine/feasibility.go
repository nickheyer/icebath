package engine

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Checks once per model that the summarising model takes what the trigger lets through: the trigger is lowered to its window, and one
// below the smallest window icebath runs on is refused
func (c *compressor) feasibility(ctx context.Context, env *contexts.Env) error {
	if c.feasible || !c.s.enabled || env == nil || env.Aux == nil {
		return nil
	}
	c.feasible = true
	route, err := env.Aux.Route(ctx, Task)
	if err != nil {
		configured := strings.TrimSpace(env.Config.GetString("auxiliary." + Task + ".provider"))
		message := "⚠ No auxiliary LLM provider configured: Icebath has no helper model for summarising long chats, so older messages will be cut " +
			"without a summary. Connect one with /providers, or set one for Compression in /settings under Helper models."
		if configured != "" && configured != "auto" {
			message = fmt.Sprintf("⚠ Configured auxiliary compression provider '%s' is unavailable, so older messages in long chats will be cut without a "+
				"summary. Sign in to that provider again, or change auxiliary.compression in your config.", configured)
		}
		c.say(message)
		slog.Warn("No auxiliary LLM provider for compression — summaries will be unavailable.")
		return nil
	}
	window := max(env.Config.GetInt("auxiliary."+Task+"."+AuxWindow), 0)
	switch {
	case window > 0:
	case strings.EqualFold(route.Model, c.route.Model) &&
		(c.route.BaseURL == "" || strings.TrimRight(route.BaseURL, "/") == strings.TrimRight(c.route.BaseURL, "/")):
		window = c.window
	case env.Measure != nil:
		window = env.Measure(ctx, route)
	}
	if window <= 0 {
		c.feasible = false
		return nil
	}
	if window < minimumWindow {
		c.feasible = false
		return fmt.Errorf("Auxiliary compression model %s has a context window of %s tokens, which is below the minimum %s required by Icebath Agent.  "+
			"Choose a compression model with at least %dK context (set auxiliary.compression.model in config.yaml), or set "+
			"auxiliary.compression.context_length to override the detected value if it is wrong.", route.Model, strs.Thousands(window),
			strs.Thousands(minimumWindow), minimumWindow/1000)
	}
	if window < c.threshold {
		c.lower(route.Model, route.Provider, route.BaseURL, window)
	}
	return nil
}

// Lowers the trigger to the summarising model's window and tells the user how to make it last
func (c *compressor) lower(model, provider, base string, window int) {
	old := c.threshold
	c.auxCeiling, c.threshold = window, window
	if c.s.tailMode == contextsconfig.TailLean {
		c.tailBudget = c.computeTailBudget()
	} else {
		c.tailBudget = int(float64(window) * c.s.targetRatio)
	}
	safe := 50
	if c.window != 0 {
		c.percent = float64(window) / float64(c.window)
		safe = int(float64(window) / float64(c.window) * 100)
	}
	recomputed := thresholdTokens(c.window, effectivePercent(c.window, float64(safe)/100), c.reserve)
	label := provider
	if label == "" || label == "auto" {
		label = cmp.Or(base, "auto")
	}
	main := cmp.Or(c.route.Model, "?")
	if c.route.Provider != "" {
		main = fmt.Sprintf("%s (%s)", main, c.route.Provider)
	}
	message := fmt.Sprintf("⚠ Compression model %s (%s) context is %s tokens, but the main model %s's compression threshold was %s tokens. Auto-lowered "+
		"this session's threshold to %s tokens so compression can run.\n", model, label, strs.Thousands(window), main, strs.Thousands(old),
		strs.Thousands(window))
	if c.window == 0 || recomputed <= window {
		message += fmt.Sprintf("  To make this permanent, edit config.yaml — either:\n  1. Use a larger compression model:\n       auxiliary:\n         "+
			"compression:\n           model: <model-with-%s+-context>\n  2. Lower the compression threshold:\n       context:\n         compression:\n"+
			"           threshold: 0.%02d", strs.Thousands(old), safe)
	} else {
		message += fmt.Sprintf("  To make this permanent, use a larger compression model in config.yaml:\n       auxiliary:\n         compression:\n"+
			"           model: <model-with-%s+-context>\n  (Lowering context.compression.threshold cannot help here — with %s's %s-token window, Icebath's "+
			"small-context floor and output reservation would recompute the trigger to %s tokens, still above the compression model's %s.)",
			strs.Thousands(old), main, strs.Thousands(c.window), strs.Thousands(recomputed), strs.Thousands(window))
	}
	c.say(message)
	slog.Warn(fmt.Sprintf("Auxiliary compression model %s has %d token context, below the main model's compression threshold of %d tokens — auto-lowered "+
		"session threshold to %d to keep compression working.", model, window, old, window))
}

// Tells the user once per text
func (c *compressor) say(text string) {
	if text == c.told {
		return
	}
	c.told = text
	if c.notify != nil {
		c.notify(text)
	}
}

// Whether the provider's count of the request puts it at or over the trigger
func (c *compressor) over(tokens int) bool {
	return c.threshold > 0 && tokens >= c.threshold
}

// Warns once for each reason and runway that a session over the trigger declined every way to shrink
func (c *compressor) warnNoReclaim(reason string, tokens, before int) {
	if !c.over(tokens) {
		c.reclaimed = ""
		return
	}
	key := fmt.Sprint(reason, c.pruneRearm)
	if c.reclaimed == key {
		return
	}
	c.reclaimed = key
	estimate := "n/a"
	if before > 0 {
		estimate = strs.Thousands(before)
	}
	slog.Warn(fmt.Sprintf("Context is over the compression threshold (~%s of %s tokens) but reclamation did not run: %s (message-token estimate %s, prune "+
		"rearm mark %s). The session may keep growing until the provider rejects the request — /compact to compress history now.",
		strs.Thousands(tokens), strs.Thousands(c.threshold), reason, estimate, strs.Thousands(c.pruneRearm)))
}

// Old tool results cut to summaries without a summary call once the session passes the early trim size, kept from doing it again until the
// transcript grows back by what it freed
func (c *compressor) Prune(ctx context.Context, env *contexts.Env, messages []*sessionsv1.Message, tokens int) ([]*sessionsv1.Message, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.pruneTokens <= 0 || tokens < c.s.pruneTokens {
		return messages, 0
	}
	if len(messages) <= c.s.protectLast+c.headSize(messages)+1 {
		c.warnNoReclaim("prune:tail_only", tokens, 0)
		return messages, 0
	}
	price := c.pricer(messages)
	before := 0
	for i := range messages {
		before += price(i)
	}
	if before < c.pruneRearm && !c.over(tokens) {
		return messages, 0
	}
	pruned, count := c.pruneResults(messages, c.s.protectLast, 0, c.s.pruneMinChars)
	pruned = withPrunedSkillSafety(messages, pruned)
	if count == 0 {
		c.warnNoReclaim("prune:nothing_eligible", tokens, 0)
		return messages, 0
	}
	price = c.pricer(pruned)
	after := 0
	for i := range pruned {
		after += price(i)
	}
	reclaimed := max(0, before-after)
	if reclaimed < c.s.pruneMinReclaim {
		c.warnNoReclaim("prune:reclaim_below_minimum", tokens, before)
		return messages, 0
	}
	c.pruneRearm = after + max(reclaimed, c.s.pruneTokens, c.s.pruneMinReclaim)
	c.setModelConfig(ctx, pruneRearmKey, c.pruneRearm)
	c.reclaimed = ""
	return pruned, count
}
