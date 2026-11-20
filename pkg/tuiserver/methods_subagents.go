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
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Bytes of a live transcript a tail read returns at most
const subagentTailBytes = 16384

var subagentsRegistry = &handlerRegistry{}

func init() {
	subagentsRegistry.method("subagent.list", subagentList)
	subagentsRegistry.method("subagent.interrupt", subagentInterrupt)
	subagentsRegistry.method("subagent.tail", subagentTail)
	subagentsRegistry.install()
}

// Typed value of a generic row a sender projects, a misfit reported against the method
func typedRow[T any](method string, raw any) T {
	var row T
	if err := tuigateway.Strict(raw, &row); err != nil {
		tuigateway.ReportMisfit("result of", method, err)
	}
	return row
}

// Live children of a session this transport steers plus those its conversation lineage owns, and its failed delegations
func subagentList(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.SubagentList.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	transport, owner := currentSessionSteerAuthority(ctx, request.SessionID)
	if transport == nil || owner == nil {
		return rpcErr(rid, 4001, "session not found or not owned by this transport", nil)
	}
	result := tuigateway.SubagentListResult{Subagents: []tuigateway.SubagentSnapshot{}, Delegations: failedDelegations(ctx, request.SessionID, owner)}
	for _, record := range agent.VisibleSubagentRecords(request.SessionID, transport, owner, sessionAgent(owner)) {
		result.Subagents = append(result.Subagents, subagentSnapshotOf(record))
	}
	return ok(rid, tuigateway.Plain(result))
}

// Snapshot row of one live child record, its private keys left out
func subagentSnapshotOf(record map[string]any) tuigateway.SubagentSnapshot {
	row := map[string]any{}
	for _, key := range tuigateway.WireNames[tuigateway.SubagentSnapshot]() {
		if value, present := record[key]; present {
			row[key] = value
		}
	}
	return typedRow[tuigateway.SubagentSnapshot]("subagent.list", row)
}

// Recently failed async delegations of the session read under its profile home, none when the store fails
func failedDelegations(ctx context.Context, sessionID string, owner *tuihost.Session) []tuigateway.FailedDelegation {
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
	out := []tuigateway.FailedDelegation{}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("subagent.list: failed-delegation read failed for %s", sessionID), "error", err)
		return out
	}
	for _, row := range rows {
		out = append(out, typedRow[tuigateway.FailedDelegation]("subagent.list", row))
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
	request, err := tuigateway.SubagentInterrupt.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	subagentID, sessionID := request.SubagentID, request.SessionID
	if subagentID == "" {
		return rpcErr(rid, 4000, "subagent_id required", nil)
	}
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
	return ok(rid, tuigateway.Plain(tuigateway.SubagentInterruptResult{Found: found, SubagentID: subagentID}))
}

// Last bytes of an owned child's transcript, a finished child's read from its stored session row
func subagentTail(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.SubagentTail.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	sessionID, subagentID := request.SessionID, request.SubagentID
	if subagentID == "" {
		return rpcErr(rid, 4000, "subagent_id required", nil)
	}
	transport, owner := currentSessionSteerAuthority(ctx, sessionID)
	if transport == nil || owner == nil {
		return rpcErr(rid, 4001, "session not found or not owned by this transport", nil)
	}
	result := tuigateway.SubagentTailResult{SubagentID: subagentID}
	path := ""
	if record := ownedSubagentRecord(sessionID, transport, owner, subagentID); record != nil {
		path = agent.LiveTranscriptPathOf(record["agent"])
	}
	if path == "" {
		if path, err = finishedTranscript(ctx, owner, subagentID); err != nil {
			return rpcErr(rid, 5000, "subagent.tail: "+err.Error(), nil)
		}
	}
	if path != "" {
		if text, size, err := readTranscriptTail(path); err == nil {
			result.Available, result.Text, result.Truncated = true, text, size > subagentTailBytes
		}
	}
	return ok(rid, tuigateway.Plain(result))
}

// Log of a finished child the session's conversation spawned, looked up in the session store
func finishedTranscript(ctx context.Context, owner *tuihost.Session, subagentID string) (string, error) {
	ownerSessionID := strOr(owner.Get("session_key"))
	if a := sessionAgent(owner); a != nil && a.SessionID() != "" {
		ownerSessionID = a.SessionID()
	}
	db, release := sessionDB(ctx, owner)
	defer release()
	if db == nil {
		return "", fmt.Errorf("the session store of %s did not open", ownerSessionID)
	}
	return tools.FinishedSubagentTranscript(ctx, db, ownerSessionID, subagentID)
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
