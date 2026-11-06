package observability

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const maxDescribed = 256

// IsConnectorName reports a string tool name carrying the connector prefix
func IsConnectorName(name any) bool {
	s, ok := name.(string)
	return ok && strings.HasPrefix(s, tools.ConnectorNamePrefix)
}

// Entry names of a tool_call invocation, empty when the arguments do not parse
func callEntries(args any) []tools.ToolCallEntry {
	d, ok := asDict(args)
	if !ok {
		return nil
	}
	entries, errText := tools.NormalizeToolCallEntries(pyjson.Plain(d).(map[string]any))
	if errText != "" {
		return nil
	}
	return entries
}

// IsConnectorBatch reports a tool_call wrapper whose entries are all connector calls
func IsConnectorBatch(event map[string]any) bool {
	if event["tool_name"] != "tool_call" {
		return false
	}
	entries := callEntries(event["args"])
	for _, e := range entries {
		if !IsConnectorName(e.Name) {
			return false
		}
	}
	return len(entries) > 0
}

// ToolCallIdentity is the session, turn and call id of one tool call
type ToolCallIdentity [3]string

// IsBatchEntry reports whether an identity is a numbered entry of the wrapper's batch
func IsBatchEntry(identity, wrapper ToolCallIdentity) bool {
	return wrapper[2] != "" && strings.HasPrefix(identity[2], wrapper[2]+"#") && identity[0] == wrapper[0] && identity[1] == wrapper[1]
}

// BatchEntryRan reports whether an entry of the wrapper's batch was already counted
func BatchEntryRan(identities []ToolCallIdentity, wrapper ToolCallIdentity) bool {
	for _, id := range identities {
		if IsBatchEntry(id, wrapper) {
			return true
		}
	}
	return false
}

// tool_describe names as its dispatcher reads them, a bare string being one name
func describedNames(args any) []string {
	var names []any
	if d, ok := asDict(args); ok {
		switch v := d["names"].(type) {
		case string:
			names = []any{v}
		case []any:
			names = v
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		text := ""
		if pyrepr.Truthy(n) {
			text = pyrepr.Str(n)
		}
		out = append(out, pystr.Strip(text))
	}
	return out
}

// ConnectorSessionState is one conversation's search to execute chain, guarded by the session lock
type ConnectorSessionState struct {
	SearchesSinceExecute int
	Described            map[string]bool
}

// Observe advances the chain after a counted call and returns the chain row for a connector entry
func (s *ConnectorSessionState) Observe(event map[string]any) map[string]string {
	name := event["tool_name"]
	switch name {
	case "tool_search":
		s.SearchesSinceExecute++
		return nil
	case "tool_describe":
		for _, described := range describedNames(event["args"]) {
			if IsConnectorName(described) && len(s.Described) < maxDescribed {
				if s.Described == nil {
					s.Described = map[string]bool{}
				}
				s.Described[described] = true
			}
		}
		return nil
	}
	if !IsConnectorName(name) {
		return nil
	}
	first := "no"
	if s.Described[name.(string)] {
		first = "yes"
	}
	fields := map[string]string{"described_first": first, "outcome": ToolOutcome(event), "searches_before_bucket": CountBucket(s.SearchesSinceExecute)}
	s.SearchesSinceExecute = 0
	return fields
}

// ToolCallFunction is the function name and raw arguments of one model tool call, nil when absent
type ToolCallFunction struct {
	Name, Arguments any
}

// ToolBatchFields counts one reply's logical calls and how many were connector calls, nil without calls
func ToolBatchFields(route map[string]string, toolCalls []ToolCallFunction) map[string]string {
	total, connectors := 0, 0
	for _, tc := range toolCalls {
		names := []any{tc.Name}
		if tc.Name == "tool_call" {
			if entries := callEntries(parsedArguments(tc.Arguments)); len(entries) > 0 {
				names = names[:0]
				for _, e := range entries {
					names = append(names, e.Name)
				}
			}
		}
		total += len(names)
		for _, n := range names {
			if IsConnectorName(n) {
				connectors++
			}
		}
	}
	if total == 0 {
		return nil
	}
	share := "some"
	if connectors == 0 {
		share = "none"
	} else if connectors == total {
		share = "all"
	}
	out := make(map[string]string, len(route)+2)
	for k, v := range route {
		out[k] = v
	}
	out["connector_share"], out["size_bucket"] = share, CountBucket(total)
	return out
}

// Tool call arguments as a dict, decoding a JSON string and nil when it does not parse
func parsedArguments(raw any) any {
	if _, ok := asDict(raw); ok {
		return raw
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	v, err := pyjson.Loads(s)
	if err != nil {
		return nil
	}
	return pyjson.Plain(v)
}

// ToolSearchFields is one tool search row, nil for a scope or outcome outside the contract
func ToolSearchFields(scope string, hits int, outcome string) map[string]string {
	if !ToolSearchScopes.Has(scope) || !ToolSearchOutcomes.Has(outcome) {
		return nil
	}
	return map[string]string{"hits_bucket": CountBucket(hits), "outcome": outcome, "scope": scope}
}
