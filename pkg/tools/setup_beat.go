package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// SetupBeatCard asks the desktop to show a beat card and blocks until the user answers or the card closes, installed by the desktop gateway
var SetupBeatCard func(env *Env, payload map[string]any) (map[string]any, error)

// SetupBeatEndTurn ends the agent's turn once a result is in, installed by the agent loop
var SetupBeatEndTurn func(env *Env)

// OnboardingPluginNames lists the onboarding catalog plugins this OS runs, installed by pkg/plugins
var OnboardingPluginNames func(env *Env) []string

// CatalogInstalledDir is the install dir of a catalog plugin, empty when not installed, installed by pkg/plugins
var CatalogInstalledDir func(env *Env, name string) string

// CatalogPluginSkill is the registered name of a plugin skill written catalog:skill, empty when absent, installed by pkg/plugins
var CatalogPluginSkill func(env *Env, written string) string

// AllSkillNames lists every skill name skill_view loads, installed by pkg/skills
var AllSkillNames func(env *Env) []string

// SetupBeatIDs are the accepted beat arguments
var SetupBeatIDs = append(append([]string{"next"}, OnboardingBeatIDs...), "done")

// Connector slugs this account has connected, sorted
func onbConnectedSlugs(env *Env) ([]string, error) {
	items, err := NewConnectorClient(env, nil, "").ListConnectors(context.Background(), 0)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, item := range items {
		if truthy(item["connected"]) {
			set[strings.ToLower(fmt.Sprint(item["connector"]))] = true
		}
	}
	return sortedSetKeys(set), nil
}

// Sorted keys of a set
func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// The connectors this account can connect as the card lists them, nil when unreachable
func onbHostedNames(env *Env) map[string]bool {
	if !ConnectorsAvailable(env) {
		return nil
	}
	items, err := NewConnectorClient(env, nil, "").ListConnectors(context.Background(), 0)
	if err != nil {
		toolsLogger(env).Debug("connector list for the routing check failed", "error", err)
		return nil
	}
	set := map[string]bool{}
	for _, item := range items {
		set[strings.ToLower(fmt.Sprint(item["connector"]))] = true
	}
	return set
}

// Hook panics when the package that must install it did not
func requireOnboardingHooks() {
	if OnboardingPluginNames == nil || CatalogInstalledDir == nil || CatalogPluginSkill == nil {
		panic("tools: onboarding plugin hooks are nil, pkg/plugins must install them")
	}
	if AllSkillNames == nil {
		panic("tools: AllSkillNames is nil, pkg/skills must install it")
	}
}

// What the beat has on this machine now, nil for beats without presence or when it cannot be read
func onbPresent(env *Env, beat string) map[string]any {
	switch beat {
	case "connectors":
		slugs, err := onbConnectedSlugs(env)
		if err != nil {
			toolsLogger(env).Warn("onboarding: could not read what the connectors beat has on this machine", "error", err)
			return nil
		}
		return map[string]any{"connected": enumValues(slugs)}
	case "plugins":
		requireOnboardingHooks()
		var installed []string
		for _, name := range OnboardingPluginNames(env) {
			if CatalogInstalledDir(env, name) != "" {
				installed = append(installed, name)
			}
		}
		return map[string]any{"installed": enumValues(installed)}
	}
	return nil
}

// An event the call carries in itself with no card, nil when it needs one
func onbDirectEvent(env *Env, beat string, args map[string]any) OnboardingEvent {
	if beat == "done" {
		return OnbDone{}
	}
	if truthy(args["skip"]) {
		return OnbSkipped{Beat: beat, Present: onbPresent(env, beat)}
	}
	if value, ok := args["value"].(string); ok && strings.TrimSpace(value) != "" {
		return OnbRecorded{Beat: beat, Value: strings.TrimSpace(value), Present: onbPresent(env, beat)}
	}
	return nil
}

// The event a card reply carries
func onbReplyEvent(env *Env, beat string, reply map[string]any) OnboardingEvent {
	if reply == nil {
		reply = map[string]any{}
	}
	if said, ok := reply["said"].(string); beat == "name" && ok && strings.TrimSpace(said) != "" {
		return OnbAnswered{Beat: beat, Answer: map[string]any{"name": strings.TrimSpace(said)}, Said: strings.TrimSpace(said)}
	}
	if truthy(reply["wrote_instead"]) {
		return OnbWrote{Beat: beat, Present: onbPresent(env, beat)}
	}
	if answer, ok := reply["answer"].(map[string]any); ok && len(answer) > 0 {
		return OnbAnswered{Beat: beat, Answer: answer}
	}
	return OnbTimedOut{Beat: beat, Present: onbPresent(env, beat)}
}

// Resolver mapping a written first task skill name to the name skill_view loads it with, empty when missing here
func onbSkillResolver(env *Env) func(string) string {
	requireOnboardingHooks()
	bundled := map[string]bool{}
	for _, name := range AllSkillNames(env) {
		bundled[name] = true
	}
	return func(name string) string {
		if strings.Contains(name, ":") {
			return CatalogPluginSkill(env, name)
		}
		if bundled[name] {
			return name
		}
		return ""
	}
}

// Result JSON of a step with its reminder rendered
func onbResult(env *Env, step OnboardingStep, beat, extra string) string {
	facts := OnboardingFactsFromStored(asMapOrEmpty(step.Record["facts"]))
	resolve := func(name string) string { return name }
	if strings.HasPrefix(step.Reminder, "build_") {
		resolve = onbSkillResolver(env)
	}
	text := OnboardingReminder(step.Reminder, step.Record, facts, beat, resolve) + extra
	out := copyMap(step.Result)
	if text != "" {
		out["reminder"] = text
	}
	return jsonCompact(out)
}

// Suggestion sentence naming the connectors the card can show
func onbSuggestFrom(slugs map[string]bool) string {
	if len(slugs) == 0 {
		return ""
	}
	return fmt.Sprintf(" Suggest only connectors from this list, by these slugs: %s.", strings.Join(sortedSetKeys(slugs), ", "))
}

// The refusal for connector suggestions the card cannot show, empty when every one is listed
func onbUnlistedSuggestions(env *Env, args map[string]any) string {
	var wanted []string
	for _, s := range toAnyList(args["suggest"]) {
		if slug := strings.ToLower(strings.TrimSpace(fmt.Sprint(s))); slug != "" {
			wanted = append(wanted, slug)
		}
	}
	var slugs map[string]bool
	if len(wanted) > 0 {
		slugs = onbHostedNames(env)
	}
	var unlisted []string
	for _, slug := range wanted {
		if slugs != nil && !slugs[slug] {
			unlisted = append(unlisted, slug)
		}
	}
	if len(unlisted) == 0 {
		return ""
	}
	return jsonCompact(map[string]any{"error": "not connectors here", "reminder": fmt.Sprintf(
		"No card shown: %s cannot be connected here. Do not mention them as connections.%s Then call setup_beat(beat=\"connectors\", suggest=[...]) again, or without suggest when none fits.",
		strings.Join(unlisted, ", "), onbSuggestFrom(slugs))})
}

// SuggestedOnboardingName is the OS account's real name for the name card only
func SuggestedOnboardingName() string { return hostSummaryFullName() }

// The card data of a beat
func onbCard(env *Env, beat string, step OnboardingStep, args map[string]any) map[string]any {
	facts := OnboardingFactsFromStored(asMapOrEmpty(step.Record["facts"]))
	card := onboardingBeatByID(beat).Card(facts, onbPicks(step.Record))
	if beat == "name" {
		card = map[string]any{}
		if name := SuggestedOnboardingName(); name != "" {
			card["suggested"] = name
		}
	}
	if beat == "connectors" {
		var suggested []any
		for _, s := range toAnyList(args["suggest"]) {
			if strings.TrimSpace(fmt.Sprint(s)) != "" {
				suggested = append(suggested, fmt.Sprint(s))
			}
		}
		if suggested == nil {
			suggested = []any{}
		}
		card = map[string]any{"suggested": suggested}
	}
	return card
}

// Shows a beat's card through the desktop and returns the user's answer with the next beat's reminder
func setupBeatTool(ctx context.Context, env *Env, db *state.DB, sessionID, toolCallID string, args map[string]any) string {
	beat, _ := args["beat"].(string)
	if !containsString(SetupBeatIDs, beat) {
		return toolErrorJSON(fmt.Sprintf("beat must be one of: %s.", strings.Join(SetupBeatIDs, ", ")), nil)
	}
	record := LoadOnboardingRecord(db, sessionID)
	if record == nil {
		return toolErrorJSON("Onboarding is not running in this chat.", nil)
	}
	if beat != "next" {
		if event := onbDirectEvent(env, beat, args); event != nil {
			step, err := AdvanceOnboarding(WithEnvHome(ctx, env), db, sessionID, event)
			if err != nil {
				return toolErrorJSON(err.Error(), nil)
			}
			return onbResult(env, step, "", "")
		}
	}
	if beat == "next" {
		beat = ""
		if OnboardingActive(record) {
			beat = OnboardingNextBeat(record)
		}
		if beat == "" {
			if OnboardingActive(record) {
				return jsonCompact(map[string]any{"next": "done", "reminder": OnboardingFinishReminder(record)})
			}
			return jsonCompact(map[string]any{"error": onboardingEndedText})
		}
	}
	if SetupBeatCard == nil {
		return toolErrorJSON("setup_beat cards show only in the Icebath desktop app.", nil)
	}
	if beat == "connectors" {
		if refused := onbUnlistedSuggestions(env, args); refused != "" {
			return refused
		}
	}
	shown, err := AdvanceOnboarding(WithEnvHome(ctx, env), db, sessionID, OnbShown{Beat: beat, ToolCallID: toolCallID, Options: args["options"], Suggest: args["suggest"]})
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	if len(shown.Result) > 0 {
		extra := ""
		if shown.Reminder == "offer_first" {
			extra = onbSuggestFrom(onbHostedNames(env))
		}
		return onbResult(env, shown, "", extra)
	}
	question := strings.TrimSpace(argS(args, "question"))
	if question == "" {
		locale, _ := asMapOrEmpty(record["facts"])["locale"].(string)
		question = OnboardingQuestion(beat, locale)
	}
	payload := map[string]any{"beat": beat, "question": question, "card": onbCard(env, beat, shown, args), "tool_call_id": emptyAsNull(toolCallID)}
	reply, err := SetupBeatCard(env, payload)
	if err != nil {
		slog.Warn("onboarding: the card did not answer", "beat", beat, "error", err)
		reply = nil
	}
	after := LoadOnboardingRecord(db, sessionID)
	if after != nil && after["phase"] == onbPhaseSkipped {
		return onbResult(env, OnboardingStep{Record: after, Result: map[string]any{"skipped_all": true}, Reminder: "skip_all"}, "", "")
	}
	event := onbReplyEvent(env, beat, reply)
	step, err := AdvanceOnboarding(WithEnvHome(ctx, env), db, sessionID, event)
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	result := onbResult(env, step, beat, "")
	if _, wrote := event.(OnbWrote); wrote && SetupBeatEndTurn != nil {
		SetupBeatEndTurn(env)
	}
	return result
}

var setupBeatSchema = map[string]any{
	"name": "setup_beat",
	"description": "Run the first-run onboarding in this chat. Shows the card for a beat and waits for the user, then " +
		"returns their answer and what to do next. beat=next shows the next beat. Pass value to record an answer " +
		"they gave in words, skip to record that they skipped a beat. The build card needs options: your picks " +
		"from the list its reminder gives. beat=done ends onboarding after the first " +
		"build is shown and its close card is answered. Nothing here ends it early: a user who wants to stop " +
		"presses Skip setup.",
	"parameters": map[string]any{
		"type":     "object",
		"required": []any{"beat"},
		"properties": map[string]any{
			"beat":     map[string]any{"type": "string", "enum": enumValues(SetupBeatIDs)},
			"question": map[string]any{"type": "string", "description": "Your line above the card."},
			"value":    map[string]any{"type": "string", "description": "Record an answer given in words (a name, or an option id). No card."},
			"skip":     map[string]any{"type": "boolean", "description": "They skipped this beat in words. No card."},
			"suggest":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "connectors only: slugs pinned as Suggested."},
			"options": map[string]any{"type": "array", "maxItems": onboardingMaxPicks, "description": "build only: up to four picks, ids from the list.",
				"items": map[string]any{"type": "object", "required": []any{"id", "label"},
					"properties": map[string]any{"id": map[string]any{"type": "string"}, "label": map[string]any{"type": "string", "maxLength": onboardingMaxLabel}}}},
		},
	},
}

// Registers the onboarding only setup_beat tool
func registerSetupBeatTool() {
	Register(Def{Name: "setup_beat", Toolset: "onboarding", Schema: setupBeatSchema, Emoji: "\U0001F39B", Timeout: OnboardingCardTimeout * time.Second,
		Handler: func(ctx context.Context, call *Call) string {
			return setupBeatTool(WithEnvHome(ctx, call.Env), call.Env, call.Env.State, call.SessionID, call.ToolCallID, call.Args)
		}})
}
