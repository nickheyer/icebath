package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	routeFieldsCodex = map[string]any{"base_url": "https://chatgpt.com/backend-api/codex", "api_key": "codex-key", "model": "gpt-6-astra"}
	routeFieldsLocal = map[string]any{"base_url": "http://127.0.0.1:8080/v1", "api_key": "k", "model": "qwen3-local"}
)

func withRoute(base map[string]any, extra ...any) map[string]any {
	out := maps.Clone(base)
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i].(string)] = extra[i+1]
	}
	return out
}

// Backend answering task index with routes[index], the config itself for a nil route
type routeFieldsBackend struct {
	tools.DelegationProviderDefaults
	routes []map[string]any
}

func newRouteFieldsBackend(routes []map[string]any) *routeFieldsBackend {
	b := &routeFieldsBackend{routes: routes}
	b.Self = b
	return b
}

func (b *routeFieldsBackend) Name() string                   { return "route-fields" }
func (b *routeFieldsBackend) DisplayName() string            { return "route-fields" }
func (b *routeFieldsBackend) GetSetupSchema() map[string]any { return map[string]any{} }
func (b *routeFieldsBackend) RouteTask(task, config map[string]any) (map[string]any, error) {
	index, _ := task["index"].(int)
	route := b.routes[index]
	if route == nil {
		return maps.Clone(config), nil
	}
	return maps.Clone(route), nil
}

type routeFieldsSpawn struct {
	result   map[string]any
	built    []AIAgentOptions
	children []*AIAgent
}

type routeFieldsOptions struct {
	cfg               map[string]any
	parent            *AIAgent
	construct         func(o AIAgentOptions) *AIAgent
	useDefaultBackend bool
}

func routeFieldsParent(requestOverrides map[string]any) *AIAgent {
	if requestOverrides == nil {
		requestOverrides = map[string]any{}
	}
	return &AIAgent{baseURL: "https://openrouter.ai/api/v1", apiKey: "parent-key", provider: "openrouter", apiMode: "chat_completions",
		model: "anthropic/claude-sonnet-4", platform: "cli", requestOverrides: requestOverrides,
		reasoningConfig: map[string]any{"enabled": true, "effort": "medium"}, activeChildrenLock: &sync.Mutex{}}
}

func routeFieldsConstruct(o AIAgentOptions) *AIAgent {
	return &AIAgent{requestOverrides: maps.Clone(o.RequestOverrides), provider: o.Provider, model: o.Model,
		reasoningConfig: o.ReasoningConfig, serviceTier: o.ServiceTier}
}

// Writes the delegation section of a temp home config the way the hermes test patches _load_config
func writeDelegationConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("delegation:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, fmt.Sprint(cfg[k]))
	}
	if len(keys) == 0 {
		b.WriteString("  {}\n")
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Tool result, the AIAgent construction options of every child and the built children for one task per route
func spawnRouteFields(t *testing.T, routes []map[string]any, opts routeFieldsOptions) routeFieldsSpawn {
	t.Helper()
	cfg := maps.Clone(opts.cfg)
	if cfg == nil {
		cfg = map[string]any{}
	}
	if !opts.useDefaultBackend {
		cfg["backend"] = "route-fields"
		backend := newRouteFieldsBackend(routes)
		prev := tools.DelegationRegistryHooks.GetProvider
		tools.DelegationRegistryHooks.GetProvider = func(ctx context.Context, name string) tools.DelegationProvider {
			if name == "route-fields" {
				return backend
			}
			return prev(ctx, name)
		}
		t.Cleanup(func() { tools.DelegationRegistryHooks.GetProvider = prev })
	}
	writeDelegationConfig(t, cfg)
	construct := opts.construct
	if construct == nil {
		construct = routeFieldsConstruct
	}
	var built []AIAgentOptions
	var batches []*delegateBatch
	prevNew, prevRun := newChildAIAgentFn, runDelegateBatchFn
	newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		built = append(built, o)
		return construct(o), nil
	}
	runDelegateBatchFn = func(_ context.Context, batch *delegateBatch, _ bool) (string, error) {
		batches = append(batches, batch)
		payload := pyjson.ObjectOf("status", "built")
		if len(batch.Refused) > 0 {
			payload.Set("refused", batch.Refused)
		}
		return pyjson.Dumps(payload, pyjson.EnsureASCII(false))
	}
	t.Cleanup(func() { newChildAIAgentFn, runDelegateBatchFn = prevNew, prevRun })
	parent := opts.parent
	if parent == nil {
		parent = routeFieldsParent(nil)
	}
	tasks := make([]any, len(routes))
	for i := range routes {
		tasks[i] = map[string]any{"goal": fmt.Sprintf("task number %d", i)}
	}
	raw, err := delegateTask(context.Background(), parent, map[string]any{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("result %q: %v", raw, err)
	}
	out := routeFieldsSpawn{result: result, built: built}
	if len(batches) > 0 {
		for _, c := range batches[0].Children {
			out.children = append(out.children, c.Child)
		}
	}
	return out
}

func refusedEntries(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	raw, _ := result["refused"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out
}

func routeFieldsReport(entry *pyjson.Object) map[string]any {
	route, _ := entryValue(entry, "route").(*pyjson.Object)
	if route == nil {
		return nil
	}
	return objectMap(route)
}

func swapUltrafastOffered(t *testing.T, fn func(model, providerName, baseURL, accessToken string) (bool, error)) {
	t.Helper()
	prev := tools.DelegateServiceTierHooks.UltrafastOffered
	tools.DelegateServiceTierHooks.UltrafastOffered = fn
	t.Cleanup(func() { tools.DelegateServiceTierHooks.UltrafastOffered = prev })
}

func TestRouteTierReachesTheChildAndItsSiblingKeepsTheDefault(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsCodex, "service_tier", "fast"), nil}, routeFieldsOptions{})
	if !reflect.DeepEqual(s.result, map[string]any{"status": "built"}) || len(s.built) != 2 {
		t.Fatalf("result %v built %d", s.result, len(s.built))
	}
	fast, plain := s.built[0], s.built[1]
	if fast.RequestOverrides["service_tier"] != "priority" || fast.ServiceTier != "priority" {
		t.Fatalf("fast %v tier %q", fast.RequestOverrides, fast.ServiceTier)
	}
	if _, present := plain.RequestOverrides["service_tier"]; present || plain.ServiceTier != "" {
		t.Fatalf("plain %v tier %q", plain.RequestOverrides, plain.ServiceTier)
	}
}

func TestATierTheRouteDoesNotOfferRefusesThatTaskAndItsSiblingRuns(t *testing.T) {
	cases := []struct {
		route  map[string]any
		needle string
	}{
		{withRoute(routeFieldsLocal, "service_tier", "fast"), "not available"},
		{withRoute(routeFieldsCodex, "service_tier", "turbo"), "turbo"},
		{withRoute(routeFieldsCodex, "service_tier", "auto"), "auto"},
	}
	for _, c := range cases {
		t.Run(c.needle, func(t *testing.T) {
			s := spawnRouteFields(t, []map[string]any{c.route, nil}, routeFieldsOptions{})
			refused := refusedEntries(t, s.result)
			if len(refused) != 1 || refused[0]["task_index"] != float64(0) || !strings.Contains(fmt.Sprint(refused[0]["error"]), c.needle) || len(s.built) != 1 {
				t.Fatalf("refused %v built %d", refused, len(s.built))
			}
			if len(s.children) != 1 {
				t.Fatalf("children %d", len(s.children))
			}
			raw, err := os.ReadFile(liveManifestPath(context.Background(), s.children[0].delegationID, ""))
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			var statuses []string
			for _, task := range manifest["tasks"].([]any) {
				statuses = append(statuses, fmt.Sprint(task.(map[string]any)["status"]))
			}
			if !reflect.DeepEqual(statuses, []string{"error", "running"}) || strings.Contains(string(raw), "completed") {
				t.Fatalf("manifest %s", raw)
			}
		})
	}
}

func TestUltrafastFollowsTheCodexAccountLikeTheFastCommand(t *testing.T) {
	cases := []struct {
		name     string
		offers   bool
		expected string
	}{{"false", false, ""}, {"true", true, "ultrafast"}, {"none", true, "ultrafast"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swapUltrafastOffered(t, func(string, string, string, string) (bool, error) { return c.offers, nil })
			s := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsCodex, "model", "gpt-6.1-sol", "service_tier", "ultrafast")}, routeFieldsOptions{})
			if c.expected == "" {
				if !strings.Contains(fmt.Sprint(s.result["error"]), "not available") || len(s.built) != 0 {
					t.Fatalf("result %v built %d", s.result, len(s.built))
				}
				return
			}
			if len(s.built) != 1 || s.built[0].RequestOverrides["service_tier"] != c.expected {
				t.Fatalf("built %v", s.built)
			}
		})
	}
}

func TestUltrafastIsCheckedAgainstTheRoutesOwnAccount(t *testing.T) {
	var seen []string
	swapUltrafastOffered(t, func(_, _, _, accessToken string) (bool, error) {
		seen = append(seen, accessToken)
		return accessToken == "routed-account-key", nil
	})
	s := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsCodex, "api_key", "routed-account-key", "model", "gpt-6.1-sol", "service_tier", "ultrafast")}, routeFieldsOptions{})
	if !reflect.DeepEqual(seen, []string{"routed-account-key"}) {
		t.Fatalf("seen %v", seen)
	}
	if !reflect.DeepEqual(s.result, map[string]any{"status": "built"}) || len(s.built) != 1 || s.built[0].RequestOverrides["service_tier"] != "ultrafast" {
		t.Fatalf("result %v built %v", s.result, s.built)
	}
}

func TestRouteTierSurvivesAProviderExtraBodyMergedAtConstruction(t *testing.T) {
	construct := func(o AIAgentOptions) *AIAgent {
		child := routeFieldsConstruct(o)
		merged := maps.Clone(child.requestOverrides)
		merged["extra_body"] = map[string]any{"service_tier": "priority", "x": 1}
		child.requestOverrides = merged
		return child
	}
	parent := routeFieldsParent(map[string]any{"service_tier": "priority"})
	s := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsCodex, "service_tier", "normal")}, routeFieldsOptions{parent: parent, construct: construct})
	if len(s.children) != 1 {
		t.Fatalf("children %d", len(s.children))
	}
	if !reflect.DeepEqual(s.children[0].requestOverrides, map[string]any{"extra_body": map[string]any{"x": 1}}) {
		t.Fatalf("child overrides %v", s.children[0].requestOverrides)
	}
	if !reflect.DeepEqual(parent.requestOverrides, map[string]any{"service_tier": "priority"}) {
		t.Fatalf("parent overrides %v", parent.requestOverrides)
	}
}

func TestDefaultBackendNeverReadsATierFromTheDelegationSection(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{nil}, routeFieldsOptions{cfg: withRoute(routeFieldsCodex, "service_tier", "fast"), useDefaultBackend: true})
	if !reflect.DeepEqual(s.result, map[string]any{"status": "built"}) || len(s.built) != 1 {
		t.Fatalf("result %v built %d", s.result, len(s.built))
	}
	if _, present := s.built[0].RequestOverrides["service_tier"]; present || s.built[0].ServiceTier != "" {
		t.Fatalf("child %v tier %q", s.built[0].RequestOverrides, s.built[0].ServiceTier)
	}
}

func TestUnknownRouteReasoningEffortRefusesTheTaskNeverTheParentsLevel(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsLocal, "reasoning_effort", "hgih"), nil}, routeFieldsOptions{})
	refused := refusedEntries(t, s.result)
	if len(refused) != 1 || !strings.Contains(fmt.Sprint(refused[0]["error"]), "hgih") || !strings.Contains(fmt.Sprint(refused[0]["error"]), "xhigh") || len(s.built) != 1 {
		t.Fatalf("refused %v built %d", refused, len(s.built))
	}
}

func TestTheDelegationSectionsOwnEffortKeepsItsWarnAndInheritRule(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{nil}, routeFieldsOptions{cfg: map[string]any{"reasoning_effort": "hgih"}})
	if !reflect.DeepEqual(s.result, map[string]any{"status": "built"}) || len(s.built) != 1 {
		t.Fatalf("result %v built %d", s.result, len(s.built))
	}
	if !reflect.DeepEqual(s.built[0].ReasoningConfig, map[string]any{"enabled": true, "effort": "medium"}) {
		t.Fatalf("reasoning %v", s.built[0].ReasoningConfig)
	}
}

func TestLabelledRouteResultReportsWhatTheChildRanOn(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{
		withRoute(routeFieldsCodex, "reasoning_effort", "high", "service_tier", "fast", "label", "deep"),
		withRoute(routeFieldsLocal, "reasoning_effort", "none", "label", "local"),
	}, routeFieldsOptions{})
	if len(s.children) != 2 {
		t.Fatalf("children %d result %v", len(s.children), s.result)
	}
	deep, local := s.children[0], s.children[1]
	entry := buildResultEntry(deep, map[string]any{"final_response": "ok", "completed": true}, 0, 1.0, schemaOutcome{})
	want := map[string]any{"backend": "route-fields", "label": "deep", "provider": "openai-codex", "model": "gpt-6-astra", "reasoning_effort": "high", "service_tier": "fast"}
	if got := routeFieldsReport(entry); !reflect.DeepEqual(got, want) {
		t.Fatalf("route %v", got)
	}
	wantLocal := map[string]any{"backend": "route-fields", "label": "local", "provider": "custom", "model": "qwen3-local", "reasoning_effort": "none", "service_tier": "normal"}
	if got := routeFieldsReport(fabricatedEntry(1, "error", "boom", local, 0)); !reflect.DeepEqual(got, wantLocal) {
		t.Fatalf("route %v", got)
	}
}

func TestEveryBackendRoutedResultNamesItsBackendAndTheDefaultKeepsItsShape(t *testing.T) {
	entryOf := func(child *AIAgent) *pyjson.Object {
		return buildResultEntry(child, map[string]any{"final_response": "ok", "completed": true}, 0, 1.0, schemaOutcome{})
	}
	routed := spawnRouteFields(t, []map[string]any{withRoute(routeFieldsLocal, "reasoning_effort", "high")}, routeFieldsOptions{})
	if len(routed.children) != 1 {
		t.Fatalf("children %d result %v", len(routed.children), routed.result)
	}
	want := map[string]any{"backend": "route-fields", "provider": "custom", "model": "qwen3-local", "reasoning_effort": "high", "service_tier": "normal"}
	if got := routeFieldsReport(entryOf(routed.children[0])); !reflect.DeepEqual(got, want) {
		t.Fatalf("route %v", got)
	}
	plain := spawnRouteFields(t, []map[string]any{nil}, routeFieldsOptions{cfg: routeFieldsLocal, useDefaultBackend: true})
	if len(plain.children) != 1 {
		t.Fatalf("children %d result %v", len(plain.children), plain.result)
	}
	if _, present := entryOf(plain.children[0]).Get("route"); present {
		t.Fatal("default backend entry carries a route")
	}
}

func TestAnInheritedFastTierIsReportedAsWhatRan(t *testing.T) {
	s := spawnRouteFields(t, []map[string]any{{"label": "inherit"}}, routeFieldsOptions{parent: routeFieldsParent(map[string]any{"service_tier": "priority"})})
	if len(s.children) != 1 {
		t.Fatalf("children %d result %v", len(s.children), s.result)
	}
	entry := buildResultEntry(s.children[0], map[string]any{"final_response": "ok", "completed": true}, 0, 1.0, schemaOutcome{})
	if got := routeFieldsReport(entry); got["service_tier"] != "fast" {
		t.Fatalf("route %v", got)
	}
}
