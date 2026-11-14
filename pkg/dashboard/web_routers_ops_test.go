package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Sources of the pool entries a provider loads in a home
func poolSources(h, providerID string) []string {
	out := []string{}
	for _, e := range provider.LoadPool(provider.HomePoolEnv(h), providerID).Entries() {
		out = append(out, e.Source)
	}
	return out
}

func saveTestEnvValue(t *testing.T, h, key, value string) {
	t.Helper()
	if err := config.SaveEnvValue(h, key, value); err != nil {
		t.Fatal(err)
	}
}

func TestEnvSeededDeleteStaysDeleted(t *testing.T) {
	c := newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	t.Setenv("OPENROUTER_API_KEY", "")
	fakeKey := "sk-or-" + strings.Repeat("x", 20)
	saveTestEnvValue(t, h, "OPENROUTER_API_KEY", fakeKey)
	if got := poolSources(h, "openrouter"); !slices.Equal(got, []string{"env:OPENROUTER_API_KEY"}) {
		t.Fatalf("seeded sources %v", got)
	}
	if r := c.send("DELETE", "/api/credentials/pool/openrouter/1", nil); r.Status != 200 {
		t.Fatalf("delete %d %s", r.Status, r.Body)
	}
	if !provider.NewStore(h).IsSourceSuppressed("openrouter", "env:OPENROUTER_API_KEY") {
		t.Fatal("source not suppressed")
	}
	saveTestEnvValue(t, h, "OPENROUTER_API_KEY", fakeKey)
	if got := poolSources(h, "openrouter"); len(got) != 0 {
		t.Fatalf("entry came back %v", got)
	}
	if providers := jsonObject(t, c.get("/api/credentials/pool"))["providers"].([]any); len(providers) != 0 {
		t.Fatalf("providers %v", providers)
	}
}

func TestPostReaddLiftsSuppression(t *testing.T) {
	c := newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	t.Setenv("OPENROUTER_API_KEY", "")
	fakeKey := "sk-or-" + strings.Repeat("y", 20)
	saveTestEnvValue(t, h, "OPENROUTER_API_KEY", fakeKey)
	poolSources(h, "openrouter")
	if r := c.send("DELETE", "/api/credentials/pool/openrouter/1", nil); r.Status != 200 {
		t.Fatalf("delete %d %s", r.Status, r.Body)
	}
	store := provider.NewStore(h)
	if !store.IsSourceSuppressed("openrouter", "env:OPENROUTER_API_KEY") {
		t.Fatal("source not suppressed")
	}
	r := c.send("POST", "/api/credentials/pool", map[string]any{"provider": "openrouter", "api_key": "sk-or-" + strings.Repeat("z", 20)})
	if r.Status != 200 {
		t.Fatalf("add %d %s", r.Status, r.Body)
	}
	if store.IsSourceSuppressed("openrouter", "env:OPENROUTER_API_KEY") {
		t.Fatal("suppression not lifted")
	}
	saveTestEnvValue(t, h, "OPENROUTER_API_KEY", fakeKey)
	sources := poolSources(h, "openrouter")
	slices.Sort(sources)
	if !slices.Equal(sources, []string{"env:OPENROUTER_API_KEY", "manual"}) {
		t.Fatalf("sources %v", sources)
	}
}

// Session client over a home that holds a memories folder
func memoryEndpointsClient(t *testing.T) (sessionClient, string) {
	c := newSessionClient(t)
	mem := filepath.Join(os.Getenv("ICEBATH_HOME"), "memories")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	return c, mem
}

func TestStatusAndSelect(t *testing.T) {
	c, _ := memoryEndpointsClient(t)
	data := jsonObject(t, c.get("/api/memory"))
	for _, key := range []string{"active", "providers", "builtin_files"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("%s absent from %v", key, data)
		}
	}
	r := c.send("PUT", "/api/memory/provider", map[string]any{"provider": "built-in"})
	if r.Status != 200 || jsonObject(t, r)["active"] != "" {
		t.Fatalf("built-in %d %s", r.Status, r.Body)
	}
	if r := c.send("PUT", "/api/memory/provider", map[string]any{"provider": "no-such-provider-xyz"}); r.Status != 400 {
		t.Fatalf("unknown provider %d %s", r.Status, r.Body)
	}
}

func TestResetTargets(t *testing.T) {
	c, mem := memoryEndpointsClient(t)
	writeTestFile(t, filepath.Join(mem, "MEMORY.md"), "notes")
	writeTestFile(t, filepath.Join(mem, "USER.md"), "user")
	r := c.send("POST", "/api/memory/reset", map[string]any{"target": "user"})
	if r.Status != 200 || !slices.Contains(jsonObject(t, r)["deleted"].([]any), any("USER.md")) {
		t.Fatalf("reset %d %s", r.Status, r.Body)
	}
	if _, err := os.Stat(filepath.Join(mem, "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if r := c.send("POST", "/api/memory/reset", map[string]any{"target": "bogus"}); r.Status != 400 {
		t.Fatalf("bogus target %d %s", r.Status, r.Body)
	}
}

// Pairing store of the bound home or of a named profile
func testPairingStore(t *testing.T, profile string) *gateway.PairingStore {
	t.Helper()
	store, err := gateway.NewPairingStore(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func generatePairingCode(t *testing.T, store *gateway.PairingStore, userID, userName string) string {
	t.Helper()
	code, ok, err := store.GenerateCode(context.Background(), "telegram", userID, userName)
	if err != nil || !ok {
		t.Fatalf("generate code %v %v", ok, err)
	}
	return code
}

// Values of one key over the rows of a JSON list
func rowValues(rows any, key string) []any {
	out := []any{}
	for _, row := range rows.([]any) {
		out = append(out, row.(map[string]any)[key])
	}
	return out
}

func TestApprovePendingRequestID(t *testing.T) {
	c := newSessionClient(t)
	botCode := generatePairingCode(t, testPairingStore(t, ""), "user1", "Alice")
	pending := jsonObject(t, c.get("/api/pairing"))["pending"].([]any)
	requestID, _ := pending[0].(map[string]any)["request_id"].(string)
	if requestID == "" || requestID == botCode {
		t.Fatalf("request id %q code %q", requestID, botCode)
	}
	r := c.send("POST", "/api/pairing/approve", map[string]any{"platform": "telegram", "request_id": requestID})
	if r.Status != 200 {
		t.Fatalf("approve %d %s", r.Status, r.Body)
	}
	if user := jsonObject(t, r)["user"].(map[string]any); user["user_id"] != "user1" {
		t.Fatalf("user %v", user)
	}
	if left := jsonObject(t, c.get("/api/pairing"))["pending"].([]any); len(left) != 0 {
		t.Fatalf("pending left %v", left)
	}
}

func TestPairingIsIsolatedPerProfile(t *testing.T) {
	c := newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	writeTestFile(t, filepath.Join(h, "profiles", "work", "config.yaml"), "{}\n")
	generatePairingCode(t, testPairingStore(t, ""), "global-1", "GlobalGuy")
	generatePairingCode(t, testPairingStore(t, "work"), "work-1", "WorkGal")
	listed := jsonObject(t, c.get("/api/pairing?profile=work"))["pending"].([]any)
	if ids := rowValues(listed, "user_id"); !slices.Equal(ids, []any{"work-1"}) {
		t.Fatalf("work pending %v", ids)
	}
	r := c.send("POST", "/api/pairing/approve", map[string]any{"platform": "telegram",
		"request_id": listed[0].(map[string]any)["request_id"], "profile": "work"})
	if r.Status != 200 {
		t.Fatalf("approve %d %s", r.Status, r.Body)
	}
	ctx := context.Background()
	if ok, err := testPairingStore(t, "work").IsApproved(ctx, "telegram", "work-1"); err != nil || !ok {
		t.Fatalf("work store approved %v %v", ok, err)
	}
	globalView := jsonObject(t, c.get("/api/pairing"))
	if ok, err := testPairingStore(t, "").IsApproved(ctx, "telegram", "work-1"); err != nil || ok {
		t.Fatalf("global store approved %v %v", ok, err)
	}
	if slices.Contains(rowValues(globalView["approved"], "user_id"), any("work-1")) {
		t.Fatalf("grant leaked into the global store %v", globalView["approved"])
	}
	if !slices.Contains(rowValues(globalView["pending"], "user_id"), any("global-1")) {
		t.Fatalf("global pending row gone %v", globalView["pending"])
	}
}

func TestUnknownProfileIsRejected(t *testing.T) {
	c := newSessionClient(t)
	if r := c.get("/api/pairing?profile=ghost"); r.Status != 404 {
		t.Fatalf("ghost profile %d %s", r.Status, r.Body)
	}
}

// Enables the webhook platform in the config of the bound home
func enableWebhookPlatformConfig(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	platforms, ok := cfg["platforms"].(map[string]any)
	if !ok {
		platforms = map[string]any{}
		cfg["platforms"] = platforms
	}
	platforms["webhook"] = map[string]any{"enabled": true, "extra": map[string]any{"host": "0.0.0.0", "port": int64(8644)}}
	if err := webSaveConfig(ctx, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWebhookPersistsScript(t *testing.T) {
	c := newSessionClient(t)
	enableWebhookPlatformConfig(t)
	r := c.send("POST", "/api/webhooks", map[string]any{"name": "todoist", "deliver": "log", "script": "todoist_filter.py"})
	if r.Status != 200 || jsonObject(t, r)["script"] != "todoist_filter.py" {
		t.Fatalf("create %d %s", r.Status, r.Body)
	}
	subs := jsonObject(t, c.get("/api/webhooks"))["subscriptions"].([]any)
	if subs[0].(map[string]any)["script"] != "todoist_filter.py" {
		t.Fatalf("listed %v", subs)
	}
	if r := c.send("PUT", "/api/webhooks/todoist/enabled", map[string]any{"enabled": false}); r.Status != 200 {
		t.Fatalf("disable %d %s", r.Status, r.Body)
	}
	if r := c.send("POST", "/api/webhooks", map[string]any{"name": "todoist", "deliver": "log"}); r.Status != 200 {
		t.Fatalf("second create %d %s", r.Status, r.Body)
	}
	route, _ := webhooksubs.Load(os.Getenv("ICEBATH_HOME")).GetOr("todoist", nil).(interface{ GetOr(string, any) any })
	if route == nil || route.GetOr("enabled", nil) != false {
		t.Fatalf("explicit disable did not survive the update: %v", route)
	}
}

// Whether the bound home config has the webhook platform enabled
func webhookPlatformEnabled(t *testing.T) bool {
	t.Helper()
	cfg, err := webLoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	platforms, _ := cfg["platforms"].(map[string]any)
	webhook, _ := platforms["webhook"].(map[string]any)
	return webhook["enabled"] == true
}

func TestEnablePlatformStartsGatewayRestart(t *testing.T) {
	c := newSessionClient(t)
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	type call struct {
		argv []string
		name string
	}
	var calls []call
	restartSpawnAction = func(_ context.Context, subcommand []string, name string, _ map[string]string) (actionProc, error) {
		calls = append(calls, call{slices.Clone(subcommand), name})
		return &fakeActionProc{pid: 4242}, nil
	}
	c.send("POST", "/api/webhooks/enable", nil).expect(t, 200, `{"ok":true,"platform":"webhook","enabled":true,"needs_restart":false,`+
		`"restart_started":true,"restart_action":"gateway-restart","restart_pid":4242}`)
	if len(calls) != 1 || !slices.Equal(calls[0].argv, []string{"-p", "default", "gateway", "restart"}) || calls[0].name != "gateway-restart" {
		t.Fatalf("restart calls %v", calls)
	}
	if !webhookPlatformEnabled(t) {
		t.Fatal("webhook platform not enabled in config")
	}
	if jsonObject(t, c.get("/api/webhooks"))["enabled"] != true {
		t.Fatal("webhooks list does not report enabled")
	}
}

func TestEnablePlatformReusesInflightGatewayRestart(t *testing.T) {
	c := newSessionClient(t)
	resetRestartState(t, map[string]actionProc{"gateway-restart": &fakeActionProc{pid: 5151}}, map[string][]string{})
	restartSpawnAction = func(context.Context, []string, string, map[string]string) (actionProc, error) {
		t.Error("must not spawn a second concurrent restart")
		return nil, fmt.Errorf("second restart spawned")
	}
	r := c.send("POST", "/api/webhooks/enable", nil)
	if r.Status != 200 {
		t.Fatalf("enable %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["needs_restart"] != false || data["restart_started"] != true || fmt.Sprint(data["restart_pid"]) != "5151" {
		t.Fatalf("answer %v", data)
	}
	if !webhookPlatformEnabled(t) {
		t.Fatal("webhook platform not enabled in config")
	}
}

func TestHooksListReadsConfig(t *testing.T) {
	c := newSessionClient(t)
	ctx := context.Background()
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["hooks"] = map[string]any{"pre_tool_call": []any{
		map[string]any{"matcher": "terminal", "command": "/bin/echo hi", "timeout": int64(5)}}}
	if err := webSaveConfig(ctx, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	data := jsonObject(t, c.get("/api/ops/hooks"))
	if hooks := data["hooks"].([]any); hooks[0].(map[string]any)["command"] != "/bin/echo hi" {
		t.Fatalf("hooks %v", hooks)
	}
	if events, _ := data["valid_events"].([]any); len(events) < 1 {
		t.Fatalf("valid events %v", data["valid_events"])
	}
}

// Hooks the list endpoint reports for one command
func hooksWithCommand(t *testing.T, c sessionClient, command string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, h := range jsonObject(t, c.get("/api/ops/hooks"))["hooks"].([]any) {
		if row := h.(map[string]any); row["command"] == command {
			out = append(out, row)
		}
	}
	return out
}

func TestHookCreateAndDelete(t *testing.T) {
	c := newSessionClient(t)
	r := c.send("POST", "/api/ops/hooks", map[string]any{"event": "pre_tool_call", "command": "/bin/echo created",
		"matcher": "terminal", "timeout": 7, "approve": true})
	if r.Status != 200 || jsonObject(t, r)["approved"] != true {
		t.Fatalf("create %d %s", r.Status, r.Body)
	}
	created := hooksWithCommand(t, c, "/bin/echo created")
	if len(created) == 0 || created[0]["allowed"] != true {
		t.Fatalf("created %v", created)
	}
	if r := c.send("POST", "/api/ops/hooks", map[string]any{"event": "no_such_event", "command": "/x"}); r.Status != 400 {
		t.Fatalf("unknown event %d %s", r.Status, r.Body)
	}
	if r := c.send("DELETE", "/api/ops/hooks", map[string]any{"event": "pre_tool_call", "command": "/bin/echo created"}); r.Status != 200 {
		t.Fatalf("delete %d %s", r.Status, r.Body)
	}
	if left := hooksWithCommand(t, c, "/bin/echo created"); len(left) != 0 {
		t.Fatalf("hook left %v", left)
	}
}

func TestListCredentialPoolRunsOffEventLoop(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	result, err := listCredentialPool(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if providers, ok := result.GetOr("providers", nil).([]any); !ok || len(providers) != 0 || result.Len() != 1 {
		t.Fatalf("result %v", result)
	}
}
