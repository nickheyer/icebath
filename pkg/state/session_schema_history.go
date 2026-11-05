package state

import (
	"iter"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One edit to a declared column order, After empty inserting at the front, Op minus removing Column
type SchemaEdit struct {
	Op     string
	Column string
	After  string
}

// One shipped release that changed a declaration, labelled by sequence number, commit time and sha
type SchemaEvent struct {
	Label string
	Edits []SchemaEdit
}

// Declared order a table first shipped with and every later edit in replay order
type TableHistory struct {
	Base   []string
	Events []SchemaEvent
}

// Python list.index ValueError text for a missing column
func columnNotInList(column string) error {
	return &pyConvError{"ValueError", pyrepr.Repr(column) + " is not in list"}
}

// Replays edits onto columns, a missing column raising the ValueError list.remove or list.index raise
func applySchemaEdits(columns []string, edits []SchemaEdit) ([]string, error) {
	for _, edit := range edits {
		if edit.Op == "-" {
			i := slices.Index(columns, edit.Column)
			if i < 0 {
				return nil, &pyConvError{"ValueError", "list.remove(x): x not in list"}
			}
			columns = slices.Delete(columns, i, i+1)
			continue
		}
		at := 0
		if edit.After != "" {
			i := slices.Index(columns, edit.After)
			if i < 0 {
				return nil, columnNotInList(edit.After)
			}
			at = i + 1
		}
		columns = slices.Insert(columns, at, edit.Column)
	}
	return columns, nil
}

// Every declared column order the table has shipped with, oldest first, unknown tables a KeyError
func DeclaredSnapshots(table string) ([][]string, error) {
	history, ok := SchemaHistory[table]
	if !ok {
		return nil, &pyConvError{"KeyError", pyrepr.Repr(table)}
	}
	columns := slices.Clone(history.Base)
	snapshots := [][]string{slices.Clone(columns)}
	for _, event := range history.Events {
		var err error
		if columns, err = applySchemaEdits(columns, event.Edits); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, slices.Clone(columns))
	}
	return snapshots, nil
}

// Column order the table declares today
func CurrentDeclaredColumns(table string) ([]string, error) {
	snapshots, err := DeclaredSnapshots(table)
	if err != nil {
		return nil, err
	}
	return snapshots[len(snapshots)-1], nil
}

// Every physical layout an upgraded store of the table can have, accept pruning a branch when it returns false
func ReachablePhysicalLayouts(table string, accept func(layout []string, firstNewIndex int) bool) (iter.Seq[[]string], error) {
	snapshots, err := DeclaredSnapshots(table)
	if err != nil {
		return nil, err
	}
	type node struct {
		layout []string
		index  int
	}
	key := func(layout []string) string { return strings.Join(layout, "\x00") }
	return func(yield func([]string) bool) {
		seen := map[string]int{}
		var frontier []node
		for index, snapshot := range snapshots {
			k := key(snapshot)
			if _, dup := seen[k]; dup {
				continue
			}
			if accept != nil && !accept(slices.Clone(snapshot), 0) {
				continue
			}
			seen[k] = index
			frontier = append(frontier, node{snapshot, index})
			if !yield(slices.Clone(snapshot)) {
				return
			}
		}
		for len(frontier) > 0 {
			var next []node
			for _, n := range frontier {
				have := make(map[string]bool, len(n.layout))
				for _, c := range n.layout {
					have[c] = true
				}
				for later := n.index + 1; later < len(snapshots); later++ {
					candidate := slices.Clone(n.layout)
					for _, c := range snapshots[later] {
						if !have[c] {
							candidate = append(candidate, c)
						}
					}
					if len(candidate) == len(n.layout) {
						continue
					}
					k := key(candidate)
					if at, dup := seen[k]; dup && at <= later {
						continue
					}
					if accept != nil && !accept(slices.Clone(candidate), len(n.layout)) {
						continue
					}
					seen[k] = later
					next = append(next, node{candidate, later})
					if !yield(slices.Clone(candidate)) {
						return
					}
				}
			}
			frontier = next
		}
	}, nil
}

var SchemaHistory = map[string]TableHistory{
	"sessions": {
		Base: []string{
			"id", "source", "user_id", "model", "model_config",
			"system_prompt", "parent_session_id", "started_at", "ended_at",
			"end_reason", "message_count", "tool_call_count", "input_tokens",
			"output_tokens",
		},
		Events: []SchemaEvent{
			{"01 2026-03-08T23:32Z c5e8166c8b", []SchemaEdit{{"+", "title", "output_tokens"}}},
			{"02 2026-03-17T10:44Z d417ba2a48", []SchemaEdit{
				{"+", "cache_read_tokens", "output_tokens"},
				{"+", "cache_write_tokens", "cache_read_tokens"},
				{"+", "reasoning_tokens", "cache_write_tokens"},
				{"+", "billing_provider", "reasoning_tokens"},
				{"+", "billing_base_url", "billing_provider"},
				{"+", "billing_mode", "billing_base_url"},
				{"+", "estimated_cost_usd", "billing_mode"},
				{"+", "actual_cost_usd", "estimated_cost_usd"},
				{"+", "cost_status", "actual_cost_usd"},
				{"+", "cost_source", "cost_status"},
				{"+", "pricing_version", "cost_source"},
			}},
			{"03 2026-04-22T12:51Z 5fb143169b", []SchemaEdit{{"+", "api_call_count", "title"}}},
			{"04 2026-05-10T20:06Z 878611a79d", []SchemaEdit{
				{"+", "handoff_pending", "api_call_count"},
				{"+", "handoff_platform", "handoff_pending"},
			}},
			{"05 2026-05-10T20:06Z 00ce5f04d9", []SchemaEdit{
				{"-", "handoff_pending", ""},
				{"+", "handoff_state", "api_call_count"},
				{"+", "handoff_error", "handoff_platform"},
			}},
			{"06 2026-05-31T22:46Z 51c68d4ab1", []SchemaEdit{{"+", "cwd", "reasoning_tokens"}}},
			{"07 2026-06-01T08:22Z 3e59be0c41", []SchemaEdit{{"+", "rewind_count", "handoff_error"}}},
			{"08 2026-06-02T01:41Z 85b65e29f0", []SchemaEdit{{"+", "archived", "rewind_count"}}},
			{"09 2026-06-25T22:49Z ffa3d3c811", []SchemaEdit{
				{"+", "git_branch", "cwd"},
				{"+", "git_repo_root", "git_branch"},
			}},
			{"10 2026-06-28T22:10Z 86e64900b9", []SchemaEdit{
				{"+", "session_key", "user_id"},
				{"+", "chat_id", "session_key"},
				{"+", "chat_type", "chat_id"},
				{"+", "thread_id", "chat_type"},
			}},
			{"11 2026-06-30T08:06Z f2ccb2859f", []SchemaEdit{
				{"+", "compression_failure_cooldown_until", "handoff_error"},
				{"+", "compression_failure_error", "compression_failure_cooldown_until"},
			}},
			{"12 2026-07-05T21:01Z 747386ecfa", []SchemaEdit{
				{"+", "display_name", "thread_id"},
				{"+", "origin_json", "display_name"},
				{"+", "expiry_finalized", "origin_json"},
			}},
			{"13 2026-07-13T20:49Z af7dceaf77", []SchemaEdit{
				{"+", "compression_fallback_streak", "compression_failure_error"},
			}},
			{"14 2026-07-15T16:50Z e8b7ce8c19", []SchemaEdit{{"+", "profile_name", "compression_fallback_streak"}}},
			{"15 2026-07-23T15:08Z ec5835ab8b", []SchemaEdit{
				{"+", "compression_ineffective_count", "compression_fallback_streak"},
			}},
			{"16 2026-07-24T16:15Z 951d606730", []SchemaEdit{{"+", "pinned", "archived"}}},
			{"17 2026-07-27T19:14Z cfb206fe2e", []SchemaEdit{
				{"+", "last_activity_at", "title"},
				{"+", "last_activity_description", "last_activity_at"},
				{"+", "last_activity_provenance", "last_activity_description"},
			}},
			{"18 2026-07-27T19:22Z 5646fed97e", []SchemaEdit{
				{"-", "last_activity_at", ""},
				{"-", "last_activity_description", ""},
				{"-", "last_activity_provenance", ""},
			}},
			{"19 2026-08-02T23:16Z c2088efe9e", []SchemaEdit{
				{"+", "last_activity_at", "title"},
				{"+", "last_activity_description", "last_activity_at"},
				{"+", "last_activity_provenance", "last_activity_description"},
			}},
			{"20 2026-08-03T15:07Z 7d066c3c56", []SchemaEdit{{"+", "system_prompt_hash", "system_prompt"}}},
			{"21 2026-08-04T19:20Z d98287fe3c", []SchemaEdit{{"+", "last_read_at", "pinned"}}},
			{"22 2026-08-08T22:17Z fe42097865", []SchemaEdit{{"+", "title_source", "title"}}},
			{"23 2026-08-15T07:31Z fbaea9bddc", []SchemaEdit{{"+", "hidden", "pinned"}}},
			{"24 2026-08-15T07:33Z e89532d97e", []SchemaEdit{{"+", "git_metadata_generation", "git_repo_root"}}},
			{"25 2026-09-02T11:14Z 238b6c1ab9", []SchemaEdit{
				{"+", "compression_recovery_deadline", "compression_ineffective_count"},
			}},
			{"26 2026-09-02T14:22Z 8e4366d358", []SchemaEdit{{"+", "tool_names", "last_read_at"}}},
			{"27 2026-09-19T00:10Z 922a0c3c87", []SchemaEdit{{"+", "transport_profile", "profile_name"}}},
			{"28 2026-09-25T23:25Z 2941aadffa", []SchemaEdit{{"+", "compression_overload_streak", "compression_recovery_deadline"}}},
			{"29 2026-09-27T00:29Z d75f29934b", []SchemaEdit{{"+", "created_source", "source"}}},
			{"30 2026-09-28T00:00Z #117713", []SchemaEdit{{"+", "auto_archived", "archived"}}},
			{"31 2026-09-08T18:45Z 0695b12b4c", []SchemaEdit{
				{"+", "runtime_revision", "id"},
				{"+", "runtime_generation", "runtime_revision"},
			}},
			{"32 2026-10-18T10:11Z 1fc1999769", []SchemaEdit{{"+", "transcript_revision", "tool_names"}}},
		},
	},
	"messages": {
		Base: []string{
			"id", "session_id", "role", "content", "tool_call_id",
			"tool_calls", "tool_name", "timestamp", "token_count",
		},
		Events: []SchemaEvent{
			{"01 2026-02-21T08:05Z b33ed9176f", []SchemaEdit{{"+", "finish_reason", "token_count"}}},
			{"02 2026-03-25T16:47Z 42fec19151", []SchemaEdit{
				{"+", "reasoning", "finish_reason"},
				{"+", "reasoning_details", "reasoning"},
				{"+", "codex_reasoning_items", "reasoning_details"},
			}},
			{"03 2026-04-22T11:31Z a7d78d3bfd", []SchemaEdit{{"+", "reasoning_content", "reasoning"}}},
			{"04 2026-04-26T01:22Z 81e01f6ee9", []SchemaEdit{{"+", "codex_message_items", "codex_reasoning_items"}}},
			{"05 2026-05-20T20:00Z 31a0100104", []SchemaEdit{{"+", "platform_message_id", "codex_message_items"}}},
			{"06 2026-05-23T08:33Z 4a91e36495", []SchemaEdit{{"+", "observed", "platform_message_id"}}},
			{"07 2026-06-01T08:22Z 3e59be0c41", []SchemaEdit{{"+", "active", "observed"}}},
			{"08 2026-06-11T03:45Z aaccaada28", []SchemaEdit{{"+", "anthropic_content_blocks", "reasoning_details"}}},
			{"09 2026-06-11T03:45Z efcbbde48c", []SchemaEdit{{"-", "anthropic_content_blocks", ""}}},
			{"10 2026-06-20T17:57Z 854d75723f", []SchemaEdit{{"+", "compacted", "active"}}},
			{"11 2026-07-11T12:41Z a0a6cd80f5", []SchemaEdit{{"+", "effect_disposition", "tool_name"}}},
			{"12 2026-07-19T02:55Z 7b3dcee928", []SchemaEdit{{"+", "api_content", "compacted"}}},
			{"13 2026-07-23T18:46Z a4bc1ca502", []SchemaEdit{
				{"+", "display_kind", "api_content"},
				{"+", "display_metadata", "display_kind"},
			}},
			{"14 2026-08-25T10:55Z 1104ffe0b9", []SchemaEdit{{"+", "_compressed_summary", "observed"}}},
			{"15 2026-09-09T17:05Z 1c6683e8e0", []SchemaEdit{
				{"+", "display_identity", "display_metadata"},
				{"+", "display_order", "display_identity"},
			}},
			{"16 2026-09-28T08:42Z eed37d63ce", []SchemaEdit{
				{"+", "message_uid", "display_order"},
				{"+", "absorbed_message_uids", "message_uid"},
				{"+", "tool_call_uids", "absorbed_message_uids"},
				{"+", "tool_call_uid", "tool_call_uids"},
			}},
		},
	},
	"session_model_usage": {
		Base: []string{
			"session_id", "model", "billing_provider", "billing_base_url",
			"api_call_count", "input_tokens", "output_tokens",
			"cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
			"estimated_cost_usd", "first_seen", "last_seen",
		},
		Events: []SchemaEvent{
			{"01 2026-07-11T12:59Z 0d63c23f36", []SchemaEdit{
				{"+", "billing_mode", "billing_base_url"},
				{"+", "actual_cost_usd", "estimated_cost_usd"},
				{"+", "cost_status", "actual_cost_usd"},
				{"+", "cost_source", "cost_status"},
			}},
			{"02 2026-07-16T11:23Z eb6aa03609", []SchemaEdit{{"+", "task", "billing_mode"}}},
		},
	},
}
