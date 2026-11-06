package memory

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Insights report over a window of days, Empty with zero sections when no session matched
type InsightsReport struct {
	Days         int
	SourceFilter string
	Empty        bool
	GeneratedAt  float64
	Overview     *InsightsOverview
	Models       []InsightsModel
	Platforms    []InsightsPlatform
	Tools        []InsightsTool
	Skills       InsightsSkills
	Activity     *InsightsActivity
	TopSessions  []InsightsTopSession
}

// Tool and skill sections of a report computed on their own
type InsightsUsageBreakdown struct {
	Tools  []InsightsTool
	Skills InsightsSkills
}

// Report totals, averages, cost buckets and date range
type InsightsOverview struct {
	TotalSessions                                                                    int
	TotalMessages, TotalToolCalls                                                    InsightsNum
	TotalInputTokens, TotalOutputTokens, TotalCacheReadTokens, TotalCacheWriteTokens int64
	TotalTokens                                                                      int64
	EstimatedCost, ActualCost                                                        float64
	TotalHours, AvgSessionDuration, AvgMessagesPerSession, AvgTokensPerSession       float64
	UserMessages, AssistantMessages, ToolMessages                                    InsightsNum
	DateRangeStart, DateRangeEnd                                                     *float64
	ModelsWithPricing, ModelsWithoutPricing                                          []string
	UnknownCostSessions, IncludedCostSessions                                        int
}

// Usage and cost of one model across the window
type InsightsModel struct {
	Model                                                        string
	Sessions                                                     int
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens InsightsNum
	ReasoningTokens, TotalTokens, APICalls, ToolCalls            InsightsNum
	Cost, ActualCost                                             float64
	CostStatus                                                   string
	HasPricing                                                   bool
}

func (m InsightsModel) tokens() [4]any {
	return [4]any{m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens}
}

// Usage of one source platform across the window
type InsightsPlatform struct {
	Platform                                                     string
	Sessions                                                     int
	Messages                                                     InsightsNum
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens InsightsNum
	TotalTokens, ToolCalls                                       InsightsNum
}

// Calls of one tool and its share of all tool calls
type InsightsTool struct {
	Tool       any
	Count      int64
	Percentage float64
}

// Skill usage summary and ranked skills
type InsightsSkills struct {
	TotalSkillLoads, TotalSkillEdits, TotalSkillActions int64
	DistinctSkillsUsed                                  int
	TopSkills                                           []InsightsSkill
}

// Loads and edits of one skill with its latest use
type InsightsSkill struct {
	Skill                              string
	ViewCount, ManageCount, TotalCount int64
	Percentage                         float64
	LastUsedAt                         any
}

// Session counts by weekday and hour with the active day streak
type InsightsActivity struct {
	ByDay       []InsightsDayCount
	ByHour      []InsightsHourCount
	BusiestDay  InsightsDayCount
	BusiestHour InsightsHourCount
	ActiveDays  int
	MaxStreak   int
}

// Sessions started on one weekday
type InsightsDayCount struct {
	Day   string
	Count int
}

// Sessions started in one hour of the day
type InsightsHourCount struct {
	Hour  int
	Count int
}

// One notable session with its formatted metric
type InsightsTopSession struct {
	Label, SessionID, Value, Date string
}

func insightsSection(title string) []string {
	return []string{"  " + title, "  " + strings.Repeat("─", 56)}
}

// One line per non zero cost bucket, estimated, included and unknown
func insightsCostLines(o *InsightsOverview, templates [3]string) []string {
	var out []string
	if o.EstimatedCost > 0 {
		out = append(out, strings.Replace(templates[0], "{}", insightsFmtEstCost(o.EstimatedCost), 1))
	}
	if o.IncludedCostSessions != 0 {
		out = append(out, strings.Replace(templates[1], "{}", fmt.Sprint(o.IncludedCostSessions), 1))
	}
	if o.UnknownCostSessions != 0 {
		out = append(out, strings.Replace(templates[2], "{}", fmt.Sprint(o.UnknownCostSessions), 1))
	}
	return out
}

// Report rendered for the terminal
func (e *InsightsEngine) FormatTerminal(r *InsightsReport) string {
	if r.Empty {
		src := ""
		if r.SourceFilter != "" {
			src = " (source: " + r.SourceFilter + ")"
		}
		return fmt.Sprintf("  No sessions found in the last %d days%s.", r.Days, src)
	}
	o := r.Overview
	period := fmt.Sprintf("Last %d days", r.Days)
	if r.SourceFilter != "" {
		period += " (" + r.SourceFilter + ")"
	}
	padding := 58 - utf8.RuneCountInString(period) - 2
	left := padding / 2
	if padding < 0 && padding%2 != 0 {
		left--
	}
	lines := []string{
		"",
		"  ╔══════════════════════════════════════════════════════════╗",
		"  ║                    📊 Icebath Insights                   ║",
		"  ║" + strings.Repeat(" ", max(0, left)) + " " + period + " " + strings.Repeat(" ", max(0, padding-left)) + "║",
		"  ╚══════════════════════════════════════════════════════════╝",
		"",
	}
	if o.DateRangeStart != nil && o.DateRangeEnd != nil {
		start, sok := pydatetime.CoerceEpoch(*o.DateRangeStart, "", "")
		end, eok := pydatetime.CoerceEpoch(*o.DateRangeEnd, "", "")
		if sok && eok {
			lines = append(lines, "  Period: "+pydatetime.Strftime(pydatetime.FromTimestamp(start), "%b %d, %Y")+" — "+
				pydatetime.Strftime(pydatetime.FromTimestamp(end), "%b %d, %Y"), "")
		}
	}
	lines = append(lines, insightsSection("📋 Overview")...)
	lines = append(lines,
		"  Sessions:          "+pyFormat(int64(o.TotalSessions), "<12")+"  Messages:        "+pyFormat(o.TotalMessages, ","),
		"  Tool calls:        "+pyFormat(o.TotalToolCalls, "<12,")+"  User messages:   "+pyFormat(o.UserMessages, ","),
		"  Input tokens:      "+pyFormat(o.TotalInputTokens, "<12,")+"  Output tokens:   "+pyFormat(o.TotalOutputTokens, ","),
		"  Total tokens:      "+pyFormat(o.TotalTokens, ","),
	)
	if o.TotalHours > 0 {
		lines = append(lines, "  Active time:       ~"+pyFormat(provider.FormatDurationCompact(o.TotalHours*3600), "<11")+
			"  Avg session:     ~"+provider.FormatDurationCompact(o.AvgSessionDuration))
	}
	lines = append(lines, "  Avg msgs/session:  "+pyFormat(o.AvgMessagesPerSession, ".1f"), "")
	if cost := insightsCostLines(o, [3]string{"  Estimated:          {}", "  Included:           {} session(s) (subscription — no provider invoice)",
		"  Unknown:            {} session(s) (no pricing data)"}); len(cost) > 0 {
		lines = append(append(append(lines, insightsSection("💰 Cost")...), cost...), "")
	}
	if len(r.Models) > 0 {
		lines = append(lines, insightsSection("🤖 Models Used")...)
		lines = append(lines, "  "+pyFormat("Model", "<30")+" "+pyFormat("Sessions", ">8")+" "+pyFormat("Tokens", ">12"))
		for _, m := range r.Models {
			lines = append(lines, "  "+pyFormat(insightsRuneSlice(m.Model, 28), "<30")+" "+pyFormat(int64(m.Sessions), ">8")+" "+pyFormat(m.TotalTokens, ">12,"))
		}
		lines = append(lines, "")
	}
	if len(r.Platforms) > 1 || (len(r.Platforms) > 0 && r.Platforms[0].Platform != "cli") {
		lines = append(lines, insightsSection("📱 Platforms")...)
		lines = append(lines, "  "+pyFormat("Platform", "<14")+" "+pyFormat("Sessions", ">8")+" "+pyFormat("Messages", ">10")+" "+pyFormat("Tokens", ">14"))
		for _, p := range r.Platforms {
			lines = append(lines, "  "+pyFormat(p.Platform, "<14")+" "+pyFormat(int64(p.Sessions), ">8")+" "+pyFormat(p.Messages, ">10,")+" "+pyFormat(p.TotalTokens, ">14,"))
		}
		lines = append(lines, "")
	}
	if len(r.Tools) > 0 {
		lines = append(lines, insightsSection("🔧 Top Tools")...)
		lines = append(lines, "  "+pyFormat("Tool", "<28")+" "+pyFormat("Calls", ">8")+" "+pyFormat("%", ">8"))
		for _, t := range r.Tools[:min(15, len(r.Tools))] {
			lines = append(lines, "  "+pyFormat(t.Tool, "<28")+" "+pyFormat(t.Count, ">8,")+" "+pyFormat(t.Percentage, ">7.1f")+"%")
		}
		if len(r.Tools) > 15 {
			lines = append(lines, fmt.Sprintf("  ... and %d more tools", len(r.Tools)-15))
		}
		lines = append(lines, "")
	}
	if top := r.Skills.TopSkills; len(top) > 0 {
		lines = append(lines, insightsSection("🧠 Top Skills")...)
		lines = append(lines, "  "+pyFormat("Skill", "<28")+" "+pyFormat("Loads", ">7")+" "+pyFormat("Edits", ">7")+" "+pyFormat("Last used", ">11"))
		for _, s := range top[:min(10, len(top))] {
			last := "—"
			if pyrepr.Truthy(s.LastUsedAt) {
				last = insightsDay(s.LastUsedAt)
			}
			lines = append(lines, "  "+pyFormat(insightsRuneSlice(s.Skill, 28), "<28")+" "+pyFormat(s.ViewCount, ">7,")+" "+pyFormat(s.ManageCount, ">7,")+" "+pyFormat(last, ">11"))
		}
		lines = append(lines, fmt.Sprintf("  Distinct skills: %d  Loads: %s  Edits: %s", r.Skills.DistinctSkillsUsed,
			pyFormat(r.Skills.TotalSkillLoads, ","), pyFormat(r.Skills.TotalSkillEdits, ",")), "")
	}
	if act := r.Activity; act != nil && len(act.ByDay) > 0 {
		lines = append(lines, insightsSection("📅 Activity Patterns")...)
		counts := make([]int64, len(act.ByDay))
		for i, d := range act.ByDay {
			counts[i] = int64(d.Count)
		}
		for i, bar := range insightsBarChart(counts, 15) {
			lines = append(lines, fmt.Sprintf("  %s  %s %d", act.ByDay[i].Day, pyFormat(bar, "<15"), act.ByDay[i].Count))
		}
		lines = append(lines, "")
		if busy := insightsBusyHours(act.ByHour); len(busy) > 0 {
			parts := make([]string, len(busy))
			for i, h := range busy {
				parts[i] = fmt.Sprintf("%s (%d)", insightsHour12(h.Hour), h.Count)
			}
			lines = append(lines, "  Peak hours: "+strings.Join(parts, ", "))
		}
		if act.ActiveDays != 0 {
			lines = append(lines, fmt.Sprintf("  Active days: %d", act.ActiveDays))
		}
		if act.MaxStreak > 1 {
			lines = append(lines, fmt.Sprintf("  Best streak: %d consecutive days", act.MaxStreak))
		}
		lines = append(lines, "")
	}
	if len(r.TopSessions) > 0 {
		lines = append(lines, insightsSection("🏆 Notable Sessions")...)
		for _, ts := range r.TopSessions {
			lines = append(lines, "  "+pyFormat(ts.Label, "<20")+" "+pyFormat(ts.Value, "<18")+" ("+ts.Date+", "+ts.SessionID+")")
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// Five busiest non empty hours, ties in hour order
func insightsBusyHours(byHour []InsightsHourCount) []InsightsHourCount {
	sorted := append([]InsightsHourCount{}, byHour...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Count > sorted[j-1].Count; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	var out []InsightsHourCount
	for _, h := range sorted {
		if h.Count > 0 && len(out) < 5 {
			out = append(out, h)
		}
	}
	return out
}

// Shorter report rendered for messaging platforms
func (e *InsightsEngine) FormatGateway(r *InsightsReport) string {
	if r.Empty {
		return fmt.Sprintf("No sessions found in the last %d days.", r.Days)
	}
	o := r.Overview
	lines := []string{
		fmt.Sprintf("📊 **Icebath Insights** — Last %d days\n", r.Days),
		fmt.Sprintf("**Sessions:** %d | **Messages:** %s | **Tool calls:** %s", o.TotalSessions, pyFormat(o.TotalMessages, ","), pyFormat(o.TotalToolCalls, ",")),
		fmt.Sprintf("**Tokens:** %s (in: %s / out: %s)", pyFormat(o.TotalTokens, ","), pyFormat(o.TotalInputTokens, ","), pyFormat(o.TotalOutputTokens, ",")),
	}
	if o.TotalHours > 0 {
		lines = append(lines, "**Active time:** ~"+provider.FormatDurationCompact(o.TotalHours*3600)+" | **Avg session:** ~"+provider.FormatDurationCompact(o.AvgSessionDuration))
	}
	lines = append(lines, "")
	if parts := insightsCostLines(o, [3]string{"{} estimated", "{} included (subscription)", "{} unknown"}); len(parts) > 0 {
		lines = append(lines, "**Cost:** "+strings.Join(parts, " | "), "")
	}
	if len(r.Models) > 0 {
		lines = append(lines, "**🤖 Models:**")
		for _, m := range r.Models[:min(5, len(r.Models))] {
			lines = append(lines, fmt.Sprintf("  %s — %d sessions, %s tokens", insightsRuneSlice(m.Model, 25), m.Sessions, pyFormat(m.TotalTokens, ",")))
		}
		lines = append(lines, "")
	}
	if len(r.Platforms) > 1 {
		lines = append(lines, "**📱 Platforms:**")
		for _, p := range r.Platforms {
			lines = append(lines, fmt.Sprintf("  %s — %d sessions, %s msgs", p.Platform, p.Sessions, pyFormat(p.Messages, ",")))
		}
		lines = append(lines, "")
	}
	if len(r.Tools) > 0 {
		lines = append(lines, "**🔧 Top Tools:**")
		for _, t := range r.Tools[:min(8, len(r.Tools))] {
			lines = append(lines, fmt.Sprintf("  %s — %s calls (%s%%)", pyrepr.Str(t.Tool), pyFormat(t.Count, ","), pyFormat(t.Percentage, ".1f")))
		}
		lines = append(lines, "")
	}
	if top := r.Skills.TopSkills; len(top) > 0 {
		lines = append(lines, "**🧠 Top Skills:**")
		for _, s := range top[:min(5, len(top))] {
			suffix := ""
			if pyrepr.Truthy(s.LastUsedAt) {
				suffix = ", last used " + insightsDay(s.LastUsedAt)
			}
			lines = append(lines, fmt.Sprintf("  %s — %s loads, %s edits%s", s.Skill, pyFormat(s.ViewCount, ","), pyFormat(s.ManageCount, ","), suffix))
		}
		lines = append(lines, "")
	}
	if act := r.Activity; act != nil {
		lines = append(lines, fmt.Sprintf("**📅 Busiest:** %ss (%d sessions), %s (%d sessions)", act.BusiestDay.Day, act.BusiestDay.Count,
			insightsHour12(act.BusiestHour.Hour), act.BusiestHour.Count))
		if act.ActiveDays != 0 {
			lines = append(lines, fmt.Sprintf("**Active days:** %d", act.ActiveDays))
		}
		if act.MaxStreak > 1 {
			lines = append(lines, fmt.Sprintf("**Best streak:** %d consecutive days", act.MaxStreak))
		}
	}
	return strings.Join(lines, "\n")
}

// The insights engine is constructed per report by the CLI, gateway and dashboard callers
func registerInsightsEngine() {}
