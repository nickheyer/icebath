package state

import (
	"fmt"
	"iter"
	"slices"
	"strings"
)

// One edit of a declared column order, an empty After inserting at the front and Remove dropping Column
type SchemaEdit struct {
	Remove bool
	Column string
	After  string
}

// One shipped change to a declaration, labelled by sequence number, commit time and sha
type SchemaEvent struct {
	Label string
	Edits []SchemaEdit
}

// Declared order a table first shipped with and every later change in replay order
type TableHistory struct {
	Base   []string
	Events []SchemaEvent
}

// Insertion of column after another, or at the front for an empty after
func add(column, after string) SchemaEdit { return SchemaEdit{Column: column, After: after} }

// Removal of column
func drop(column string) SchemaEdit { return SchemaEdit{Remove: true, Column: column} }

// Physical column history of the tables the page level salvage lane maps
func SchemaHistory() map[string]TableHistory {
	return map[string]TableHistory{
		"sessions": {
			Base: []string{"id", "source", "user_id", "model", "model_config", "system_prompt", "parent_session_id", "started_at",
				"ended_at", "end_reason", "message_count", "tool_call_count", "input_tokens", "output_tokens"},
			Events: []SchemaEvent{
				{"01 2026-03-08T23:32Z c5e8166c8b", []SchemaEdit{add("title", "output_tokens")}},
				{"02 2026-03-17T10:44Z d417ba2a48", []SchemaEdit{
					add("cache_read_tokens", "output_tokens"), add("cache_write_tokens", "cache_read_tokens"),
					add("reasoning_tokens", "cache_write_tokens"), add("billing_provider", "reasoning_tokens"),
					add("billing_base_url", "billing_provider"), add("billing_mode", "billing_base_url"),
					add("estimated_cost_usd", "billing_mode"), add("actual_cost_usd", "estimated_cost_usd"),
					add("cost_status", "actual_cost_usd"), add("cost_source", "cost_status"), add("pricing_version", "cost_source"),
				}},
				{"03 2026-04-22T12:51Z 5fb143169b", []SchemaEdit{add("api_call_count", "title")}},
				{"04 2026-05-10T20:06Z 878611a79d", []SchemaEdit{add("handoff_pending", "api_call_count"), add("handoff_platform", "handoff_pending")}},
				{"05 2026-05-10T20:06Z 00ce5f04d9", []SchemaEdit{drop("handoff_pending"), add("handoff_state", "api_call_count"), add("handoff_error", "handoff_platform")}},
				{"06 2026-05-31T22:46Z 51c68d4ab1", []SchemaEdit{add("cwd", "reasoning_tokens")}},
				{"07 2026-06-01T08:22Z 3e59be0c41", []SchemaEdit{add("rewind_count", "handoff_error")}},
				{"08 2026-06-02T01:41Z 85b65e29f0", []SchemaEdit{add("archived", "rewind_count")}},
				{"09 2026-06-25T22:49Z ffa3d3c811", []SchemaEdit{add("git_branch", "cwd"), add("git_repo_root", "git_branch")}},
				{"10 2026-06-28T22:10Z 86e64900b9", []SchemaEdit{add("session_key", "user_id"), add("chat_id", "session_key"),
					add("chat_type", "chat_id"), add("thread_id", "chat_type")}},
				{"11 2026-06-30T08:06Z f2ccb2859f", []SchemaEdit{add("compression_failure_cooldown_until", "handoff_error"),
					add("compression_failure_error", "compression_failure_cooldown_until")}},
				{"12 2026-07-05T21:01Z 747386ecfa", []SchemaEdit{add("display_name", "thread_id"), add("origin_json", "display_name"),
					add("expiry_finalized", "origin_json")}},
				{"13 2026-07-13T20:49Z af7dceaf77", []SchemaEdit{add("compression_fallback_streak", "compression_failure_error")}},
				{"14 2026-07-15T16:50Z e8b7ce8c19", []SchemaEdit{add("profile_name", "compression_fallback_streak")}},
				{"15 2026-07-23T15:08Z ec5835ab8b", []SchemaEdit{add("compression_ineffective_count", "compression_fallback_streak")}},
				{"16 2026-07-24T16:15Z 951d606730", []SchemaEdit{add("pinned", "archived")}},
				{"17 2026-07-27T19:14Z cfb206fe2e", []SchemaEdit{add("last_activity_at", "title"),
					add("last_activity_description", "last_activity_at"), add("last_activity_provenance", "last_activity_description")}},
				{"18 2026-07-27T19:22Z 5646fed97e", []SchemaEdit{drop("last_activity_at"), drop("last_activity_description"), drop("last_activity_provenance")}},
				{"19 2026-08-02T23:16Z c2088efe9e", []SchemaEdit{add("last_activity_at", "title"),
					add("last_activity_description", "last_activity_at"), add("last_activity_provenance", "last_activity_description")}},
				{"20 2026-08-03T15:07Z 7d066c3c56", []SchemaEdit{add("system_prompt_hash", "system_prompt")}},
				{"21 2026-08-04T19:20Z d98287fe3c", []SchemaEdit{add("last_read_at", "pinned")}},
				{"22 2026-08-08T22:17Z fe42097865", []SchemaEdit{add("title_source", "title")}},
				{"23 2026-08-15T07:31Z fbaea9bddc", []SchemaEdit{add("hidden", "pinned")}},
				{"24 2026-08-15T07:33Z e89532d97e", []SchemaEdit{add("git_metadata_generation", "git_repo_root")}},
				{"25 2026-09-02T11:14Z 238b6c1ab9", []SchemaEdit{add("compression_recovery_deadline", "compression_ineffective_count")}},
				{"26 2026-09-02T14:22Z 8e4366d358", []SchemaEdit{add("tool_names", "last_read_at")}},
				{"27 2026-09-19T00:10Z 922a0c3c87", []SchemaEdit{add("transport_profile", "profile_name")}},
				{"28 2026-09-25T23:25Z 2941aadffa", []SchemaEdit{add("compression_overload_streak", "compression_recovery_deadline")}},
				{"29 2026-09-27T00:29Z d75f29934b", []SchemaEdit{add("created_source", "source")}},
				{"30 2026-09-28T00:00Z #117713", []SchemaEdit{add("auto_archived", "archived")}},
				{"31 2026-09-08T18:45Z 0695b12b4c", []SchemaEdit{add("runtime_revision", "id"), add("runtime_generation", "runtime_revision")}},
				{"32 2026-10-18T10:11Z 1fc1999769", []SchemaEdit{add("transcript_revision", "tool_names")}},
			},
		},
		"messages": {
			Base: []string{"id", "session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp", "token_count"},
			Events: []SchemaEvent{
				{"01 2026-02-21T08:05Z b33ed9176f", []SchemaEdit{add("finish_reason", "token_count")}},
				{"02 2026-03-25T16:47Z 42fec19151", []SchemaEdit{add("reasoning", "finish_reason"), add("reasoning_details", "reasoning"),
					add("codex_reasoning_items", "reasoning_details")}},
				{"03 2026-04-22T11:31Z a7d78d3bfd", []SchemaEdit{add("reasoning_content", "reasoning")}},
				{"04 2026-04-26T01:22Z 81e01f6ee9", []SchemaEdit{add("codex_message_items", "codex_reasoning_items")}},
				{"05 2026-05-20T20:00Z 31a0100104", []SchemaEdit{add("platform_message_id", "codex_message_items")}},
				{"06 2026-05-23T08:33Z 4a91e36495", []SchemaEdit{add("observed", "platform_message_id")}},
				{"07 2026-06-01T08:22Z 3e59be0c41", []SchemaEdit{add("active", "observed")}},
				{"08 2026-06-11T03:45Z aaccaada28", []SchemaEdit{add("anthropic_content_blocks", "reasoning_details")}},
				{"09 2026-06-11T03:45Z efcbbde48c", []SchemaEdit{drop("anthropic_content_blocks")}},
				{"10 2026-06-20T17:57Z 854d75723f", []SchemaEdit{add("compacted", "active")}},
				{"11 2026-07-11T12:41Z a0a6cd80f5", []SchemaEdit{add("effect_disposition", "tool_name")}},
				{"12 2026-07-19T02:55Z 7b3dcee928", []SchemaEdit{add("api_content", "compacted")}},
				{"13 2026-07-23T18:46Z a4bc1ca502", []SchemaEdit{add("display_kind", "api_content"), add("display_metadata", "display_kind")}},
				{"14 2026-08-25T10:55Z 1104ffe0b9", []SchemaEdit{add("_compressed_summary", "observed")}},
				{"15 2026-09-09T17:05Z 1c6683e8e0", []SchemaEdit{add("display_identity", "display_metadata"), add("display_order", "display_identity")}},
				{"16 2026-09-28T08:42Z eed37d63ce", []SchemaEdit{add("message_uid", "display_order"), add("absorbed_message_uids", "message_uid"),
					add("tool_call_uids", "absorbed_message_uids"), add("tool_call_uid", "tool_call_uids")}},
			},
		},
		"session_model_usage": {
			Base: []string{"session_id", "model", "billing_provider", "billing_base_url", "api_call_count", "input_tokens", "output_tokens",
				"cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "estimated_cost_usd", "first_seen", "last_seen"},
			Events: []SchemaEvent{
				{"01 2026-07-11T12:59Z 0d63c23f36", []SchemaEdit{add("billing_mode", "billing_base_url"), add("actual_cost_usd", "estimated_cost_usd"),
					add("cost_status", "actual_cost_usd"), add("cost_source", "cost_status")}},
				{"02 2026-07-16T11:23Z eb6aa03609", []SchemaEdit{add("task", "billing_mode")}},
			},
		},
	}
}

// Columns with edits replayed, a missing anchor or removed column an error
func applySchemaEdits(columns []string, edits []SchemaEdit) ([]string, error) {
	for _, e := range edits {
		if e.Remove {
			i := slices.Index(columns, e.Column)
			if i < 0 {
				return nil, fmt.Errorf("schema history removes %q, which the declaration lacks", e.Column)
			}
			columns = slices.Delete(columns, i, i+1)
			continue
		}
		at := 0
		if e.After != "" {
			i := slices.Index(columns, e.After)
			if i < 0 {
				return nil, fmt.Errorf("schema history anchors %q after %q, which the declaration lacks", e.Column, e.After)
			}
			at = i + 1
		}
		columns = slices.Insert(columns, at, e.Column)
	}
	return columns, nil
}

// Every declared column order table has shipped with, oldest first
func DeclaredSnapshots(table string) ([][]string, error) {
	h, ok := SchemaHistory()[table]
	if !ok {
		return nil, fmt.Errorf("no schema history for table %q", table)
	}
	columns := slices.Clone(h.Base)
	out := [][]string{slices.Clone(columns)}
	for _, ev := range h.Events {
		var err error
		if columns, err = applySchemaEdits(columns, ev.Edits); err != nil {
			return nil, err
		}
		out = append(out, slices.Clone(columns))
	}
	return out, nil
}

// Column order the table declares today
func CurrentDeclaredColumns(table string) ([]string, error) {
	snaps, err := DeclaredSnapshots(table)
	if err != nil {
		return nil, err
	}
	return snaps[len(snaps)-1], nil
}

// Every physical layout an upgraded store of table can have, accept pruning a branch from where it first differs
func ReachablePhysicalLayouts(table string, accept func(layout []string, firstNew int) bool) (iter.Seq[[]string], error) {
	snaps, err := DeclaredSnapshots(table)
	if err != nil {
		return nil, err
	}
	return func(yield func([]string) bool) {
		type node struct {
			layout []string
			index  int
		}
		seen := map[string]int{}
		key := func(l []string) string { return strings.Join(l, "\x00") }
		var frontier []node
		for i, snap := range snaps {
			if _, dup := seen[key(snap)]; dup {
				continue
			}
			if accept != nil && !accept(snap, 0) {
				continue
			}
			seen[key(snap)] = i
			frontier = append(frontier, node{snap, i})
			if !yield(slices.Clone(snap)) {
				return
			}
		}
		for len(frontier) > 0 {
			var next []node
			for _, n := range frontier {
				have := map[string]bool{}
				for _, c := range n.layout {
					have[c] = true
				}
				for later := n.index + 1; later < len(snaps); later++ {
					var added []string
					for _, c := range snaps[later] {
						if !have[c] {
							added = append(added, c)
						}
					}
					if len(added) == 0 {
						continue
					}
					candidate := append(slices.Clone(n.layout), added...)
					if at, ok := seen[key(candidate)]; ok && at <= later {
						continue
					}
					if accept != nil && !accept(candidate, len(n.layout)) {
						continue
					}
					seen[key(candidate)] = later
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
