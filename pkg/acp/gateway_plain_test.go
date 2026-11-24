package acp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func seededStore(t *testing.T, path string, sessions ...state.NewSession) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range sessions {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessage(ctx, n.ID, map[string]any{"role": "user", "content": "saved turn"}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func rawExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sessionIDs(rows []*jsonutil.Object) []string {
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, anyutil.String(row.ValueOr("session_id", nil)))
	}
	return ids
}

func TestCatalogPrefersCanonicalCwdWithoutRewritingStorage(t *testing.T) {
	dir := testingHome(t)
	path := filepath.Join(dir, "state.db")
	seededStore(t, path, state.NewSession{ID: "canonical-acp", Source: "acp", Cwd: dir, ModelConfig: map[string]any{"cwd": "/stale-legacy-workspace"}})
	before := readBytes(t, path)
	ctx := context.Background()
	rows, err := CatalogSessions(ctx, path, dir)
	if err != nil || !reflect.DeepEqual(sessionIDs(rows), []string{"canonical-acp"}) || rows[0].ValueOr("cwd", nil) != dir {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if stale, err := CatalogSessions(ctx, path, "/stale-legacy-workspace"); err != nil || len(stale) != 0 {
		t.Fatalf("stale rows = %v, %v", stale, err)
	}
	if !bytes.Equal(readBytes(t, path), before) {
		t.Fatal("the catalog read rewrote the store")
	}
}

func TestCatalogListsGatewayOwnedEditorThreadsOnly(t *testing.T) {
	dir := testingHome(t)
	path := filepath.Join(dir, "state.db")
	seededStore(t, path, state.NewSession{ID: "editor-thread", Source: "local", Cwd: dir}, state.NewSession{ID: "cli-chat", Source: "local", Cwd: dir})
	rawExec(t, path, "UPDATE sessions SET created_source = ? WHERE id = ?", "acp", "editor-thread")
	rawExec(t, path, "UPDATE sessions SET created_source = ? WHERE id = ?", "cli", "cli-chat")
	rows, err := CatalogSessions(context.Background(), path, dir)
	if err != nil || !reflect.DeepEqual(sessionIDs(rows), []string{"editor-thread"}) {
		t.Fatalf("rows = %v, %v", sessionIDs(rows), err)
	}
}

func TestDefaultAcpDoesNotConstructAnEmbeddedSessionManager(t *testing.T) {
	testingHome(t)
	if _, ok := NewAgent().(*GatewayACPAgent); !ok {
		t.Fatalf("default agent is %T", NewAgent())
	}
}

func TestEffortPickAppliesThroughTheReasoningMutationAndReturnsThePickers(t *testing.T) {
	swap(t, &gatewayModelState, func(context.Context, *jsonutil.Object) *acputil.SessionModelState {
		return &acputil.SessionModelState{AvailableModels: []acputil.ModelInfo{{ModelID: "openrouter:fixture", Name: "fixture"}}, CurrentModelID: "openrouter:fixture"}
	})
	info := jsonObj(t, `{"model": "fixture", "provider": "openrouter", "reasoning_effort": "medium"}`)
	type mutation struct {
		operation string
		payload   any
	}
	var mutations []mutation
	a, link := bareGatewayAgent(t, map[string]string{"s": `{"revision": 1, "execution_generation": 1}`})
	link.set(func(ctx context.Context, method string, params map[string]any) (*jsonutil.Object, error) {
		copyInfo := func() *jsonutil.Object {
			out := jsonutil.NewObject()
			for _, key := range info.Keys() {
				out.Set(key, info.Value(key))
			}
			return out
		}
		switch method {
		case "session.mutate":
			payload := jsonutil.Plain(params["payload"]).(map[string]any)
			mutations = append(mutations, mutation{anyutil.String(params["operation"]), payload})
			info.Set("reasoning_effort", payload["effort"])
			return jsonObj(t, `{"status": "applied"}`), nil
		case "session.info":
			return copyInfo(), nil
		}
		return jsonutil.ObjectOf("revision", int64(1), "execution_generation", int64(1), "info", copyInfo()), nil
	})
	reply, err := a.SetConfigOption(context.Background(), &acputil.SetConfigOptionParams{ConfigID: "reasoning_effort", SessionID: "s", Value: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mutations, []mutation{{"reasoning", map[string]any{"effort": "high"}}}) {
		t.Fatalf("mutations = %v", mutations)
	}
	pickers := map[string]string{}
	for _, option := range reply.ConfigOptions {
		sel := option.(*acputil.SessionConfigOptionSelect)
		pickers[*sel.Category] = sel.CurrentValue
	}
	if !reflect.DeepEqual(pickers, map[string]string{"mode": "ask", "model": "openrouter:fixture", "thought_level": "high"}) {
		t.Fatalf("pickers = %v", pickers)
	}
}

func TestTheEffortPickerOffersDefaultOnlyWhileNoLevelIsSet(t *testing.T) {
	effort := func(level string) (string, []string) {
		options := gatewayConfigOptions(nil, jsonutil.ObjectOf("reasoning_effort", level), DefaultMode)
		sel := options[len(options)-1].(*acputil.SessionConfigOptionSelect)
		var values []string
		for _, choice := range sel.Options {
			values = append(values, choice.Value)
		}
		return sel.CurrentValue, values
	}
	current, values := effort("medium")
	for _, value := range values {
		if value == "default" {
			t.Fatalf("default offered beside a set level: %v", values)
		}
	}
	if current != "medium" {
		t.Fatalf("current = %q", current)
	}
	if current, values = effort(""); current != "default" || values[0] != "default" {
		t.Fatalf("current %q values %v", current, values)
	}
}

func fixedVersion(t *testing.T) {
	t.Helper()
	swap(t, &getVersionInfo, func() (updaterecord.VersionInfo, error) {
		return updaterecord.VersionInfo{BaseVersion: "1.2.3", DerivedVersion: "1.2.3+4.gabcdef0"}, nil
	})
}

func TestVersionFlagDisplaysDerivedVersion(t *testing.T) {
	fixedVersion(t)
	code, stdout, _ := runMain("--version")
	if code != 0 || stdout != "1.2.3+4.gabcdef0\n" {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
}

func TestInitializeAdvertisesImagePromptCapability(t *testing.T) {
	testingHome(t)
	resp, err := NewAgent().(*GatewayACPAgent).Initialize(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentCapabilities == nil || resp.AgentCapabilities.PromptCapabilities == nil ||
		resp.AgentCapabilities.PromptCapabilities.Image == nil || !*resp.AgentCapabilities.PromptCapabilities.Image {
		t.Fatalf("capabilities = %+v", resp.AgentCapabilities)
	}
}

func TestCancelUsesObservedGenerationAndMutationsFailExplicitly(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"shared": `{"execution_generation": 4, "revision": 7}`})
	link.set(func(context.Context, string, map[string]any) (*jsonutil.Object, error) {
		return nil, &gateway.GatewayClientError{Msg: "stale_generation"}
	})
	ctx := context.Background()
	if err := a.Cancel(ctx, &acputil.CancelNotification{SessionID: "shared"}); err != nil || len(link.log()) != 0 {
		t.Fatalf("err %v calls %v", err, link.log())
	}
	onLoop(a, func() bool { a.admissions["shared"] = "adm-1"; return true })
	if err := a.Cancel(ctx, &acputil.CancelNotification{SessionID: "shared"}); !refusedWith(err, "stale_generation") {
		t.Fatalf("err = %v", err)
	}
	calls := link.log()
	if !reflect.DeepEqual(calls[0], rpcCall{"prompt.cancel", map[string]any{"session_id": "shared", "admission_id": "adm-1"}}) || calls[1].method != "prompt.receipt" {
		t.Fatalf("calls = %v", calls)
	}
	fenced := func(what string, call func() error) {
		link.reset()
		if err := call(); !refusedWith(err, "stale_generation") {
			t.Fatalf("%s err = %v", what, err)
		}
		methods := link.methods()
		if len(methods) == 0 {
			t.Fatalf("%s made no call", what)
		}
		for _, method := range methods {
			if method != "session.resume" && method != "session.mutate" {
				t.Fatalf("%s methods = %v", what, methods)
			}
		}
	}
	fenced("set_session_model", func() error {
		_, err := a.SetSessionModel(ctx, &acputil.SetSessionModelRequest{SessionID: "shared", ModelID: "new-model"})
		return err
	})
	fenced("set_session_mode", func() error {
		_, err := a.SetSessionMode(ctx, &acputil.SetSessionModeRequest{SessionID: "shared", ModeID: "dont_ask"})
		return err
	})
}

func TestGatewayAcpCatalogDoesNotBootARuntime(t *testing.T) {
	dir := testingHome(t)
	a := NewGatewayACPAgent()
	ctx := context.Background()
	resp, err := a.ListSessions(ctx, &acputil.ListSessionsRequest{})
	if err != nil || len(resp.Sessions) != 0 {
		t.Fatalf("sessions = %v, %v", resp, err)
	}
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state.db stat err = %v", err)
	}
	seededStore(t, path, state.NewSession{ID: "persisted-acp", Source: "acp", Model: "fixture", ModelConfig: map[string]any{"cwd": dir}})
	resp, err = a.ListSessions(ctx, &acputil.ListSessionsRequest{Cwd: &dir})
	if err != nil || len(resp.Sessions) != 1 || resp.Sessions[0].SessionID != "persisted-acp" {
		t.Fatalf("sessions = %+v, %v", resp, err)
	}
	if onLoop(a, func() bool { return a.gateway != nil }) {
		t.Fatal("listing booted a gateway connection")
	}
}

func TestGatewayAcpAdvertisesEverySessionCapabilityItImplements(t *testing.T) {
	testingHome(t)
	ctx := context.Background()
	gatewayAgent := NewGatewayACPAgent()
	advertised, err := gatewayAgent.Initialize(ctx, &acputil.InitializeRequest{ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	dump := func(c *acputil.SessionCapabilities) string {
		text, _ := jsonutil.Encode(c, jsonutil.SortKeys(), jsonutil.Spaced(), jsonutil.ASCII())
		return text
	}
	got := dump(advertised.AgentCapabilities.SessionCapabilities)
	var agent any = gatewayAgent
	_, forks := agent.(acputil.SessionForker)
	_, lists := agent.(acputil.SessionLister)
	_, resumes := agent.(acputil.SessionResumer)
	if got != `{"fork": {}, "list": {}, "resume": {}}` || !forks || !lists || !resumes {
		t.Fatalf("capabilities %s fork %v list %v resume %v", got, forks, lists, resumes)
	}
}
