package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Owner of session local-abc whose model mutates rewrite the route the connection's state holds, every turn answering ok
type scopeOwner struct {
	client   *scriptedClient
	conn     *gatewaycli.GatewayConnection
	guard    bool
	onSubmit func(params map[string]any) (any, bool, error)
}

// Route the session runs as the connection's state holds it
func (o *scopeOwner) model() any {
	state := o.conn.State(scriptSID)
	state.Lock()
	defer state.Unlock()
	return state.Info["model"]
}

// Model mutates and one turn overrides the CLI asked the owner for
func (o *scopeOwner) routes() ([]any, []any) {
	var mutates, overrides []any
	for _, params := range o.client.callsOf("session.mutate") {
		if params["operation"] == "model" {
			mutates = append(mutates, params["payload"])
		}
	}
	for _, params := range o.client.callsOf("prompt.submit") {
		if override, present := params["model_override"]; present {
			overrides = append(overrides, override)
		}
	}
	return mutates, overrides
}

// Every prompt.submit the CLI sent
func (o *scopeOwner) submits() []map[string]any { return o.client.callsOf("prompt.submit") }

const scopeConfirmMessage = "Large context: this switch re-reads the whole session."

// Recording owner over the scripted gateway, the selection guard asking once for a confirm when guarded
func newScopeOwner(t *testing.T, info map[string]any, guarded bool) (*scopeOwner, *gatewaycli.GatewayConnection) {
	t.Helper()
	o := &scopeOwner{guard: guarded}
	o.client = &scriptedClient{script: []any{scriptFrame("message.complete", map[string]any{"text": "ok", "status": "complete"})},
		result: map[string]any{"final_response": "ok"}}
	o.client.override = func(method string, params map[string]any) (any, bool, error) {
		switch method {
		case "prompt.submit":
			if o.onSubmit != nil {
				return o.onSubmit(params)
			}
		case "session.resume":
			o.client.record(method, params)
			state := o.conn.State(scriptSID)
			state.Lock()
			snapshot := map[string]any{"revision": 7, "execution_generation": 7, "info": copyMap(state.Info), "messages": []any{},
				"stored_session_id": scriptSID, "session_id": scriptSID, "prompts": []any{}, "pending": []any{}}
			state.Unlock()
			return snapshot, true, nil
		case "session.mutate":
			if params["operation"] != "model" {
				return nil, false, nil
			}
			o.client.record(method, params)
			payload, _ := params["payload"].(map[string]any)
			if o.guard && payload["confirm"] != "tok" {
				return map[string]any{"status": "confirmation_required", "confirm_required": true, "confirm": "tok",
					"confirm_message": scopeConfirmMessage}, true, nil
			}
			state := o.conn.State(scriptSID)
			state.Lock()
			state.Info["model"] = payload["model"]
			if p, _ := payload["provider"].(string); p != "" {
				state.Info["provider"] = p
			}
			state.Unlock()
			return map[string]any{}, true, nil
		}
		return nil, false, nil
	}
	o.conn = scriptedConnection(t, o.client)
	state := o.conn.State(scriptSID)
	state.Lock()
	state.Info = copyMap(info)
	state.Unlock()
	return o, o.conn
}

// Makes the model resolver pick the given route
func scopePick(t *testing.T, model, prov, baseURL, apiMode string) {
	t.Helper()
	prev := switchModelResolve
	t.Cleanup(func() { switchModelResolve = prev })
	switchModelResolve = func(context.Context, provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return provider.ModelSwitchResult{Success: true, NewModel: model, TargetProvider: prov, APIKey: "sk-" + prov,
			BaseURL: baseURL, APIMode: apiMode, ProviderLabel: strings.ToUpper(prov[:1]) + prov[1:]}, nil
	}
}

// CLI on the owner's session with the real chat path, the agent on old/model through openrouter
func scopeCLI(t *testing.T, conn *gatewaycli.GatewayConnection) (*CLI, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	swap(t, &ensureTurnCredentials, func(*CLI) bool { return true })
	c := configuredCLI(t, map[string]any{}, CLIOptions{})
	c.model, c.provider, c.requestedProvider, c.apiKey = "old/model", "openrouter", "openrouter", "sk-old"
	c.baseURL, c.apiMode = "https://openrouter.ai/api/v1", "chat_completions"
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = scriptSID, gatewaycli.NewGatewaySessionStore(conn), true
	c.setAgent(scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "old/model", Provider: "openrouter"}))
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	scopePick(t, "claude-sonnet-4.6", "anthropic", "https://api.anthropic.com", "anthropic_messages")
	return c, rec
}

// Printed lines that report a failure
func failedLines(rec *printRecorder) []string {
	var failed []string
	for _, line := range rec.plain() {
		if strings.Contains(strings.ToLower(line), "fail") {
			failed = append(failed, line)
		}
	}
	return failed
}

func TestModelOnceRidesOneTurnAndNeverTouchesTheSession(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, rec := scopeCLI(t, conn)
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once")
	if failed := failedLines(rec); len(failed) > 0 {
		t.Fatalf("printed %v", failed)
	}
	if reply, _ := c.chat("the once turn", nil, false); reply != "ok" {
		t.Fatalf("once turn %q", reply)
	}
	if reply, _ := c.chat("the turn after", nil, false); reply != "ok" {
		t.Fatalf("turn after %q printed %q", reply, rec.plain())
	}
	if mutates, _ := owner.routes(); len(mutates) > 0 {
		t.Fatalf("--once wrote the shared session model %v", mutates)
	}
	submits := owner.submits()
	if len(submits) != 2 {
		t.Fatalf("submits %v", submits)
	}
	if want := map[string]any{"model": "claude-sonnet-4.6", "provider": "anthropic"}; !reflect.DeepEqual(submits[0]["model_override"], want) {
		t.Fatalf("once submit %v", submits[0])
	}
	if _, present := submits[1]["model_override"]; present {
		t.Fatalf("after submit %v", submits[1])
	}
	if owner.model() != "old/model" || c.model != "old/model" || c.agentRef().Model() != "old/model" {
		t.Fatalf("owner %v cli %q agent %q", owner.model(), c.model, c.agentRef().Model())
	}
}

// Session model mutate params the CLI sent, oldest first
func (o *scopeOwner) modelMutates() []map[string]any {
	var out []map[string]any
	for _, params := range o.client.callsOf("session.mutate") {
		if params["operation"] == "model" {
			out = append(out, params)
		}
	}
	return out
}

func TestASecondOnceBeforeTheTurnRestoresTheRouteBeforeTheFirst(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, _ := scopeCLI(t, conn)
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once")
	scopePick(t, "gpt-5.4", "openai", "https://api.openai.com/v1", "codex_responses")
	c.handleModelSwitch("/model gpt-5.4 --provider openai --once")
	c.chat("the once turn", nil, false)
	c.chat("the turn after", nil, false)
	submits := owner.submits()
	if len(submits) != 2 {
		t.Fatalf("submits %v", submits)
	}
	if want := map[string]any{"model": "gpt-5.4", "provider": "openai"}; !reflect.DeepEqual(submits[0]["model_override"], want) {
		t.Fatalf("once submit %v", submits[0])
	}
	if _, present := submits[1]["model_override"]; present {
		t.Fatalf("after submit %v", submits[1])
	}
	if mutates, _ := owner.routes(); len(mutates) > 0 {
		t.Fatalf("mutates %v", mutates)
	}
	if c.model != "old/model" || c.provider != "openrouter" || c.baseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("route %q %q %q", c.model, c.provider, c.baseURL)
	}
}

func TestSessionSwitchOfTheOneTurnTargetCommitsOnTheFence(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, rec := scopeCLI(t, conn)
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once")
	if mutates, _ := owner.routes(); len(mutates) > 0 {
		t.Fatalf("the one turn pick wrote the session %v", mutates)
	}
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic")
	if failed := failedLines(rec); len(failed) > 0 {
		t.Fatalf("printed %v", failed)
	}
	mutates := owner.modelMutates()
	if len(mutates) == 0 {
		t.Fatal("no session mutate")
	}
	last := mutates[len(mutates)-1]
	if payload, _ := last["payload"].(map[string]any); payload["model"] != "claude-sonnet-4.6" {
		t.Fatalf("mutates %v", mutates)
	}
	if _, fenced := last["expected_generation"]; !fenced {
		t.Fatalf("unfenced %v", last)
	}
	if owner.model() != "claude-sonnet-4.6" {
		t.Fatalf("owner model %v", owner.model())
	}
	c.chat("now the session route", nil, false)
	c.chat("and the next", nil, false)
	for _, submit := range owner.submits() {
		if _, present := submit["model_override"]; present {
			t.Fatalf("submits %v", owner.submits())
		}
	}
	if c.model != "claude-sonnet-4.6" || c.agentRef().Model() != "claude-sonnet-4.6" {
		t.Fatalf("cli %q agent %q", c.model, c.agentRef().Model())
	}
}

func TestSessionModelSwitchCommitsTheRouteOnTheOwner(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "guarded"}[guarded]
		t.Run(name, func(t *testing.T) {
			owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, guarded)
			c, rec := scopeCLI(t, conn)
			var asked []any
			c.agentRef().(*GatewayAgent).opts.ModelConfirmCallback = func(refusal map[string]any) bool {
				asked = append(asked, refusal["confirm_message"])
				return true
			}
			c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic")
			if failed := failedLines(rec); len(failed) > 0 {
				t.Fatalf("printed %v", failed)
			}
			wantAsked := []any(nil)
			if guarded {
				wantAsked = []any{scopeConfirmMessage}
			}
			if !reflect.DeepEqual(asked, wantAsked) {
				t.Fatalf("asked %v", asked)
			}
			runTurn(t, c.agentRef().(*GatewayAgent), "next turn")
			mutates := owner.modelMutates()
			want := map[string]any{"model": "claude-sonnet-4.6", "provider": "anthropic"}
			if guarded {
				want["confirm"] = "tok"
			}
			if len(mutates) == 0 || !reflect.DeepEqual(mutates[len(mutates)-1]["payload"], want) {
				t.Fatalf("mutates %v", mutates)
			}
			for _, m := range mutates {
				if _, fenced := m["expected_generation"]; !fenced {
					t.Fatalf("unfenced %v", mutates)
				}
			}
			if owner.model() != "claude-sonnet-4.6" {
				t.Fatalf("owner model %v", owner.model())
			}
			submits := owner.submits()
			if _, present := submits[len(submits)-1]["model_override"]; present {
				t.Fatal("the session already runs it")
			}
		})
	}
}

func TestSwitchingToTheRouteTheSessionRunsCommitsNothing(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, _ := scopeCLI(t, conn)
	scopePick(t, "old/model", "openrouter", "https://openrouter.ai/api/v1", "chat_completions")
	c.handleModelSwitch("/model old/model --provider openrouter")
	runTurn(t, c.agentRef().(*GatewayAgent), "next turn")
	if mutates, overrides := owner.routes(); len(mutates) > 0 || len(overrides) > 0 {
		t.Fatalf("mutates %v overrides %v", mutates, overrides)
	}
}

// Reasoning mutate payloads the CLI sent
func (o *scopeOwner) reasoningMutates() []any {
	var out []any
	for _, params := range o.client.callsOf("session.mutate") {
		if params["operation"] == "reasoning" {
			out = append(out, params["payload"])
		}
	}
	return out
}

func TestOnceReasoningRidesTheAdmissionAndNeverMutatesTheSession(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, rec := scopeCLI(t, conn)
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once --reasoning high")
	if failed := failedLines(rec); len(failed) > 0 {
		t.Fatalf("printed %v", failed)
	}
	if reply, _ := c.chat("the once turn", nil, false); reply != "ok" {
		t.Fatalf("once turn %q", reply)
	}
	if reply, _ := c.chat("the turn after", nil, false); reply != "ok" {
		t.Fatalf("turn after %q", reply)
	}
	if mutates := owner.reasoningMutates(); len(mutates) != 0 {
		t.Fatalf("reasoning mutates %v", mutates)
	}
	submits := owner.submits()
	if len(submits) != 2 {
		t.Fatalf("submits %v", submits)
	}
	want := map[string]any{"model": "claude-sonnet-4.6", "provider": "anthropic", "reasoning": "high"}
	if !reflect.DeepEqual(submits[0]["model_override"], want) {
		t.Fatalf("once submit %v", submits[0])
	}
	if _, present := submits[1]["model_override"]; present {
		t.Fatalf("after submit %v", submits[1])
	}
	if c.reasoningUserSet {
		t.Fatal("the once reasoning became a user set level")
	}
}

func TestOnceReasoningEqualToTheInheritedLevelCommitsNothing(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter", "reasoning_effort": "high"}, false)
	c, _ := scopeCLI(t, conn)
	high := map[string]any{"enabled": true, "effort": "high"}
	c.setAgent(scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "old/model", Provider: "openrouter", ReasoningConfig: high}))
	c.reasoningConfig, c.reasoningUserSet = high, false
	result := provider.ModelSwitchResult{Success: true, NewModel: "next/model", TargetProvider: "openrouter",
		APIKey: "key", BaseURL: c.baseURL, APIMode: "chat_completions"}
	if err := c.commitModelSwitch(&result, false, true, false, "high"); err != nil {
		t.Fatal(err)
	}
	c.restoreModelRuntimeSnapshot(c.pendingOneTurnModelRestore)
	if mutates := owner.reasoningMutates(); len(mutates) != 0 {
		t.Fatalf("reasoning mutates %v", mutates)
	}
}

// Session route of the alias cases on its own endpoint and key
func aliasSessionCLI(t *testing.T, sessionModel string) (*scopeOwner, *CLI, *printRecorder) {
	t.Helper()
	owner, conn := newScopeOwner(t, map[string]any{"model": sessionModel, "provider": "custom"}, false)
	c, rec := scopeCLI(t, conn)
	swap(t, &switchModelResolve, provider.SwitchModel)
	c.model, c.provider, c.requestedProvider = sessionModel, "custom", "custom"
	c.baseURL, c.explicitBaseURL = "http://127.0.0.1:7001/v1", "http://127.0.0.1:7001/v1"
	c.apiKey, c.explicitAPIKey = "sk-session", "sk-session"
	c.setAgent(scriptedAgentOn(t, conn, gatewayAgentOptions{Model: sessionModel, Provider: "custom"}))
	return owner, c, rec
}

// Loopback endpoint whose model listing serves shared-model, its base url
func sharedModelEndpoint(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"shared-model","object":"model"}]}`))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1"
}

// Sets direct alias entries for the case, restoring the table afterwards
func setDirectAliases(t *testing.T, entries ...provider.NamedDirectAlias) {
	t.Helper()
	saved := provider.DirectAliases.Entries()
	t.Cleanup(func() {
		provider.DirectAliases.Clear()
		for _, e := range saved {
			provider.DirectAliases.Set(e.Name, e.Alias)
		}
	})
	for _, e := range entries {
		provider.DirectAliases.Set(e.Name, e.Alias)
	}
}

// Endpoint and key the owner runs a one turn override on over the alias session
func aliasOneTurnEndpoint(t *testing.T, override any, sessionModel string) (any, any) {
	t.Helper()
	obj := pyjson.NewObject()
	for k, v := range override.(map[string]any) {
		obj.Set(k, v)
	}
	_, runtime, err := gatewayrun.OneTurnRuntime(context.Background(), obj, sessionModel, map[string]any{"provider": "custom",
		"api_key": "sk-session", "base_url": "http://127.0.0.1:7001/v1", "api_mode": "chat_completions"}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime["base_url"], runtime["api_key"]
}

func TestOnceSendsTheAliasItResolvedThrough(t *testing.T) {
	for _, sessionModel := range []string{"shared-model", "other-model"} {
		t.Run(sessionModel, func(t *testing.T) {
			owner, c, rec := aliasSessionCLI(t, sessionModel)
			firstURL, secondURL := sharedModelEndpoint(t), sharedModelEndpoint(t)
			setDirectAliases(t,
				provider.NamedDirectAlias{Name: "first", Alias: provider.DirectAlias{Model: "shared-model", Provider: "custom", BaseURL: firstURL, APIKey: "sk-first"}},
				provider.NamedDirectAlias{Name: "second", Alias: provider.DirectAlias{Model: "shared-model", Provider: "custom", BaseURL: secondURL, APIKey: "sk-second"}})
			c.handleModelSwitch("/model second --once")
			if failed := failedLines(rec); len(failed) > 0 {
				t.Fatalf("printed %v", failed)
			}
			if c.agentRef().Model() != "shared-model" {
				t.Fatalf("the agent shows %q, not the resolved model", c.agentRef().Model())
			}
			c.chat("run on second", nil, false)
			c.chat("the turn after", nil, false)
			submits := owner.submits()
			if len(submits) != 2 {
				t.Fatalf("submits %v", submits)
			}
			override := submits[0]["model_override"]
			if !reflect.DeepEqual(override, map[string]any{"model": "second", "provider": "custom"}) {
				t.Fatalf("override %v", override)
			}
			if _, present := submits[1]["model_override"]; present {
				t.Fatalf("after submit %v", submits[1])
			}
			if base, key := aliasOneTurnEndpoint(t, override, sessionModel); base != secondURL || key != "sk-second" {
				t.Fatalf("one turn runtime %v %v", base, key)
			}
			if mutates, _ := owner.routes(); len(mutates) > 0 {
				t.Fatalf("mutates %v", mutates)
			}
		})
	}
}

func TestOnceThroughAnAliasNamedLikeTheSessionModelStillSendsIt(t *testing.T) {
	owner, c, rec := aliasSessionCLI(t, "shared-model")
	aliasURL := sharedModelEndpoint(t)
	setDirectAliases(t, provider.NamedDirectAlias{Name: "shared-model", Alias: provider.DirectAlias{Model: "shared-model", Provider: "custom",
		BaseURL: aliasURL, APIKey: "sk-alias"}})
	c.handleModelSwitch("/model shared-model --once")
	if failed := failedLines(rec); len(failed) > 0 {
		t.Fatalf("printed %v", failed)
	}
	if c.baseURL != aliasURL {
		t.Fatalf("the CLI shows %q, not the alias's endpoint", c.baseURL)
	}
	c.chat("run on the alias", nil, false)
	c.chat("the turn after", nil, false)
	submits := owner.submits()
	if len(submits) != 2 {
		t.Fatalf("submits %v", submits)
	}
	override := submits[0]["model_override"]
	if !reflect.DeepEqual(override, map[string]any{"model": "shared-model", "provider": "custom"}) {
		t.Fatalf("override %v", submits[0])
	}
	if _, present := submits[1]["model_override"]; present {
		t.Fatalf("after submit %v", submits[1])
	}
	if base, key := aliasOneTurnEndpoint(t, override, "shared-model"); base != aliasURL || key != "sk-alias" {
		t.Fatalf("one turn runtime %v %v", base, key)
	}
	if mutates, _ := owner.routes(); len(mutates) > 0 {
		t.Fatalf("mutates %v", mutates)
	}
}

func TestAOncePickRefusedAsSessionBusyRidesTheResend(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	refused := false
	owner.onSubmit = func(params map[string]any) (any, bool, error) {
		if refused {
			return nil, false, nil
		}
		refused = true
		owner.client.record("prompt.submit", params)
		return nil, true, &gateway.GatewayRPCError{Msg: "session_busy"}
	}
	c, _ := scopeCLI(t, conn)
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once")
	c.chat("refused", nil, false)
	if reply, _ := c.chat("the resend", nil, false); reply != "ok" {
		t.Fatalf("resend %q", reply)
	}
	if reply, _ := c.chat("the turn after", nil, false); reply != "ok" {
		t.Fatalf("turn after %q", reply)
	}
	submits := owner.submits()
	if len(submits) != 3 {
		t.Fatalf("submits %v", submits)
	}
	pick := map[string]any{"model": "claude-sonnet-4.6", "provider": "anthropic"}
	if !reflect.DeepEqual(submits[0]["model_override"], pick) || !reflect.DeepEqual(submits[1]["model_override"], pick) {
		t.Fatalf("refused %v resend %v", submits[0], submits[1])
	}
	if _, present := submits[2]["model_override"]; present {
		t.Fatalf("after submit %v", submits[2])
	}
	if c.model != "old/model" || c.agentRef().Model() != "old/model" {
		t.Fatalf("cli %q agent %q", c.model, c.agentRef().Model())
	}
}

func TestTheOncePickIsTakenWhenTheAdmissionIsBuilt(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "claude-sonnet-4.6", Provider: "anthropic", OneTurn: true})
	a.mu.Lock()
	staged := a.turnRoute != nil
	a.mu.Unlock()
	if !staged {
		t.Fatal("the one turn pick was not staged")
	}
	var seen []map[string]any
	owner.onSubmit = func(map[string]any) (any, bool, error) {
		a.mu.Lock()
		seen = append(seen, a.turnRoute)
		a.mu.Unlock()
		return nil, false, nil
	}
	runTurn(t, a, "the once turn")
	if len(seen) != 1 || seen[0] != nil {
		t.Fatalf("seen %v", seen)
	}
	if want := map[string]any{"model": "claude-sonnet-4.6", "provider": "anthropic"}; !reflect.DeepEqual(owner.submits()[0]["model_override"], want) {
		t.Fatalf("submit %v", owner.submits()[0])
	}
}

func TestAnImageStagingFailureIsAFailedTurnNotAnException(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	inner := owner.client.override
	owner.client.override = func(method string, params map[string]any) (any, bool, error) {
		if method == "image.attach_bytes" {
			owner.client.record(method, params)
			return nil, true, &gateway.GatewayClientError{Msg: "unsupported_method"}
		}
		return inner(method, params)
	}
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "old/model", Provider: "openrouter"})
	parts := []any{map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}}}
	result, err := a.RunConversation(context.Background(), parts, agent.RunConversationOptions{})
	if err != nil {
		t.Fatalf("staging raised %v", err)
	}
	if result["failed"] != true || result["failure_reason"] != "gateway_connection" {
		t.Fatalf("result %v", result)
	}
	if submits := owner.submits(); len(submits) != 0 {
		t.Fatalf("submits %v", submits)
	}
}

func TestASessionReasoningPickIsCommittedOnceNotOnEveryRebuild(t *testing.T) {
	owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
	c, _ := scopeCLI(t, conn)
	c.setAgent(nil)
	c.reasoningConfig, c.reasoningUserSet = map[string]any{"enabled": true, "effort": "high"}, true
	if reply, _ := c.chat("first", nil, false); reply != "ok" {
		t.Fatalf("first %q", reply)
	}
	retireAgent(c)
	if reply, _ := c.chat("second", nil, false); reply != "ok" {
		t.Fatalf("second %q", reply)
	}
	if mutates := owner.reasoningMutates(); !reflect.DeepEqual(mutates, []any{map[string]any{"effort": "high"}}) {
		t.Fatalf("reasoning mutates %v", mutates)
	}
	if c.reasoningUserSet {
		t.Fatal("the reasoning pick stays user set")
	}
}

func TestOnceReasoningAdmissionPassesTheRealSubmitContract(t *testing.T) {
	overrides := []map[string]any{
		{"model": "first", "reasoning": "high"},
		{"model": "claude-sonnet-4.6", "provider": "anthropic", "reasoning": "high"},
	}
	contract := tuigateway.CanonicalMethods["prompt.submit"]
	for i, override := range overrides {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			params := map[string]any{"session_id": "s", "text": "t", "submission_id": "x", "model_override": override}
			if problems := tuigateway.CanonicalParamProblems(contract, params); len(problems) != 0 {
				t.Fatalf("problems %v", problems)
			}
			instance, verr := contract.Params.Validate(params)
			if verr != nil {
				t.Fatal(verr)
			}
			if got := instance.Get("model_override").(*pydantic.Instance).Get("reasoning"); got != "high" {
				t.Fatalf("reasoning %v", got)
			}
			owner, conn := newScopeOwner(t, map[string]any{"model": "old/model", "provider": "openrouter"}, false)
			c, _ := scopeCLI(t, conn)
			c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic --once --reasoning high")
			if reply, _ := c.chat("the once turn", nil, false); reply != "ok" {
				t.Fatalf("once turn %q", reply)
			}
			submits := owner.submits()
			if len(submits) != 1 {
				t.Fatalf("submits %v", submits)
			}
			if got := submits[0]["model_override"].(map[string]any)["reasoning"]; got != "high" {
				t.Fatalf("submit %v", submits[0])
			}
			if problems := tuigateway.CanonicalParamProblems(contract, submits[0]); len(problems) != 0 {
				t.Fatalf("submit problems %v", problems)
			}
		})
	}
}

func TestEveryModelOverrideKeyTheCliSendsIsInTheContract(t *testing.T) {
	fields := tuigateway.CanonicalModelOverride.ModelFields()
	for _, key := range []string{"model", "provider", "reasoning"} {
		if !slices.Contains(fields, key) {
			t.Fatalf("fields %v lack %s", fields, key)
		}
	}
}
