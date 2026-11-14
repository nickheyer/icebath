package state

import (
	"fmt"
	"iter"
	"slices"
	"strings"
)

// One edit of a declared column order, an empty after inserting at the front and remove dropping column
type schemaEdit struct {
	remove bool
	column string
	after  string
}

// One shipped change to a declaration, labelled by sequence number, commit time and sha
type schemaEvent struct {
	label string
	edits []schemaEdit
}

// Declared order a table first shipped with and every later change in replay order
type tableHistory struct {
	base   []string
	events []schemaEvent
}

// Insertion of column after another, or at the front for an empty after
func add(column, after string) schemaEdit { return schemaEdit{column: column, after: after} }

// Removal of column
func drop(column string) schemaEdit { return schemaEdit{remove: true, column: column} }

// Physical column history of the tables the page level salvage lane maps
var schemaHistory = map[string]tableHistory{
	"sessions": {
		base: []string{"id", "source", "user_id", "model", "model_config", "system_prompt", "parent_session_id", "started_at",
			"ended_at", "end_reason", "message_count", "tool_call_count", "input_tokens", "output_tokens"},
		events: []schemaEvent{
			{"01 2026-03-08T23:32Z c5e8166c8b", []schemaEdit{add("title", "output_tokens")}},
			{"02 2026-03-17T10:44Z d417ba2a48", []schemaEdit{
				add("cache_read_tokens", "output_tokens"), add("cache_write_tokens", "cache_read_tokens"),
				add("reasoning_tokens", "cache_write_tokens"), add("billing_provider", "reasoning_tokens"),
				add("billing_base_url", "billing_provider"), add("billing_mode", "billing_base_url"),
				add("estimated_cost_usd", "billing_mode"), add("actual_cost_usd", "estimated_cost_usd"),
				add("cost_status", "actual_cost_usd"), add("cost_source", "cost_status"), add("pricing_version", "cost_source"),
			}},
			{"03 2026-04-22T12:51Z 5fb143169b", []schemaEdit{add("api_call_count", "title")}},
			{"04 2026-05-10T20:06Z 878611a79d", []schemaEdit{add("handoff_pending", "api_call_count"), add("handoff_platform", "handoff_pending")}},
			{"05 2026-05-10T20:06Z 00ce5f04d9", []schemaEdit{drop("handoff_pending"), add("handoff_state", "api_call_count"), add("handoff_error", "handoff_platform")}},
			{"06 2026-05-31T22:46Z 51c68d4ab1", []schemaEdit{add("cwd", "reasoning_tokens")}},
			{"07 2026-06-01T08:22Z 3e59be0c41", []schemaEdit{add("rewind_count", "handoff_error")}},
			{"08 2026-06-02T01:41Z 85b65e29f0", []schemaEdit{add("archived", "rewind_count")}},
			{"09 2026-06-25T22:49Z ffa3d3c811", []schemaEdit{add("git_branch", "cwd"), add("git_repo_root", "git_branch")}},
			{"10 2026-06-28T22:10Z 86e64900b9", []schemaEdit{add("session_key", "user_id"), add("chat_id", "session_key"),
				add("chat_type", "chat_id"), add("thread_id", "chat_type")}},
			{"11 2026-06-30T08:06Z f2ccb2859f", []schemaEdit{add("compression_failure_cooldown_until", "handoff_error"),
				add("compression_failure_error", "compression_failure_cooldown_until")}},
			{"12 2026-07-05T21:01Z 747386ecfa", []schemaEdit{add("display_name", "thread_id"), add("origin_json", "display_name"),
				add("expiry_finalized", "origin_json")}},
			{"13 2026-07-13T20:49Z af7dceaf77", []schemaEdit{add("compression_fallback_streak", "compression_failure_error")}},
			{"14 2026-07-15T16:50Z e8b7ce8c19", []schemaEdit{add("profile_name", "compression_fallback_streak")}},
			{"15 2026-07-23T15:08Z ec5835ab8b", []schemaEdit{add("compression_ineffective_count", "compression_fallback_streak")}},
			{"16 2026-07-24T16:15Z 951d606730", []schemaEdit{add("pinned", "archived")}},
			{"17 2026-07-27T19:14Z cfb206fe2e", []schemaEdit{add("last_activity_at", "title"),
				add("last_activity_description", "last_activity_at"), add("last_activity_provenance", "last_activity_description")}},
			{"18 2026-07-27T19:22Z 5646fed97e", []schemaEdit{drop("last_activity_at"), drop("last_activity_description"), drop("last_activity_provenance")}},
			{"19 2026-08-02T23:16Z c2088efe9e", []schemaEdit{add("last_activity_at", "title"),
				add("last_activity_description", "last_activity_at"), add("last_activity_provenance", "last_activity_description")}},
			{"20 2026-08-03T15:07Z 7d066c3c56", []schemaEdit{add("system_prompt_hash", "system_prompt")}},
			{"21 2026-08-04T19:20Z d98287fe3c", []schemaEdit{add("last_read_at", "pinned")}},
			{"22 2026-08-08T22:17Z fe42097865", []schemaEdit{add("title_source", "title")}},
			{"23 2026-08-15T07:31Z fbaea9bddc", []schemaEdit{add("hidden", "pinned")}},
			{"24 2026-08-15T07:33Z e89532d97e", []schemaEdit{add("git_metadata_generation", "git_repo_root")}},
			{"25 2026-09-02T11:14Z 238b6c1ab9", []schemaEdit{add("compression_recovery_deadline", "compression_ineffective_count")}},
			{"26 2026-09-02T14:22Z 8e4366d358", []schemaEdit{add("tool_names", "last_read_at")}},
			{"27 2026-09-19T00:10Z 922a0c3c87", []schemaEdit{add("transport_profile", "profile_name")}},
			{"28 2026-09-25T23:25Z 2941aadffa", []schemaEdit{add("compression_overload_streak", "compression_recovery_deadline")}},
			{"29 2026-09-27T00:29Z d75f29934b", []schemaEdit{add("created_source", "source")}},
			{"30 2026-09-28T00:00Z #117713", []schemaEdit{add("auto_archived", "archived")}},
			{"31 2026-09-08T18:45Z 0695b12b4c", []schemaEdit{add("runtime_revision", "id"), add("runtime_generation", "runtime_revision")}},
			{"32 2026-10-18T10:11Z 1fc1999769", []schemaEdit{add("transcript_revision", "tool_names")}},
		},
	},
	"messages": {
		base: []string{"id", "session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp", "token_count"},
		events: []schemaEvent{
			{"01 2026-02-21T08:05Z b33ed9176f", []schemaEdit{add("finish_reason", "token_count")}},
			{"02 2026-03-25T16:47Z 42fec19151", []schemaEdit{add("reasoning", "finish_reason"), add("reasoning_details", "reasoning"),
				add("codex_reasoning_items", "reasoning_details")}},
			{"03 2026-04-22T11:31Z a7d78d3bfd", []schemaEdit{add("reasoning_content", "reasoning")}},
			{"04 2026-04-26T01:22Z 81e01f6ee9", []schemaEdit{add("codex_message_items", "codex_reasoning_items")}},
			{"05 2026-05-20T20:00Z 31a0100104", []schemaEdit{add("platform_message_id", "codex_message_items")}},
			{"06 2026-05-23T08:33Z 4a91e36495", []schemaEdit{add("observed", "platform_message_id")}},
			{"07 2026-06-01T08:22Z 3e59be0c41", []schemaEdit{add("active", "observed")}},
			{"08 2026-06-11T03:45Z aaccaada28", []schemaEdit{add("anthropic_content_blocks", "reasoning_details")}},
			{"09 2026-06-11T03:45Z efcbbde48c", []schemaEdit{drop("anthropic_content_blocks")}},
			{"10 2026-06-20T17:57Z 854d75723f", []schemaEdit{add("compacted", "active")}},
			{"11 2026-07-11T12:41Z a0a6cd80f5", []schemaEdit{add("effect_disposition", "tool_name")}},
			{"12 2026-07-19T02:55Z 7b3dcee928", []schemaEdit{add("api_content", "compacted")}},
			{"13 2026-07-23T18:46Z a4bc1ca502", []schemaEdit{add("display_kind", "api_content"), add("display_metadata", "display_kind")}},
			{"14 2026-08-25T10:55Z 1104ffe0b9", []schemaEdit{add("_compressed_summary", "observed")}},
			{"15 2026-09-09T17:05Z 1c6683e8e0", []schemaEdit{add("display_identity", "display_metadata"), add("display_order", "display_identity")}},
			{"16 2026-09-28T08:42Z eed37d63ce", []schemaEdit{add("message_uid", "display_order"), add("absorbed_message_uids", "message_uid"),
				add("tool_call_uids", "absorbed_message_uids"), add("tool_call_uid", "tool_call_uids")}},
		},
	},
	"session_model_usage": {
		base: []string{"session_id", "model", "billing_provider", "billing_base_url", "api_call_count", "input_tokens", "output_tokens",
			"cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "estimated_cost_usd", "first_seen", "last_seen"},
		events: []schemaEvent{
			{"01 2026-07-11T12:59Z 0d63c23f36", []schemaEdit{add("billing_mode", "billing_base_url"), add("actual_cost_usd", "estimated_cost_usd"),
				add("cost_status", "actual_cost_usd"), add("cost_source", "cost_status")}},
			{"02 2026-07-16T11:23Z eb6aa03609", []schemaEdit{add("task", "billing_mode")}},
		},
	},
}

// Reports whether the physical column history of table is recorded
func HasSchemaHistory(table string) bool {
	_, ok := schemaHistory[table]
	return ok
}

// Columns with the edits of one event replayed, a missing anchor or removed column an error naming the event
func applySchemaEdits(columns []string, ev schemaEvent) ([]string, error) {
	for _, e := range ev.edits {
		if e.remove {
			i := slices.Index(columns, e.column)
			if i < 0 {
				return nil, fmt.Errorf("schema history event %s removes %q, which the declaration lacks", ev.label, e.column)
			}
			columns = slices.Delete(columns, i, i+1)
			continue
		}
		at := 0
		if e.after != "" {
			i := slices.Index(columns, e.after)
			if i < 0 {
				return nil, fmt.Errorf("schema history event %s anchors %q after %q, which the declaration lacks", ev.label, e.column, e.after)
			}
			at = i + 1
		}
		columns = slices.Insert(columns, at, e.column)
	}
	return columns, nil
}

// Every declared column order table has shipped with, oldest first, the last one checked against the schema text
func declaredSnapshots(table string) ([][]string, error) {
	h, ok := schemaHistory[table]
	if !ok {
		return nil, fmt.Errorf("no schema history for table %q", table)
	}
	columns := slices.Clone(h.base)
	out := [][]string{slices.Clone(columns)}
	for _, ev := range h.events {
		var err error
		if columns, err = applySchemaEdits(columns, ev); err != nil {
			return nil, err
		}
		out = append(out, slices.Clone(columns))
	}
	tables, err := declaredSchema()
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		if t.name != table {
			continue
		}
		declared := make([]string, len(t.columns))
		for i, c := range t.columns {
			declared[i] = c.name
		}
		if !slices.Equal(columns, declared) {
			return nil, fmt.Errorf("schema history of %q ends at %q while the schema declares %q", table, columns, declared)
		}
		return out, nil
	}
	return nil, fmt.Errorf("schema history names table %q, which the schema does not declare", table)
}

// Every physical layout an upgraded store of table can have, accept pruning a branch from where it first differs
func ReachablePhysicalLayouts(table string, accept func(layout []string, firstNew int) bool) (iter.Seq[[]string], error) {
	snaps, err := declaredSnapshots(table)
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
