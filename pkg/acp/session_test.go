package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func wslHost(t *testing.T) {
	t.Helper()
	previous := home.SetWSLProbe(func() bool { return true })
	t.Cleanup(func() { home.SetWSLProbe(previous) })
}

func fakeAgentBuild(t *testing.T, cfg config.Tree, runtime func(requested, target string) (*provider.Runtime, error), build func(o agent.AIAgentOptions) (any, error)) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swap(t, &loadSessionConfig, func(ctx context.Context) (config.Tree, error) { return cfg, nil })
	swap(t, &resolveSessionRuntime, func(ctx context.Context, requested, target string) (*provider.Runtime, error) {
		return runtime(requested, target)
	})
	swap(t, &ensureMCPDiscoveryBeforeAgentBuild, func(ctx context.Context, cfg config.Tree) {})
	swap(t, &registerTaskCwdHook, func(ctx context.Context, taskID, cwd string) {})
	swap(t, &newAIAgent, func(ctx context.Context, o agent.AIAgentOptions) (any, error) { return build(o) })
}

func emptyRuntime(requested, target string) (*provider.Runtime, error) {
	return &provider.Runtime{}, nil
}

func jsonField(t *testing.T, row *pyjson.Object, key string) *pyjson.Object {
	t.Helper()
	text, _ := row.GetOr(key, nil).(string)
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("%s is not JSON: %v", key, err)
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		t.Fatalf("%s is not an object: %#v", key, v)
	}
	return obj
}

func getRow(t *testing.T, db *state.SessionDB, sid string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestCreateSessionReturnsState(t *testing.T) {
	m := newMockManager(t)
	st := mustCreate(t, m, "/tmp/work")
	if st.Cwd != "/tmp/work" || st.SessionID == "" || len(st.History) != 0 || st.History == nil || st.Agent == nil {
		t.Fatalf("unexpected state %+v", st)
	}
}

func TestRegisterTaskCwdTranslatesWindowsDriveForWslTools(t *testing.T) {
	wslHost(t)
	registerTaskCwd(context.Background(), "session-1", `E:\Projects\AI\paperclip`)
	overrides, err := tools.ResolveTaskOverrides(nil, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(overrides, map[string]any{"cwd": "/mnt/e/Projects/AI/paperclip"}) {
		t.Fatalf("overrides = %#v", overrides)
	}
}

func TestMakeAgentUsesSessionCwdDuringInitAndStampsRuntime(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	observed := ""
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"default": "fake-model", "provider": "fake-provider"}, "mcp_servers": map[string]any{}},
		func(requested, target string) (*provider.Runtime, error) {
			return &provider.Runtime{Provider: requested, APIMode: "codex_app_server", BaseURL: "https://example.invalid", APIKey: "test-key"}, nil
		},
		func(o agent.AIAgentOptions) (any, error) {
			observed = o.Cwd
			return &nsAgent{model: "fake-model"}, nil
		})
	if _, err := NewSessionManager(nil, nil).CreateSession(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if observed != workspace {
		t.Fatalf("cwd = %q, want %q", observed, workspace)
	}
}

func TestMakeAgentPrefersPassedToolsetsOverConfigServers(t *testing.T) {
	var seen []agent.AIAgentOptions
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"default": "m", "provider": "p"}, "mcp_servers": map[string]any{"cfg-server": map[string]any{}}},
		emptyRuntime, func(o agent.AIAgentOptions) (any, error) {
			seen = append(seen, o)
			return &nsAgent{}, nil
		})
	m := NewSessionManager(nil, nil)
	if _, err := m.MakeAgent(context.Background(), MakeAgentOptions{SessionID: "fresh", Cwd: "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MakeAgent(context.Background(), MakeAgentOptions{SessionID: "rebuilt", Cwd: ".",
		EnabledToolsets: []string{"icebath-acp", "mcp-acp-server"}, DisabledToolsets: []string{"browser"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(seen[0].EnabledToolsets, "mcp-cfg-server") || seen[0].DisabledToolsets != nil {
		t.Fatalf("fresh toolsets %v disabled %v", seen[0].EnabledToolsets, seen[0].DisabledToolsets)
	}
	if !reflect.DeepEqual(seen[1].EnabledToolsets, []string{"icebath-acp", "mcp-acp-server"}) || !reflect.DeepEqual(seen[1].DisabledToolsets, []string{"browser"}) {
		t.Fatalf("rebuilt toolsets %v disabled %v", seen[1].EnabledToolsets, seen[1].DisabledToolsets)
	}
}

func TestFreshAgentToolSurfaceHonoursToolsetConfig(t *testing.T) {
	cases := []struct {
		name     string
		cfg      config.Tree
		offered  string
		withheld string
	}{
		{"agent_disabled_toolsets", config.Tree{"agent": map[string]any{"disabled_toolsets": "['code_execution']"}}, "file", "code_execution"},
		{"platform_toolsets_acp", config.Tree{"platform_toolsets": map[string]any{"acp": []any{"file"}}}, "file", "code_execution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []agent.AIAgentOptions
			cfg := config.Tree{"model": map[string]any{"default": "m"}}
			for k, v := range tc.cfg {
				cfg[k] = v
			}
			fakeAgentBuild(t, cfg, emptyRuntime, func(o agent.AIAgentOptions) (any, error) {
				seen = append(seen, o)
				return &nsAgent{}, nil
			})
			ctx := context.Background()
			if _, err := NewSessionManager(nil, nil).MakeAgent(ctx, MakeAgentOptions{SessionID: "fresh", Cwd: "."}); err != nil {
				t.Fatal(err)
			}
			env := &tools.Env{Home: home.Dir(ctx), Config: cfg}
			surface := func(enabled, disabled []string) map[string]bool {
				names := map[string]bool{}
				for _, def := range tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true}) {
					fn, _ := def["function"].(map[string]any)
					names[pyStr(fn["name"])] = true
				}
				return names
			}
			withheld := tools.Default.ResolveToolset(ctx, "", tc.withheld)
			offered := tools.Default.ResolveToolset(ctx, "", tc.offered)
			if len(withheld) == 0 || len(offered) == 0 {
				t.Fatalf("toolsets resolve to nothing: %v %v", offered, withheld)
			}
			byDefault := surface([]string{"icebath-acp"}, nil)
			for _, name := range withheld {
				if !byDefault[name] {
					t.Fatalf("%s is not offered by default", name)
				}
			}
			names := surface(seen[0].EnabledToolsets, seen[0].DisabledToolsets)
			for _, name := range offered {
				if !names[name] {
					t.Fatalf("%s missing from the surface %v", name, names)
				}
			}
			for _, name := range withheld {
				if names[name] {
					t.Fatalf("%s must be withheld", name)
				}
			}
		})
	}
}

func TestFreshAgentMcpServersFollowPlatformToolsets(t *testing.T) {
	cases := []struct {
		name        string
		acpToolsets []any
		expected    []string
	}{
		{"default", nil, []string{"mcp-alpha", "mcp-beta"}},
		{"allowlist", []any{"icebath-acp", "alpha"}, []string{"mcp-alpha"}},
		{"no_mcp", []any{"icebath-acp", "no_mcp"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []agent.AIAgentOptions
			cfg := config.Tree{"model": map[string]any{"default": "m"}, "mcp_servers": map[string]any{
				"alpha": map[string]any{"command": "a"}, "beta": map[string]any{"command": "b"}, "off": map[string]any{"enabled": false}}}
			if tc.acpToolsets != nil {
				cfg["platform_toolsets"] = map[string]any{"acp": tc.acpToolsets}
			}
			fakeAgentBuild(t, cfg, emptyRuntime, func(o agent.AIAgentOptions) (any, error) {
				seen = append(seen, o)
				return &nsAgent{}, nil
			})
			if _, err := NewSessionManager(nil, nil).MakeAgent(context.Background(), MakeAgentOptions{SessionID: "fresh", Cwd: "."}); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, name := range seen[0].EnabledToolsets {
				if strings.HasPrefix(name, "mcp-") {
					got = append(got, name)
				}
				if name == "alpha" || name == "beta" || name == "no_mcp" {
					t.Fatalf("bare name %s in %v", name, seen[0].EnabledToolsets)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.expected) {
				t.Fatalf("mcp toolsets = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestMakeAgentSurfacesTheProviderResolutionFailure(t *testing.T) {
	bareFails := true
	var built []agent.AIAgentOptions
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"default": "m", "provider": "openai-codex"}},
		func(requested, target string) (*provider.Runtime, error) {
			return nil, errors.New("No Codex credentials stored. Run `hermes auth add openai-codex`")
		},
		func(o agent.AIAgentOptions) (any, error) {
			if bareFails {
				return nil, errors.New("No LLM provider configured. Run `hermes setup`")
			}
			built = append(built, o)
			return &nsAgent{}, nil
		})
	m := NewSessionManager(nil, nil)
	_, err := m.MakeAgent(context.Background(), MakeAgentOptions{SessionID: "rebuilt", Cwd: ".", RequestedProvider: "openai-codex"})
	if err == nil || !strings.Contains(err.Error(), "No Codex credentials stored") {
		t.Fatalf("err = %v", err)
	}
	var failure *resolveFailure
	if !errors.As(err, &failure) || !strings.Contains(failure.Cause().Error(), "No LLM provider configured") {
		t.Fatalf("cause = %v", err)
	}
	bareFails = false
	if _, err := m.MakeAgent(context.Background(), MakeAgentOptions{SessionID: "fresh", Cwd: "."}); err != nil {
		t.Fatal(err)
	}
	if built[0].Provider != "" {
		t.Fatalf("provider = %q", built[0].Provider)
	}
}

func TestMakeAgentForwardsResolvedCredentialPool(t *testing.T) {
	var seen []agent.AIAgentOptions
	pool := &provider.Pool{}
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"default": "m", "provider": "openai-codex"}},
		func(requested, target string) (*provider.Runtime, error) {
			return &provider.Runtime{Provider: "openai-codex", APIMode: "codex_app_server", APIKey: "test-key", CredentialPool: pool}, nil
		},
		func(o agent.AIAgentOptions) (any, error) {
			seen = append(seen, o)
			return &nsAgent{}, nil
		})
	if _, err := NewSessionManager(nil, nil).MakeAgent(context.Background(), MakeAgentOptions{SessionID: "s", Cwd: "."}); err != nil {
		t.Fatal(err)
	}
	if seen[0].CredentialPool != pool {
		t.Fatal("credential pool not forwarded by identity")
	}
}

func TestTranslateAcpCwdConvertsWindowsDrivePathWhenWsl(t *testing.T) {
	wslHost(t)
	if got := translateACPCwd(`E:\Projects\AI\paperclip`); got != "/mnt/e/Projects/AI/paperclip" {
		t.Fatalf("got %q", got)
	}
}

func TestForkSessionStoresTranslatedCwdOnWsl(t *testing.T) {
	m := newMockManager(t)
	wslHost(t)
	original := mustCreate(t, m, "/tmp/base")
	forked, err := m.ForkSession(context.Background(), original.SessionID, `D:\work\project`)
	if err != nil || forked == nil {
		t.Fatalf("fork = %v, %v", forked, err)
	}
	if forked.Cwd != "/mnt/d/work/project" {
		t.Fatalf("cwd = %q", forked.Cwd)
	}
}

func TestUpdateCwdStoresTranslatedCwdOnWsl(t *testing.T) {
	m := newMockManager(t)
	wslHost(t)
	st := mustCreate(t, m, "/tmp/old")
	updated := m.UpdateCwd(context.Background(), st.SessionID, `C:\Users\foo\project`)
	if updated == nil || updated.Cwd != "/mnt/c/Users/foo/project" {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestSymlinkAliasComparesEqual(t *testing.T) {
	dir := t.TempDir()
	realDir, alias := filepath.Join(dir, "real"), filepath.Join(dir, "alias")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	if normalizeCwdForCompare(alias) != normalizeCwdForCompare(realDir) {
		t.Fatal("alias and target compare different")
	}
}

func TestDistinctDirsStillCompareDifferent(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if normalizeCwdForCompare(a) == normalizeCwdForCompare(b) {
		t.Fatal("distinct dirs compare equal")
	}
}

func TestMissingPathKeepsLexicalNormalization(t *testing.T) {
	if normalizeCwdForCompare("/nonexistent-hermes-test/x/../y") != normalizeCwdForCompare("/nonexistent-hermes-test/y") {
		t.Fatal("missing path not normalized lexically")
	}
}

func TestListSessionsMatchesSymlinkAliasCwd(t *testing.T) {
	m := newMockManager(t)
	dir := t.TempDir()
	realDir, alias := filepath.Join(dir, "proj"), filepath.Join(dir, "link")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	st := mustCreate(t, m, realDir)
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	listed, err := m.ListSessions(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].GetOr("session_id", nil) != st.SessionID {
		t.Fatalf("listed = %v", listed)
	}
}

func TestListSessionsEmpty(t *testing.T) {
	m := newMockManager(t)
	listed, err := m.ListSessions(context.Background(), "")
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("listed = %v, %v", listed, err)
	}
}

func TestSaveSessionPreservesExistingMessagesOnEncodeFailure(t *testing.T) {
	m := newMockManager(t)
	ctx := context.Background()
	st := mustCreate(t, m, ".")
	st.History = append(st.History, map[string]any{"role": "user", "content": "original"})
	m.SaveSession(ctx, st.SessionID)
	st.History = []map[string]any{
		{"role": "user", "content": "replacement"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"bad": make(chan int)}}},
	}
	m.SaveSession(ctx, st.SessionID)
	messages, err := m.GetDB(ctx).GetMessagesAsConversation(ctx, st.SessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "original" {
		t.Fatalf("messages = %v", messages)
	}
	if _, ok := pyFloat(messages[0]["timestamp"]); !ok {
		t.Fatalf("timestamp = %#v", messages[0]["timestamp"])
	}
}

func TestFirstPersistKeepsProviderSnapshot(t *testing.T) {
	db := provenanceDB(t)
	a := &nsAgent{model: "test-model", provider: "anthropic", baseURL: "https://anthropic.example/v1", apiMode: "anthropic_messages"}
	m := NewSessionManager(func() any { return a }, db)
	ctx := context.Background()
	st := mustCreate(t, m, "/work")
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	m.SaveSession(ctx, st.SessionID)
	mc := jsonField(t, getRow(t, db, st.SessionID), "model_config")
	want := map[string]any{"cwd": "/work", "provider": "anthropic", "base_url": "https://anthropic.example/v1", "api_mode": "anthropic_messages"}
	if !reflect.DeepEqual(mc.Vals, want) {
		t.Fatalf("model_config = %v", mc.Vals)
	}
}

func TestOnlyRestoresAcpSessions(t *testing.T) {
	m := newMockManager(t)
	ctx := context.Background()
	if err := m.GetDB(ctx).CreateSession(state.NewSession{ID: "cli-session-123", Source: "cli", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if m.GetSession(ctx, "cli-session-123") != nil {
		t.Fatal("a cli session was restored")
	}
}

func pruneIDs(t *testing.T, db *state.SessionDB) map[string]bool {
	t.Helper()
	zero := 0.0
	rows, err := db.ListPruneCandidates(context.Background(), &zero, "acp", false, state.PruneFilters{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, row := range rows {
		ids[pyStr(row.GetOr("id", nil))] = true
	}
	return ids
}

func TestEndAllSessionsStampsEndedAndUnlocksPrune(t *testing.T) {
	db := provenanceDB(t)
	a := &nsAgent{model: "test-model"}
	m := NewSessionManager(func() any { return a }, db)
	ctx := context.Background()
	endedEarlier, liveOne, liveTwo := mustCreate(t, m, "/work"), mustCreate(t, m, "/work"), mustCreate(t, m, "/work")
	for _, st := range []*SessionState{endedEarlier, liveOne, liveTwo} {
		st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
		m.SaveSession(ctx, st.SessionID)
	}
	if err := db.EndSession(endedEarlier.SessionID, "compression"); err != nil {
		t.Fatal(err)
	}
	if got := pruneIDs(t, db); !reflect.DeepEqual(got, map[string]bool{endedEarlier.SessionID: true}) {
		t.Fatalf("candidates before = %v", got)
	}
	if ended := m.EndAllSessions(ctx, ""); ended != 3 {
		t.Fatalf("ended = %d", ended)
	}
	if got := getRow(t, db, endedEarlier.SessionID).GetOr("end_reason", nil); got != "compression" {
		t.Fatalf("first end reason = %v", got)
	}
	for _, sid := range []string{liveOne.SessionID, liveTwo.SessionID} {
		row := getRow(t, db, sid)
		if row.GetOr("ended_at", nil) == nil || row.GetOr("end_reason", nil) != "acp_disconnect" {
			t.Fatalf("row %s = %v", sid, row.Vals)
		}
	}
	want := map[string]bool{endedEarlier.SessionID: true, liveOne.SessionID: true, liveTwo.SessionID: true}
	if got := pruneIDs(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates after = %v", got)
	}
}

func TestRestoreReopensASessionEndedByPreviousProcess(t *testing.T) {
	db := provenanceDB(t)
	a := &nsAgent{model: "test-model"}
	ctx := context.Background()
	first := NewSessionManager(func() any { return a }, db)
	st := mustCreate(t, first, "/work")
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	first.SaveSession(ctx, st.SessionID)
	first.EndAllSessions(ctx, "")
	if getRow(t, db, st.SessionID).GetOr("ended_at", nil) == nil {
		t.Fatal("session not ended")
	}
	second := NewSessionManager(func() any { return a }, db)
	if second.GetSession(ctx, st.SessionID) == nil {
		t.Fatal("session not restored")
	}
	row := getRow(t, db, st.SessionID)
	if row == nil || row.GetOr("ended_at", nil) != nil || row.GetOr("end_reason", nil) != nil {
		t.Fatalf("row = %v", row)
	}
}

func TestEndAllSessionsWithoutDbIsANoop(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swap(t, &acquireSessionDB, func(ctx context.Context, path string) (*state.DB, error) { return nil, errNoStore })
	m := NewSessionManager(mockFactory, nil)
	if got := m.EndAllSessions(context.Background(), ""); got != 0 {
		t.Fatalf("ended = %d", got)
	}
}

func TestSessionsSearchableViaFts(t *testing.T) {
	m := newMockManager(t)
	ctx := context.Background()
	st := mustCreate(t, m, ".")
	st.History = append(st.History, map[string]any{"role": "user", "content": "how do I configure nginx"},
		map[string]any{"role": "assistant", "content": "Here is the nginx config..."})
	m.SaveSession(ctx, st.SessionID)
	results, err := m.GetDB(ctx).SearchMessages(ctx, "nginx", state.NewSearchMessagesOptions())
	if err != nil || len(results) == 0 {
		t.Fatalf("results = %v, %v", results, err)
	}
	found := false
	for _, r := range results {
		found = found || r.GetOr("session_id", nil) == st.SessionID
	}
	if !found {
		t.Fatal("session not in search results")
	}
}

func TestAssistantReasoningFieldsPersisted(t *testing.T) {
	m := newMockManager(t)
	ctx := context.Background()
	st := mustCreate(t, m, ".")
	st.History = append(st.History, map[string]any{
		"role": "assistant", "content": "hello", "reasoning": "step-by-step",
		"reasoning_details":     []any{map[string]any{"type": "thinking", "thinking": "first thought"}},
		"codex_reasoning_items": []any{map[string]any{"type": "reasoning", "id": "rs_123", "encrypted_content": "enc_blob"}},
	})
	m.SaveSession(ctx, st.SessionID)
	dropFromMemory(m, st.SessionID)
	restored := m.GetSession(ctx, st.SessionID)
	if restored == nil {
		t.Fatal("session not restored")
	}
	msg := restored.History[0]
	if _, ok := pyFloat(msg["timestamp"]); !ok {
		t.Fatalf("timestamp = %#v", msg["timestamp"])
	}
	delete(msg, "timestamp")
	if msg["_db_persisted"] != true {
		t.Fatalf("_db_persisted = %#v", msg["_db_persisted"])
	}
	delete(msg, "_db_persisted")
	uid, _ := msg["message_uid"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(uid) {
		t.Fatalf("message_uid = %#v", msg["message_uid"])
	}
	delete(msg, "message_uid")
	if _, ok := msg["_db_row_snapshot"].(string); !ok {
		t.Fatalf("_db_row_snapshot = %#v", msg["_db_row_snapshot"])
	}
	delete(msg, "_db_row_snapshot")
	if len(restored.History) != 1 || len(msg) != 5 || msg["role"] != "assistant" || msg["content"] != "hello" || msg["reasoning"] != "step-by-step" {
		t.Fatalf("history = %v", restored.History)
	}
	for key, want := range map[string]string{
		"reasoning_details":     `[{"type": "thinking", "thinking": "first thought"}]`,
		"codex_reasoning_items": `[{"type": "reasoning", "id": "rs_123", "encrypted_content": "enc_blob"}]`,
	} {
		text, err := state.DumpsUnicode(msg[key])
		if err != nil {
			t.Fatal(err)
		}
		got, _ := pyjson.Loads(text)
		wanted, _ := pyjson.Loads(want)
		gotText, _ := pyjson.Dumps(got, pyjson.SortKeys(true))
		wantText, _ := pyjson.Dumps(wanted, pyjson.SortKeys(true))
		if gotText != wantText {
			t.Fatalf("%s = %s, want %s", key, gotText, wantText)
		}
	}
}

func TestAcpAgentsRouteHumanOutputToStderr(t *testing.T) {
	db := provenanceDB(t)
	var built *nsAgent
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"provider": "openrouter", "default": "test-model"}},
		func(requested, target string) (*provider.Runtime, error) {
			return &provider.Runtime{Provider: "openrouter", APIMode: "chat_completions", BaseURL: "https://openrouter.example/v1", APIKey: "test-key", Args: []string{}}, nil
		},
		func(o agent.AIAgentOptions) (any, error) {
			built = &nsAgent{model: o.Model}
			return built, nil
		})
	swap(t, &registerTaskCwdHook, registerTaskCwd)
	st := mustCreate(t, NewSessionManager(nil, db), "/work")
	if st.Agent != any(built) || built.printFn == nil {
		t.Fatal("print function not installed")
	}
	capture := func(target **os.File) (restore func() string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := *target
		*target = w
		return func() string {
			*target = previous
			w.Close()
			var buf bytes.Buffer
			io.Copy(&buf, r)
			return buf.String()
		}
	}
	stdout, stderr := capture(&os.Stdout), capture(&os.Stderr)
	built.printFn("ACP noise")
	errText, outText := stderr(), stdout()
	if outText != "" || errText != "ACP noise\n" {
		t.Fatalf("stdout %q stderr %q", outText, errText)
	}
}
