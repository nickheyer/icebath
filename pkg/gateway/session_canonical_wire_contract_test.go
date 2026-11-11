package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Socket double for a connection whose frames the case never reads
type wireContractTransport struct{}

func (wireContractTransport) Write(*pyjson.Object) bool { return true }
func (wireContractTransport) Close()                    {}

// Owned authority over a fresh store holding session s live, with a verified owner connection
func newWireContractOwner(t *testing.T) (context.Context, *state.SessionDB, *AuthorityConnection) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "current", db, epoch)
	a.Sessions["s"] = NewLiveSession(nil, "route")
	connection, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, connection
}

// Plain JSON tree of a dispatch reply
func wireReply(t *testing.T, reply *pyjson.Object) map[string]any {
	t.Helper()
	plain, ok := pyjson.Plain(reply).(map[string]any)
	if !ok {
		t.Fatalf("reply %v is not an object", reply)
	}
	return plain
}

func TestEveryCanonicalVerbHasADeclaredContract(t *testing.T) {
	_, _, connection := newWireContractOwner(t)
	served := map[string]bool{"profiles.list": true}
	for name := range connection.handlers() {
		served[name] = true
	}
	for name := range groupMethods {
		served[name] = true
	}
	var undeclared, orphaned []string
	for name := range served {
		if tuigateway.Methods[name] == nil && tuigateway.CanonicalMethods[name] == nil {
			undeclared = append(undeclared, name)
		}
	}
	for name := range tuigateway.CanonicalMethods {
		if !served[name] {
			orphaned = append(orphaned, name)
		}
	}
	slices.Sort(undeclared)
	slices.Sort(orphaned)
	if len(undeclared) != 0 {
		t.Fatalf("served without a declared contract: %v", undeclared)
	}
	if len(orphaned) != 0 {
		t.Fatalf("canonical declarations without a handler: %v", orphaned)
	}
}

func TestDeclaredParamsGateCanonicalVerbsBeforeTheirHandlers(t *testing.T) {
	ctx, db, connection := newWireContractOwner(t)
	defer connection.Close(ctx)
	row, err := db.GetSession(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	revision := row.GetOr("runtime_revision", nil)
	mutate := func(extra ...any) *pyjson.Object {
		params := pyjson.ObjectOf("session_id", "s", "request_id", "r1", "expected_revision", revision,
			"operation", "rename", "payload", pyjson.ObjectOf("title", "Declared"))
		for i := 0; i+1 < len(extra); i += 2 {
			params.Set(extra[i].(string), extra[i+1])
		}
		return params
	}
	for _, tc := range []struct {
		method string
		params *pyjson.Object
		field  string
	}{
		{"prompt.receipt", pyjson.ObjectOf("session_id", "s", "admission_id", "a", "bogus", int64(1)), "bogus"},
		{"prompt.cancel", pyjson.ObjectOf("session_id", "s"), "admission_id"},
		{"session.mutate", mutate("expected_revison", revision), "expected_revison"},
	} {
		reply, err := connection.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", tc.method, "params", tc.params))
		if err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		response := wireReply(t, reply)
		failure, _ := response["error"].(map[string]any)
		if fmt.Sprint(failure["code"]) != "4001" {
			t.Fatalf("%s: %v", tc.method, response)
		}
		data, _ := failure["data"].(map[string]any)
		if data["reason"] != "invalid_params" {
			t.Fatalf("%s: %v", tc.method, response)
		}
		var fields []string
		switch named := data["fields"].(type) {
		case []string:
			fields = named
		case []any:
			for _, field := range named {
				fields = append(fields, fmt.Sprint(field))
			}
		}
		if !slices.Contains(fields, tc.field) {
			t.Fatalf("%s: field %s not named in %v", tc.method, tc.field, response)
		}
	}
	applied, err := connection.Dispatch(ctx, pyjson.ObjectOf("id", int64(2), "method", "session.mutate", "params", mutate()))
	if err != nil {
		t.Fatal(err)
	}
	response := wireReply(t, applied)
	if invalid := tuigateway.CanonicalMethods["session.mutate"].Result.ModelValidate(response["result"]); invalid != nil {
		t.Fatalf("reply %v violates the contract: %v", response, invalid)
	}
	row, err = db.GetSession(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if title := row.GetOr("title", nil); title != "Declared" {
		t.Fatalf("title %v", title)
	}
}

func TestCanonicalResumeRejectsUnknownKeysAndValidatesRealSnapshot(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "test", "test", db, epoch)
	a.Sessions["s"] = NewLiveSession(nil, "route")
	viewer, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "human"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close(ctx)
	served := map[string]bool{"profiles.list": true}
	for name := range viewer.handlers() {
		served[name] = true
	}
	for name := range groupMethods {
		served[name] = true
	}
	for name := range served {
		if tuigateway.Methods[name] == nil && tuigateway.CanonicalMethods[name] == nil {
			t.Fatalf("%s is served without a declared contract", name)
		}
	}
	for i, params := range []*pyjson.Object{
		pyjson.ObjectOf("session_id", "s", "execution_generaton", int64(100)),
		pyjson.ObjectOf("session_id", []any{"s"}),
	} {
		reply, err := viewer.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", "session.resume", "params", params))
		if err != nil {
			t.Fatal(err)
		}
		failure, _ := wireReply(t, reply)["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		if data["reason"] != "invalid_params" {
			t.Fatalf("call %d answered %v", i, reply)
		}
	}
	viewer.mu.Lock()
	subscribed := len(viewer.subscriptions)
	viewer.mu.Unlock()
	if subscribed != 0 {
		t.Fatalf("refused resumes subscribed %d sessions", subscribed)
	}
	reply, err := viewer.Dispatch(ctx, pyjson.ObjectOf("id", int64(2), "method", "session.resume", "params", pyjson.ObjectOf("session_id", "s")))
	if err != nil {
		t.Fatal(err)
	}
	result := wireReply(t, reply)["result"]
	if invalid := tuigateway.CanonicalMethods["session.resume"].Result.ModelValidate(result); invalid != nil {
		t.Fatalf("snapshot %v violates the contract: %v", reply, invalid)
	}
	snapshot, _ := result.(map[string]any)
	viewer.mu.Lock()
	subscription := viewer.subscriptions["s"]
	viewer.mu.Unlock()
	if fmt.Sprint(snapshot["subscription_id"]) != fmt.Sprint(subscription) {
		t.Fatalf("subscription %v, connection holds %v", snapshot["subscription_id"], subscription)
	}
	if fmt.Sprint(snapshot["revision"]) != "0" {
		t.Fatalf("revision %v", snapshot["revision"])
	}
	if pending, ok := snapshot["pending"].([]any); !ok || len(pending) != 0 {
		t.Fatalf("pending %#v", snapshot["pending"])
	}
	info, _ := snapshot["info"].(map[string]any)
	if info["desktop_protocol"] != "hermes-gateway-v1" {
		t.Fatalf("desktop_protocol %v", info["desktop_protocol"])
	}
}

func TestCanonicalResultValidationCatchesAHandlerContractViolation(t *testing.T) {
	ctx, db := newTestStateDB(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "test", "test", db, epoch)
	viewer, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "human"}, false)
	if err != nil {
		t.Fatal(err)
	}
	strict := tuigateway.Strict
	tuigateway.Strict = true
	declared := tuigateway.Methods["ping"]
	broken := *declared
	broken.Result = pydantic.NewModel("BrokenPingResult", tuigateway.ClosedConfig, pydantic.Req("not_pong", pydantic.Bool))
	tuigateway.Methods["ping"] = &broken
	t.Cleanup(func() { tuigateway.Strict, tuigateway.Methods["ping"] = strict, declared })
	defer func() {
		violation, ok := recover().(*tuigateway.ContractViolation)
		if !ok || !strings.Contains(violation.Message, "ping") {
			t.Fatalf("dispatch raised %v, want a ContractViolation naming ping", violation)
		}
	}()
	_, _ = viewer.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", "ping", "params", pyjson.NewObject()))
	t.Fatal("a result outside its contract dispatched without a ContractViolation")
}
