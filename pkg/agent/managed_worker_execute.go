package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs the managed worker child argv names through the binary exit hooks, otherwise latches the dispatch
func DispatchManagedWorkerEntry(exit func(code int)) {
	gateway.MarkManagedWorkerEntryDispatched()
	if len(os.Args) < 2 || os.Args[1] != gateway.ManagedWorkerEntry {
		return
	}
	exit(ManagedWorkerMain(context.Background()))
}

// Child entry, hello then the validated bootstrap then one execution, the owner told only that it failed
func ManagedWorkerMain(ctx context.Context) (code int) {
	stream, err := managedWorkerStdio()
	if err != nil {
		fmt.Fprintf(os.Stderr, "managed worker failed\n%s\n", formatWorkerFailure(err))
		return 1
	}
	channel := NewWorkerChannel(stream)
	defer channel.Close()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "managed worker failed\n%v\n", r)
			_ = channel.Send("error", pyjson.ObjectOf("reason", "managed_worker_failed"))
			code = 1
		}
	}()
	if err := runManagedWorker(ctx, channel); err != nil {
		fmt.Fprintf(os.Stderr, "managed worker failed\n%s\n", formatWorkerFailure(err))
		_ = channel.Send("error", pyjson.ObjectOf("reason", "managed_worker_failed"))
		return 1
	}
	return 0
}

// Exception class and message of a failure the way the worker's stderr log carries it
func formatWorkerFailure(err error) string {
	if typed, ok := err.(interface{ PyTypeName() string }); ok {
		return typed.PyTypeName() + ": " + err.Error()
	}
	return fmt.Sprintf("%T: %s", err, err)
}

// Hello, bootstrap read and validation, then the execution over the stdin control pipe
func runManagedWorker(ctx context.Context, channel *WorkerChannel) error {
	hello, err := ManagedWorkerHello()
	if err != nil {
		return err
	}
	if err := channel.Send("hello", hello); err != nil {
		return err
	}
	input := bufio.NewReader(os.Stdin)
	raw, err := gateway.ReadFrame(input)
	if err != nil {
		return err
	}
	frame, err := ValidateBootstrap(raw)
	if err != nil {
		return err
	}
	return ExecuteManagedWorker(ctx, frame, channel, input)
}

// Connects the profile's configured MCP servers allowed by the frozen toolsets before tool discovery, never prompting for OAuth
func discoverProfileMCP(ctx context.Context, env *tools.Env, policy *gateway.LocalSessionPolicy) error {
	_, err := mcp.ConnectAll(mcp.SuppressInteractiveOAuth(ctx), env, append([]string{}, policy.Toolsets...))
	return err
}

// Optional text of a policy pointer field as a keyword value, None when unset
func optionalText(text *string) any {
	if text == nil {
		return nil
	}
	return *text
}

// Adopts the execution, builds the agent under the frozen policy, runs the turn and settles it with the owner
func ExecuteManagedWorker(ctx context.Context, frame *pyjson.Object, channel *WorkerChannel, input *bufio.Reader) error {
	if err := BindWorkerPolicy(frame); err != nil {
		return err
	}
	scope := frame.Vals["scope"].(*pyjson.Object).Copy()
	profileHome := frame.Vals["home"].(string)
	rpc := NewWorkerRPC(profileHome)
	adoptParams := pyjson.NewObject()
	for _, key := range scope.Keys {
		if key != "epoch" {
			adoptParams.Set(key, scope.Vals[key])
		}
	}
	adoptedRaw, err := rpc.Call(ctx, "worker.adopt", adoptParams)
	if err != nil {
		return err
	}
	adopted, isDict := adoptedRaw.(*pyjson.Object)
	if !isDict {
		return &workerStoreError{"TypeError", "'" + pyrepr.TypeName(adoptedRaw) + "' object is not subscriptable"}
	}
	ownerEpoch, present := adopted.Get("owner_epoch")
	if !present {
		return &workerStoreError{"KeyError", "'owner_epoch'"}
	}
	if !pyjson.Equal(ownerEpoch, scope.Vals["epoch"]) {
		return &workerStoreError{"RuntimeError", "stale_epoch"}
	}
	outbox, err := ManagedWorkerOutboxDir(profileHome, scope.Vals["execution_id"])
	if err != nil {
		return err
	}
	store, err := NewRuntimeSessionStore(ctx, rpc.Call, scope, outbox, adopted, 0)
	if err != nil {
		return err
	}
	if err := gateway.BindWorkerContext(ctx, frame); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	env := &tools.Env{Home: home.Dir(ctx), Cwd: cwd, Values: &sync.Map{}}
	ctx = tools.WithToolEnv(ctx, env)
	if _, err := tools.DefaultProcessRegistry().RecoverFromCheckpoint(ctx, env); err != nil {
		return err
	}
	policy, err := gateway.RestorePolicy(frame.Vals["policy"])
	if err != nil {
		return err
	}
	if err := gateway.RegisterWorkerHooks(ctx, policy); err != nil {
		return err
	}
	if err := discoverProfileMCP(ctx, env, policy); err != nil {
		return err
	}
	route := frame.Vals["route"].(string)
	tools.SetCurrentSessionKey(env, route)
	if err := os.Setenv("ICEBATH_GATEWAY_SESSION", "1"); err != nil {
		return err
	}
	inputs, err := gateway.ConstructKwargs(frame)
	if err != nil {
		return err
	}
	construct := map[string]any{"ephemeral_system_prompt": optionalText(policy.SkillsPrompt)}
	for _, key := range inputs.Keys {
		construct[key] = pyjson.Plain(inputs.Vals[key])
	}
	controls := NewWorkerControls(channel, route, input, env)
	tools.RegisterGatewayNotify(route, controls.Approval)
	var agent *AIAgent
	runErr := gateway.WorkerPolicyScope(ctx, policy, func(ctx context.Context) error {
		kwargs, err := managedAgentKwargs(ctx, frame, policy, store, scope, channel, controls, construct)
		if err != nil {
			return err
		}
		agent, err = constructManagedAgent(ctx, frame, policy, kwargs)
		if err != nil {
			return err
		}
		return runManagedTurn(ctx, frame, scope, channel, controls, store, &agent)
	})
	tools.UnregisterGatewayNotify(route)
	if agent != nil {
		RetireAgent(ctx, agent)
	}
	closeErr := store.Close()
	rpc.Close()
	if closeErr != nil {
		return closeErr
	}
	return runErr
}

// The keyword arguments execute builds the worker's AIAgent from, before the worker route overrides
func managedAgentKwargs(ctx context.Context, frame *pyjson.Object, policy *gateway.LocalSessionPolicy, store *RuntimeSessionStore,
	scope *pyjson.Object, channel *WorkerChannel, controls *WorkerControls, construct map[string]any) (map[string]any, error) {
	cfg, _, err := policy.EffectiveFor(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	platform := policy.Source
	if policy.Source == "tool" || policy.Source == "oneshot" {
		platform = policy.Platform
	}
	model := optionalText(policy.Model)
	kwargs := map[string]any{
		"model": model, "provider": policy.Provider(), "base_url": policy.BaseURL(), "api_key": frame.Vals["api_key"],
		"session_db": store, "session_id": scope.Vals["session_id"], "enabled_toolsets": append([]string{}, policy.Toolsets...),
		"max_iterations": policy.MaxTurns(), "reasoning_config": policy.ReasoningConfig(), "platform": platform,
		"gateway_session_key": frame.Vals["route"], "user_id": frame.Vals["user_id"], "chat_id": frame.Vals["chat_id"],
		"skip_context_files": policy.IgnoreRules(), "load_soul_identity": !policy.IgnoreRules(), "skip_memory": policy.IgnoreRules(),
		"skip_background_review": true, "quiet_mode": true, "pass_session_id": policy.PassSessionID(),
		"stream_delta_callback": func(text any) {
			if pyrepr.Truthy(text) {
				if err := channel.Send("delta", pyjson.ObjectOf("text", text)); err != nil {
					panic(err)
				}
			}
		},
		"clarify_callback": tools.ClarifyCallback(func(ctx context.Context, shown []any) (map[string]any, error) {
			return controls.Clarify(ctx, shown), nil
		}),
		"tool_start_callback": func(callID, name string, args map[string]any) {
			sendToolFrame(ctx, channel, "tool.start", callID, name, args)
		},
		"tool_complete_callback": func(callID, name string, args map[string]any, result string) {
			sendToolFrame(ctx, channel, "tool.complete", callID, name, args, result)
		},
	}
	for key, value := range gateway.CheckpointAgentKwargs(cfg) {
		kwargs[key] = value
	}
	for key, value := range construct {
		kwargs[key] = value
	}
	modelText, _ := model.(string)
	for key, value := range gateway.RoutingAgentKwargs(cfg, modelText, routeText(policy.Provider()), routeText(policy.BaseURL())) {
		kwargs[key] = value
	}
	return kwargs, nil
}

// Text of a frozen route field, empty for None
func routeText(value any) string {
	if value == nil {
		return ""
	}
	return pyrepr.Str(value)
}

// Sends one tool lifecycle frame built from the callback arguments, raising into the executor's callback guard on failure
func sendToolFrame(ctx context.Context, channel *WorkerChannel, kind, callID, name string, args map[string]any, result ...any) {
	frame, err := gateway.ToolFrame(ctx, callID, name, args, result...)
	if err == nil {
		err = channel.Send(kind, frame)
	}
	if err != nil {
		panic(err)
	}
}

// Builds the worker's agent with the worker route applied, booking a kanban start failure before the error propagates
func constructManagedAgent(ctx context.Context, frame *pyjson.Object, policy *gateway.LocalSessionPolicy, kwargs map[string]any) (*AIAgent, error) {
	agent, err := func() (*AIAgent, error) {
		route, err := gateway.WorkerRoute(ctx, policy, kwargs)
		if err != nil {
			return nil, err
		}
		merged := make(map[string]any, len(kwargs)+len(route))
		for key, value := range kwargs {
			merged[key] = value
		}
		for key, value := range route {
			merged[key] = value
		}
		o, err := AIAgentOptionsFromKwargs(merged)
		if err != nil {
			return nil, err
		}
		return NewAIAgent(ctx, o)
	}()
	if err != nil {
		if recordErr := gateway.RecordStartFailure(ctx, frame, err); recordErr != nil {
			return nil, recordErr
		}
		return nil, err
	}
	return agent, nil
}

// Runs the adopted turn and settles it, the agent retired here once its turn is done
func runManagedTurn(ctx context.Context, frame, scope *pyjson.Object, channel *WorkerChannel, controls *WorkerControls,
	store *RuntimeSessionStore, agentRef **AIAgent) error {
	agent := *agentRef
	controls.SetAgent(agent)
	if controls.Stopped() {
		RequestHardInterrupt(agent, "", "")
	}
	if err := channel.Send("ready", pyjson.ObjectOf("pid", int64(os.Getpid()))); err != nil {
		return err
	}
	sessionID := pyrepr.Str(scope.Vals["session_id"])
	history, err := store.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{})
	if err != nil {
		return err
	}
	if _, present := frame.Get("attachments_v1"); present {
		if frame, err = managedAttachmentFrame(ctx, agent.Env(), frame); err != nil {
			return err
		}
	}
	beforeIn, beforeOut := agent.SessionPromptTokens(), agent.SessionCompletionTokens()
	turn, err := RunWorkerTurns(ctx, agent, frame, history)
	if err != nil {
		return err
	}
	result := pyjson.NewObject()
	if provider := agent.Provider(); provider != "" {
		result.Set("provider", provider)
	} else {
		result.Set("provider", nil)
	}
	if model := agent.Model(); model != "" {
		result.Set("model", model)
	} else {
		result.Set("model", nil)
	}
	keys := make([]string, 0, len(turn))
	for key := range turn {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result.Set(key, objectTree(turn[key]))
	}
	result.Set("_turn_input_tokens", int64(max(0, agent.SessionPromptTokens()-beforeIn)))
	result.Set("_turn_output_tokens", int64(max(0, agent.SessionCompletionTokens()-beforeOut)))
	RetireAgent(ctx, agent)
	*agentRef = nil
	waitForTitleUpgrades(10 * time.Second)
	if _, err := store.FlushTokenCountsChecked(5.0); err != nil {
		return err
	}
	if result.GetOr("final_response", nil) == nil && (pyrepr.Truthy(result.GetOr("interrupted", nil)) || pyrepr.Truthy(result.GetOr("failed", nil))) {
		result.Set("final_response", "")
	}
	if err := channel.Send("result", pyjson.ObjectOf("result", gateway.ResultFrame(result))); err != nil {
		return err
	}
	if !controls.WaitFinish(30 * time.Second) {
		return &workerStoreError{"ValueError", "managed_finish_timeout"}
	}
	if _, err := store.Finish(ctx); err != nil {
		return err
	}
	return channel.Send("finished", nil)
}

// Bootstrap frame whose text carries the committed attachments as native content parts, refusing one that cannot be read
func managedAttachmentFrame(ctx context.Context, env *tools.Env, frame *pyjson.Object) (*pyjson.Object, error) {
	data, _ := frame.Get("attachments_v1")
	if !pyrepr.Truthy(data) {
		return nil, &workerStoreError{"KeyError", "'media_urls'"}
	}
	urls, _, err := gateway.RestoreAttachments(ctx, frame)
	if err != nil {
		return nil, err
	}
	content, skipped, err := tools.BuildNativeContentParts(env, frame.Vals["text"].(string), urls, nil)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, &workerStoreError{"ValueError", "managed_attachment_unavailable"}
	}
	out := frame.Copy()
	out.Set("text", content)
	return out, nil
}

// Plain result value with every nested map as an ordered object, the dict shape the receipt projection reads
func objectTree(value any) any {
	switch v := value.(type) {
	case map[string]any:
		obj, _ := pyjson.AsObject(v)
		for _, key := range obj.Keys {
			obj.Vals[key] = objectTree(obj.Vals[key])
		}
		return obj
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = objectTree(item)
		}
		return out
	}
	return value
}
