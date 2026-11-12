package tuiserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Record keys a subagent snapshot carries to the client
var subagentSnapshotFields = []string{"subagent_id", "parent_id", "depth", "goal", "delegation_id", "model",
	"started_at", "status", "tool_count", "last_tool", "accepting_steer"}

// Bytes of a live transcript a tail read returns at most
const subagentTailBytes = 16384

var subagentsRegistry = &handlerRegistry{}

func init() {
	subagentsRegistry.method("subagent.list", subagentList)
	subagentsRegistry.method("subagent.roster", subagentRoster)
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

// One piece of live work with its kind and the stored id and route that may name its owner
type liveWorkOwner struct {
	kind     string
	storedID string
	route    string
}

// Every live top level child and running background process of this process with its owner candidates
func liveWorkOwners(ctx context.Context) []liveWorkOwner {
	var owners []liveWorkOwner
	for _, owner := range agent.TopLevelSubagentOwners() {
		owners = append(owners, liveWorkOwner{kind: "subagents", storedID: owner})
	}
	registry := tools.DefaultProcessRegistry()
	env := tools.ToolEnvFrom(ctx)
	for _, entry := range registry.ListSessions(ctx, env, "", "", false) {
		id, _ := entry["session_id"].(string)
		proc := registry.Get(env, id)
		if proc != nil && !proc.IsExited() {
			owners = append(owners, liveWorkOwner{kind: "background", storedID: proc.ParentSessionID, route: proc.SessionKey})
		}
	}
	return owners
}

// Session key namespaces a route of the requested profile can carry, main trusted only for the sole served profile
func routeNamespaces(profileDir string) map[string]bool {
	served := profileDir
	if served == "" {
		served = importedHome()
	}
	_, own, _ := strings.Cut(gateway.SessionKeyNamespace(gateway.ServedProfileName(served)), ":")
	servedProfileMu.Lock()
	sole := profileDir == "" && len(servedProfileHomes) == 0
	servedProfileMu.Unlock()
	if sole {
		return map[string]bool{own: true, "main": true}
	}
	return map[string]bool{own: true}
}

// Conversation of db owning a piece of work, empty when none of this profile's conversations does
func storedOwner(ctx context.Context, db *state.DB, owner liveWorkOwner, namespaces map[string]bool) (string, error) {
	if owner.storedID != "" {
		row, err := db.GetSession(ctx, owner.storedID)
		if err != nil {
			return "", err
		}
		if row != nil && len(row.Keys) > 0 {
			return owner.storedID, nil
		}
		return "", nil
	}
	parts := strings.Split(owner.route, ":")
	if len(parts) < 3 || parts[0] != "agent" || !namespaces[parts[1]] {
		return "", nil
	}
	key := owner.route
	row, err := db.FindLatestGatewaySessionForPeer(ctx, state.GatewayPeerQuery{Source: parts[2], SessionKey: &key})
	if err != nil || row == nil {
		return "", err
	}
	if id := row.GetOr("id", nil); truthy(id) {
		return pyrepr.Str(id), nil
	}
	return "", nil
}

// Conversations of the requested profile that still have children or background processes running
func subagentRoster(ctx context.Context, rid any, params map[string]any) map[string]any {
	owners := liveWorkOwners(ctx)
	if len(owners) == 0 {
		return ok(rid, map[string]any{"sessions": []any{}})
	}
	scope := params
	if scope == nil {
		scope = map[string]any{}
	}
	if strings.TrimSpace(strOr(scope["profile"])) == "" {
		if ticketHome := connectionProfileHome(ctx); ticketHome != "" {
			scope = map[string]any{"profile": filepath.Base(ticketHome)}
		}
	}
	profileDir, err := profileHome(strOr(scope["profile"]))
	if err != nil {
		panic(err)
	}
	namespaces := routeNamespaces(profileDir)
	db, release, err := profileDB(ctx, scope, false)
	if err != nil {
		panic(err)
	}
	defer release()
	if db == nil {
		return ok(rid, map[string]any{"sessions": []any{}})
	}
	var order []string
	counts := map[string]map[string]int{}
	for _, owner := range owners {
		sid, err := storedOwner(ctx, db, owner, namespaces)
		if err != nil {
			panic(err)
		}
		if sid == "" {
			continue
		}
		if counts[sid] == nil {
			counts[sid] = map[string]int{"subagents": 0, "background": 0}
			order = append(order, sid)
		}
		counts[sid][owner.kind]++
	}
	sessions := make([]any, 0, len(order))
	for _, sid := range order {
		sessions = append(sessions, map[string]any{"session_id": sid, "subagents": counts[sid]["subagents"], "background": counts[sid]["background"]})
	}
	return ok(rid, map[string]any{"sessions": sessions})
}

// Recently failed async delegations of the session read under its profile home, none when the store fails
func failedDelegations(ctx context.Context, sessionID string, owner *tuigateway.Session) []any {
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
		logger().Debug(fmt.Sprintf("subagent.list: failed-delegation read failed for %s", sessionID), "error", err)
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
					logger().Debug("subagent interrupt failed", "error", failure)
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
