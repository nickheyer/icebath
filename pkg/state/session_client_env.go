package state

import (
	"context"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	ClientBehaviourEnv     = []string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_INFERENCE_PROVIDER", "ICEBATH_ENABLE_PROJECT_PLUGINS"}
	ClientHostBehaviour    = []string{"ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_ENABLE_PROJECT_PLUGINS"}
	ClientToolEnv          = []string{"PATH", "VIRTUAL_ENV", "GOPATH", "GOROOT", "CARGO_HOME", "RUSTUP_HOME", "JAVA_HOME"}
	ClientToolEnvPrefixes  = []string{"CONDA_", "NODE_", "NVM_", "PYENV_"}
	SecretEnvName          func(name string) bool
	SecretEnvSubstrings    []string
	errClientEnvNotFlatMap = pyValueError("client_env must be a map of variable names to string values")
)

const (
	ClientEnvConfigKey       = "client_env_passthrough"
	ClientEnvNotLocalWarning = "this session uses the gateway's own PATH, toolchain and prefill file: the client's environment " +
		"was not applied because the client is not attached to this gateway locally."
	clientEnvMaxNames = 256
	clientEnvMaxBytes = 256 * 1024
)

// Normalisation rules a session client environment is filtered by, nil Secret meaning the live secret policy
type ClientEnvRules struct {
	Behaviour []string
	Names     []string
	Prefixes  []string
	Secret    []string
}

// Rules as the dict hermes freezes into client_env_rules_json
func (r *ClientEnvRules) Object() *pyjson.Object {
	out := pyjson.NewObject()
	for _, kv := range []struct {
		key  string
		list []string
	}{{"behaviour", r.Behaviour}, {"names", r.Names}, {"prefixes", r.Prefixes}, {"secret", r.Secret}} {
		if kv.list == nil && kv.key == "secret" {
			out.Set(kv.key, nil)
			continue
		}
		items := make([]any, len(kv.list))
		for i, s := range kv.list {
			items[i] = s
		}
		out.Set(kv.key, items)
	}
	return out
}

// Rules from their decoded dict, a missing key a KeyError and a non string list a TypeError
func ParseClientEnvRules(raw any) (*ClientEnvRules, error) {
	obj, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, &stateTypedError{"TypeError", "client_env rules must be a dict"}
	}
	lists := make([][]string, 4)
	for i, key := range []string{"behaviour", "names", "prefixes", "secret"} {
		v, ok := obj.Get(key)
		if !ok {
			return nil, &stateTypedError{"KeyError", pyRepr(key)}
		}
		if v == nil && key == "secret" {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			return nil, &stateTypedError{"TypeError", "client_env rules " + key + " must be a list"}
		}
		lists[i] = make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, &stateTypedError{"TypeError", "client_env rules " + key + " must hold strings"}
			}
			lists[i] = append(lists[i], s)
		}
	}
	return &ClientEnvRules{Behaviour: lists[0], Names: lists[1], Prefixes: lists[2], Secret: lists[3]}, nil
}

// An error carrying the Python exception class hermes raises
type stateTypedError struct{ kind, msg string }

func (e *stateTypedError) Error() string      { return e.msg }
func (e *stateTypedError) PyTypeName() string { return e.kind }

// Value of a dict shaped config node
func configDictGet(v any, key string) (any, bool) {
	switch m := v.(type) {
	case map[string]any:
		x, ok := m[key]
		return x, ok
	case *pyjson.Object:
		return m.Get(key)
	}
	return nil, false
}

// Profile extra tool names from terminal.client_env_passthrough
func ClientEnvConfiguredNames(config any) map[string]bool {
	out := map[string]bool{}
	terminal, _ := configDictGet(config, "terminal")
	raw, _ := configDictGet(terminal, ClientEnvConfigKey)
	var items []any
	switch l := raw.(type) {
	case []any:
		items = l
	case []string:
		for _, s := range l {
			items = append(items, s)
		}
	default:
		return out
	}
	for _, item := range items {
		if s, ok := item.(string); ok && pystr.Strip(s) != "" {
			out[pystr.Strip(s)] = true
		}
	}
	return out
}

// Whether a toolchain name may travel, refusing credentials by the secret policy and icebath control names
func clientEnvTravels(name string, secret []string, useLive bool) bool {
	var hidden bool
	if useLive {
		hidden = secretEnvNameLive(name)
	} else {
		upper := pystr.Upper(name)
		for _, s := range secret {
			if strings.Contains(upper, s) {
				hidden = true
				break
			}
		}
	}
	return !hidden && !strings.HasPrefix(name, "ICEBATH_")
}

// Live secret policy the process wiring installs from codeexec
func secretEnvNameLive(name string) bool {
	if SecretEnvName == nil {
		panic("state.SecretEnvName unassigned: the process wiring installs codeexec.IsSecretEnvName")
	}
	return SecretEnvName(name)
}

// Rules frozen with a session map, a client not proven local keeping only values that name nothing on this host
func ClientEnvRulesFor(config any, local bool) *ClientEnvRules {
	if SecretEnvSubstrings == nil {
		panic("state.SecretEnvSubstrings unassigned: the process wiring installs codeexec.SecretSubstrings")
	}
	rules := &ClientEnvRules{Names: []string{}, Prefixes: []string{}, Secret: slices.Clone(SecretEnvSubstrings)}
	for _, n := range ClientBehaviourEnv {
		if local || !slices.Contains(ClientHostBehaviour, n) {
			rules.Behaviour = append(rules.Behaviour, n)
		}
	}
	if rules.Behaviour == nil {
		rules.Behaviour = []string{}
	}
	if local {
		names := ClientEnvConfiguredNames(config)
		for _, n := range ClientToolEnv {
			names[n] = true
		}
		for n := range names {
			rules.Names = append(rules.Names, n)
		}
		sort.Strings(rules.Names)
		rules.Prefixes = slices.Clone(ClientToolEnvPrefixes)
	}
	return rules
}

// Whether the rules keep one name
func clientEnvKept(name string, rules *ClientEnvRules) bool {
	if slices.Contains(rules.Behaviour, name) {
		return true
	}
	if !clientEnvTravels(name, rules.Secret, rules.Secret == nil) {
		return false
	}
	return slices.Contains(rules.Names, name) || hasAnyPrefix(name, rules.Prefixes)
}

// Whether a name starts with any of the prefixes
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Environment as a map, the process environment when nil
func environMap(environ map[string]string) map[string]string {
	if environ != nil {
		return environ
	}
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Client side allowlisted values of this shell, the profile extra names included
func CollectClientEnv(environ map[string]string, config any) map[string]string {
	rules := ClientEnvRulesFor(config, true)
	out := map[string]string{}
	for name, value := range environMap(environ) {
		if clientEnvKept(name, rules) {
			out[name] = value
		}
	}
	return out
}

// Flat string map of a decoded client_env value, a ValueError for anything else or past the bounds
func clientEnvFlatMap(value any) (map[string]string, error) {
	out := map[string]string{}
	switch m := value.(type) {
	case map[string]string:
		for k, v := range m {
			out[k] = v
		}
	case map[string]any:
		for k, v := range m {
			s, ok := v.(string)
			if !ok {
				return nil, errClientEnvNotFlatMap
			}
			out[k] = s
		}
	case *pyjson.Object:
		for _, k := range m.Keys {
			s, ok := m.Vals[k].(string)
			if !ok {
				return nil, errClientEnvNotFlatMap
			}
			out[k] = s
		}
	default:
		return nil, errClientEnvNotFlatMap
	}
	if len(out) > clientEnvMaxNames {
		return nil, errClientEnvNotFlatMap
	}
	total := 0
	for k, v := range out {
		total += pystr.Len(k) + pystr.Len(v)
	}
	if total > clientEnvMaxBytes {
		return nil, errClientEnvNotFlatMap
	}
	return out, nil
}

// Validated map filtered by the rules, nil rules leaving it unfiltered for a map frozen before rules existed
func NormaliseClientEnv(value any, rules *ClientEnvRules) (map[string]string, error) {
	flat, err := clientEnvFlatMap(value)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, v := range flat {
		if rules == nil || clientEnvKept(name, rules) {
			out[name] = v
		}
	}
	return out, nil
}

// Gateway side map filtered by the session profile's current rules
func AcceptClientEnv(value any, config any, local bool) (map[string]string, error) {
	return NormaliseClientEnv(value, ClientEnvRulesFor(config, local))
}

// Whether a retried session.create carries the client environment the committed session froze
func RetryMatches(committed, request *pyjson.Object) (bool, error) {
	stored, _ := committed.Get("client_env_json")
	sentRaw, hasSent := request.Get("client_env")
	if stored == nil || !hasSent {
		return stored == nil && !hasSent, nil
	}
	var rules *ClientEnvRules
	flat, err := clientEnvFlatMap(sentRaw)
	if rulesRaw, _ := committed.Get("client_env_rules_json"); rulesRaw != nil {
		text, ok := rulesRaw.(string)
		if !ok {
			return false, &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(rulesRaw)}
		}
		decoded, derr := pyjson.Loads(text)
		if derr != nil {
			return false, nil
		}
		if err == nil && len(flat) > 0 && decoded != nil {
			if rules, err = ParseClientEnvRules(decoded); err != nil {
				return false, err
			}
		}
	}
	if err != nil {
		var ve *stateValueError
		if errors.As(err, &ve) {
			return false, nil
		}
		return false, err
	}
	sent, _ := NormaliseClientEnv(flat, rules)
	storedText, ok := stored.(string)
	if !ok {
		return false, &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(stored)}
	}
	decodedStored, err := pyjson.Loads(storedText)
	if err != nil {
		return false, err
	}
	obj, ok := decodedStored.(*pyjson.Object)
	if !ok || obj.Len() != len(sent) {
		return false, nil
	}
	for _, k := range obj.Keys {
		if s, ok := obj.Vals[k].(string); !ok || sent[k] != s {
			return false, nil
		}
		if _, ok := sent[k]; !ok {
			return false, nil
		}
	}
	return true, nil
}

type clientEnvKey struct{}

// One session client environment bound for a context
type ClientEnvBinding struct {
	Env   map[string]string
	Local bool
	Rules *ClientEnvRules
}

// Context binding one session client environment, a nil env binding the gateway's own
func ClientEnvScope(ctx context.Context, env map[string]string, local bool, rules *ClientEnvRules) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if env == nil {
		return context.WithValue(ctx, clientEnvKey{}, (*ClientEnvBinding)(nil))
	}
	return context.WithValue(ctx, clientEnvKey{}, &ClientEnvBinding{Env: env, Local: local, Rules: rules})
}

// Binding of the session executing under the context, nil when unbound
func BoundClientEnvBinding(ctx context.Context) *ClientEnvBinding {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(clientEnvKey{}).(*ClientEnvBinding)
	return b
}

// Client environment of the session executing under the context, nil when unbound
func BoundClientEnv(ctx context.Context) map[string]string {
	if b := BoundClientEnvBinding(ctx); b != nil {
		return b.Env
	}
	return nil
}

// Behaviour value of the session, the gateway's own for an unbound session or a host value of a remote client
func SessionEnv(ctx context.Context, name, def string) string {
	b := BoundClientEnvBinding(ctx)
	if b == nil || (slices.Contains(ClientHostBehaviour, name) && !b.Local) {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	}
	if v, ok := b.Env[name]; ok {
		return v
	}
	return def
}

// Toolchain values the session tool subprocesses start with, empty when unbound or remote
func ToolEnv(ctx context.Context) map[string]string {
	out := map[string]string{}
	b := BoundClientEnvBinding(ctx)
	if b == nil || !b.Local {
		return out
	}
	for name, value := range b.Env {
		if !slices.Contains(ClientBehaviourEnv, name) && clientEnvTravels(name, nil, true) {
			out[name] = value
		}
	}
	return out
}

// Environment the session tool subprocesses start from, the client's toolchain replacing the gateway's
func ToolchainEnviron(ctx context.Context, base map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range environMap(base) {
		env[k] = v
	}
	b := BoundClientEnvBinding(ctx)
	if b == nil {
		return env
	}
	for _, name := range ClientBehaviourEnv {
		if b.Local || !slices.Contains(ClientHostBehaviour, name) {
			delete(env, name)
		}
	}
	if !b.Local {
		return env
	}
	rules := b.Rules
	useLive := false
	if rules == nil {
		rules = &ClientEnvRules{Names: ClientToolEnv, Prefixes: ClientToolEnvPrefixes}
		useLive = true
	} else if rules.Secret == nil {
		useLive = true
	}
	names := map[string]bool{}
	for _, name := range rules.Names {
		if clientEnvTravels(name, rules.Secret, useLive) {
			names[name] = true
		}
	}
	for k := range env {
		if k != "PATH" && (names[k] || hasAnyPrefix(k, rules.Prefixes)) {
			delete(env, k)
		}
	}
	for k, v := range ToolEnv(ctx) {
		env[k] = v
	}
	return env
}
