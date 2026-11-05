package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Isolated home, emptied ledgers and every mcp toolset entry removed from the given scopes afterwards
func regSetup(t *testing.T, scopes ...string) (*tools.Env, context.Context) {
	t.Helper()
	dir := isolateHome(t)
	clearExtra := func() {
		mcpMu.Lock()
		for k := range toolReadOnlyHints {
			delete(toolReadOnlyHints, k)
		}
		for k := range mcpToolServerNames {
			delete(mcpToolServerNames, k)
		}
		mcpMu.Unlock()
	}
	purge := func() {
		ctx := context.Background()
		for _, scope := range append([]string{""}, scopes...) {
			for _, d := range tools.Default.Snapshot(ctx, scope) {
				if strings.HasPrefix(d.Toolset, "mcp-") {
					_ = tools.Default.Deregister(d.Name, scope, "")
					tools.Default.Remove(d.Name)
				}
			}
		}
	}
	clearExtra()
	purge()
	t.Cleanup(func() { purge(); clearExtra() })
	return &tools.Env{Home: dir}, context.Background()
}

// Tool with an optional readOnlyHint annotation
func regTool(name string, readOnly *bool) *Tool {
	t := &Tool{Name: name, Description: "desc " + name, InputSchema: map[string]any{"type": "object"}}
	if readOnly != nil {
		t.Annotations = &ToolAnnotations{ReadOnlyHint: readOnly}
	}
	return t
}

func bptr(b bool) *bool { return &b }

// Server task with tools, capabilities and a session stand in
func regServer(env *tools.Env, name string, caps *ServerCapabilities, list ...*Tool) *serverTask {
	s := newServerTask(name, env)
	s.tools = list
	s.session = &Session{}
	if caps != nil {
		s.initializeResult = &InitializeResult{Capabilities: caps}
	}
	return s
}

func capsOf(resources, prompts bool) *ServerCapabilities {
	c := &ServerCapabilities{Tools: map[string]any{"listChanged": true}}
	if resources {
		c.Resources = map[string]any{"listChanged": true}
	}
	if prompts {
		c.Prompts = map[string]any{"listChanged": true}
	}
	return c
}

func mustRegister(t *testing.T, ctx context.Context, s *serverTask, cfg map[string]any) []string {
	t.Helper()
	names, err := registerServerTools(ctx, s.name, s, cfg)
	if err != nil {
		t.Fatalf("registerServerTools: %v", err)
	}
	return names
}

func TestRegistrationRecordsHintsAndTrust(t *testing.T) {
	env, ctx := regSetup(t)
	s := regServer(env, "srv", nil, regTool("list_repos", bptr(true)), regTool("delete_repo", bptr(false)), regTool("no_annotations", nil))
	mustRegister(t, ctx, s, map[string]any{"trust": "untrusted", "tools": map[string]any{"resources": false, "prompts": false}})
	key := ServerKey{Name: "srv"}
	if serverTrustLevels[key] != TrustUntrusted {
		t.Fatalf("trust = %q", serverTrustLevels[key])
	}
	hints := toolReadOnlyHints[key]
	if !hints["list_repos"] || hints["delete_repo"] || hints["no_annotations"] {
		t.Fatalf("hints = %v", hints)
	}
	if mcpToolServerNames["mcp__srv__list_repos"] != "srv" {
		t.Fatalf("provenance = %v", mcpToolServerNames)
	}
}

func TestTrustNormalizationAtRegistration(t *testing.T) {
	cases := map[any]string{"banana": TrustUntrusted, "full": TrustFull, "UNTRUSTED": TrustUntrusted, "  Full ": TrustFull}
	for in, want := range cases {
		if got := normalizeServerTrust(in); got != want {
			t.Fatalf("normalizeServerTrust(%q) = %q", in, got)
		}
	}
	if normalizeServerTrust(nil) != TrustFull {
		t.Fatal("absent trust must default to full")
	}
}

func TestRegistrationCachedAnnotations(t *testing.T) {
	list := cachedTools([]any{
		map[string]any{"name": "a", "annotations": map[string]any{"readOnlyHint": true}},
		map[string]any{"name": "b", "annotations": map[string]any{"readOnlyHint": "yes"}},
		map[string]any{"name": "c"},
		map[string]any{"name": "d", "annotations": map[string]any{"read_only_hint": true}},
		map[string]any{"name": "e", "annotations": "bogus", "inputSchema": "bogus"},
		map[string]any{"description": "nameless"},
		"not a row",
	})
	got := map[string]bool{}
	for _, tool := range list {
		got[tool.Name] = annotationReadOnlyHint(tool)
		if tool.InputSchema == nil {
			t.Fatalf("%s: input schema must default to an empty mapping", tool.Name)
		}
	}
	want := map[string]bool{"a": true, "b": false, "c": false, "d": true, "e": false}
	if len(got) != len(want) {
		t.Fatalf("rows = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s read only = %v want %v", k, got[k], v)
		}
	}
}

func TestRegistrationUtilityCapabilityGating(t *testing.T) {
	env, _ := regSetup(t)
	keys := func(list []UtilitySchema) []string {
		var out []string
		for _, u := range list {
			out = append(out, u.HandlerKey)
		}
		sort.Strings(out)
		return out
	}
	all := []string{"get_prompt", "list_prompts", "list_resources", "read_resource"}
	cases := []struct {
		name   string
		server *serverTask
		cfg    map[string]any
		want   []string
	}{
		{"tools only", regServer(env, "context7", capsOf(false, false)), map[string]any{}, nil},
		{"resources only", regServer(env, "res-only", capsOf(true, false)), map[string]any{}, []string{"list_resources", "read_resource"}},
		{"fully capable", regServer(env, "full", capsOf(true, true)), map[string]any{}, all},
		{"config disables resources", regServer(env, "f1", capsOf(true, true)), map[string]any{"tools": map[string]any{"resources": false}}, []string{"get_prompt", "list_prompts"}},
		{"config disables prompts", regServer(env, "f2", capsOf(true, true)), map[string]any{"tools": map[string]any{"prompts": "no"}}, []string{"list_resources", "read_resource"}},
		{"legacy session", regServer(env, "legacy", nil), map[string]any{}, all},
		{"events opt in", regServer(env, "ev", capsOf(false, false)), map[string]any{"events": true}, []string{"list_event_types", "subscribe_event", "unsubscribe_event"}},
	}
	noSession := regServer(env, "legacy-partial", nil)
	noSession.session = nil
	cases = append(cases, struct {
		name   string
		server *serverTask
		cfg    map[string]any
		want   []string
	}{"legacy without session", noSession, map[string]any{}, nil})
	for _, c := range cases {
		if got := keys(selectUtilitySchemas(c.server.name, c.server, c.cfg)); !slices.Equal(got, c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRegistrationUtilityToolsRegistered(t *testing.T) {
	env, ctx := regSetup(t)
	s := regServer(env, "fs", capsOf(true, true), regTool("read_file", nil))
	names := mustRegister(t, ctx, s, map[string]any{})
	want := []string{"mcp__fs__read_file", "mcp__fs__list_resources", "mcp__fs__read_resource", "mcp__fs__list_prompts", "mcp__fs__get_prompt"}
	if !slices.Equal(names, want) {
		t.Fatalf("registered %v", names)
	}
	for _, n := range want {
		if d := tools.Default.Lookup(ctx, "", n); d == nil || d.Toolset != "mcp-fs" || d.Check == nil {
			t.Fatalf("%s missing from registry", n)
		}
	}
	if !tools.Default.ValidateToolset(ctx, "", "fs") || !tools.Default.ValidateToolset(ctx, "", "mcp-fs") {
		t.Fatal("server alias must validate")
	}
	if !slices.Contains(tools.Default.ResolveToolset(ctx, "", "fs"), "mcp__fs__read_file") {
		t.Fatal("alias must resolve to the server tools")
	}
}

func TestRegistrationSelectiveToolLoading(t *testing.T) {
	env, ctx := regSetup(t)
	mk := func(name string) *serverTask {
		return regServer(env, name, capsOf(false, false), regTool("create_service", nil), regTool("delete_service", nil), regTool("list_services", nil))
	}
	got := mustRegister(t, ctx, mk("ink"), map[string]any{"tools": map[string]any{
		"include": []any{"create_service"}, "exclude": []any{"create_service", "delete_service"}}})
	if !slices.Equal(got, []string{"mcp__ink__create_service"}) {
		t.Fatalf("include must win over exclude, got %v", got)
	}
	if got := mustRegister(t, ctx, mk("empty"), map[string]any{"tools": map[string]any{"include": []any{}}}); len(got) != 0 {
		t.Fatalf("empty include must register nothing, got %v", got)
	}
	got = mustRegister(t, ctx, mk("glob"), map[string]any{"tools": map[string]any{"exclude": []any{"*_service"}}})
	if !slices.Equal(got, []string{"mcp__glob__list_services"}) {
		t.Fatalf("exclude glob, got %v", got)
	}
	got = mustRegister(t, ctx, mk("one"), map[string]any{"tools": map[string]any{"include": "list_services"}})
	if !slices.Equal(got, []string{"mcp__one__list_services"}) {
		t.Fatalf("string include, got %v", got)
	}
}

func TestRegistrationBuiltinCollisionGuard(t *testing.T) {
	env, ctx := regSetup(t)
	logs := captureLogs(t)
	registerForTest(t, "mcp__abc__search", "web")
	got := mustRegister(t, ctx, regServer(env, "abc", capsOf(false, false), regTool("search", nil)), map[string]any{})
	if len(got) != 0 || tools.Default.ToolsetForTool(ctx, "", "mcp__abc__search") != "web" {
		t.Fatalf("built-in must be preserved, got %v", got)
	}
	if logs.count("collides with built-in tool in toolset 'web'") != 1 {
		t.Fatalf("logs: %s", logs.text())
	}
	def := testToolDef("mcp__srv__do_thing", "mcp-old")
	def.Schema = map[string]any{"name": "mcp__srv__do_thing", "description": "From another MCP server"}
	tools.Default.Remove(def.Name)
	tools.Default.Register(def)
	t.Cleanup(func() { tools.Default.Remove(def.Name) })
	got = mustRegister(t, ctx, regServer(env, "srv", capsOf(false, false), regTool("do_thing", nil)), map[string]any{})
	entry := tools.Default.Lookup(ctx, "", "mcp__srv__do_thing")
	if len(got) != 0 || entry == nil || entry.Toolset != "mcp-old" || entry.Schema["description"] != "From another MCP server" {
		t.Fatalf("existing MCP owner must be preserved, got %v", got)
	}
	if logs.count("already owned by MCP toolset 'mcp-old'") != 1 {
		t.Fatalf("logs: %s", logs.text())
	}
}

func TestRegistrationNameCollisions(t *testing.T) {
	env, ctx := regSetup(t)
	logs := captureLogs(t)
	s := regServer(env, "srv", capsOf(true, false), regTool("list_resources", nil), regTool("read-file", nil), regTool("read_file", nil), regTool("ok", nil))
	got := mustRegister(t, ctx, s, map[string]any{})
	if !slices.Equal(got, []string{"mcp__srv__list_resources", "mcp__srv__ok", "mcp__srv__read_resource"}) {
		t.Fatalf("registered %v", got)
	}
	if d := tools.Default.Lookup(ctx, "", "mcp__srv__list_resources"); d.Description != "desc list_resources" {
		t.Fatalf("native tool must shadow the utility, got %q", d.Description)
	}
	if logs.count("keeping the native tool") != 1 || logs.count("name normalization collision for 'mcp__srv__read_file'") != 1 {
		t.Fatalf("logs: %s", logs.text())
	}
	dup := []candidate{{registryName: "x", origin: "tool 'x'"}, {registryName: "x", origin: "tool 'x'"}}
	if out := resolveNameCollisions("srv", dup); len(out) != 1 {
		t.Fatalf("exact duplicates keep one, got %d", len(out))
	}
}

func TestRegistrationDeregisterAllScopes(t *testing.T) {
	a, b := tools.ScopeKey("scope-a"), tools.ScopeKey("scope-b")
	env, ctx := regSetup(t, a, b)
	s := regServer(env, "srv", capsOf(false, false), regTool("ping", nil))
	key := ServerKey{Scope: a, Name: "srv"}
	mcpMu.Lock()
	mcpServers[key] = s
	mcpMu.Unlock()
	names := mustRegister(t, tools.WithSessionToolScope(ctx, a), s, map[string]any{})
	s.toolNames = names
	candidates := toolCandidates("srv", s.tools, func(string) bool { return true }, 30)
	registerCandidates(ctx, "srv", candidates, func(*tools.Env) bool { return true }, func() string { return b }, false, &key, nil)
	if !serverToolScopes[key][a] || !serverToolScopes[key][b] {
		t.Fatalf("scopes = %v", serverToolScopes[key])
	}
	deregisterMCPToolAllScopes(ctx, s, "mcp__srv__ping")
	for _, scope := range []string{a, b} {
		if tools.Default.SnapshotRegistration(scope, "mcp__srv__ping") != nil {
			t.Fatalf("tool still registered in %s", scope)
		}
	}
	if _, ok := mcpToolServerNames["mcp__srv__ping"]; ok {
		t.Fatal("provenance must be forgotten")
	}
}

func TestRegistrationEditorManifestFreeze(t *testing.T) {
	scope := tools.ScopeKey("editor-session:abc")
	env, ctx := regSetup(t, scope)
	s := regServer(env, "ed", capsOf(false, false), regTool("one", nil))
	key := ServerKey{Scope: scope, Name: "ed"}
	mcpMu.Lock()
	mcpServers[key] = s
	mcpMu.Unlock()
	if editorManifestFrozen(s) {
		t.Fatal("fresh task must not be frozen")
	}
	got := mustRegister(t, ctx, s, map[string]any{})
	if !slices.Equal(got, []string{"mcp__ed__one"}) || !editorManifestFrozen(s) {
		t.Fatalf("first registration %v frozen=%v", got, editorManifestFrozen(s))
	}
	if tools.Default.SnapshotRegistration(scope, "mcp__ed__one") == nil {
		t.Fatal("editor tools register into the owner scope")
	}
	if entry := GetCachedEntry(ctx, "ed", ConfigFingerprint(map[string]any{})); len(entry) != 0 {
		t.Fatal("editor sessions never write the schema cache")
	}
	if _, err := registerServerTools(ctx, "ed", s, map[string]any{}); err != nil {
		t.Fatalf("unchanged manifest must pass: %v", err)
	}
	s.tools = append(s.tools, regTool("two", nil))
	if _, err := registerServerTools(ctx, "ed", s, map[string]any{}); !errors.Is(err, ErrEditorSchemaChanged) || err.Error() != "acp_mcp_schema_changed" {
		t.Fatalf("changed manifest err = %v", err)
	}
	if err := s.refreshTools(ctx); err != nil {
		t.Fatalf("frozen refresh must be skipped: %v", err)
	}
}

func TestRegistrationWritesSchemaCache(t *testing.T) {
	env, ctx := regSetup(t)
	s := regServer(env, "cached", capsOf(true, false), regTool("read", bptr(true)), regTool("write", nil), regTool("hidden", nil))
	s.listCacheMeta = CacheHints{TTLMs: fptr(5000), CacheScope: "user"}
	cfg := map[string]any{"command": "x", "tools": map[string]any{"exclude": []any{"hidden"}}}
	mustRegister(t, ctx, s, cfg)
	entry := GetCachedEntry(ctx, "cached", ConfigFingerprint(cfg))
	rows := ToolsFromCacheEntry(entry)
	if len(rows) != 2 {
		t.Fatalf("cache rows %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "read" || first["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("first row %v", first)
	}
	if len(UtilityToolsFromCacheEntry(entry)) != 2 || entry["cache_scope"] != "user" {
		t.Fatalf("entry %v", entry)
	}
}

func TestRegistrationFromCacheLazy(t *testing.T) {
	env, ctx := regSetup(t)
	cfg := map[string]any{"command": "x", "trust": "untrusted", "timeout": 90}
	utility := utilityRows(buildUtilitySchemas("lz")[:2])
	utility = append(utility, map[string]any{"schema": map[string]any{"name": "bad"}, "handler_key": "nope"})
	entry := map[string]any{"tools": []any{
		map[string]any{"name": "a", "description": "A", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
		map[string]any{"name": "b"},
	}, "utility_tools": utility}
	names, err := registerFromCacheSync(ctx, env, "lz", cfg, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"mcp__lz__a", "mcp__lz__b", "mcp__lz__list_resources", "mcp__lz__read_resource"}) {
		t.Fatalf("names %v", names)
	}
	key := ServerKey{Name: "lz"}
	if !slices.Equal(lazyServerToolNames[key], names) || lazyServerFingerprints[key] != ConfigFingerprint(cfg) || lazyServerConfigs[key]["trust"] != "untrusted" {
		t.Fatal("lazy ledgers not recorded")
	}
	if serverTrustLevels[key] != TrustUntrusted || !toolReadOnlyHints[key]["a"] || toolReadOnlyHints[key]["b"] {
		t.Fatal("trust metadata must be recorded before lazy registration")
	}
	if d := tools.Default.Lookup(ctx, "", "mcp__lz__a"); d == nil || d.Timeout.Seconds() != 90 {
		t.Fatalf("execution timeout not carried: %+v", d)
	}
	if got := existingToolNames(ctx); !slices.Equal(got, names) {
		t.Fatalf("existing names %v", got)
	}
}

func TestRegistrationIdentityDigest(t *testing.T) {
	inputs := []any{"https://x.example/mcp", map[string]string{"B": "2", "A": "\u00e9"}}
	sum := sha256.Sum256([]byte(`["https://x.example/mcp", {"A": "\u00e9", "B": "2"}]`))
	if got := identityDigest(inputs); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s", got)
	}
	base := map[string]any{"url": "https://x.example/mcp", "headers": map[string]any{"Authorization": "Bearer a"}}
	same := map[string]any{"url": "https://x.example/mcp", "headers": map[string]any{"Authorization": "Bearer a"}, "ssl_verify": 1}
	other := map[string]any{"url": "https://x.example/mcp", "headers": map[string]any{"Authorization": "Bearer b"}}
	if connectionIdentity(base) != connectionIdentity(same) {
		t.Fatal("ssl_verify 1 equals the default True")
	}
	if connectionIdentity(base) == connectionIdentity(other) {
		t.Fatal("different credentials are different identities")
	}
	if authType(map[string]any{"auth": " OAuth "}) != "oauth" || authType(map[string]any{}) != "" {
		t.Fatal("auth type normalisation")
	}
}

func TestRegistrationHealsSharedConnectionIntoScope(t *testing.T) {
	ownerScope, adopterScope := tools.ScopeKey("owner"), tools.ScopeKey("adopter")
	env, ctx := regSetup(t, ownerScope, adopterScope)
	cfg := map[string]any{"url": "https://shared.example/mcp", "trust": "untrusted"}
	r := &Rendered{Config: cfg}
	s := regServer(env, "shared", capsOf(false, false), regTool("go", nil))
	s.config = map[string]any{"url": "https://shared.example/mcp", "trust": "full"}
	digest, ok := adopterIdentityDigest(ctx, env, "shared", r)
	if !ok {
		t.Fatal("http identity must resolve")
	}
	s.resolvedIdentity = digest
	owner := ServerKey{Scope: ownerScope, Name: "shared"}
	mcpMu.Lock()
	mcpServers[owner] = s
	mcpMu.Unlock()
	adopter := tools.WithSessionToolScope(ctx, adopterScope)
	n, err := registerConnectedIntoCurrentScope(adopter, env, map[string]*Rendered{"shared": r})
	if err != nil || n != 1 {
		t.Fatalf("healed %d err %v", n, err)
	}
	if tools.Default.SnapshotRegistration(adopterScope, "mcp__shared__go") == nil || !serverToolScopes[owner][adopterScope] {
		t.Fatal("adopter overlay missing")
	}
	if serverTrustLevels[ServerKey{Scope: adopterScope, Name: "shared"}] != TrustUntrusted {
		t.Fatal("adopter records its own trust policy")
	}
	if !slices.Contains(s.registeredToolNames(), "mcp__shared__go") {
		t.Fatal("registered names must include the adopted tools")
	}
	if n, _ := registerConnectedIntoCurrentScope(ctx, env, map[string]*Rendered{"shared": r}); n != 0 {
		t.Fatal("no scope means nothing to heal")
	}
	changed := &Rendered{Config: map[string]any{"url": "https://shared.example/mcp", "headers": map[string]any{"X": "y"}}}
	if n, _ := registerConnectedIntoCurrentScope(adopter, env, map[string]*Rendered{"shared": changed}); n != 0 {
		t.Fatal("a different identity must not be shared")
	}
	if tools.Default.SnapshotRegistration(adopterScope, "mcp__shared__go") != nil || serverToolScopes[owner][adopterScope] {
		t.Fatal("changed config removes the adopter overlay")
	}
	oauth := map[string]any{"url": "https://shared.example/mcp", "auth": "oauth"}
	if sameServerRoute(s, oauth, true, digest, true) {
		t.Fatal("oauth connections are never shared across profiles")
	}
}
