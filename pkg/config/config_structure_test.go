package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func noRemovedBackends(section, name string) string { return "" }

func validate(cfg map[string]any) []ConfigIssue {
	return ValidateConfigStructure(cfg, noRemovedBackends)
}

func issuesWhere(issues []ConfigIssue, keep func(ConfigIssue) bool) []ConfigIssue {
	var out []ConfigIssue
	for _, i := range issues {
		if keep(i) {
			out = append(out, i)
		}
	}
	return out
}

func TestValidationCustomProvidersDictInsteadOfList(t *testing.T) {
	issues := validate(map[string]any{
		"custom_providers": map[string]any{
			"name": "Generativelanguage.googleapis.com", "base_url": "https://generativelanguage.googleapis.com/v1beta",
			"api_key": "xxx", "model": "models/gemini-2.5-flash", "rate_limit_delay": 2.0,
			"fallback_model": map[string]any{"provider": "openrouter", "model": "qwen/qwen3.6-plus:free"},
		},
		"fallback_providers": []any{},
	})
	if len(issuesWhere(issues, func(i ConfigIssue) bool {
		return i.Severity == "error" && strings.Contains(i.Message, "dict") && strings.Contains(i.Message, "list")
	})) == 0 {
		t.Fatalf("custom_providers dict not detected: %+v", issues)
	}
}

func TestValidationCustomProvidersScalarNamesKeyAndType(t *testing.T) {
	issues := validate(map[string]any{"custom_providers": "oops", "model": map[string]any{"provider": "openrouter"}})
	if len(issuesWhere(issues, func(i ConfigIssue) bool {
		return i.Severity == "error" && strings.HasPrefix(i.Message, "custom_providers is a str") && strings.Contains(i.Message, "list")
	})) == 0 {
		t.Fatalf("scalar custom_providers: %+v", issues)
	}
	ok := validate(map[string]any{"custom_providers": []any{map[string]any{"name": "x", "base_url": "http://h/v1"}},
		"model": map[string]any{"provider": "custom"}})
	if errs := issuesWhere(ok, func(i ConfigIssue) bool { return i.Severity == "error" }); len(errs) != 0 {
		t.Fatalf("valid list reported errors: %+v", errs)
	}
}

func TestValidationCustomProvidersDictMisplacedFields(t *testing.T) {
	issues := validate(map[string]any{"custom_providers": map[string]any{"name": "test", "base_url": "https://example.com", "api_key": "xxx"}})
	misplaced := issuesWhere(issues, func(i ConfigIssue) bool {
		return i.Severity == "warning" && strings.Contains(i.Message, "custom_providers entry fields")
	})
	if len(misplaced) != 1 {
		t.Fatalf("misplaced = %+v", misplaced)
	}
	if !strings.Contains(misplaced[0].Message, "['api_key', 'base_url']") {
		t.Fatalf("message = %q", misplaced[0].Message)
	}
}

func TestValidationCustomProvidersListEntryNotDict(t *testing.T) {
	issues := validate(map[string]any{"custom_providers": []any{"not-a-dict"}, "model": map[string]any{"provider": "custom"}})
	if len(issuesWhere(issues, func(i ConfigIssue) bool { return strings.Contains(i.Message, "not a dict") })) == 0 {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestValidationCustomProvidersWithModelSection(t *testing.T) {
	issues := validate(map[string]any{
		"custom_providers": []any{map[string]any{"name": "test", "base_url": "https://example.com/v1"}},
		"model":            map[string]any{"provider": "custom", "default": "test-model"},
	})
	if len(issuesWhere(issues, func(i ConfigIssue) bool { return strings.Contains(i.Message, "no 'model' section") })) != 0 {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestValidationVoiceSubmitMode(t *testing.T) {
	for _, mode := range []string{"direct", "draft"} {
		issues := validate(map[string]any{"voice": map[string]any{"submit_mode": mode}})
		if len(issuesWhere(issues, func(i ConfigIssue) bool { return strings.Contains(i.Message, "voice.submit_mode") })) != 0 {
			t.Fatalf("%s flagged: %+v", mode, issues)
		}
	}
	issues := validate(map[string]any{"voice": map[string]any{"submit_mode": "refine"}})
	if len(issuesWhere(issues, func(i ConfigIssue) bool {
		return i.Severity == "error" && strings.Contains(i.Message, "voice.submit_mode") &&
			strings.Contains(i.Hint, "direct") && strings.Contains(i.Hint, "draft")
	})) == 0 {
		t.Fatalf("refine not flagged: %+v", issues)
	}
}

func tzIssues(cfg map[string]any) []ConfigIssue {
	return issuesWhere(validate(cfg), func(i ConfigIssue) bool { return strings.Contains(i.Message, "timezone") })
}

func TestValidationTimezoneInvalidOrNonString(t *testing.T) {
	if _, err := time.LoadLocation("Etc/UTC"); err != nil {
		t.Skip("no tz database on this host")
	}
	got := tzIssues(map[string]any{"timezone": "Asia/Tokio", "model": map[string]any{"provider": "nous"}})
	if len(got) != 1 || got[0].Severity != "error" || !strings.Contains(got[0].Message, "Asia/Tokio") ||
		!strings.Contains(got[0].Hint, "IANA") || !strings.Contains(got[0].Hint, "ICEBATH_TIMEZONE") {
		t.Fatalf("invalid zone: %+v", got)
	}
	got = tzIssues(map[string]any{"timezone": 9, "model": map[string]any{"provider": "nous"}})
	if len(got) != 1 || got[0].Severity != "error" || !strings.Contains(got[0].Message, "string") {
		t.Fatalf("non string zone: %+v", got)
	}
}

func TestValidationTimezoneValidBlankMissingOrUnverifiable(t *testing.T) {
	for _, cfg := range []map[string]any{{"timezone": "Asia/Tokyo"}, {}, {"timezone": ""}, {"timezone": "   "}, {"timezone": nil}} {
		cfg["model"] = map[string]any{"provider": "nous"}
		if got := tzIssues(cfg); len(got) != 0 {
			t.Fatalf("%v flagged: %+v", cfg, got)
		}
	}
	saved := loadZone
	loadZone = func(string) (*time.Location, error) { return nil, errors.New("no tz database") }
	t.Cleanup(func() { loadZone = saved })
	if got := tzIssues(map[string]any{"timezone": "Asia/Tokio", "model": map[string]any{"provider": "nous"}}); len(got) != 0 {
		t.Fatalf("unverifiable zone flagged: %+v", got)
	}
}

func TestValidationProviderLikeUnknownRootKeepsMisplacedMessage(t *testing.T) {
	issues := validate(map[string]any{"base_url": "https://example.com/v1", "api_key": "secret"})
	misplaced := issuesWhere(issues, func(i ConfigIssue) bool {
		return i.Severity == "warning" && strings.Contains(i.Message, "looks misplaced")
	})
	for _, key := range []string{"base_url", "api_key"} {
		if len(issuesWhere(misplaced, func(i ConfigIssue) bool { return strings.Contains(i.Message, key) })) == 0 {
			t.Fatalf("%s not flagged: %+v", key, misplaced)
		}
	}
}

func TestValidationQuotedListInContainerSlotFlaggedWithRemedy(t *testing.T) {
	issues := validate(map[string]any{
		"plugins":       map[string]any{"enabled": `["a","b"]`},
		"model_catalog": map[string]any{"excluded_providers": `["openai-api"]`},
	})
	flagged := map[string]ConfigIssue{}
	for _, i := range issuesWhere(issues, func(i ConfigIssue) bool { return strings.Contains(i.Message, "quoted string") }) {
		flagged[strings.SplitN(i.Message, " ", 2)[0]] = i
	}
	if len(flagged) != 2 || flagged["plugins.enabled"].Message == "" || flagged["model_catalog.excluded_providers"].Message == "" {
		t.Fatalf("flagged = %+v", flagged)
	}
	if want := `icebath config set plugins.enabled '["a","b"]'`; !strings.Contains(flagged["plugins.enabled"].Hint, want) {
		t.Fatalf("hint = %q", flagged["plugins.enabled"].Hint)
	}
}

func TestValidationStringTypedAndTolerantSlotsNotFlagged(t *testing.T) {
	issues := validate(map[string]any{
		"approvals": map[string]any{"mode": "[off]"},
		"model":     "gpt-4o",
		"agent":     map[string]any{"disabled_toolsets": `["web"]`},
		"plugins":   map[string]any{"enabled": []any{"a"}},
	})
	if got := issuesWhere(issues, func(i ConfigIssue) bool { return strings.Contains(i.Message, "quoted string") }); len(got) != 0 {
		t.Fatalf("flagged = %+v", got)
	}
}

const legacyNote = "the LegacySearch backend was removed in v0.0.0 (alternatives: exa, parallel)"

func legacyRemoved(section, name string) string {
	if section == "web" && strings.ToLower(strings.Trim(strings.TrimSpace(name), `'"`)) == "legacysearch" {
		return legacyNote
	}
	return ""
}

func removedIssues(cfg map[string]any) []ConfigIssue {
	return issuesWhere(ValidateConfigStructure(cfg, legacyRemoved), func(i ConfigIssue) bool {
		return strings.Contains(i.Message, "removed") && strings.Contains(i.Message, "legacysearch")
	})
}

func TestValidationRemovedWebBackend(t *testing.T) {
	if got := removedIssues(map[string]any{"web": map[string]any{"backend": "legacysearch"}}); len(got) != 1 || got[0].Severity != "warning" {
		t.Fatalf("backend: %+v", got)
	}
	for _, key := range []string{"search_backend", "extract_backend"} {
		if got := removedIssues(map[string]any{"web": map[string]any{key: "legacysearch"}}); len(got) != 1 {
			t.Fatalf("%s: %+v", key, got)
		}
	}
	all := map[string]any{"backend": "legacysearch", "search_backend": "legacysearch", "extract_backend": "legacysearch"}
	if got := removedIssues(map[string]any{"web": all}); len(got) != 1 {
		t.Fatalf("same value warned %d times", len(got))
	}
	for _, cfg := range []map[string]any{{"web": map[string]any{"backend": "exa"}}, {"web": map[string]any{}}, {}} {
		if got := removedIssues(cfg); len(got) != 0 {
			t.Fatalf("%v: %+v", cfg, got)
		}
	}
}

func TestConfigStructureIssueLinesTagsAndFirstHintLine(t *testing.T) {
	lines := ConfigStructureIssueLines([]ConfigIssue{
		{Severity: "error", Message: "bad", Hint: "first\nsecond"},
		{Severity: "warning", Message: "meh", Hint: ""},
		{Severity: "warning", Message: "blank", Hint: "\nlater"},
	})
	want := []string{"  [ERROR] bad", "    → first", "  [WARNING] meh", "  [WARNING] blank"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", lines)
	}
}
