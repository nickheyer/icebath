package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Value of key in a suggestion or job record, def when the key is absent
func suggestionGet(rec any, key string, def any) (any, error) {
	switch m := rec.(type) {
	case *pyjson.Object:
		if v, ok := m.Vals[key]; ok {
			return v, nil
		}
		return def, nil
	case map[string]any:
		if v, ok := m[key]; ok {
			return v, nil
		}
		return def, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(rec))
}

// Nested job_spec value of a record, falling back to def as the Python or chain does
func suggestionSpecGet(rec any, key string, def any) (any, error) {
	spec, err := suggestionGet(rec, "job_spec", pyjson.NewObject())
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(spec) {
		spec = pyjson.NewObject()
	}
	return suggestionGet(spec, key, def)
}

// Pending list as numbered lines with schedule and source, or the empty state hint
func fmtPendingSuggestions(pending []any) (string, error) {
	if len(pending) == 0 {
		return "No suggested automations right now.\n" +
			"Try `/suggestions catalog` to see the curated starter set, or " +
			"install a blueprint skill to get one.", nil
	}
	lines := []string{"Suggested automations — `/suggestions accept N` or `dismiss N`:\n"}
	for i, s := range pending {
		sched, err := suggestionSpecGet(s, "schedule", "?")
		if err != nil {
			return "", err
		}
		title, err := suggestionGet(s, "title", "(untitled)")
		if err != nil {
			return "", err
		}
		source, err := suggestionGet(s, "source", "?")
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("  %d. %s  [%s]  (%s)", i+1, pyrepr.Str(title), pyrepr.Str(sched), pyrepr.Str(source)))
		descVal, err := suggestionGet(s, "description", "")
		if err != nil {
			return "", err
		}
		descStr, ok := descVal.(string)
		if !ok {
			return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(descVal))
		}
		if desc := pystr.Strip(descStr); desc != "" {
			lines = append(lines, "     "+desc)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Current chat origin from the bound session env so an accepted job delivers back to it, nil for the home channel
func resolveSuggestionOrigin(ctx context.Context, s *Store) map[string]any {
	return s.originFromEnv(tools.ToolEnvFrom(ctx), nil)
}

// Schedules a pending suggestion and reports the created job
func acceptSuggestionCmd(ctx context.Context, s *Store, rest string, origin map[string]any, surface string) (string, error) {
	if rest == "" {
		return "Usage: /suggestions accept <number|id>", nil
	}
	job, err := s.AcceptSuggestion(ctx, rest, origin)
	if err != nil {
		var regErr *CronSchedulerRegistrationError
		if errors.As(err, &regErr) {
			return regErr.UserMessage(), nil
		}
		return "", err
	}
	if job == nil {
		return fmt.Sprintf("No pending suggestion matches '%s'. Run /suggestions to list them.", rest), nil
	}
	rec, err := jobAsMap(job)
	if err != nil {
		return "", err
	}
	sched, _ := suggestionGet(rec, "schedule_display", nil)
	if !pyrepr.Truthy(sched) {
		if sched, err = suggestionSpecGet(rec, "schedule", ""); err != nil {
			return "", err
		}
	}
	name, _ := suggestionGet(rec, "name", "automation")
	manage := "Ask me to list, pause, or remove it any time."
	if surface == "cli" {
		manage = "Manage it with /cron."
	}
	out := "Scheduled '" + pyrepr.Str(name) + "'"
	if pyrepr.Truthy(sched) {
		out += " (" + pyrepr.Str(sched) + ")"
	}
	return out + ". " + manage, nil
}

// Dismisses a pending suggestion so it is never offered again
func dismissSuggestionCmd(s *Store, rest string) (string, error) {
	if rest == "" {
		return "Usage: /suggestions dismiss <number|id>", nil
	}
	ok, err := s.DismissSuggestion(rest)
	if err != nil {
		return "", err
	}
	if ok {
		return "Dismissed. Won't suggest that again.", nil
	}
	return fmt.Sprintf("No pending suggestion matches '%s'.", rest), nil
}

// Seeds the curated catalog and reports which suggestions were added
func catalogSuggestionsCmd(ctx context.Context, s *Store) string {
	created, err := SeedCatalogSuggestions(ctx, s.AddSuggestion, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("catalog seed failed: %s", pyrepr.ExceptionMessage(err)))
		return "Couldn't load the catalog."
	}
	if len(created) == 0 {
		return "No new catalog automations to add (already offered, dismissed, " +
			"or your suggestion list is full). Run /suggestions to see pending."
	}
	titles := make([]string, len(created))
	for i, c := range created {
		title, _ := suggestionGet(c, "title", "?")
		titles[i] = pyrepr.Str(title)
	}
	return "Added " + strconv.Itoa(len(created)) + " suggestion(s): " + strings.Join(titles, ", ") + ".\nRun /suggestions to review."
}

const suggestionsUsage = "Usage:\n" +
	"  /suggestions              list pending\n" +
	"  /suggestions accept N     schedule suggestion N\n" +
	"  /suggestions dismiss N    dismiss suggestion N\n" +
	"  /suggestions catalog      add curated starter automations\n" +
	"  /suggestions clear        housekeeping"

// Dispatches a /suggestions invocation for the CLI or gateway, origin nil meaning the session env's chat
func HandleSuggestionsCommand(ctx context.Context, args string, origin map[string]any, surface string) (string, error) {
	s := openCronStore(home.Dir(ctx), time.Now)
	if origin == nil {
		origin = resolveSuggestionOrigin(ctx, s)
	}
	parts := pystr.SplitWhitespace(pystr.Strip(args), -1)
	sub := ""
	if len(parts) > 0 {
		sub = pystr.Lower(parts[0])
	}
	rest := ""
	if len(parts) > 1 {
		rest = pystr.Strip(strings.Join(parts[1:], " "))
	}
	switch sub {
	case "":
		pending, err := s.ListPendingSuggestions()
		if err != nil {
			return "", err
		}
		return fmtPendingSuggestions(pending)
	case "accept", "add", "schedule":
		return acceptSuggestionCmd(ctx, s, rest, origin, surface)
	case "dismiss", "no", "reject":
		return dismissSuggestionCmd(s, rest)
	case "catalog":
		return catalogSuggestionsCmd(ctx, s), nil
	case "clear":
		n, err := s.ClearResolvedSuggestions()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Cleared %d resolved suggestion record(s).", n), nil
	}
	return suggestionsUsage, nil
}
