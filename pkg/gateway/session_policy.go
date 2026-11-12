package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch policy section and marker names a local session policy carries
const (
	FollowAdopted    = "follow_adopted"
	LocalDesktopChat = "desktop_local"
	SessionPins      = "session_pins"
)

var (
	// Fields a local session create request may carry
	CreateFields = map[string]bool{"request_id": true, "source": true, "cwd": true, "model": true, "toolsets": true,
		"provider": true, "base_url": true, "reasoning": true, "max_turns": true, "ignore_rules": true, "api_key": true, "editor": true,
		"yolo": true, "safe_mode": true, "ignore_user_config": true,
		"skills": true, "checkpoints": true, "accept_hooks": true, "pass_session_id": true, "follow_profile_config": true,
		"service_tier": true, "cwd_explicit": true, "run_budget": true, "verbose": true, "row_cwd": true, "client_env": true}
	// Sections a profile following session re-reads from the owning profile each turn
	FollowSections = []string{"model", "providers", "custom_providers", "fallback_providers", "fallback_model", "model_aliases",
		"platform_toolsets"}
	// Agent keys a profile following session re-reads each turn
	FollowAgentKeys = []string{"reasoning_effort", "reasoning_overrides", "service_tier", "disabled_toolsets", "coding_context"}
	// Model block keys a provider pick owns
	SelectionKeys = []string{"default", "provider", "base_url", "api_mode", "selection_profile", "api_key_from", "key_env",
		"api_key_env"}
	// Troubleshooting isolation fields of a launch request
	BypassFields = []string{"safe_mode", "ignore_user_config"}
	// Creation label to agent surface
	Surfaces = map[string]string{"cli": "cli", "tui": "tui", "gui": "desktop", "acp": "acp", "tool": "cli", "oneshot": "cli"}
	// Creation labels of the owner chats that carry the Daybreak route
	DaybreakLabels = map[string]bool{"cli": true, "tui": true, "gui": true, "oneshot": true}
	// Creation labels stored under the local source
	storedAsLocal = map[string]bool{"cli": true, "tui": true, "gui": true, "acp": true}
	// Policy label and agent surface of one conversation keyed by either name
	SurfaceGroups = surfaceGroups()
	// ACP edit approval policies in mode order
	EditApprovalPolicies = []string{"ask", "workspace_session", "session"}
)

func surfaceGroups() map[string]map[string]bool {
	groups := map[string]map[string]bool{}
	for label := range storedAsLocal {
		group := map[string]bool{label: true, Surfaces[label]: true}
		for name := range group {
			groups[name] = group
		}
	}
	return groups
}

// Whether a launch field is the session choice by its last committed session edit, else launched
func sessionPinned(config map[string]any, field string, launched bool) bool {
	if pins, ok := config[SessionPins].(map[string]any); ok {
		if pinned, ok := pins[field].(bool); ok {
			return pinned
		}
	}
	return launched
}

// Writes an explicit reasoning level over any per model default, recording the pin for a session edit
func pinReasoningEffort(config map[string]any, effort any, session bool) error {
	agent, err := setdefaultMap(config, "agent")
	if err != nil {
		return err
	}
	agent["reasoning_effort"] = effort
	delete(agent, "reasoning_overrides")
	if session {
		return mergeSessionPin(config, "reasoning", true)
	}
	return nil
}

// Nested mapping at key, created empty when absent, a TypeError when the present value is no mapping
func setdefaultMap(config map[string]any, key string) (map[string]any, error) {
	value, present := config[key]
	if !present {
		created := map[string]any{}
		config[key] = created
		return created, nil
	}
	if m, ok := value.(map[string]any); ok {
		return m, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(value) + "' object does not support item assignment"}
}

// Replaces the session pins mapping with a copy carrying field set to pinned
func mergeSessionPin(config map[string]any, field string, pinned bool) error {
	merged := map[string]any{}
	if existing := config[SessionPins]; pyrepr.Truthy(existing) {
		m, ok := existing.(map[string]any)
		if !ok {
			return &PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(existing) + "' object is not a mapping"}
		}
		for k, v := range m {
			merged[k] = v
		}
	}
	merged[field] = pinned
	config[SessionPins] = merged
	return nil
}

// Refusal naming the launch field a create check read and why it failed
func refusePolicy(field string, detail *string) error {
	return state.NewRuntimeStoreError("invalid_params", &field, detail)
}

// Model and provider a profile config selects with auto as no provider
func profileTarget(config map[string]any) (string, string) {
	switch model := config["model"].(type) {
	case map[string]any:
		providerName := strings.TrimSpace(routeStr(model["provider"]))
		if strings.ToLower(providerName) == "auto" {
			providerName = ""
		}
		return strings.TrimSpace(routeStr(model["default"])), providerName
	case string:
		return strings.TrimSpace(model), ""
	}
	return "", ""
}

// Provider and normalized endpoint identity of a model block
func modelRoute(block any) (string, string) {
	m, _ := block.(map[string]any)
	return strings.ToLower(strings.TrimSpace(routeStr(m["provider"]))), provider.NormalizeRouteBaseURL(routeStr(m["base_url"]))
}

// Whether a block resolved in config is the route bound was bound to in its own config
func boundRouteHolds(ctx context.Context, bound any, boundConfig any, block any, config any) (bool, error) {
	boundMap, ok := bound.(map[string]any)
	if !ok {
		boundMap = map[string]any{}
	}
	identity, url, err := ResolvedRoute(ctx, boundMap, boundConfig)
	if err != nil {
		return false, err
	}
	blockID, _, err := ResolvedRoute(ctx, block, config)
	if err != nil {
		return false, err
	}
	if identity == "auto" || blockID == "auto" {
		bp, bu := modelRoute(boundMap)
		kp, ku := modelRoute(block)
		return bp == kp && bu == ku, nil
	}
	compared := boundMap
	if url != "" {
		compared = make(map[string]any, len(boundMap)+1)
		for k, v := range boundMap {
			compared[k] = v
		}
		compared["base_url"] = url
	}
	return SameRoute(ctx, compared, block, config)
}

// False when a block pins a base url its credential entry no longer serves in config
func entryServes(ctx context.Context, block any, config map[string]any) bool {
	m, _ := block.(map[string]any)
	url := provider.NormalizeRouteBaseURL(routeStr(m["base_url"]))
	if url == "" {
		return true
	}
	providerName := strings.TrimSpace(routeStr(m["provider"]))
	var entries []any
	if providerName != "" {
		entries = append(entries, routeNamedEntry(ctx, providerName, config))
	}
	if source, ok := m["api_key_from"].([]any); ok && len(source) > 1 {
		var alias any = config
		for _, part := range source[:len(source)-1] {
			if am, ok := alias.(map[string]any); ok {
				key, _ := part.(string)
				alias = am[key]
			} else {
				alias = nil
			}
		}
		entries = append(entries, alias)
	}
	for _, entry := range entries {
		em, ok := entry.(map[string]any)
		if !ok || strings.TrimSpace(routeStr(em["base_url"])) == "" {
			continue
		}
		if provider.NormalizeRouteBaseURL(routeStr(em["base_url"])) != url {
			return false
		}
	}
	return true
}

// Held pick keeps its selection keys over the profile block on the same endpoint, else stays frozen
func heldPick(frozen, live map[string]any) map[string]any {
	fp, fu := modelRoute(frozen)
	lp, lu := modelRoute(live)
	if fp != lp || fu != lu {
		return frozen
	}
	out := map[string]any{}
	for k, v := range live {
		if !containsString(SelectionKeys, k) {
			out[k] = v
		}
	}
	for _, k := range SelectionKeys {
		if v, ok := frozen[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Config without null sections and with a scalar model id read as the model default
func presentSections(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		if v != nil {
			out[k] = v
		}
	}
	if model, ok := out["model"].(string); ok {
		if strings.TrimSpace(model) != "" {
			out["model"] = map[string]any{"default": model}
		} else {
			out["model"] = map[string]any{}
		}
	}
	return out
}

// Whether the terminal works in the session host cwd, local or docker mounting it at the workspace
func terminalUsesHostCwd(terminal map[string]any) bool {
	backend := strings.ToLower(strings.TrimSpace(routeStr(terminal["TERMINAL_ENV"])))
	if backend == "" {
		backend = "local"
	}
	switch strings.ToLower(strings.TrimSpace(routeStr(terminal["TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE"]))) {
	case "1", "true", "yes", "on":
		return backend == "local" || backend == "docker"
	}
	return backend == "local"
}

// Launch params without an auto provider, the thin client sentinel for no pick
func withoutAutoPick(params map[string]any) map[string]any {
	if p, ok := params["provider"].(string); ok && strings.ToLower(strings.TrimSpace(p)) == "auto" {
		out := make(map[string]any, len(params))
		for k, v := range params {
			if k != "provider" {
				out[k] = v
			}
		}
		return out
	}
	return params
}

// Whether a provider pick resolves to the profile own route so its endpoint and key pins stay
func keepsProfileRoute(ctx context.Context, picked any, config map[string]any) (bool, error) {
	heldID, err := ProfileRouteIdentity(ctx, config)
	if err != nil {
		return false, err
	}
	name, ok := picked.(string)
	if heldID == "auto" || !ok || strings.TrimSpace(name) == "" {
		return false, nil
	}
	identity, _, err := ResolvedRoute(ctx, map[string]any{"provider": name}, config)
	return identity == heldID, err
}

// Frozen launch policy of one local session as its committed private JSON fields
type LocalSessionPolicy struct {
	Source             string
	Platform           string
	Cwd                string
	Model              *string
	Toolsets           []string
	ConfigJSON         string
	RequestJSON        string
	TerminalJSON       string
	CredentialRef      *string
	ConfigSecretRef    *string
	EditorMCPJSON      *string
	KanbanJSON         *string
	SafeMode           bool
	IgnoreUserConfig   bool
	SkillsPrompt       *string
	CodingPosture      bool
	ClientEnvJSON      *string
	ClientEnvRemote    bool
	ClientEnvRulesJSON *string
	EditApprovalPolicy string
}

// Installs the launch policy source read the tool and skill prompt one shot checks make
func init() { tools.ActivePolicySource = activePolicySource }

// Source of the launch policy bound to ctx, false when the turn runs under none
func activePolicySource(ctx context.Context) (string, bool) {
	policy, ok := ActivePolicy(ctx).(*LocalSessionPolicy)
	if !ok || policy == nil {
		return "", false
	}
	return policy.Source, true
}

// Platform label the policy turn runs on
func (p *LocalSessionPolicy) PolicyPlatform() string { return p.Platform }

// Host cwd the policy turn runs in
func (p *LocalSessionPolicy) PolicyCwd() string { return p.Cwd }

// Decoded JSON object of a policy field, empty when the text holds no object
func policyObject(text string) map[string]any {
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return map[string]any{}
	}
	m, _ := pyjson.Plain(parsed).(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Shell launched client environment bound for this session, nil when the client sent none
func (p *LocalSessionPolicy) ClientEnv() map[string]any {
	if p.ClientEnvJSON == nil {
		return nil
	}
	return policyObject(*p.ClientEnvJSON)
}

// Whether the session re-reads its owning profile config each turn
func (p *LocalSessionPolicy) FollowsProfile() bool {
	return policyObject(p.RequestJSON)["follow_profile_config"] == true || policyObject(p.ConfigJSON)[FollowAdopted] == true
}

// Whether project rule files are skipped, always under safe mode
func (p *LocalSessionPolicy) IgnoreRules() bool {
	return p.SafeMode || pyrepr.Truthy(policyObject(p.RequestJSON)["ignore_rules"])
}

// Whether this session bypasses dangerous command approvals like a chat scoped yolo
func (p *LocalSessionPolicy) Yolo() bool { return pyrepr.Truthy(policyObject(p.RequestJSON)["yolo"]) }

// Whether the session id rides the agent system prompt
func (p *LocalSessionPolicy) PassSessionID() bool {
	return pyrepr.Truthy(policyObject(p.RequestJSON)["pass_session_id"])
}

// Whether the session agent runs with quiet mode off so status lines reach the viewer
func (p *LocalSessionPolicy) Verbose() bool {
	return pyrepr.Truthy(policyObject(p.RequestJSON)["verbose"])
}

// Whether a stored client map holds string values, or string lists for its rules, nil counting as valid
func validClientEnv(value any, rules bool) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	if !ok {
		return false
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return false
	}
	env, ok := pyjson.Plain(parsed).(map[string]any)
	if !ok {
		return false
	}
	for _, v := range env {
		if !rules {
			if _, ok := v.(string); !ok {
				return false
			}
			continue
		}
		items, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
	}
	return true
}

// In memory slot of a durable credential ref, the part before the fingerprint
func keySlot(credentialRef string) string {
	return strings.SplitN(credentialRef, "#", 2)[0]
}

// Launch key the policy was bound with, nil when it has none and refused once the owner lost it
func launchKey(owner launchSecretOwner, policy *LocalSessionPolicy) (*string, error) {
	if policy.CredentialRef == nil {
		return nil, nil
	}
	var value string
	held := false
	if slots := owner.launchSecretSlots(); slots != nil {
		slots.mu.Lock()
		value, held = slots.keys[keySlot(*policy.CredentialRef)]
		slots.mu.Unlock()
	}
	if !held {
		return nil, state.NewRuntimeStoreError("launch_credentials_unavailable", nil, nil)
	}
	return &value, nil
}

// Launch key a turn on the config sends, nil once the followed route left the endpoint the key was bound with
func routeLaunchKey(ctx context.Context, owner launchSecretOwner, policy *LocalSessionPolicy, config map[string]any) (*string, error) {
	bound := presentSections(policyObject(policy.ConfigJSON))
	holds, err := boundRouteHolds(ctx, bound["model"], bound, config["model"], config)
	if err != nil || !holds {
		return nil, err
	}
	return launchKey(owner, policy)
}

// Optional text field of a stored policy, false for any other type
func restoredText(data map[string]any, name string) (*string, bool) {
	value, present := data[name]
	if !present || value == nil {
		return nil, true
	}
	text, ok := value.(string)
	return &text, ok
}

// Stored private policy restored and validated, refusing an incomplete one rather than rebuilding defaults
func restorePolicy(raw any) (*LocalSessionPolicy, error) {
	policy, cause := restoredPolicy(pyjson.Plain(raw))
	if cause != nil {
		refusal := state.NewRuntimeStoreError("storage_unavailable", nil, nil)
		refusal.Cause = cause
		return nil, refusal
	}
	return policy, nil
}

// Policy built from its stored fields, the error of the dataclass constructor or restore check that refused it otherwise
func restoredPolicy(raw any) (*LocalSessionPolicy, error) {
	data, ok := raw.(map[string]any)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "LocalSessionPolicy() argument after ** must be a mapping, not " + pyTypeName(raw)}
	}
	known := map[string]bool{}
	for name := range (&LocalSessionPolicy{}).asDict() {
		known[name] = true
	}
	for name := range data {
		if !known[name] {
			return nil, &PyError{Kind: "TypeError", Msg: "LocalSessionPolicy.__init__() got an unexpected keyword argument '" + name + "'"}
		}
	}
	for _, name := range []string{"source", "platform", "cwd", "model", "toolsets", "config_json", "request_json", "terminal_json"} {
		if _, present := data[name]; !present {
			return nil, &PyError{Kind: "TypeError", Msg: "LocalSessionPolicy.__init__() missing 1 required positional argument: '" + name + "'"}
		}
	}
	p := &LocalSessionPolicy{EditApprovalPolicy: "ask"}
	var fine bool
	if p.Source, fine = data["source"].(string); !fine {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	if p.Platform, fine = data["platform"].(string); !fine {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	if p.Cwd, fine = data["cwd"].(string); !fine || !filepath.IsAbs(p.Cwd) {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	model, fine := data["model"].(string)
	if !fine || pystr.Strip(model) == "" {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	p.Model = &model
	toolsets, fine := data["toolsets"].([]any)
	if !fine {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	p.Toolsets = make([]string, 0, len(toolsets))
	for _, name := range toolsets {
		text, isStr := name.(string)
		if !isStr {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
		}
		p.Toolsets = append(p.Toolsets, text)
	}
	for _, field := range []struct {
		name string
		dst  *string
	}{{"config_json", &p.ConfigJSON}, {"request_json", &p.RequestJSON}, {"terminal_json", &p.TerminalJSON}} {
		text, isStr := data[field.name].(string)
		if !isStr {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
		}
		*field.dst = text
	}
	for _, field := range []struct {
		name string
		dst  **string
	}{{"credential_ref", &p.CredentialRef}, {"config_secret_ref", &p.ConfigSecretRef}, {"editor_mcp_json", &p.EditorMCPJSON},
		{"kanban_json", &p.KanbanJSON}, {"skills_prompt", &p.SkillsPrompt}, {"client_env_json", &p.ClientEnvJSON},
		{"client_env_rules_json", &p.ClientEnvRulesJSON}} {
		if *field.dst, fine = restoredText(data, field.name); !fine {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
		}
	}
	for _, field := range []struct {
		name string
		dst  *bool
	}{{"safe_mode", &p.SafeMode}, {"ignore_user_config", &p.IgnoreUserConfig}, {"coding_posture", &p.CodingPosture},
		{"client_env_remote", &p.ClientEnvRemote}} {
		if value, present := data[field.name]; present {
			if *field.dst, fine = value.(bool); !fine {
				return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
			}
		}
	}
	if value, present := data["edit_approval_policy"]; present {
		if p.EditApprovalPolicy, fine = value.(string); !fine {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
		}
	}
	surfaces := map[string]string{"cron": "cron", "kanban": "cli", "bot_room": "bot_room"}
	for label, surface := range Surfaces {
		surfaces[label] = surface
	}
	if surface, known := surfaces[p.Source]; !isForwardPolicy(p) && (!known || p.Platform != surface) {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	approval := false
	for _, name := range EditApprovalPolicies {
		approval = approval || name == p.EditApprovalPolicy
	}
	if !validClientEnv(optValue(p.ClientEnvJSON), false) || !validClientEnv(optValue(p.ClientEnvRulesJSON), true) || !approval ||
		(p.EditApprovalPolicy != "ask" && p.Source != "acp") || (p.SafeMode && !p.IgnoreUserConfig) {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid policy"}
	}
	for _, text := range []string{p.ConfigJSON, p.RequestJSON, p.TerminalJSON} {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return nil, err
		}
		if _, isMap := pyjson.Plain(parsed).(map[string]any); !isMap {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid policy object"}
		}
	}
	terminal := policyObject(p.TerminalJSON)
	if cwd, set := terminal["TERMINAL_CWD"]; terminalUsesHostCwd(terminal) && (!set || cwd != any(p.Cwd)) {
		return nil, &PyError{Kind: "ValueError", Msg: "terminal policy mismatch"}
	}
	return p, nil
}

// Owning profile's current config.yaml present sections, read fresh
func profileConfig(ctx context.Context, owner launchSecretOwner) (map[string]any, error) {
	home, err := launchOwnerHome(owner)
	if err != nil {
		return nil, err
	}
	return presentSections(loadGatewayConfig(ctx, filepath.Join(home, "config.yaml"))), nil
}

// Copies a key from the live section or drops it when the live section lacks it
func followKey(target, live map[string]any, key string) {
	if value, ok := live[key]; ok {
		target[key] = value
	} else {
		delete(target, key)
	}
}

// Reasoning level back to the profile's own and the session reasoning pin cleared
func unpinReasoning(config, profile map[string]any) error {
	followed, _ := profile["agent"].(map[string]any)
	agent, err := setDefaultMapping(config, "agent")
	if err != nil {
		return err
	}
	for _, key := range []string{"reasoning_effort", "reasoning_overrides"} {
		followKey(agent, followed, key)
	}
	pins := map[string]any{}
	switch held := config[SessionPins].(type) {
	case map[string]any:
		for k, v := range held {
			pins[k] = v
		}
	case nil:
	default:
		if pyrepr.Truthy(held) {
			return &PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(held) + "' object is not a mapping"}
		}
	}
	pins["reasoning"] = false
	config[SessionPins] = pins
	return nil
}

// Dict at a key, inserted empty when absent, a TypeError style error when the key holds another type
func setDefaultMapping(config map[string]any, key string) (map[string]any, error) {
	switch held := config[key].(type) {
	case map[string]any:
		return held, nil
	case nil:
		if _, present := config[key]; !present {
			fresh := map[string]any{}
			config[key] = fresh
			return fresh, nil
		}
	}
	return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(config[key]) + "' object has no attribute 'setdefault'"}
}

// One profile read a policy turn scope holds for its own policy
type turnProfileKey struct{}

type turnProfile struct {
	policy  LocalSessionPolicy
	profile map[string]any
}

// Context carrying the turn's single profile read for the policy
func withTurnProfile(ctx context.Context, policy *LocalSessionPolicy, profile map[string]any) context.Context {
	return context.WithValue(ctx, turnProfileKey{}, &turnProfile{policy: *policy, profile: profile})
}

// Owning profile's current config for a following session, the turn scope read inside its own turn
func (p *LocalSessionPolicy) readProfile(ctx context.Context, owner launchSecretOwner) (map[string]any, error) {
	if owner == nil || !p.FollowsProfile() {
		return nil, nil
	}
	if held, ok := ctx.Value(turnProfileKey{}).(*turnProfile); ok && reflect.DeepEqual(held.policy, *p) {
		return held.profile, nil
	}
	return profileConfig(ctx, owner)
}

// Writes a recovered secret into the config tree, inserting dicts an alias added after creation lacks
func setSecretAt(config map[string]any, path secretPath, value any) error {
	var target any = config
	step := func(key any, last bool) error {
		switch node := target.(type) {
		case map[string]any:
			name, ok := key.(string)
			if !ok {
				return &PyError{Kind: "TypeError", Msg: "config keys are strings"}
			}
			if last {
				node[name] = value
				return nil
			}
			next, present := node[name]
			if !present {
				next = map[string]any{}
				node[name] = next
			}
			target = next
		case []any:
			i, ok := pathIndex(key)
			if !ok {
				return &PyError{Kind: "TypeError", Msg: "list indices must be integers or slices, not " + pyrepr.TypeName(key)}
			}
			if i < 0 {
				i += len(node)
			}
			if i < 0 || i >= len(node) {
				return &PyError{Kind: "IndexError", Msg: "list index out of range"}
			}
			if last {
				node[i] = value
				return nil
			}
			target = node[i]
		default:
			return &PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(target) + "' object is not subscriptable"}
		}
		return nil
	}
	for i, key := range path {
		if err := step(key, i == len(path)-1); err != nil {
			return err
		}
	}
	return nil
}

// Next turn's config and model from one read of the owning profile, explicit picks applied over followed defaults
func (p *LocalSessionPolicy) effective(ctx context.Context, owner launchSecretOwner, secrets bool) (map[string]any, *string, error) {
	config := presentSections(policyObject(p.ConfigJSON))
	live, err := p.readProfile(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	if p.ConfigSecretRef != nil && owner != nil && secrets {
		recovered, err := recoverConfigSecrets(ctx, owner, p)
		if err != nil {
			return nil, nil, err
		}
		for i, path := range recovered.paths {
			if path[0] == nil {
				continue
			}
			if err := setSecretAt(config, path, recovered.values[i]); err != nil {
				return nil, nil, err
			}
		}
		disableDroppedSecrets(ctx, config, recovered.dropped, recovered.session)
	}
	model := p.Model
	if len(live) > 0 {
		var frozen map[string]any
		switch block := config["model"].(type) {
		case map[string]any:
			frozen = block
		default:
			if pyrepr.Truthy(block) {
				return nil, nil, &PyError{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(block) + "' object has no attribute 'get'"}
			}
			frozen = map[string]any{}
		}
		request := policyObject(p.RequestJSON)
		liveModel, liveProvider := profileTarget(live)
		pick, picked := frozen["selection_profile"].([]any)
		held := picked && len(pick) == 2 && pick[0] == any(liveModel) && pick[1] == any(liveProvider) && entryServes(ctx, frozen, live)
		pinned := map[string]bool{}
		if _, ok := request["toolsets"]; ok {
			pinned["platform_toolsets"], pinned["disabled_toolsets"] = true, true
		}
		_, launchedReasoning := request["reasoning"]
		if sessionPinned(config, "reasoning", launchedReasoning) {
			pinned["reasoning_effort"], pinned["reasoning_overrides"] = true, true
		}
		if _, ok := request["service_tier"]; ok {
			pinned["service_tier"] = true
		}
		for _, key := range FollowSections {
			if !pinned[key] {
				followKey(config, live, key)
			}
		}
		if held {
			current, _ := config["model"].(map[string]any)
			if current == nil {
				current = map[string]any{}
			}
			config["model"] = heldPick(frozen, current)
		} else if resolved := resolveGatewayModel(ctx, live); resolved != "" {
			model = &resolved
		}
		agent, err := setDefaultMapping(config, "agent")
		if err != nil {
			return nil, nil, err
		}
		liveAgent, _ := live["agent"].(map[string]any)
		for _, key := range FollowAgentKeys {
			if !pinned[key] {
				followKey(agent, liveAgent, key)
			}
		}
	}
	if block, ok := config["model"].(map[string]any); ok {
		if source, isList := block["api_key_from"].([]any); isList && len(source) > 0 {
			var value any = config
			for _, part := range source {
				m, isMap := value.(map[string]any)
				name, isStr := part.(string)
				if !isMap || !isStr {
					value = nil
					continue
				}
				value = m[name]
			}
			block["api_key"] = value
		}
	}
	return config, model, nil
}

// Next turn's effective config alone
func (p *LocalSessionPolicy) config(ctx context.Context, owner launchSecretOwner, secrets bool) (map[string]any, error) {
	config, _, err := p.effective(ctx, owner, secrets)
	return config, err
}

// Model the session's next turn runs, the profile's current default when following
func (p *LocalSessionPolicy) currentModel(ctx context.Context, owner launchSecretOwner) (*string, error) {
	_, model, err := p.effective(ctx, owner, false)
	return model, err
}

// Frozen config with no owner, so no secret is recovered and no profile is followed
func (p *LocalSessionPolicy) frozenConfig() map[string]any {
	config, _, _ := p.effective(context.Background(), nil, true)
	return config
}

// Frozen model section field, nil when the section or field is absent
func (p *LocalSessionPolicy) modelField(key string) any {
	block, _ := p.frozenConfig()["model"].(map[string]any)
	return block[key]
}

// Frozen model provider
func (p *LocalSessionPolicy) Provider() any { return p.modelField("provider") }

// Frozen model base URL
func (p *LocalSessionPolicy) BaseURL() any { return p.modelField("base_url") }

// Frozen checkpoints enabled flag accepting the legacy bool form
func (p *LocalSessionPolicy) CheckpointsEnabled() bool {
	section, present := p.frozenConfig()["checkpoints"]
	if !present {
		return false
	}
	if m, ok := section.(map[string]any); ok {
		return pyrepr.Truthy(m["enabled"])
	}
	return pyrepr.Truthy(section)
}

// Frozen iteration cap from agent max_turns, else the top level max_turns
func (p *LocalSessionPolicy) MaxTurns() int {
	cfg := p.frozenConfig()
	agent, _ := cfg["agent"].(map[string]any)
	raw, ok := agent["max_turns"]
	if !ok {
		raw = cfg["max_turns"]
	}
	return config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
}

// Frozen reasoning config for the frozen model
func (p *LocalSessionPolicy) ReasoningConfig() map[string]any {
	model := ""
	if p.Model != nil {
		model = *p.Model
	}
	return config.ResolveReasoningConfig(p.frozenConfig(), model)
}

// Context binding the session's client environment under the rules it was normalised by
func (p *LocalSessionPolicy) ClientEnvScope(ctx context.Context) (context.Context, error) {
	var rules *state.ClientEnvRules
	if p.ClientEnvRulesJSON != nil {
		parsed, err := pyjson.Loads(*p.ClientEnvRulesJSON)
		if err != nil {
			return nil, err
		}
		if rules, err = state.ParseClientEnvRules(parsed); err != nil {
			return nil, err
		}
	}
	var env map[string]string
	if raw := p.ClientEnv(); raw != nil {
		env = make(map[string]string, len(raw))
		for k, v := range raw {
			env[k], _ = v.(string)
		}
	}
	return state.ClientEnvScope(ctx, env, !p.ClientEnvRemote, rules), nil
}

// Config keys whose whole value is a private container of secrets
var secretContainers = map[string]bool{"env": true, "headers": true, "extra_headers": true, "docker_env": true,
	"docker_extra_args": true, "terminal_docker_env": true, "terminal_docker_extra_args": true}

// Moves every secret value of a config tree into private and leaves None in its place, sorted key order
func extractConfigSecrets(value any, private *RecoveredSecrets, path secretPath) error {
	visit := func(key, child any, clear func()) error {
		childPath := append(append(secretPath{}, path...), key)
		name, isStr := key.(string)
		sensitive := isStr && (config.SecretConfigKeys[strings.ToLower(name)] || secretContainers[strings.ToLower(name)] ||
			provider.IsSecretPayloadKey(name))
		if text, ok := child.(string); sensitive && pyrepr.Truthy(child) && !(ok && (text == "{}" || text == "[]")) {
			if private == nil {
				return state.NewRuntimeStoreError("launch_credentials_unavailable", nil, nil)
			}
			private.Set(childPath, child)
			clear()
			return nil
		}
		return extractConfigSecrets(child, private, childPath)
	}
	switch node := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := visit(key, node[key], func() { node[key] = nil }); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range node {
			if err := visit(i, child, func() { node[i] = nil }); err != nil {
				return err
			}
		}
	}
	return nil
}

// Whether the workspace directory exists, a removed one keeps the chat readable but runs no turn
func workspaceReady(policy *LocalSessionPolicy) bool {
	info, err := os.Stat(policy.Cwd)
	return err == nil && info.IsDir()
}

// Context holding one read of a following session's profile for every resolution inside it
func profileReadScope(ctx context.Context, policy *LocalSessionPolicy, owner launchSecretOwner) (context.Context, error) {
	profile, err := policy.readProfile(ctx, owner)
	if err != nil {
		return nil, err
	}
	return withTurnProfile(ctx, policy, profile), nil
}
