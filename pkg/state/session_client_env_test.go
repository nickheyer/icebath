package state

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var clientEnvTestSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

func installSecretPolicy(t *testing.T) {
	t.Helper()
	prevName, prevSubs := SecretEnvName, SecretEnvSubstrings
	SecretEnvSubstrings = clientEnvTestSubstrings
	SecretEnvName = func(name string) bool {
		upper := pystr.Upper(name)
		for _, s := range clientEnvTestSubstrings {
			if strings.Contains(upper, s) {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { SecretEnvName, SecretEnvSubstrings = prevName, prevSubs })
}

var clientEnvTestConfig = map[string]any{"terminal": map[string]any{"client_env_passthrough": []any{"GEM_HOME"}}}

func clientEnvObject(m map[string]string) *pyjson.Object {
	anyMap := map[string]any{}
	for k, v := range m {
		anyMap[k] = v
	}
	return modelConfigPatchObject(anyMap)
}

func committedPolicy(t *testing.T, sent map[string]string, config any, local bool) *pyjson.Object {
	t.Helper()
	rules := ClientEnvRulesFor(config, local)
	kept, err := NormaliseClientEnv(sent, rules)
	if err != nil {
		t.Fatal(err)
	}
	envText, _ := pyjson.Dumps(clientEnvObject(kept))
	rulesText, _ := pyjson.Dumps(rules.Object())
	policy := pyjson.NewObject()
	policy.Set("client_env_json", envText)
	policy.Set("client_env_rules_json", rulesText)
	return policy
}

func retryRequest(env any) *pyjson.Object {
	req := pyjson.NewObject()
	req.Set("request_id", "x")
	if env != nil {
		req.Set("client_env", env)
	}
	return req
}

func mustRetry(t *testing.T, committed *pyjson.Object, env any) bool {
	t.Helper()
	ok, err := RetryMatches(committed, retryRequest(env))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func withEnv(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestClientEnvIsFilteredAndValidated(t *testing.T) {
	installSecretPolicy(t)
	got, err := AcceptClientEnv(map[string]string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "P", "OPENAI_API_KEY": "sk-leak", "ICEBATH_YOLO_MODE": "1",
		"NODE_CREDS": "c", "CONDA_DSN": "postgres://u:p@h/db", "NVM_DIR": "/client/.nvm"}, clientEnvTestConfig, true)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "P", "NVM_DIR": "/client/.nvm"}) {
		t.Fatalf("accepted %v %v", got, err)
	}
	bad := map[string]any{"PATH": int64(1)}
	for _, value := range []any{"PATH=/x", bad} {
		if _, err := AcceptClientEnv(value, clientEnvTestConfig, true); err == nil || err.Error() != "client_env must be a map of variable names to string values" {
			t.Fatalf("bad %v accepted %v", value, err)
		}
	}
	huge := map[string]string{}
	for i := 0; i <= clientEnvMaxNames; i++ {
		huge["NVM_"+strings.Repeat("x", i)] = "v"
	}
	if _, err := NormaliseClientEnv(huge, nil); err == nil {
		t.Fatal("more than the name bound accepted")
	}
	if _, err := NormaliseClientEnv(map[string]string{"PATH": strings.Repeat("y", clientEnvMaxBytes)}, nil); err == nil {
		t.Fatal("more than the byte bound accepted")
	}
}

func TestCredentialAndControlNamesAreRefusedByTheSecretPolicy(t *testing.T) {
	installSecretPolicy(t)
	config := map[string]any{"terminal": map[string]any{"client_env_passthrough": []any{"DEPLOY_WEBHOOK", "ICEBATH_KANBAN_DB", " MY_TOOLCHAIN_HOME ", 3}}}
	sent := map[string]string{"NODE_BEARER": "b", "NODE_CREDS": "c", "CONDA_DSN": "postgres://u:p@h/db", "DEPLOY_WEBHOOK": "https://h",
		"ICEBATH_KANBAN_DB": "/board.db", "ICEBATH_DELEGATED_CHILD_CONTEXT": "1", "NVM_DIR": "/client/.nvm", "MY_TOOLCHAIN_HOME": "/opt/tc", "PATH": "/client/bin"}
	kept := map[string]string{"NVM_DIR": "/client/.nvm", "MY_TOOLCHAIN_HOME": "/opt/tc", "PATH": "/client/bin"}
	if got, err := AcceptClientEnv(sent, config, true); err != nil || !reflect.DeepEqual(got, kept) {
		t.Fatalf("gateway kept %v %v", got, err)
	}
	if got := CollectClientEnv(sent, config); !reflect.DeepEqual(got, kept) {
		t.Fatalf("client kept %v", got)
	}
}

func TestExactRetryOfASessionCreatedBeforeClientEnv(t *testing.T) {
	installSecretPolicy(t)
	legacy := pyjson.NewObject()
	if !mustRetry(t, legacy, nil) || mustRetry(t, legacy, map[string]string{"PATH": "/x"}) || mustRetry(t, legacy, map[string]string{}) {
		t.Fatal("a session committed without a map matches only a retry without one")
	}
	desktop := committedPolicy(t, map[string]string{}, clientEnvTestConfig, true)
	if mustRetry(t, desktop, nil) {
		t.Fatal("a committed map never matches a retry without one")
	}
}

func TestExactRetryIsJudgedByTheCommittedMapNotTheCurrentAllowlist(t *testing.T) {
	installSecretPolicy(t)
	env := map[string]string{"GEM_HOME": "/gems", "BUNDLE_PATH": "/bundle", "PATH": "/client/bin"}
	committed := committedPolicy(t, env, clientEnvTestConfig, true)
	stored, _ := committed.Get("client_env_json")
	if stored != `{"GEM_HOME": "/gems", "PATH": "/client/bin"}` {
		t.Fatalf("stored %v", stored)
	}
	if !mustRetry(t, committed, env) {
		t.Fatal("exact retry refused")
	}
	for _, changed := range []map[string]string{withEnv(env, map[string]string{"GEM_HOME": "/other"}), {"PATH": "/client/bin"}} {
		if mustRetry(t, committed, changed) {
			t.Fatalf("changed retry %v matched", changed)
		}
	}
	fresh, _ := AcceptClientEnv(env, map[string]any{"terminal": map[string]any{"client_env_passthrough": []any{"BUNDLE_PATH"}}}, true)
	if !reflect.DeepEqual(fresh, map[string]string{"BUNDLE_PATH": "/bundle", "PATH": "/client/bin"}) {
		t.Fatalf("fresh create %v", fresh)
	}
}

func TestExactRetryComparesTheCompleteMapUnderItsCommittedRules(t *testing.T) {
	installSecretPolicy(t)
	env := map[string]string{"GEM_HOME": "/gems", "PATH": "/client/bin"}
	ref := committedPolicy(t, env, clientEnvTestConfig, true)
	empty := committedPolicy(t, map[string]string{}, clientEnvTestConfig, true)
	remote := committedPolicy(t, map[string]string{"PATH": "/a", "ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "P"}, clientEnvTestConfig, false)
	if !mustRetry(t, ref, withEnv(env, map[string]string{"BUNDLE_PATH": "/b", "NODE_AUTH_TOKEN": "t", "ICEBATH_YOLO_MODE": "1"})) {
		t.Fatal("names the committed rules drop must not count")
	}
	for _, added := range []map[string]string{{"VIRTUAL_ENV": "/venv"}, {"NVM_DIR": "/nvm"}, {"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "P"}} {
		if mustRetry(t, ref, withEnv(env, added)) {
			t.Fatalf("added %v matched", added)
		}
	}
	if !mustRetry(t, empty, map[string]string{"BUNDLE_PATH": "/b"}) {
		t.Fatal("a name not allowlisted at commit counted")
	}
	for _, sent := range []any{map[string]string{"PATH": "/client/bin"}, map[string]string{"GEM_HOME": "/g"}, "PATH=/x"} {
		if mustRetry(t, empty, sent) {
			t.Fatalf("sent %v matched the empty commit", sent)
		}
	}
	if !mustRetry(t, remote, map[string]string{"PATH": "/b", "ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "P"}) {
		t.Fatal("host values dropped at a remote commit counted")
	}
	if mustRetry(t, remote, map[string]string{"PATH": "/a", "ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "Q"}) {
		t.Fatal("a changed behaviour value matched")
	}
	broken := pyjson.NewObject()
	broken.Set("client_env_json", `{}`)
	broken.Set("client_env_rules_json", "{not json")
	if mustRetry(t, broken, map[string]string{"PATH": "/a"}) {
		t.Fatal("undecodable rules matched")
	}
	unfiltered := pyjson.NewObject()
	unfiltered.Set("client_env_json", `{"PATH": "/a"}`)
	if !mustRetry(t, unfiltered, map[string]string{"PATH": "/a"}) || mustRetry(t, unfiltered, map[string]string{"PATH": "/a", "GEM_HOME": "/g"}) {
		t.Fatal("a map frozen before rules is compared unfiltered")
	}
}

func TestToolchainEnvironStartsFromTheSessionsToolchain(t *testing.T) {
	installSecretPolicy(t)
	daemon := map[string]string{"PATH": "/daemon/bin:/usr/bin:/bin", "ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "daemon prompt",
		"ICEBATH_PREFILL_MESSAGES_FILE": "/daemon/prefill", "ICEBATH_INFERENCE_PROVIDER": "daemon", "GEM_HOME": "/daemon/gems", "CONDA_PREFIX": "/daemon/conda", "HOME": "/home/d"}
	client := map[string]string{"PATH": "/client/venv/bin:/usr/bin", "VIRTUAL_ENV": "/client/venv", "NVM_DIR": "/client/.nvm"}
	ctx := ClientEnvScope(context.Background(), client, true, nil)
	env := ToolchainEnviron(ctx, daemon)
	if env["PATH"] != client["PATH"] || env["NVM_DIR"] != "/client/.nvm" || env["VIRTUAL_ENV"] != "/client/venv" || env["HOME"] != "/home/d" {
		t.Fatalf("local session env %v", env)
	}
	for _, name := range []string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_INFERENCE_PROVIDER", "CONDA_PREFIX"} {
		if _, ok := env[name]; ok {
			t.Fatalf("%s leaked from the daemon", name)
		}
	}
	if env["GEM_HOME"] != "/daemon/gems" {
		t.Fatal("a name outside the built in rules was unset")
	}
	frozen := ClientEnvRulesFor(clientEnvTestConfig, true)
	if got := ToolchainEnviron(ClientEnvScope(context.Background(), client, true, frozen), daemon); got["GEM_HOME"] != "" {
		t.Fatalf("configured name the client omitted leaked %v", got["GEM_HOME"])
	}
	noPath := ToolchainEnviron(ClientEnvScope(context.Background(), map[string]string{"NVM_DIR": "/n"}, true, nil), daemon)
	if noPath["PATH"] != daemon["PATH"] {
		t.Fatal("a client that sent no PATH lost the gateway's")
	}
	remote := ToolchainEnviron(ClientEnvScope(context.Background(), client, false, nil), daemon)
	if remote["PATH"] != daemon["PATH"] || remote["ICEBATH_PREFILL_MESSAGES_FILE"] != "/daemon/prefill" || remote["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] != "" {
		t.Fatalf("remote session env %v", remote)
	}
	if got := ToolchainEnviron(context.Background(), daemon); !reflect.DeepEqual(got, daemon) {
		t.Fatal("unbound session changed the gateway environment")
	}
	behaviourOnly := ToolchainEnviron(ClientEnvScope(context.Background(), map[string]string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "p"}, true, nil), daemon)
	if _, ok := behaviourOnly["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"]; ok {
		t.Fatal("behaviour names are not tool env")
	}
	refused := withEnv(client, map[string]string{"NODE_BEARER": "b", "ICEBATH_KANBAN_DB": "/board.db"})
	if tools := ToolEnv(ClientEnvScope(context.Background(), refused, true, nil)); tools["NODE_BEARER"] != "" || tools["ICEBATH_KANBAN_DB"] != "" || tools["NVM_DIR"] != "/client/.nvm" {
		t.Fatalf("tool env %v", tools)
	}
	if len(ToolEnv(ClientEnvScope(context.Background(), client, false, nil))) != 0 || len(ToolEnv(context.Background())) != 0 {
		t.Fatal("remote or unbound tool env not empty")
	}
}

func TestHostValuesApplyOnlyOverAGatewayProvenLocalAttach(t *testing.T) {
	installSecretPolicy(t)
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "/daemon/prefill")
	t.Setenv("ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "daemon prompt")
	client := map[string]string{"ICEBATH_PREFILL_MESSAGES_FILE": "/client/prefill", "ICEBATH_EPHEMERAL_SYSTEM_PROMPT": "client prompt"}
	local := ClientEnvScope(context.Background(), client, true, nil)
	remote := ClientEnvScope(context.Background(), client, false, nil)
	if SessionEnv(local, "ICEBATH_PREFILL_MESSAGES_FILE", "") != "/client/prefill" || SessionEnv(remote, "ICEBATH_PREFILL_MESSAGES_FILE", "") != "/daemon/prefill" {
		t.Fatal("prefill host value")
	}
	if SessionEnv(remote, "ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "") != "client prompt" {
		t.Fatal("remote prompt must be the client's")
	}
	if SessionEnv(local, "ICEBATH_INFERENCE_PROVIDER", "dflt") != "dflt" {
		t.Fatal("an unexported behaviour name must be unset")
	}
	if SessionEnv(context.Background(), "ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "") != "daemon prompt" {
		t.Fatal("unbound session reads the gateway environment")
	}
	shadow := ClientEnvScope(local, nil, true, nil)
	if BoundClientEnv(shadow) != nil || BoundClientEnv(local)["ICEBATH_PREFILL_MESSAGES_FILE"] != "/client/prefill" {
		t.Fatal("a nil scope must bind the gateway's own environment")
	}
	rules := ClientEnvRulesFor(clientEnvTestConfig, false)
	if len(rules.Names) != 0 || len(rules.Prefixes) != 0 || !reflect.DeepEqual(rules.Behaviour, []string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "ICEBATH_INFERENCE_PROVIDER"}) {
		t.Fatalf("remote rules %+v", rules)
	}
	text, _ := pyjson.Dumps(rules.Object())
	parsed, err := ParseClientEnvRules(mustLoads(t, text))
	if err != nil || !reflect.DeepEqual(parsed.Behaviour, rules.Behaviour) || len(parsed.Secret) != len(clientEnvTestSubstrings) {
		t.Fatalf("rules round trip %+v %v", parsed, err)
	}
	if _, err := ParseClientEnvRules(mustLoads(t, `{"behaviour": []}`)); err == nil {
		t.Fatal("rules missing a key parsed")
	}
}

func mustLoads(t *testing.T, text string) any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
