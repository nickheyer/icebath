package cron

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// BlueprintCommandResult is the reply text of one blueprint command and the seed for a guided agent turn
type BlueprintCommandResult struct {
	Text      string
	AgentSeed *string
}

// Explicit origin, else the bound session platform and chat, nil without both
func blueprintResolveOrigin(ctx context.Context, explicit map[string]any) map[string]any {
	if explicit != nil {
		return explicit
	}
	env := tools.ToolEnvFrom(ctx)
	platform := tools.SessionEnvValue(env, "ICEBATH_SESSION_PLATFORM")
	chatID := tools.SessionEnvValue(env, "ICEBATH_SESSION_CHAT_ID")
	if platform == "" || chatID == "" {
		return nil
	}
	orNone := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	return map[string]any{
		"platform":  platform,
		"chat_id":   chatID,
		"chat_name": orNone(tools.SessionEnvValue(env, "ICEBATH_SESSION_CHAT_NAME")),
		"thread_id": orNone(tools.SessionEnvValue(env, "ICEBATH_SESSION_THREAD_ID")),
	}
}

// Splits key=value tokens into stripped values, keeping every other token as a leftover
func blueprintParseKV(tokens []string) (map[string]any, []string) {
	values := map[string]any{}
	var leftovers []string
	for _, tok := range tokens {
		k, v, sep := strings.Cut(tok, "=")
		if sep && pystr.Strip(k) != "" {
			values[pystr.Strip(k)] = pystr.Strip(v)
		} else {
			leftovers = append(leftovers, tok)
		}
	}
	return values, leftovers
}

// One candidate is the match, several are the ambiguity, none tries the next pass
func blueprintPick(candidates []AutomationBlueprint) (*AutomationBlueprint, []AutomationBlueprint, bool) {
	if len(candidates) == 1 {
		return &candidates[0], nil, true
	}
	if len(candidates) > 0 {
		return nil, candidates, true
	}
	return nil, nil, false
}

// Exact key, then key or title word prefix, then substring, then close key matches
func matchBlueprint(ctx context.Context, query string) (*AutomationBlueprint, []AutomationBlueprint) {
	q := pystr.Lower(pystr.Strip(query))
	if q == "" {
		return nil, nil
	}
	catalog := listBlueprints(ctx)
	byKey := map[string]AutomationBlueprint{}
	var order []string
	for _, r := range catalog {
		k := pystr.Lower(r.Key)
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = r
	}
	if exact, ok := byKey[q]; ok {
		return &exact, nil
	}
	prefix := func() []AutomationBlueprint {
		var out []AutomationBlueprint
		for _, r := range catalog {
			key := pystr.Lower(r.Key)
			hit := strings.HasPrefix(key, q) || strings.HasPrefix(key[strings.LastIndex(key, ":")+1:], q)
			for _, w := range pystr.SplitWhitespace(r.Title, -1) {
				hit = hit || strings.HasPrefix(pystr.Lower(w), q)
			}
			if hit {
				out = append(out, r)
			}
		}
		return out
	}
	substring := func() []AutomationBlueprint {
		var out []AutomationBlueprint
		for _, r := range catalog {
			if strings.Contains(pystr.Lower(r.Key), q) || strings.Contains(pystr.Lower(r.Title), q) || strings.Contains(pystr.Lower(r.Description), q) {
				out = append(out, r)
			}
		}
		return out
	}
	close := func() []AutomationBlueprint {
		var out []AutomationBlueprint
		for _, k := range pydifflib.GetCloseMatches(q, order, 3, 0.6) {
			out = append(out, byKey[k])
		}
		return out
	}
	for _, pass := range []func() []AutomationBlueprint{prefix, substring, close} {
		if bp, candidates, ok := blueprintPick(pass()); ok {
			return bp, candidates
		}
	}
	return nil, nil
}

// Human schedule of a blueprint, a generic phrase when the template cannot be described
func blueprintCommandSchedule(b *AutomationBlueprint) (out string) {
	defer func() {
		if recover() != nil {
			out = "on a schedule"
		}
	}()
	return humanizeBlueprintSchedule(b)
}

// Agent seed asking for each slot in turn, then creating the job from the schedule and prompt templates
func buildBlueprintSeed(b *AutomationBlueprint) string {
	lines := []string{
		"Set up the '" + b.Title + "' automation for me (automation blueprint '" + b.Key + "'). " + b.Description,
		"",
		"Ask me for each of these, one at a time, offering the default in brackets if I don't have a preference:",
	}
	for _, s := range b.Slots {
		bits := "- " + s.Label + " (" + s.Name + ")"
		if len(s.Options) > 0 {
			opts := make([]string, len(s.Options))
			for i, o := range s.Options {
				opts[i] = pyrepr.Str(o)
			}
			bits += " — one of: " + strings.Join(opts, ", ")
		}
		if str, isStr := s.Default.(string); s.Default != nil && (!isStr || str != "") {
			bits += " [default: " + pyrepr.Str(s.Default) + "]"
		}
		if s.Optional {
			bits += " (optional)"
		}
		if s.Help != "" {
			bits += " — " + s.Help
		}
		lines = append(lines, bits)
	}
	presets := make([]string, len(weekdayPresetOrder))
	for i, k := range weekdayPresetOrder {
		presets[i] = pyrepr.StrRepr(k) + ": " + pyrepr.StrRepr(weekdayPresets[k])
	}
	lines = append(lines, "", "Once you have my answers, create the job by calling the cronjob tool with action='create'. "+
		"Build the schedule as a cron expression from this template: `"+b.ScheduleTemplate+"` (fill {minute}/{hour} from the chosen time, "+
		"{dow} from the weekday choice using {"+strings.Join(presets, ", ")+"}, {interval_min} from any interval). "+
		"Use this exact prompt for the job (substituting my answers into any {slot} placeholders): \""+b.PromptTemplate+"\". "+
		"Confirm the schedule and what it will do before you create it.")
	return strings.Join(lines, "\n")
}

// Catalog listing of every built-in and plugin blueprint with the inline value tip
func blueprintFmtCatalog(ctx context.Context) string {
	lines := []string{"Automation Blueprints — `/blueprint <name>` and I'll ask you what I need:\n"}
	for _, r := range listBlueprints(ctx) {
		source := ""
		if r.Plugin != "" {
			source = " (plugin: " + r.Plugin + ")"
		}
		lines = append(lines, "  • "+r.Key+" — "+r.Title+source, "    "+r.Description)
	}
	lines = append(lines, "\nTip: `/blueprint <name>` walks you through it. Power users can pass values inline, e.g. `/blueprint morning-brief time=08:00`.")
	return strings.Join(lines, "\n")
}

// Ambiguity reply listing each candidate key and title
func blueprintFmtCandidates(query string, candidates []AutomationBlueprint) string {
	lines := []string{"'" + query + "' matches several blueprints — which one?\n"}
	for _, r := range candidates {
		lines = append(lines, "  • "+r.Key+" — "+r.Title)
	}
	lines = append(lines, "\nRun `/blueprint <name>` with one of the names above.")
	return strings.Join(lines, "\n")
}

// No match reply with up to three loosely close keys
func blueprintFmtNoMatch(ctx context.Context, query string) string {
	var keys []string
	for _, r := range listBlueprints(ctx) {
		keys = append(keys, r.Key)
	}
	msg := "No automation blueprint matches '" + query + "'."
	if near := pydifflib.GetCloseMatches(pystr.Lower(query), keys, 3, 0.4); len(near) > 0 {
		msg += " Did you mean: " + strings.Join(near, ", ") + "?"
	}
	return msg + " Run /blueprint to see the catalog."
}

// Where the user manages the new job on this surface
func blueprintManageHint(surface string) string {
	if surface == "cli" {
		return "Manage it with /cron."
	}
	return "Ask me to list, pause, or remove it any time."
}

// Lists, matches, seeds a guided setup or fills and schedules a blueprint for the home
func HandleBlueprintCommand(ctx context.Context, h, args string, origin map[string]any, surface string) (BlueprintCommandResult, error) {
	tokens, err := pyshlex.SplitPOSIX(args, false)
	if err != nil {
		tokens = pystr.SplitWhitespace(args, -1)
	}
	if len(tokens) == 0 {
		return BlueprintCommandResult{Text: blueprintFmtCatalog(ctx)}, nil
	}
	query := tokens[0]
	values, _ := blueprintParseKV(tokens[1:])
	bp, candidates := matchBlueprint(ctx, query)
	if bp == nil {
		if len(candidates) > 0 {
			return BlueprintCommandResult{Text: blueprintFmtCandidates(query, candidates)}, nil
		}
		return BlueprintCommandResult{Text: blueprintFmtNoMatch(ctx, query)}, nil
	}
	if len(values) == 0 {
		seed := buildBlueprintSeed(bp)
		return BlueprintCommandResult{Text: "Setting up '" + bp.Title + "' (" + blueprintCommandSchedule(bp) + "). I'll ask you a couple of things…", AgentSeed: &seed}, nil
	}
	var resolved any
	if o := blueprintResolveOrigin(ctx, origin); o != nil {
		resolved = o
	}
	spec, err := fillBlueprint(bp, values, resolved)
	var fillErr *BlueprintFillError
	if errors.As(err, &fillErr) {
		return BlueprintCommandResult{Text: "Can't set up '" + bp.Title + "': " + err.Error() + "\nOr just run /blueprint " + bp.Key + " and I'll ask you for the values."}, nil
	}
	if err != nil {
		return BlueprintCommandResult{}, err
	}
	job, err := openCronStore(h, nil).createJobWithSchedulerRegistration(spec)
	var regErr *CronSchedulerRegistrationError
	if errors.As(err, &regErr) {
		return BlueprintCommandResult{Text: regErr.UserMessage()}, nil
	}
	if err != nil {
		slog.Debug("blueprint create_job failed: " + err.Error())
		return BlueprintCommandResult{Text: "Failed to create the job: " + err.Error()}, nil
	}
	sched := job.ScheduleDisplay
	if v, ok := spec.Vals["schedule"]; sched == "" && ok {
		sched = pyrepr.Str(v)
	}
	text := "Scheduled '" + bp.Title + "'"
	if sched != "" {
		text += " (" + sched + ")"
	}
	deliver := "origin"
	if v, ok := spec.Vals["deliver"]; ok {
		deliver = pyrepr.Str(v)
	}
	return BlueprintCommandResult{Text: text + ", delivering to " + deliver + ". " + blueprintManageHint(surface)}, nil
}
