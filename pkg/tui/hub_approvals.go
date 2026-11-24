package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Settings group whose screen carries the approval tools: the proposals from the approval history and the check of a command
const safetyGroup = "safety"

// Allowed commands the approval history proposes, the most approved first: enter adds the highlighted one after a question and edit changes how
// much history is read; done runs after every addition so the group's rows show the new allowed commands
func (a *app) approvalProposals(h *cards.Hub, done func() tea.Cmd) tea.Cmd {
	var (
		params tuigateway.ApprovalsSuggestParams
		result tuigateway.ApprovalsSuggestResult
		loaded bool
		list   *cards.List
	)
	load := func() tea.Cmd {
		return hubCall(a, h, tr(txt.HubsApprovalsReading), tuigateway.ApprovalsSuggest, params, func(r *tuigateway.ApprovalsSuggestResult) tea.Cmd {
			result, loaded = *r, true
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.ApprovalProposal, bool) {
		if i < 0 || i >= len(result.Proposals) {
			return tuigateway.ApprovalProposal{}, false
		}
		return result.Proposals[i], true
	}
	allow := func(l *cards.List, i int) tea.Cmd {
		p, ok := at(i)
		if !ok {
			return nil
		}
		l.Confirm(tr(txt.HubsApprovalsAllowConfirm, p.Pattern), func() tea.Cmd {
			return hubCall(a, h, "", tuigateway.ApprovalsAllow, tuigateway.ApprovalsAllowParams{Patterns: []string{p.Pattern}}, func(r *tuigateway.ApprovalsAllowResult) tea.Cmd {
				if !slices.Contains(r.Added, p.Pattern) {
					return h.Fail(tr(txt.HubsApprovalsNotSaved, p.Pattern))
				}
				return tea.Batch(h.Done(tr(txt.HubsApprovalsAllowed, p.Pattern)), load(), done())
			})
		})
		return nil
	}
	window := func(*cards.List, int) tea.Cmd {
		days := int64(90)
		if params.Days != nil {
			days = *params.Days
		}
		number := func(n, fallback int64) string {
			if n <= 0 {
				n = fallback
			}
			return strconv.FormatInt(n, 10)
		}
		spec := cards.FormSpec{Title: tr(txt.HubsApprovalsWindowTitle), Submit: tr(txt.HubsApprovalsWindowSubmit), Fields: []cards.Field{
			{ID: "days", Label: tr(txt.HubsApprovalsDaysLabel), Kind: cards.FieldNumber, Value: strconv.FormatInt(days, 10), Help: tr(txt.HubsApprovalsDaysHelp)},
			{ID: "min", Label: tr(txt.HubsApprovalsMinCountLabel), Kind: cards.FieldNumber, Value: number(params.MinCount, 2)},
			{ID: "limit", Label: tr(txt.HubsApprovalsLimitLabel), Kind: cards.FieldNumber, Value: number(params.Limit, 20)},
		}}
		return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
			read := func(id string) (int64, bool) {
				n, err := strconv.ParseInt(strings.TrimSpace(v[id]), 10, 64)
				return n, err == nil && n >= 0
			}
			d, okDays := read("days")
			m, okMin := read("min")
			l, okLimit := read("limit")
			if !okDays || !okMin || !okLimit {
				return h.Fail(tr(txt.KitFormNumber))
			}
			params = tuigateway.ApprovalsSuggestParams{Days: &d, MinCount: m, Limit: l}
			return load()
		})
	}
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsApprovalsTitle), Glyph: a.toolkit.Glyphs.Settings, Key: "tui:settings:approvals", Context: settingsRowsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsApprovalsColPattern), Max: 40}, {Title: tr(txt.HubsApprovalsColApproved)}, {Title: tr(txt.HubsApprovalsColExample), Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			out := make([][]string, len(result.Proposals))
			for i, p := range result.Proposals {
				example := ""
				if len(p.Examples) > 0 {
					example = p.Examples[0]
				}
				out[i] = []string{p.Pattern, strconv.FormatInt(p.Count, 10) + "×", example}
			}
			return out
		},
		Detail: func(i int) []string {
			p, ok := at(i)
			if !ok {
				return nil
			}
			return approvalProposalDetail(p)
		},
		Banner: func() string {
			if !loaded {
				return ""
			}
			return approvalsWindowText(result.Days) + " · " + tr(txt.HubsApprovalsAllowedCount, result.Allowed)
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsApprovalsEmpty, approvalsWindowText(result.Days))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if _, ok := at(i); !ok {
				return []wm.Hint{{Action: "hub.edit", Label: tr(txt.HintsLabelChange)}}
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HubsApprovalsAllowLabel)}, {Action: "hub.edit", Label: tr(txt.HintsLabelChange)}}
		},
		Free: map[string]bool{"hub.edit": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":     allow,
			"hub.edit":    window,
			"hub.refresh": func(*cards.List, int) tea.Cmd { return load() },
		},
	})
	return tea.Batch(h.Push(list), load())
}

// How much approval history a reading covers, in words
func approvalsWindowText(days int64) string {
	if days <= 0 {
		return tr(txt.HubsApprovalsWindowAll)
	}
	return tr(txt.HubsApprovalsWindowDays, days)
}

// Lines of a proposal's detail pane: the pattern and what it covers, how often it was approved, its risky classes and the commands approved
func approvalProposalDetail(p tuigateway.ApprovalProposal) []string {
	covers := tr(txt.HubsApprovalsKindGlob)
	if p.Kind == "class" {
		covers = tr(txt.HubsApprovalsKindClass)
	}
	out := []string{p.Pattern + " · " + covers, tr(txt.HubsApprovalsApprovedTimes, p.Count)}
	if len(p.Classes) > 0 {
		out = append(out, tr(txt.HubsApprovalsClassesTitle))
		for _, c := range p.Classes {
			out = append(out, "  "+c)
		}
	}
	if len(p.Examples) > 0 {
		out = append(out, tr(txt.HubsApprovalsExamplesTitle))
		for _, e := range p.Examples {
			out = append(out, "  "+e)
		}
	}
	return out
}

// Asks for a command and shows what the approval rules do with it where this profile runs commands, the command never run
func (a *app) approvalCheck(h *cards.Hub) tea.Cmd {
	return a.hubAsk(h, tr(txt.HubsApprovalsCheckTitle), tr(txt.HubsApprovalsCheckLabel), "", false, func(command string) tea.Cmd {
		if command == "" {
			return nil
		}
		return hubCall(a, h, "", tuigateway.ApprovalsTest, tuigateway.ApprovalsTestParams{Command: command}, func(r *tuigateway.ApprovalsTestResult) tea.Cmd {
			return a.hubLinesPage(h, tr(txt.HubsApprovalsCheckTitle), approvalVerdictLines(*r))
		})
	})
}

// Lines of a verdict: the command, where it would run, what the rules do with it, the rule that decided and the spellings the rules read
func approvalVerdictLines(r tuigateway.ApprovalsTestResult) []string {
	out := []string{tr(txt.HubsApprovalsCheckCommand, r.Command), tr(txt.HubsApprovalsCheckWhere, r.EnvType), tr(txt.HubsApprovalsCheckVerdict, approvalVerdictText(r.Verdict))}
	if r.Rule != "" {
		out = append(out, tr(txt.HubsApprovalsCheckRule, r.Rule))
	}
	if r.Detail != "" {
		out = append(out, r.Detail)
	}
	if len(r.Variants) > 0 {
		out = append(out, "", tr(txt.HubsApprovalsCheckVariants))
		for _, v := range r.Variants {
			out = append(out, "  "+v)
		}
	}
	return out
}

// What the approval rules do with a command, in plain words
func approvalVerdictText(v tuigateway.ApprovalVerdict) string {
	switch v {
	case "allow":
		return tr(txt.HubsApprovalsVerdictAllow)
	case "ask-approval":
		return tr(txt.HubsApprovalsVerdictAsk)
	case "hardline-deny":
		return tr(txt.HubsApprovalsVerdictHardline)
	case "user-deny":
		return tr(txt.HubsApprovalsVerdictUserDeny)
	}
	return string(v)
}
