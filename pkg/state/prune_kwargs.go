package state

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Filter keyword names the prune store call accepts beside older_than_days
var PruneFilterNames = map[string]bool{
	"last_active_before": true, "last_active_after": true, "started_before": true, "started_after": true,
	"source": true, "title_like": true, "end_reason": true, "cwd_prefix": true, "min_messages": true,
	"max_messages": true, "model_like": true, "provider": true, "user_id": true, "chat_id": true,
	"chat_type": true, "branch_like": true, "min_tokens": true, "max_tokens": true, "min_cost": true,
	"max_cost": true, "min_tool_calls": true, "max_tool_calls": true,
	"archived": true, "include_pinned": true, "lineage_tips_only": true,
}

// Number of a filter dict value as the store binds it, nil for None
func FilterNumber(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	default:
		return nil
	}
	return &f
}

// Store keyword arguments of a filter dict, the retention bound, the source and the remaining filters
func PruneKwargs(filters map[string]any) (*float64, string, PruneFilters) {
	text := func(key string) string {
		s, _ := filters[key].(string)
		return s
	}
	f := PruneFilters{
		IncludePinned: pyrepr.Truthy(filters["include_pinned"]), LineageTipsOnly: pyrepr.Truthy(filters["lineage_tips_only"]),
		IncludeOpen:      pyrepr.Truthy(filters["include_open"]),
		LastActiveBefore: FilterNumber(filters["last_active_before"]), LastActiveAfter: FilterNumber(filters["last_active_after"]),
		StartedBefore: FilterNumber(filters["started_before"]), StartedAfter: FilterNumber(filters["started_after"]),
		TitleLike: text("title_like"), EndReason: text("end_reason"), CwdPrefix: text("cwd_prefix"),
		MinMessages: FilterNumber(filters["min_messages"]), MaxMessages: FilterNumber(filters["max_messages"]),
		ModelLike: text("model_like"), Provider: text("provider"), UserID: text("user_id"), ChatID: text("chat_id"),
		ChatType: text("chat_type"), BranchLike: text("branch_like"),
		MinTokens: FilterNumber(filters["min_tokens"]), MaxTokens: FilterNumber(filters["max_tokens"]),
		MinCost: FilterNumber(filters["min_cost"]), MaxCost: FilterNumber(filters["max_cost"]),
		MinToolCalls: FilterNumber(filters["min_tool_calls"]), MaxToolCalls: FilterNumber(filters["max_tool_calls"]),
	}
	if archived, ok := filters["archived"].(bool); ok {
		f.Archived = &archived
	}
	return FilterNumber(filters["older_than_days"]), text("source"), f
}
