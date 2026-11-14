package state

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Installs the gateway route builders as JSON shaped doubles for the whole package test run
func installRouteBuilderDoubles() {
	ResetSessionEntry = func(previous *pyjson.Object, childID string) (*pyjson.Object, error) {
		entry := previous.Copy()
		stamp := pydatetime.ISOFormatNaive(time.Now())
		entry.Set("session_id", childID)
		entry.Set("created_at", stamp)
		entry.Set("updated_at", stamp)
		entry.Set("is_fresh_reset", true)
		return entry, nil
	}
	BranchLocalRoute = func(childID, principalID string, originProfile, routeProfile any) (string, *pyjson.Object, *pyjson.Object, float64, error) {
		source := pyjson.ObjectOf("platform", "local", "chat_id", childID, "chat_name", nil, "chat_type", "dm", "user_id", principalID,
			"user_name", nil, "thread_id", nil, "chat_topic", nil)
		if profile, ok := originProfile.(string); ok && profile != "" {
			source.Set("profile", profile)
		}
		namespace := "agent:main"
		if profile, ok := routeProfile.(string); ok && profile != "" && profile != "default" {
			namespace = "agent:" + profile
		}
		route := namespace + ":local:dm:" + childID
		at := time.Now()
		entry := localEntryFixture(route, childID, source, at)
		return route, source, entry, float64(at.UnixNano()) / 1e9, nil
	}
}

// Routing entry dict in the shape SessionEntry.to_dict publishes for a local session
func localEntryFixture(route, sessionID string, origin *pyjson.Object, at time.Time) *pyjson.Object {
	stamp := pydatetime.ISOFormatNaive(at)
	return pyjson.ObjectOf("session_key", route, "session_id", sessionID, "created_at", stamp, "updated_at", stamp,
		"display_name", nil, "platform", "local", "chat_type", "dm", "metadata", pyjson.NewObject(),
		"input_tokens", int64(0), "output_tokens", int64(0), "cache_read_tokens", int64(0), "cache_write_tokens", int64(0),
		"total_tokens", int64(0), "estimated_cost_usd", 0.0, "cost_status", "unknown", "last_prompt_tokens", int64(0),
		"expiry_finalized", false, "suspended", false, "resume_pending", false, "resume_reason", nil,
		"yolo", false, "yolo_toggle", nil, "last_resume_marked_at", nil, "active_turn_token", nil, "active_turn_started_at", nil,
		"is_fresh_reset", false, "was_auto_reset", false, "auto_reset_reason", nil, "reset_had_activity", false, "prev_session_id", nil,
		"origin", origin)
}

// Origin dict in the shape SessionSource.to_dict publishes for a local DM
func localOriginFixture(chatID, userID string) *pyjson.Object {
	return pyjson.ObjectOf("platform", "local", "chat_id", chatID, "chat_name", nil, "chat_type", "dm", "user_id", userID,
		"user_name", nil, "thread_id", nil, "chat_topic", nil)
}

// Frozen policy dict in the shape asdict(build_policy) publishes for a cli create of cwd with model m
func localPolicyFixture(cwd string) *pyjson.Object {
	request, _ := pyjson.Dumps(pyjson.ObjectOf("source", "cli", "cwd", cwd, "model", "m", "toolsets", []any{}))
	config, _ := pyjson.Dumps(pyjson.ObjectOf("platform_toolsets", pyjson.ObjectOf("cli", []any{})))
	return pyjson.ObjectOf("source", "cli", "platform", "cli", "cwd", cwd, "model", "m", "toolsets", []any{},
		"config_json", config, "request_json", request, "terminal_json", "{}", "credential_ref", nil, "config_secret_ref", nil,
		"editor_mcp_json", nil, "kanban_json", nil, "safe_mode", false, "ignore_user_config", false, "skills_prompt", nil,
		"coding_posture", false, "client_env_json", nil, "client_env_remote", false, "client_env_rules_json", nil,
		"edit_approval_policy", "ask")
}

// Commits a local cli owned logical session for principal human and request r, returning its id
func localSessionFixture(t *testing.T, db *SessionDB, epoch int64, cwd string) string {
	t.Helper()
	sid, err := LocalIdentity("profile", "human", "r")
	if err != nil {
		t.Fatal(err)
	}
	entry := localEntryFixture("local:"+sid, sid, localOriginFixture(sid, "human"), time.Now())
	receipt := pyjson.ObjectOf("profile_id", "profile", "principal_id", "human", "request_id", "r", "session_id", sid,
		"route", "local:"+sid, "entry", entry, "policy", localPolicyFixture(cwd))
	if _, err := db.CommitLocalSession(context.Background(), epoch, LocalCreateReceipt{Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	return sid
}

// Commits a local session and resets it to a physical child, the managed worker layout after new
func localResetFixture(t *testing.T, db *SessionDB, epoch int64, child string) string {
	t.Helper()
	sid := localSessionFixture(t, db, epoch, t.TempDir())
	reset := localEntryFixture("local:"+sid, child, localOriginFixture(sid, "human"), time.Now())
	reset.Set("is_fresh_reset", true)
	if err := db.ResetLocalTarget(context.Background(), epoch, sid, reset); err != nil {
		t.Fatal(err)
	}
	return sid
}

// Mutation arguments at the session's current revision and generation
func mutationAt(db *SessionDB, sid, operation, requestID string, payload *pyjson.Object) RuntimeMutation {
	row, _ := db.GetSession(context.Background(), sid)
	revision, _ := exactInt(rowValue(row, "runtime_revision"))
	generation, _ := exactInt(rowValue(row, "runtime_generation"))
	return RuntimeMutation{PrincipalID: "human", SessionID: sid, RequestID: requestID, Operation: operation, Payload: payload,
		ExpectedRevision: revision, ExpectedGeneration: &generation}
}

// Result of a mutation that must succeed
func mustMutate(t *testing.T, db *SessionDB, in RuntimeMutation) *pyjson.Object {
	t.Helper()
	out, err := db.MutateRuntimeSession(context.Background(), in)
	if err != nil {
		t.Fatalf("%s %v", in.Operation, err)
	}
	return out
}

// Refusal reason of a mutation that must fail
func mutateRefusal(t *testing.T, db *SessionDB, in RuntimeMutation) string {
	t.Helper()
	_, err := db.MutateRuntimeSession(context.Background(), in)
	return refusalReason(t, err)
}

// Pointer to an int64 literal
func int64Ptr(n int64) *int64 { return &n }

// Admits one queued input for principal human
func admitInput(t *testing.T, db *SessionDB, epoch int64, principal, sid, requestID string, payload *pyjson.Object) *pyjson.Object {
	t.Helper()
	row, err := db.AdmitSessionInput(context.Background(), SessionInput{Epoch: epoch, PrincipalID: principal, SessionID: sid, RequestID: requestID, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Claims the head of a session FIFO, fatal when nothing is claimable
func claimHead(t *testing.T, db *SessionDB, epoch int64, sid string) *pyjson.Object {
	t.Helper()
	claim, _, err := db.ClaimSessionInput(context.Background(), ClaimInput{Epoch: epoch, SessionID: sid})
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	return claim
}

// Settles a claimed admission completed with the result the gateway retain_result stores
func retainResult(t *testing.T, db *SessionDB, epoch int64, row *pyjson.Object, result *pyjson.Object) {
	t.Helper()
	generation, _ := exactInt(rowValue(row, "generation"))
	if _, err := db.SettleSessionInput(context.Background(), SettleInput{Epoch: epoch, AdmissionID: objStr(row, "admission_id"), Generation: generation, Outcome: "completed", Result: result}); err != nil {
		t.Fatal(err)
	}
}

// Stored result of a terminal admission decoded as the gateway admission_result reads it, nil when none
func admissionResult(t *testing.T, db *SessionDB, admissionID string) *pyjson.Object {
	t.Helper()
	ctx := context.Background()
	row, err := db.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		t.Fatal(err)
	}
	if objStr(row, "status") != "terminal" {
		return nil
	}
	raw, ok, err := db.GetMeta(ctx, TerminalResultPrefix+admissionID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return nil
	}
	decoded, err := LoadTerminalResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.(*pyjson.Object)
}

// Message contents of a session through the direct store
func contentsOf(t *testing.T, db *SessionDB, sid string, o GetMessagesOptions) []any {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, o)
	if err != nil {
		t.Fatal(err)
	}
	out := []any{}
	for _, row := range rows {
		out = append(out, rowValue(row, "content"))
	}
	return out
}

// Session row of the direct store, nil when missing
func sessionOf(t *testing.T, db *SessionDB, sid string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// Session id strings of a deleted_ids receipt value as a set
func idSet(values any) map[string]bool {
	out := map[string]bool{}
	for _, v := range values.([]any) {
		out[pyStr(v)] = true
	}
	return out
}

// True when two string sets hold the same ids
func deletedIDsAre(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// State meta keys matching a GLOB prefix
func metaKeys(t *testing.T, db *SessionDB, prefix string) []string {
	t.Helper()
	rows := rowsOf(t, db, "SELECT key FROM state_meta WHERE key GLOB ?", prefix+"*")
	out := []string{}
	for _, row := range rows {
		out = append(out, pyStr(rowValue(row, "key")))
	}
	return out
}

// Every state meta value joined, the blob the retirement tests scan for leaked text
func metaBlob(t *testing.T, db *SessionDB, query string, args ...any) string {
	t.Helper()
	out := ""
	for _, row := range rowsOf(t, db, query, args...) {
		out += pyStr(rowValue(row, "value"))
	}
	return out
}
