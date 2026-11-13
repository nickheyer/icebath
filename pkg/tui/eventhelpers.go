package tui

import (
	"encoding/json"
	"math"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Command that hands a URL to the desktop, nil on a platform without one
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
	SwitchValue  string
}

// Dialog of an out of credits or payment required block
func billingDialogCopy(block map[string]any) billingDialog {
	if truthyJS(block["is_nous"]) {
		return billingDialog{CancelLabel: tr("libText.billingDialog.dismiss"), ConfirmLabel: tr("libText.billingDialog.topUp"), Detail: tr("libText.billingDialog.nousDetail"), Title: tr("libText.billingDialog.nousTitle")}
	}
	label := tr("libText.billingDialog.yourProvider")
	if truthyJS(block["provider_label"]) {
		label = jsString(block["provider_label"])
	}
	confirm := tr("libText.billingDialog.switchProvider")
	if truthyJS(block["billing_url"]) {
		confirm = tr("libText.billingDialog.openBillingPage")
	}
	return billingDialog{CancelLabel: tr("libText.billingDialog.dismiss"), ConfirmLabel: confirm, Detail: tr("libText.billingDialog.providerDetail", label), Title: tr("libText.billingDialog.providerTitle", label)}
}

var creditsStep = map[string]string{
	"add_credits":                "libText.billingDialog.limitCreditsAdd",
	"add_credits_and_keep_going": "libText.billingDialog.limitCreditsBoth",
	"use_credits":                "libText.billingDialog.limitCreditsUse",
}

// Dialog of a window plan's limit, switching to its first suggestion when it names one
func usageLimitDialog(limit map[string]any) billingDialog {
	var suggestion map[string]any
	if list, _ := limit["suggestions"].([]any); len(list) > 0 {
		suggestion, _ = list[0].(map[string]any)
	}
	sentence := ""
	if truthyJS(limit["message"]) {
		sentence = strings.TrimFunc(jsString(limit["message"]), isJSSpace)
	}
	credits := ""
	creditsInfo, _ := limit["credits"].(map[string]any)
	if action, isStr := creditsInfo["action"].(string); isStr {
		if key, known := creditsStep[action]; known {
			credits = tr(key)
		}
	}
	detail := []string{}
	for _, part := range []string{sentence, credits} {
		if part != "" {
			detail = append(detail, part)
		}
	}
	if suggestion == nil {
		return billingDialog{CancelLabel: tr("libText.billingDialog.dismiss"), ConfirmLabel: tr("libText.billingDialog.limitOpenTopup"), Detail: strings.Join(detail, "\n"), Title: tr("libText.billingDialog.limitTitle")}
	}
	note := ""
	if truthyJS(suggestion["note"]) {
		note = " (" + jsString(suggestion["note"]) + ")"
	}
	provider := "nous"
	if truthyJS(suggestion["provider"]) {
		provider = jsString(suggestion["provider"])
	}
	return billingDialog{
		CancelLabel:  tr("libText.billingDialog.dismiss"),
		ConfirmLabel: tr("libText.billingDialog.limitSwitchTo", jsTemplate(suggestion, "label")+note),
		Detail:       strings.Join(detail, "\n"),
		SwitchValue:  jsTemplate(suggestion, "model") + " --provider " + provider + " --session",
		Title:        tr("libText.billingDialog.limitTitle"),
	}
}

// Title of the setup required panel
func setupRequiredTitle() string { return tr("content.setup.title") }

// Sections of the setup required panel, the intro, the actions and the footer
func buildSetupRequiredSections() []panelSection {
	intro, actions, footer := tr("content.setup.intro"), tr("content.setup.actions"), tr("content.setup.footer")
	return []panelSection{
		{Text: &intro},
		{Rows: [][2]string{{"/setup", tr("content.setup.setupRow")}, {"/model", tr("content.setup.modelRow")}, {"Ctrl+C", tr("content.setup.exitRow")}}, Title: &actions},
		{Text: &footer},
	}
}

// Finite number of a payload field, false when absent or not a finite number
func finiteNum(p map[string]any, k string) (float64, bool) {
	n, isNum := p[k].(float64)
	return n, isNum && !math.IsInf(n, 0) && !math.IsNaN(n)
}

// Whole percent of part in whole clamped to 0 through 100
func usagePct(part, whole float64) float64 {
	return math.Max(0, math.Min(100, jsRound(part/whole*100)))
}

var usageCounters = []struct {
	key     string
	sources []string
}{
	{"calls", []string{"api_calls"}},
	{"input", []string{"input_tokens", "prompt_tokens"}},
	{"output", []string{"output_tokens", "completion_tokens"}},
	{"reasoning", []string{"reasoning_tokens"}},
	{"prompt", []string{"prompt_tokens"}},
	{"completion", []string{"completion_tokens"}},
	{"total", []string{"total_tokens"}},
}

// Status bar usage of a live tick or stats payload, nil when it carries nothing
func usageFromTick(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	if nested, has := p["usage"]; has && truthyJS(nested) {
		out, _ := nested.(map[string]any)
		return out
	}
	usage := map[string]any{}
	for _, counter := range usageCounters {
		var first *float64
		for _, source := range counter.sources {
			if n, ok := finiteNum(p, source); ok {
				if n != 0 {
					first = &n
					break
				}
				if first == nil {
					first = &n
				}
			}
		}
		if first != nil {
			usage[counter.key] = *first
		}
	}
	used, _ := finiteNum(p, "last_prompt_tokens")
	limit, _ := finiteNum(p, "context_length")
	compressions, hasCompressions := finiteNum(p, "compression_count")
	if p["context_source"] != nil && hasCompressions {
		usage["compressions"] = compressions
	}
	if used != 0 && limit != 0 {
		source := "provider_usage"
		if truthyJS(p["context_source"]) {
			source = jsString(p["context_source"])
		}
		clamped := math.Min(used, limit)
		usage["context_estimated"] = source != "provider_usage"
		usage["context_max"] = limit
		usage["context_percent"] = usagePct(clamped, limit)
		usage["context_source"] = source
		usage["context_used"] = clamped
	}
	promptTotal, _ := finiteNum(p, "prompt_tokens")
	cacheRead, _ := finiteNum(p, "cache_read_tokens")
	if promptTotal > 0 && cacheRead > 0 {
		usage["cache_hit_pct"] = usagePct(cacheRead, promptTotal)
	}
	if latency, ok := finiteNum(p, "avg_latency_s"); ok {
		usage["avg_latency_s"] = jsRound(latency*10) / 10
	}
	if tps, ok := finiteNum(p, "tokens_per_s"); ok {
		usage["avg_tps"] = jsRound(tps*10) / 10
	}
	if subagents, ok := finiteNum(p, "active_subagents"); ok {
		usage["active_subagents"] = subagents
	}
	if truthyJS(p["plan_gauge"]) {
		usage["plan_gauge"] = p["plan_gauge"]
	}
	if len(usage) == 0 {
		return nil
	}
	if truthyJS(p["model"]) {
		usage["model"] = p["model"]
	}
	return usage
}

// Usage of a fresh session
func zeroUsage() map[string]any {
	return map[string]any{"calls": float64(0), "input": float64(0), "output": float64(0), "total": float64(0)}
}

// Whether any key of either usage differs by strict equality
func usageChanged(prev, next map[string]any) bool {
	for k, v := range prev {
		if w, has := next[k]; !has || !jsStrictEqual(v, w) {
			return true
		}
	}
	for k := range next {
		if _, has := prev[k]; !has {
			return true
		}
	}
	return false
}

// Usage merged with a patch, the previous map kept when nothing changed
func mergeUsageStable(prev, patch map[string]any) map[string]any {
	if patch == nil {
		return prev
	}
	merged := make(map[string]any, len(prev)+len(patch))
	for k, v := range prev {
		merged[k] = v
	}
	for k, v := range patch {
		merged[k] = v
	}
	if usageChanged(prev, merged) {
		return merged
	}
	return prev
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
