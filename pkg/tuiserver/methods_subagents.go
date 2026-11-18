package tuiserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Record keys a subagent snapshot carries to the client
var subagentSnapshotFields = []string{"subagent_id", "parent_id", "depth", "goal", "delegation_id", "model",
	"started_at", "status", "tool_count", "last_tool", "accepting_steer"}

// Bytes of a live transcript a tail read returns at most
const subagentTailBytes = 16384

var subagentsRegistry = &handlerRegistry{}

func init() {
	subagentsRegistry.method("subagent.list", subagentList)
	subagentsRegistry.method("subagent.interrupt", subagentInterrupt)
	subagentsRegistry.method("subagent.tail", subagentTail)
	subagentsRegistry.install()
}

// Live children of a session this transport steers plus those its conversation lineage owns, and its failed delegations
func subagentList(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionID := strParam(params, "session_id", "")
	transport, owner := currentSessionSteerAuthority(ctx, sessionID)
	if transport == nil || owner == nil {
		return rpcErr(rid, 4001, "session not found or not owned by this transport", nil)
	}
	live := agent.VisibleSubagentRecords(sessionID, transport, owner, sessionAgent(owner))
	rows := make([]any, 0, len(live))
	for _, record := range live {
		row := make(map[string]any, len(subagentSnapshotFields))
		for _, key := range subagentSnapshotFields {
			row[key] = record[key]
		}
		rows = append(rows, row)
	}
	return ok(rid, map[string]any{"subagents": rows, "delegations": failedDelegations(ctx, sessionID, owner)})
}

// Recently failed async delegations of the session read under its profile home, none when the store fails
func failedDelegations(ctx context.Context, sessionID string, owner *tuihost.Session) []any {
	agentSessionID := ""
	if a := sessionAgent(owner); a != nil {
		agentSessionID = a.SessionID()
	}
	rows, err := func() (rows []map[string]any, err error) {
		defer func() {
			if failure := recover(); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
		}()
		scoped, err := sessionHomeScope(owner, "", "")(ctx)
		if err != nil {
			return nil, err
		}
		env := &tools.Env{Home: home.Dir(scoped)}
		return tools.FailedDelegationsForSession(env, sessionID, agentSessionID, 20, nil)
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("subagent.list: failed-delegation read failed for %s", sessionID), "error", err)
		return []any{}
	}
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Owned live child record of the session generation with the given id, nil when none
func ownedSubagentRecord(sessionID string, transport, owner any, subagentID string) map[string]any {
	for _, record := range agent.OwnedSubagentRecords(sessionID, transport, owner) {
		if record["subagent_id"] == subagentID {
			return record
		}
	}
	return nil
}

// Hard interrupts one owned live child by the object the authority resolved
func subagentInterrupt(ctx context.Context, rid any, params map[string]any) map[string]any {
	subagentID := strParam(params, "subagent_id", "")
	if subagentID == "" {
		return rpcErr(rid, 4000, "subagent_id required", nil)
	}
	sessionID := strParam(params, "session_id", "")
	transport, owner := currentSessionSteerAuthority(ctx, sessionID)
	if transport == nil || owner == nil {
		return rpcErr(rid, 4001, "session not found or not owned by this transport", nil)
	}
	var child any
	if record := ownedSubagentRecord(sessionID, transport, owner, subagentID); record != nil {
		child = record["agent"]
	}
	found := false
	if child != nil {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					pkgLog().Debug("subagent interrupt failed", "error", failure)
					found = false
				}
			}()
			reason := fmt.Sprintf("Interrupted via TUI (%s)", subagentID)
			found = agent.RequestHardInterruptOf(child, &reason, nil)
		}()
	}
	return ok(rid, map[string]any{"found": found, "subagent_id": subagentID})
}

// Last bytes of one owned live child's transcript, unavailable while it has none or the file races away
func subagentTail(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionID := strParam(params, "session_id", "")
	subagentID := strParam(params, "subagent_id", "")
	if subagentID == "" {
		return rpcErr(rid, 4000, "subagent_id required", nil)
	}
	transport, owner := currentSessionSteerAuthority(ctx, sessionID)
	if transport == nil || owner == nil {
		return rpcErr(rid, 4001, "session not found or not owned by this transport", nil)
	}
	result := map[string]any{"subagent_id": subagentID, "available": false, "text": "", "truncated": false}
	path := ""
	if record := ownedSubagentRecord(sessionID, transport, owner, subagentID); record != nil {
		path = agent.LiveTranscriptPathOf(record["agent"])
	}
	if path == "" {
		return ok(rid, result)
	}
	text, size, err := readTranscriptTail(path)
	if err != nil {
		return ok(rid, result)
	}
	result["available"], result["text"], result["truncated"] = true, text, size > subagentTailBytes
	return ok(rid, result)
}

// Last tail bytes of a file decoded as UTF-8 with invalid sequences dropped, and the file size
func readTranscriptTail(path string) (string, int64, error) {
	stream, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer stream.Close()
	size, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, err
	}
	if _, err := stream.Seek(max(0, size-subagentTailBytes), io.SeekStart); err != nil {
		return "", 0, err
	}
	raw, err := io.ReadAll(io.LimitReader(stream, subagentTailBytes))
	if err != nil {
		return "", 0, err
	}
	var text strings.Builder
	for len(raw) > 0 {
		r, width := utf8.DecodeRune(raw)
		if r != utf8.RuneError || width > 1 {
			text.WriteRune(r)
		}
		raw = raw[width:]
	}
	return text.String(), size, nil
}
