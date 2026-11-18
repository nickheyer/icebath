package tui

import (
	"encoding/json"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"math"
	"net/url"
	"os/exec"
	"reflect"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Command that opens a URL with the OS default handler, nil on a platform without one
func openCommand(platformID string) []string {
	switch platformID {
	case "darwin":
		return []string{"open"}
	case "windows":
		return []string{"explorer.exe"}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return []string{"xdg-open"}
	}
	return nil
}

// Web URL as the WHATWG parser serializes it, nil for anything but http or https with a host
func parseSafeURL(value string) *url.URL {
	value = strings.Trim(value, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	value = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(value)
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || strings.TrimFunc(u.Hostname(), isJSSpace) == "" {
		return nil
	}
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	return u
}

// Starts the URL opener detached from the terminal, false when the URL or platform has none
func openExternalURL(raw string) bool {
	u := parseSafeURL(raw)
	command := openCommand(runtime.GOOS)
	if u == nil || command == nil {
		return false
	}
	cmd := exec.Command(command[0], append(command[1:], u.String())...)
	cmd.SysProcAttr = home.DetachAttr(false)
	if cmd.Start() != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// Labels and text of the billing wall dialog
type billingDialog struct {
	CancelLabel  string
	ConfirmLabel string
	Detail       string
	Title        string
}

// Dialog of an out of credits or payment required block
func billingDialogCopy(block *tuigateway.BillingBlock) billingDialog {
	label := tr("libText.billingDialog.yourProvider")
	if block.ProviderLabel != "" {
		label = block.ProviderLabel
	}
	confirm := tr("libText.billingDialog.switchProvider")
	if val(block.BillingURL) != "" {
		confirm = tr("libText.billingDialog.openBillingPage")
	}
	return billingDialog{CancelLabel: tr("libText.billingDialog.dismiss"), ConfirmLabel: confirm, Detail: tr("libText.billingDialog.providerDetail", label), Title: tr("libText.billingDialog.providerTitle", label)}
}

// Title of the setup required panel
func setupRequiredTitle() string { return tr("content.setup.title") }

// Sections of the setup required panel, the intro and the actions
func buildSetupRequiredSections() []panelSection {
	intro, actions := tr("content.setup.intro"), tr("content.setup.actions")
	return []panelSection{
		{Text: &intro},
		{Rows: [][2]string{{"/setup", tr("content.setup.setupRow")}, {"/model", tr("content.setup.modelRow")}, {"Ctrl+C", tr("content.setup.exitRow")}}, Title: &actions},
	}
}

// Whole percent of part in whole clamped to 0 through 100
func usagePct(part, whole float64) float64 {
	return math.Max(0, math.Min(100, jsRound(part/whole*100)))
}

// First counter that is set and non zero, else the first one set
func firstCounter(sources ...*int64) (int64, bool) {
	var first *int64
	for _, source := range sources {
		if source != nil && *source != 0 {
			return *source, true
		}
		if source != nil && first == nil {
			first = source
		}
	}
	return val(first), first != nil
}

// Status bar usage of a usage tick or stats answer, its usage view or one built from the flat counters, nil when it carries nothing
func usageOfCounters(p tuigateway.SessionUsagePayload) *tuigateway.Usage {
	if p.Usage != nil {
		return p.Usage
	}
	u, set := tuigateway.Usage{}, false
	for _, counter := range []struct {
		into    *int64
		sources []*int64
	}{{&u.Calls, []*int64{p.APICalls}}, {&u.Input, []*int64{p.InputTokens, p.PromptTokens}}, {&u.Output, []*int64{p.OutputTokens, p.CompletionTokens}},
		{&u.Reasoning, []*int64{p.ReasoningTokens}}, {&u.Prompt, []*int64{p.PromptTokens}}, {&u.Completion, []*int64{p.CompletionTokens}}, {&u.Total, []*int64{p.TotalTokens}}} {
		if n, ok := firstCounter(counter.sources...); ok {
			*counter.into, set = n, true
		}
	}
	if p.ContextSource != nil && p.CompressionCount != nil {
		u.Compressions, set = p.CompressionCount, true
	}
	if used, limit := val(p.LastPromptTokens), val(p.ContextLength); used != 0 && limit != 0 {
		source := "provider_usage"
		if named := val(p.ContextSource); named != "" {
			source = named
		}
		clamped := min(used, limit)
		estimated, pct := source != "provider_usage", int64(usagePct(float64(clamped), float64(limit)))
		u.ContextEstimated, u.ContextMax, u.ContextPercent, u.ContextSource, u.ContextUsed, set = &estimated, &limit, &pct, &source, &clamped, true
	}
	if prompt, cached := val(p.PromptTokens), val(p.CacheReadTokens); prompt > 0 && cached > 0 {
		pct := int64(usagePct(float64(cached), float64(prompt)))
		u.CacheHitPct, set = &pct, true
	}
	if p.AvgLatencyS != nil {
		latency := jsRound(*p.AvgLatencyS*10) / 10
		u.AvgLatencyS, set = &latency, true
	}
	if p.TokensPerS != nil {
		tps := jsRound(*p.TokensPerS*10) / 10
		u.AvgTPS, set = &tps, true
	}
	if p.ActiveSubagents != nil {
		u.ActiveSubagents, set = p.ActiveSubagents, true
	}
	if !set {
		return nil
	}
	u.Model = val(p.Model)
	return &u
}

// Usage of a fresh session
func zeroUsage() *tuigateway.Usage { return &tuigateway.Usage{} }

// Usage with a newer snapshot laid over it, the held one kept when nothing changed
func mergeUsageStable(prev, patch *tuigateway.Usage) *tuigateway.Usage {
	if patch == nil {
		return prev
	}
	merged := *usageOf(prev)
	tuigateway.Merge(&merged, *patch)
	if prev != nil && reflect.DeepEqual(*prev, merged) {
		return prev
	}
	return &merged
}

// Usage or an empty one, so every counter reads as zero without a session
func usageOf(u *tuigateway.Usage) *tuigateway.Usage {
	if u == nil {
		return zeroUsage()
	}
	return u
}

// String of a JSON value as JavaScript renders it, arrays joined by commas
func jsValueString(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			if item != nil {
				parts[i] = jsValueString(item)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return jsString(v)
}

// Field as a JavaScript template literal renders it, undefined when absent
func jsTemplate(m map[string]any, k string) string {
	v, has := m[k]
	if !has {
		return "undefined"
	}
	return jsValueString(v)
}

// Answer of a multi select question as its comma joined items, other answers as typed
func clarifyAnswerText(answer string, multiSelect bool) string {
	if !multiSelect {
		return answer
	}
	var parsed any
	if json.Unmarshal([]byte(answer), &parsed) != nil {
		return answer
	}
	items, isList := parsed.([]any)
	if !isList {
		return answer
	}
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = jsValueString(item)
	}
	return strings.Join(parts, ", ")
}

// Transcript block of an unanswered clarify, each question with its locked answer and why it ended
func formatAbandonedClarify(questions []clarifyQuestion, answers map[string]string, reason string) string {
	lines := []string{tr("libText.text.clarifyHead", len(questions))}
	for _, q := range questions {
		if answer := answers[q.QID]; answer != "" {
			lines = append(lines, "  "+tr("libText.text.clarifyAnswered", q.Question, clarifyAnswerText(answer, q.MultiSelect)))
		} else {
			lines = append(lines, "  "+tr("libText.text.clarifyUnanswered", q.Question))
		}
	}
	return strings.Join(append(lines, "  "+tr("libText.text.clarifyReason", reason)), "\n")
}

// Subagents with no parent in the same snapshot
func topLevelSubagents(items []subagentProgress) []subagentProgress {
	ids := map[string]bool{}
	for _, s := range items {
		ids[s.ID] = true
	}
	out := []subagentProgress{}
	for _, s := range items {
		if s.ParentID == "" || !ids[s.ParentID] {
			out = append(out, s)
		}
	}
	return out
}

// Number of a JSON value as JavaScript Number converts it, NaN when it has none
func jsNumberOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		if f, ok := jsNumber(t); ok {
			return f
		}
	case nil:
		return 0
	}
	return math.NaN()
}

// Whether two decoded JSON values are equal under JavaScript ===, compound values never
func jsStrictEqual(a, b any) bool {
	switch a.(type) {
	case nil, bool, float64, string:
		return a == b
	}
	return false
}
