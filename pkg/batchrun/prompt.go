package batchrun

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Limits of the docker image probe and pull a container prompt runs before its agent
const (
	dockerInspectTimeout = 10 * time.Second
	dockerPullTimeout    = 600 * time.Second
)

// Returned by a prompt or batch the run stopped before it finished, so nothing is recorded for it
var errStopped = errors.New("batch run stopped")

// One dataset entry with its position in the dataset
type indexedEntry struct {
	index int
	entry *pyjson.Object
}

// State one worker keeps across the prompts it runs
type workerState struct {
	// Toolset sampling source of this worker alone, so workers never share a sequence
	rng *rand.Rand
}

// Outcome of one prompt, a failure carrying only its error
type promptResult struct {
	success      bool
	promptIndex  int
	err          string
	trajectory   []any
	toolStats    *toolStats
	reasoning    reasoningStats
	hasReasoning bool
	completed    any
	partial      any
	apiCalls     any
	toolsetsUsed []string
	metadata     *pyjson.Object
}

// Result of a prompt that produced no trajectory
func failureResult(promptIndex, batchNum int, message string) promptResult {
	return promptResult{
		promptIndex: promptIndex, err: message, toolStats: newToolStats(),
		metadata: pyjson.ObjectOf("batch_num", batchNum, "timestamp", pydatetime.ISOFormatNaive(time.Now())),
	}
}

// Terminal backend the tool environment selects, local when it cannot be resolved
func terminalBackend(env *tools.Env) string {
	cfg, err := tools.GetEnvConfig(env)
	if err != nil {
		return "local"
	}
	if backend, ok := cfg["env_type"].(string); ok && backend != "" {
		return backend
	}
	return "local"
}

// Runs a docker command under a timeout and returns its stderr and whether it exited zero, an error when docker could not run or timed out
func runDocker(ctx context.Context, timeout time.Duration, args ...string) (string, bool, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "docker", args...)
	var captured strings.Builder
	cmd.Stderr = &captured
	err := cmd.Run()
	if err == nil {
		return captured.String(), true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && runCtx.Err() == nil {
		return strings.ToValidUTF8(captured.String(), "�"), false, nil
	}
	return "", false, err
}

// Checks a docker image locally and pulls it when absent, before any tokens are spent on the prompt
func (r *Runner) verifyDockerImage(ctx context.Context, promptIndex, batchNum int, image any) *promptResult {
	name, ok := image.(string)
	if !ok {
		if r.o.Verbose {
			r.printf("   Prompt %d: Docker image check failed: the image is %s, expected text\n", promptIndex, pyrepr.TypeName(image))
		}
		return nil
	}
	_, present, err := runDocker(ctx, dockerInspectTimeout, "image", "inspect", name)
	if err == nil && !present {
		if r.o.Verbose {
			r.printf("   Prompt %d: Pulling docker image %s...\n", promptIndex, name)
		}
		var stderr string
		var pulled bool
		if stderr, pulled, err = runDocker(ctx, dockerPullTimeout, "pull", name); err == nil && !pulled {
			if detail := []rune(stderr); len(detail) > 500 {
				stderr = string(detail[:500])
			}
			failure := failureResult(promptIndex, batchNum, fmt.Sprintf("Docker image not available: %s\n%s", name, stderr))
			return &failure
		}
	}
	if err != nil && !errors.Is(err, exec.ErrNotFound) && r.o.Verbose {
		r.printf("   Prompt %d: Docker image check failed: %v\n", promptIndex, err)
	}
	return nil
}

// Registers the container image a dataset row names for its task sandbox, a failure result when a docker image cannot be pulled
func (r *Runner) prepareContainerImage(ctx context.Context, env *tools.Env, promptIndex int, entry *pyjson.Object, batchNum int, taskID string) (*promptResult, error) {
	image := entry.Vals["image"]
	if !pyrepr.Truthy(image) {
		image = entry.Vals["docker_image"]
	}
	if !pyrepr.Truthy(image) {
		return nil, nil
	}
	backend := terminalBackend(env)
	if backend == "docker" {
		if failure := r.verifyDockerImage(ctx, promptIndex, batchNum, image); failure != nil {
			return failure, nil
		}
	}
	overrides := map[string]any{"docker_image": image, "modal_image": image, "daytona_image": image, backend + "_image": image}
	if cwd := entry.Vals["cwd"]; pyrepr.Truthy(cwd) {
		overrides["cwd"] = cwd
	}
	if err := tools.RegisterTaskEnvOverrides(env, taskID, overrides); err != nil {
		return nil, err
	}
	if r.o.Verbose {
		r.printf("   Prompt %d: Using container image %s\n", promptIndex, pyrepr.Str(image))
	}
	return nil, nil
}

// Message history of a finished conversation as a list of message maps
func resultMessages(result map[string]any) ([]map[string]any, error) {
	raw, err := requiredKey(result, "messages")
	if err != nil {
		return nil, err
	}
	if messages, ok := raw.([]map[string]any); ok {
		return messages, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("the conversation messages are %s, expected a list", pyrepr.TypeName(raw))
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		msg, ok := pyjson.Dict(item)
		if !ok {
			return nil, fmt.Errorf("a conversation message is %s, expected a mapping", pyrepr.TypeName(item))
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

// Names as a JSON list, empty rather than null when there are none
func stringList(names []string) []any {
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

// Tracks the agents of running prompts so an interrupt can stop every one of them
type agentRegistry struct {
	mu       sync.Mutex
	live     map[*agent.AIAgent]struct{}
	stopping bool
}

// Adds a running agent, false once the run is stopping so the agent is closed unused
func (g *agentRegistry) add(a *agent.AIAgent) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return false
	}
	if g.live == nil {
		g.live = map[*agent.AIAgent]struct{}{}
	}
	g.live[a] = struct{}{}
	return true
}

// Drops an agent whose prompt finished
func (g *agentRegistry) remove(a *agent.AIAgent) {
	g.mu.Lock()
	delete(g.live, a)
	g.mu.Unlock()
}

// Whether the run is stopping
func (g *agentRegistry) isStopping() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopping
}

// Marks the run as stopping and hard interrupts every running agent
func (g *agentRegistry) interruptAll(reason string) {
	g.mu.Lock()
	g.stopping = true
	live := make([]*agent.AIAgent, 0, len(g.live))
	for a := range g.live {
		live = append(live, a)
	}
	g.mu.Unlock()
	for _, a := range live {
		func() {
			defer func() { _ = recover() }()
			a.HardInterrupt("", reason)
		}()
	}
}

// Panic recovered from a prompt with the stack it was raised on
type promptPanic struct {
	value any
	stack []byte
}

// Error is the message of the recovered value
func (p *promptPanic) Error() string {
	if err, ok := p.value.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(p.value)
}

// What one finished conversation yields for its trajectory row
type conversation struct {
	result       map[string]any
	trajectory   []any
	stats        *toolStats
	reasoning    reasoningStats
	hasReasoning bool
}

// Builds the agent of one prompt, runs its conversation and closes the agent whatever the outcome, a panic returned as an error
func (r *Runner) converse(ctx context.Context, opts agent.AIAgentOptions, prompt any, taskID string) (conv conversation, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &promptPanic{value: recovered, stack: debug.Stack()}
		}
	}()
	a, err := agent.NewAIAgent(ctx, opts)
	if err != nil {
		return conv, err
	}
	defer func() {
		r.agents.remove(a)
		defer func() { _ = recover() }()
		_ = a.Close()
	}()
	if !r.agents.add(a) {
		return conv, errStopped
	}
	query, isText := prompt.(string)
	if isText {
		conv.result, err = a.RunConversation(ctx, query, agent.RunConversationOptions{TaskID: taskID})
	} else {
		conv.result, err = a.RunConversationContent(ctx, prompt, agent.RunConversationOptions{TaskID: taskID})
	}
	if err != nil {
		return conv, err
	}
	messages, err := resultMessages(conv.result)
	if err != nil {
		return conv, err
	}
	if conv.stats, err = extractToolStats(messages); err != nil {
		return conv, err
	}
	if conv.reasoning, conv.hasReasoning, err = extractReasoningStats(messages); err != nil {
		return conv, err
	}
	if conv.trajectory, err = a.ConvertToTrajectoryFormat(messages, query); err != nil {
		return conv, err
	}
	if !isText && len(conv.trajectory) > 1 {
		if human, ok := conv.trajectory[1].(*pyjson.Object); ok {
			human.Set("value", prompt)
		}
	}
	return conv, nil
}

// Text option as the value the agent reads, nil when unset
func optionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// List option as the value the agent reads, nil when unset
func optionalList(values []string) any {
	if values == nil {
		return nil
	}
	return stringList(values)
}

// Agent keywords of one prompt, the sampled toolsets enabled and nothing persisted outside the run
func (r *Runner) agentOptions(ctx context.Context, env *tools.Env, selected []string, batchNum, promptIndex int) agent.AIAgentOptions {
	opts := agent.AIAgentOptions{
		Model: r.o.Model, MaxIterations: new(r.o.MaxIterations), EnabledToolsets: selected,
		VerboseLogging: r.o.Verbose, LogPrefixChars: new(r.o.LogPrefixChars),
		LogPrefix:        fmt.Sprintf("[B%d:P%d]", batchNum, promptIndex),
		SkipContextFiles: true, SkipMemory: !agent.CheckpointRequiredFromConfig(ctx), MemoryAgentContext: "subagent",
		BaseURL: r.o.BaseURL, EphemeralSystemPrompt: r.o.EphemeralSystemPrompt,
		ProvidersAllowed: optionalList(r.o.ProvidersAllowed), ProvidersIgnored: optionalList(r.o.ProvidersIgnored),
		ProvidersOrder: optionalList(r.o.ProvidersOrder), ProviderSort: optionalText(r.o.ProviderSort),
		ReasoningConfig: r.o.ReasoningConfig, PrefillMessages: r.o.PrefillMessages, Platform: "batch", Env: env,
	}
	if r.o.APIKey != "" {
		opts.APIKey = r.o.APIKey
	}
	return opts
}

// Runs the agent on one prompt and returns its trajectory, statistics and metadata, or a failure result
func (r *Runner) processPrompt(ctx context.Context, w *workerState, promptIndex int, entry *pyjson.Object, batchNum int) (promptResult, error) {
	prompt := entry.Vals["prompt"]
	taskID := fmt.Sprintf("task_%d", promptIndex)
	env := &tools.Env{Home: r.home, Cwd: r.cwd, Values: &sync.Map{}}
	ctx = tools.WithToolEnv(ctx, env)
	failure, err := r.prepareContainerImage(ctx, env, promptIndex, entry, batchNum, taskID)
	if err != nil {
		return promptResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}
	fail := func(err error) (promptResult, error) {
		r.printf("❌ Error processing prompt %d: %s\n", promptIndex, err.Error())
		var panicked *promptPanic
		if r.o.Verbose && errors.As(err, &panicked) {
			fmt.Fprintf(os.Stderr, "%s\n", panicked.stack)
		}
		return failureResult(promptIndex, batchNum, err.Error()), nil
	}
	selected, warnings, err := tools.Default.SampleToolsetsFromDistribution(ctx, "", r.o.Distribution, w.rng)
	for _, warning := range warnings {
		r.printf("⚠️  Warning: %s\n", warning)
	}
	if err != nil {
		return fail(fmt.Errorf("Unknown distribution: %s", r.o.Distribution))
	}
	if selected == nil {
		selected = []string{}
	}
	if r.o.Verbose {
		r.printf("   Prompt %d: Using toolsets %s\n", promptIndex, pyrepr.Repr(selected))
	}
	switch prompt.(type) {
	case string, []any:
	default:
		return fail(fmt.Errorf("the prompt is %s, expected text or a list of content parts", pyrepr.TypeName(prompt)))
	}
	conv, err := r.converse(ctx, r.agentOptions(ctx, env, selected, batchNum, promptIndex), prompt, taskID)
	if r.agents.isStopping() || ctx.Err() != nil {
		return promptResult{}, errStopped
	}
	if err != nil {
		return fail(err)
	}
	completed, err := requiredKey(conv.result, "completed")
	if err != nil {
		return fail(err)
	}
	apiCalls, err := requiredKey(conv.result, "api_calls")
	if err != nil {
		return fail(err)
	}
	var partial any = false
	if v, ok := conv.result["partial"]; ok {
		partial = v
	}
	return promptResult{
		success: true, promptIndex: promptIndex, trajectory: conv.trajectory, toolStats: conv.stats,
		reasoning: conv.reasoning, hasReasoning: conv.hasReasoning, completed: completed, partial: partial, apiCalls: apiCalls,
		toolsetsUsed: selected,
		metadata:     pyjson.ObjectOf("batch_num", batchNum, "timestamp", pydatetime.ISOFormatNaive(time.Now()), "model", r.o.Model),
	}, nil
}
