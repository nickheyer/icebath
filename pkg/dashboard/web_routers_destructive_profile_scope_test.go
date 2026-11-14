package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Launch home and one named profile home seeded with memory, webhook, config and update files, multiplex off for the case
type scopeHomes struct {
	launch, beta string
}

func (h scopeHomes) all() []string { return []string{h.launch, h.beta} }

func seedScopeHome(t *testing.T, h string) {
	t.Helper()
	name := filepath.Base(h)
	writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), "memory of "+name+"\n")
	writeTestFile(t, filepath.Join(h, "memories", "USER.md"), "user of "+name+"\n")
	writeTestFile(t, filepath.Join(h, "webhook_subscriptions.json"), `{"alerts": {"secret": "s", "events": []}}`)
	writeTestFile(t, filepath.Join(h, "config.yaml"), "hooks:\n  pre_tool_call:\n  - command: /bin/true\nproxy:\n  label: "+name+"\n")
	writeTestFile(t, filepath.Join(h, ".update_check"), "cached\n")
}

// Session client over a launch home with the named profile worker_beta beside it, or outside the profiles root when detached
func scopeClient(t *testing.T, detached bool) (sessionClient, scopeHomes) {
	t.Helper()
	wasActive := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	t.Cleanup(func() { provider.SetMultiplexActive(wasActive) })
	c := newSessionClient(t)
	launch := os.Getenv("ICEBATH_HOME")
	beta := filepath.Join(launch, "profiles", "worker_beta")
	if detached {
		beta = filepath.Join(t.TempDir(), "worker_beta")
	}
	for _, h := range []string{launch, beta} {
		seedScopeHome(t, h)
	}
	writeTestFile(t, filepath.Join(beta, ".env"), "BETA=1\n")
	return c, scopeHomes{launch: launch, beta: beta}
}

// Arms the unnamed profile guard through the boot probe that sees two servable homes
func armMultiplex(t *testing.T) {
	t.Helper()
	activated, err := provider.ActivateMultiProfileHostingEagerly(context.Background())
	if err != nil || !activated {
		t.Fatalf("boot probe did not see two homes: %v %v", activated, err)
	}
	if !provider.IsMultiplexActive(context.Background()) {
		t.Fatal("multiplex not active after the probe")
	}
}

func scopeHooks(t *testing.T, h string) map[string]any {
	t.Helper()
	cfg, err := config.ReadRawConfig(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	return hooks
}

func scopeWebhooks(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "webhook_subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var subs map[string]any
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatal(err)
	}
	return subs
}

func scopeZip(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func profileQueryOf(profile string) string {
	if profile == "" {
		return ""
	}
	return "?profile=" + profile
}

func withBodyProfile(body map[string]any, profile string) map[string]any {
	if profile != "" {
		body["profile"] = profile
	}
	return body
}

// Every destructive or privileged route as a call taking the profile name or none
var destructiveRoutes = map[string]func(t *testing.T, c sessionClient, profile string) *testResponse{
	"memory-reset": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/memory/reset"+profileQueryOf(p), map[string]any{"target": "all"})
	},
	"webhook-delete": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("DELETE", "/api/webhooks/alerts"+profileQueryOf(p), nil)
	},
	"hook-delete": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("DELETE", "/api/ops/hooks"+profileQueryOf(p), map[string]any{"event": "pre_tool_call", "command": "/bin/true"})
	},
	"hook-create": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/ops/hooks"+profileQueryOf(p), map[string]any{"event": "pre_tool_call", "command": "/bin/armed"})
	},
	"checkpoints-prune": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/ops/checkpoints/prune"+profileQueryOf(p), nil)
	},
	"import": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/ops/import"+profileQueryOf(p), map[string]any{"archive": scopeZip(t)})
	},
	"import-upload": func(t *testing.T, c sessionClient, p string) *testResponse {
		archive, err := os.ReadFile(scopeZip(t))
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", "backup.zip")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(archive); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		return c.request("POST", "/api/ops/import-upload"+profileQueryOf(p), body.Bytes(), "content-type", form.FormDataContentType(),
			sessionHeaderName, SessionToken())
	},
	"credentials-pool-delete": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("DELETE", "/api/credentials/pool/anthropic/1"+profileQueryOf(p), nil)
	},
	"curator-run": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/curator/run"+profileQueryOf(p), nil)
	},
	"sessions-prune": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/sessions/prune", withBodyProfile(map[string]any{"dry_run": false}, p))
	},
	"sessions-empty": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("DELETE", "/api/sessions/empty"+profileQueryOf(p), nil)
	},
	"sessions-bulk-delete": func(t *testing.T, c sessionClient, p string) *testResponse {
		return c.send("POST", "/api/sessions/bulk-delete", withBodyProfile(map[string]any{"ids": []string{"abc"}}, p))
	},
}

func sortedRouteNames(routes map[string]func(t *testing.T, c sessionClient, profile string) *testResponse) []string {
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Records the argv of every backgrounded action a case spawns instead of running it
func recordActionSpawns(t *testing.T) *[][]string {
	t.Helper()
	isolateActionRegistries(t, t.TempDir())
	var spawned [][]string
	saved := actionPopen
	actionPopen = func(argv []string, _ string, _ map[string]string, _ *os.File, _ *syscall.SysProcAttr) (actionProc, error) {
		spawned = append(spawned, slices.Clone(argv))
		return &fakeActionProc{pid: 4242}, nil
	}
	t.Cleanup(func() { actionPopen = saved })
	return &spawned
}

// Adds one manual pool entry for a provider to the auth store of a home
func seedPoolEntry(t *testing.T, h, providerID string) {
	t.Helper()
	pool := provider.LoadPool(provider.HomePoolEnv(h), providerID)
	if _, err := pool.AddEntry(provider.PooledCredential{Provider: providerID, ID: "seed01", Label: "seeded", AuthType: provider.AuthTypeAPIKey,
		Source: provider.SourceManual, AccessToken: "sk-test-" + strings.Repeat("a", 16)}); err != nil {
		t.Fatal(err)
	}
}

// Pool rows the auth file of a home itself holds for a provider, without the root fallback
func poolEntryCount(h, providerID string) int {
	raw, err := os.ReadFile(filepath.Join(h, "auth.json"))
	if err != nil {
		return 0
	}
	var doc struct {
		Pool map[string][]any `json:"credential_pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return -1
	}
	return len(doc.Pool[providerID])
}

func TestDestructiveRouteRefusesAnUnnamedProfileWhileMultiplexing(t *testing.T) {
	for _, route := range sortedRouteNames(destructiveRoutes) {
		t.Run(route, func(t *testing.T) {
			c, homes := scopeClient(t, false)
			spawned := recordActionSpawns(t)
			for _, h := range homes.all() {
				seedPoolEntry(t, h, "anthropic")
			}
			armMultiplex(t)
			resp := destructiveRoutes[route](t, c, "")
			if resp.Status != 400 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			if len(*spawned) != 0 {
				t.Fatalf("%s spawned %v anyway", route, *spawned)
			}
			for _, h := range homes.all() {
				if _, err := os.Stat(filepath.Join(h, "memories", "MEMORY.md")); err != nil {
					t.Fatalf("%s: %v", h, err)
				}
				if _, ok := scopeWebhooks(t, h)["alerts"]; !ok {
					t.Fatalf("%s lost its webhook", h)
				}
				hooks, _ := json.Marshal(scopeHooks(t, h))
				if string(hooks) != `{"pre_tool_call":[{"command":"/bin/true"}]}` {
					t.Fatalf("%s hooks %s", h, hooks)
				}
				if _, err := os.Stat(filepath.Join(h, "state.db")); !os.IsNotExist(err) {
					t.Fatalf("%s: a session store was opened: %v", h, err)
				}
				if poolEntryCount(h, "anthropic") != 1 {
					t.Fatalf("%s: the pool entry was removed", h)
				}
			}
		})
	}
}

// Routes whose effect is a file inside the target home, with the check that it happened there
var fileEffects = map[string]func(t *testing.T, h string) bool{
	"memory-reset": func(t *testing.T, h string) bool {
		_, err := os.Stat(filepath.Join(h, "memories", "MEMORY.md"))
		return os.IsNotExist(err)
	},
	"webhook-delete": func(t *testing.T, h string) bool {
		_, ok := scopeWebhooks(t, h)["alerts"]
		return !ok
	},
	"hook-delete": func(t *testing.T, h string) bool { return len(scopeHooks(t, h)) == 0 },
	"hook-create": func(t *testing.T, h string) bool {
		entries, _ := scopeHooks(t, h)["pre_tool_call"].([]any)
		for _, e := range entries {
			if m, ok := e.(map[string]any); ok && m["command"] == "/bin/armed" {
				return true
			}
		}
		return false
	},
}

func TestNamedProfileIsTheOnlyOneTouched(t *testing.T) {
	routes := make([]string, 0, len(fileEffects))
	for route := range fileEffects {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			c, homes := scopeClient(t, false)
			resp := destructiveRoutes[route](t, c, "worker_beta")
			if resp.Status != 200 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			if !fileEffects[route](t, homes.beta) {
				t.Fatalf("%s did not act on worker_beta", route)
			}
			if fileEffects[route](t, homes.launch) {
				t.Fatalf("%s also hit the launch profile", route)
			}
		})
	}
}

func TestSpawnedActionCarriesTheNamedProfile(t *testing.T) {
	first := map[string]string{"checkpoints-prune": "checkpoints", "curator-run": "curator", "import": "import"}
	for _, route := range []string{"checkpoints-prune", "curator-run", "import"} {
		t.Run(route, func(t *testing.T) {
			c, _ := scopeClient(t, false)
			spawned := recordActionSpawns(t)
			resp := destructiveRoutes[route](t, c, "worker_beta")
			if resp.Status != 200 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			if len(*spawned) != 1 {
				t.Fatalf("spawned %v", *spawned)
			}
			argv := (*spawned)[0]
			at := slices.Index(argv, "-p")
			if at < 0 || at+2 >= len(argv) || argv[at+1] != "worker_beta" || argv[at+2] != first[route] {
				t.Fatalf("argv %v does not carry -p worker_beta before the subcommand", argv)
			}
		})
	}
}

func TestSessionRouteOpensTheNamedProfilesStore(t *testing.T) {
	for _, route := range []string{"sessions-prune", "sessions-empty", "sessions-bulk-delete"} {
		t.Run(route, func(t *testing.T) {
			c, homes := scopeClient(t, false)
			resp := destructiveRoutes[route](t, c, "worker_beta")
			if resp.Status != 200 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			if _, err := os.Stat(filepath.Join(homes.beta, "state.db")); err != nil {
				t.Fatalf("worker_beta store was not opened: %v", err)
			}
			if _, err := os.Stat(filepath.Join(homes.launch, "state.db")); !os.IsNotExist(err) {
				t.Fatalf("the launch store was opened: %v", err)
			}
		})
	}
}

func TestCredentialPoolDeleteRunsInTheNamedProfilesHome(t *testing.T) {
	c, homes := scopeClient(t, false)
	for _, h := range homes.all() {
		seedPoolEntry(t, h, "anthropic")
	}
	resp := c.send("DELETE", "/api/credentials/pool/anthropic/1?profile=worker_beta", nil)
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if poolEntryCount(homes.beta, "anthropic") != 0 {
		t.Fatal("the worker_beta entry survived")
	}
	if poolEntryCount(homes.launch, "anthropic") != 1 {
		t.Fatal("the launch entry was removed")
	}
}

func TestARequestForAnotherProfileArmsTheGuard(t *testing.T) {
	c, homes := scopeClient(t, false)
	ctx := context.Background()
	if provider.IsMultiplexActive(ctx) {
		t.Fatal("multiplex active before any cross profile request")
	}
	if r := c.get("/api/memory?profile=worker_beta"); r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if !provider.IsMultiplexActive(ctx) {
		t.Fatal("the cross profile request did not arm the guard")
	}
	if r := c.send("POST", "/api/memory/reset", map[string]any{"target": "all"}); r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if _, err := os.Stat(filepath.Join(homes.launch, "memories", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
}

func TestUnnamedProfileStillMeansTheLaunchProfileOnASingleProfileHost(t *testing.T) {
	c, homes := scopeClient(t, true)
	ctx := context.Background()
	if activated, err := provider.ActivateMultiProfileHostingEagerly(ctx); err != nil || activated {
		t.Fatalf("the boot probe activated on a single profile host: %v %v", activated, err)
	}
	if provider.IsMultiplexActive(ctx) {
		t.Fatal("multiplex active on a single profile host")
	}
	resp := c.send("POST", "/api/memory/reset", map[string]any{"target": "all"})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if _, err := os.Stat(filepath.Join(homes.launch, "memories", "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("launch memory survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homes.beta, "memories", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
}

func TestCuratorPauseWritesTheNamedProfilesState(t *testing.T) {
	c, homes := scopeClient(t, false)
	resp := c.send("PUT", "/api/curator/paused?profile=worker_beta", map[string]any{"paused": true})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	raw, err := os.ReadFile(filepath.Join(homes.beta, "skills", ".curator_state"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil || state["paused"] != true {
		t.Fatalf("state %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(homes.launch, "skills", ".curator_state")); !os.IsNotExist(err) {
		t.Fatalf("the launch profile was paused too: %v", err)
	}
}
