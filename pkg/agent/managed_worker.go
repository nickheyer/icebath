package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keys of the owner's bootstrap frame, attachments_v1 riding beside them
var bootstrapFields = []string{"version", "home", "scope", "policy", "api_key", "text", "route", "user_id", "chat_id",
	"turn_author", "safe_mode", "ignore_user_config"}

// Seconds cooperative tools get after the owner's control pipe closed under a live worker
const orphanGraceSeconds = 10

// The managed worker child's framed event writer, one frame per write under its lock
type WorkerChannel struct {
	stream io.WriteCloser
	mu     sync.Mutex
}

// Channel writing frames to the stream
func NewWorkerChannel(stream io.WriteCloser) *WorkerChannel { return &WorkerChannel{stream: stream} }

// Writes one typed frame with its payload keys
func (c *WorkerChannel) Send(kind string, payload *pyjson.Object) error {
	frame := pyjson.ObjectOf("type", kind)
	if payload != nil {
		for _, k := range payload.Keys {
			frame.Set(k, payload.Vals[k])
		}
	}
	data, err := gateway.EncodeFrame(frame)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.stream.Write(data)
	return err
}

// Closes the underlying stream
func (c *WorkerChannel) Close() error { return c.stream.Close() }

// Validated bootstrap frame of the owner, ValueError invalid_managed_worker_bootstrap for anything else
func ValidateBootstrap(frame *pyjson.Object) (*pyjson.Object, error) {
	invalid := &workerStoreError{"ValueError", "invalid_managed_worker_bootstrap"}
	keys := map[string]bool{}
	for _, k := range frame.Keys {
		if k != "attachments_v1" {
			keys[k] = true
		}
	}
	if len(keys) != len(bootstrapFields) {
		return nil, invalid
	}
	for _, k := range bootstrapFields {
		if !keys[k] {
			return nil, invalid
		}
	}
	if !pyjson.Equal(frame.Vals["version"], int64(1)) {
		return nil, invalid
	}
	scope, isDict := frame.Vals["scope"].(*pyjson.Object)
	homeText, homeIsText := frame.Vals["home"].(string)
	if !isDict || !sameKeySet(scope, "profile_id", "session_id", "execution_id", "generation", "pid", "birth", "secret", "epoch") ||
		!pyjson.Equal(scope.Vals["pid"], int64(os.Getpid())) || !pyjson.Equal(scope.Vals["profile_id"], frame.Vals["home"]) ||
		!homeIsText || !filepath.IsAbs(homeText) {
		return nil, invalid
	}
	if _, ok := frame.Vals["policy"].(*pyjson.Object); !ok {
		return nil, invalid
	}
	for _, k := range []string{"text", "route", "user_id", "chat_id"} {
		if _, ok := frame.Vals[k].(string); !ok {
			return nil, invalid
		}
	}
	safeMode, safeOK := frame.Vals["safe_mode"].(bool)
	ignore, ignoreOK := frame.Vals["ignore_user_config"].(bool)
	if !safeOK || !ignoreOK || (safeMode && !ignore) {
		return nil, invalid
	}
	if key := frame.Vals["api_key"]; key != nil {
		if _, ok := key.(string); !ok {
			return nil, invalid
		}
	}
	if author := frame.Vals["turn_author"]; author != nil {
		obj, ok := author.(*pyjson.Object)
		if !ok || provider.ParseTurnAuthor(pyjson.Plain(obj).(map[string]any)) == nil {
			return nil, invalid
		}
	}
	return frame, nil
}

func sameKeySet(obj *pyjson.Object, keys ...string) bool {
	if obj.Len() != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := obj.Get(k); !ok {
			return false
		}
	}
	return true
}

// Freezes the owner's assignment process wide before any config or provider read, bypass sessions binding the safe policy too
func BindWorkerPolicy(frame *pyjson.Object) error {
	policy, _ := frame.Vals["policy"].(*pyjson.Object)
	text, _ := policy.GetOr("config_json", nil).(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	cfg, _ := config.ConfigFromJSON(decoded).(map[string]any)
	if ignore, _ := frame.Vals["ignore_user_config"].(bool); !ignore {
		_, err := config.BindWorkerConfig(cfg)
		return err
	}
	safeMode, _ := frame.Vals["safe_mode"].(bool)
	_, err = config.BindSafeWorkerPolicy(safeMode, true, cfg)
	return err
}

var outboxName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// Private outbox directory of an execution, its ledger key spelled portably and never escaping worker-outboxes
func ManagedWorkerOutboxDir(home string, executionID any) (string, error) {
	name := ""
	if text, ok := executionID.(string); ok {
		name = strings.ReplaceAll(text, ":", "-")
	}
	if !outboxName.MatchString(name) {
		return "", &workerStoreError{"ValueError", "invalid_execution_id"}
	}
	return filepath.Join(home, "worker-outboxes", name), nil
}

// Agent surface a settled admission retires between turns, the memory extraction drained before the provider shuts down
type retiringAgent interface {
	MemoryFlushPending(timeout time.Duration) bool
	RetiringSessionMessages() []map[string]any
	ShutdownMemoryProvider(ctx context.Context, messages []map[string]any)
	ReleaseClients(ctx context.Context)
}

// Ends the turn's memory work and releases clients, keeping background processes, sandbox and browser for the next turn
func RetireAgent(ctx context.Context, agent retiringAgent) {
	agent.MemoryFlushPending(10 * time.Second)
	agent.ShutdownMemoryProvider(ctx, agent.RetiringSessionMessages())
	agent.ReleaseClients(ctx)
}

// Drains the memory manager's queued boundary work within the timeout, true without a manager
func (a *AIAgent) MemoryFlushPending(timeout time.Duration) bool {
	if a.memoryManager == nil {
		return true
	}
	return a.memoryManager.FlushPending(timeout)
}

// Session messages as dicts for the provider shutdown, nil when the agent holds none
func (a *AIAgent) RetiringSessionMessages() []map[string]any {
	if a.sessionMessages == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(a.sessionMessages))
	for _, m := range a.sessionMessages {
		out = append(out, state.MessageToDict(m))
	}
	return out
}

// This process's pid and birth with the ancestors it observes, the owner deciding whether they match its handle
func ManagedWorkerHello() (*pyjson.Object, error) {
	pid := os.Getpid()
	birth, err := proc.WorkerBirth(pid)
	if err != nil {
		return nil, err
	}
	parents, err := proc.Parents(pid)
	if err != nil {
		return nil, err
	}
	ancestors := []any{}
	for i, p := range parents {
		if i == 3 {
			break
		}
		ancestors = append(ancestors, int64(p))
	}
	return pyjson.ObjectOf("pid", int64(pid), "birth", birth, "ancestors", ancestors), nil
}

// Answer a stopped or cancelled clarification carries
type interruptedAnswer struct{}

// Owner prompt waiting on its answer
type clarifyState struct {
	done   chan struct{}
	once   sync.Once
	answer any
}

func (s *clarifyState) settle(answer any) {
	s.once.Do(func() {
		s.answer = answer
		close(s.done)
	})
}

// The worker's stop, finish, approval and clarify controls read from the owner's stdin pipe
type WorkerControls struct {
	channel        *WorkerChannel
	route          string
	mu             sync.Mutex
	agent          *AIAgent
	stopped        chan struct{}
	stopOnce       sync.Once
	finish         chan struct{}
	finishOnce     sync.Once
	clarifications map[string]*clarifyState
	env            *tools.Env
}

// Controls bound to the route, their reader started on the input pipe
func NewWorkerControls(channel *WorkerChannel, route string, input io.Reader, env *tools.Env) *WorkerControls {
	c := &WorkerControls{channel: channel, route: route, stopped: make(chan struct{}), finish: make(chan struct{}),
		clarifications: map[string]*clarifyState{}, env: env}
	go c.read(bufio.NewReader(input))
	return c
}

// Binds the constructed agent a stop interrupts
func (c *WorkerControls) SetAgent(a *AIAgent) {
	c.mu.Lock()
	c.agent = a
	c.mu.Unlock()
}

// Whether the owner asked this worker to stop
func (c *WorkerControls) Stopped() bool {
	select {
	case <-c.stopped:
		return true
	default:
		return false
	}
}

// Waits for the owner's finish within the timeout, false when it never came
func (c *WorkerControls) WaitFinish(timeout time.Duration) bool {
	select {
	case <-c.finish:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stops the turn as the canonical Stop does and cancels every pending clarification
func (c *WorkerControls) Stop() {
	c.stopOnce.Do(func() { close(c.stopped) })
	c.mu.Lock()
	agent := c.agent
	pending := make([]*clarifyState, 0, len(c.clarifications))
	for _, s := range c.clarifications {
		pending = append(pending, s)
	}
	c.mu.Unlock()
	if agent != nil {
		RequestHardInterrupt(agent, "", "")
	}
	for _, s := range pending {
		s.settle(interruptedAnswer{})
	}
}

func (c *WorkerControls) setFinish() { c.finishOnce.Do(func() { close(c.finish) }) }

// Reads control frames until finish, a closed pipe stopping the worker and ending it with the owner
func (c *WorkerControls) read(input *bufio.Reader) {
	for {
		frame, err := gateway.ReadFrame(input)
		if err == nil {
			if sameKeySet(frame, "type") && pyjson.Equal(frame.Vals["type"], "stop") {
				c.Stop()
				continue
			}
			if sameKeySet(frame, "type") && pyjson.Equal(frame.Vals["type"], "finish") {
				c.setFinish()
				return
			}
			err = c.control(frame)
			if err == nil {
				continue
			}
		}
		c.Stop()
		c.setFinish()
		var valueErr interface{ PyTypeName() string }
		if !(errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError") {
			dieWithOwner()
		}
		return
	}
}

// Applies one approval or clarify answer, ValueError invalid_managed_control for anything else
func (c *WorkerControls) control(frame *pyjson.Object) error {
	invalid := &workerStoreError{"ValueError", "invalid_managed_control"}
	kind, _ := frame.Vals["type"].(string)
	promptID, idOK := frame.Vals["prompt_id"].(string)
	value, valueOK := frame.Vals["value"].(string)
	if !sameKeySet(frame, "type", "prompt_id", "value") || (kind != "approval" && kind != "clarify") || !idOK || !valueOK || pystr.Len(value) > 16384 {
		return invalid
	}
	if kind == "approval" {
		if value != "once" && value != "deny" && value != "session" && value != "always" {
			return invalid
		}
		tools.ResolveGatewayApproval(c.route, value, false, "", promptID)
		return nil
	}
	c.mu.Lock()
	state := c.clarifications[promptID]
	c.mu.Unlock()
	if state != nil {
		state.settle(value)
	}
	return nil
}

// Forwards an approval request to the owner and acknowledges it as delivered
func (c *WorkerControls) Approval(data map[string]any) error {
	fields := map[string]bool{"request_id": true, "command": true, "description": true, "allow_session": true, "allow_permanent": true, "smart_denied": true, "edit": true}
	forwarded := map[string]any{}
	for k, v := range data {
		if fields[k] {
			forwarded[k] = v
		}
	}
	obj, _ := pyjson.AsObject(forwarded)
	if err := c.channel.Send("approval", pyjson.ObjectOf("data", obj)); err != nil {
		return err
	}
	requestID, _ := data["request_id"].(string)
	tools.AckGatewayApproval(c.route, requestID)
	return nil
}

// clarify_tool's batch contract, one owner prompt per question until one is interrupted or unanswered
func (c *WorkerControls) Clarify(ctx context.Context, questions []any) map[string]any {
	if finite, _ := gateway.FiniteTurnRequired(ctx).(bool); finite {
		return pyjson.Plain(gateway.FiniteClarifyReply()).(map[string]any)
	}
	answers := map[string]any{}
	reply := map[string]any{"answers": answers, "outcome": "submitted"}
	for _, raw := range questions {
		entry, _ := raw.(map[string]any)
		answer := c.ask(entry["question"], entry["choices"], entry["multi_select"])
		if _, interrupted := answer.(interruptedAnswer); interrupted {
			reply["outcome"] = "cancelled"
			break
		}
		if answer == nil {
			reply["outcome"] = "timed_out"
			break
		}
		if text := pystr.Strip(answer.(string)); text != "" {
			answers[pyrepr.Str(entry["qid"])] = text
		} else {
			answers[pyrepr.Str(entry["qid"])] = nil
		}
	}
	return reply
}

// One owner prompt answered within the clarify timeout, interrupted once stopped or with sixteen already open
func (c *WorkerControls) ask(question, choices, multiSelect any) any {
	timeout := tools.GetClarifyTimeout(c.env)
	id, err := tokenHex(16)
	if err != nil {
		return interruptedAnswer{}
	}
	state := &clarifyState{done: make(chan struct{})}
	c.mu.Lock()
	if c.Stopped() || len(c.clarifications) >= 16 {
		c.mu.Unlock()
		return interruptedAnswer{}
	}
	c.clarifications[id] = state
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.clarifications, id)
		c.mu.Unlock()
		_ = c.channel.Send("prompt_settled", pyjson.ObjectOf("prompt_id", id))
	}()
	list := []any{}
	if items, ok := choices.([]any); ok {
		list = append(list, items...)
	}
	if err := c.channel.Send("clarify", pyjson.ObjectOf("prompt_id", id, "question", question, "choices", list,
		"multi_select", pyrepr.Truthy(multiSelect))); err != nil {
		return nil
	}
	if timeout > 0 {
		select {
		case <-state.done:
		case <-time.After(time.Duration(timeout) * time.Second):
			return nil
		}
	} else {
		<-state.done
	}
	return state.answer
}

// uuid4 hex of random bytes
func tokenHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x", b), nil
}
