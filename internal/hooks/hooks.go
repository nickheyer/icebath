// Package hooks is the host of the agent's lifecycle hook points: the points defined once, the callbacks plugins and the user's shell hooks
// set on them, and how a firing runs them, in order, each bounded by its time limit, the policy of its point deciding what a failure means
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Hook points, by the names hooks are set on
const (
	PreToolCall                     = "pre_tool_call"
	PostToolCall                    = "post_tool_call"
	TransformTerminalOutput         = "transform_terminal_output"
	TransformToolResult             = "transform_tool_result"
	TransformLLMOutput              = "transform_llm_output"
	PreLLMCall                      = "pre_llm_call"
	PostLLMCall                     = "post_llm_call"
	StreamStart                     = "on_stream_start"
	StreamDelta                     = "on_stream_delta"
	StreamEnd                       = "on_stream_end"
	InterimMessage                  = "on_interim_message"
	PreVerify                       = "pre_verify"
	PreAPIRequest                   = "pre_api_request"
	PostAPIRequest                  = "post_api_request"
	APIRequestError                 = "api_request_error"
	PreAuxiliaryCall                = "pre_auxiliary_call"
	PostAuxiliaryCall               = "post_auxiliary_call"
	TransformAPIErrorClassification = "transform_api_error_classification"
	SessionStart                    = "on_session_start"
	SessionEnd                      = "on_session_end"
	SessionFinalize                 = "on_session_finalize"
	SessionReset                    = "on_session_reset"
	SessionDelete                   = "on_session_delete"
	SkillLifecycle                  = "on_skill_lifecycle"
	SubagentStart                   = "subagent_start"
	SubagentStop                    = "subagent_stop"
	PreGatewayDispatch              = "pre_gateway_dispatch"
	PostGatewayAdmission            = "post_gateway_admission"
	AgentLoopStopped                = "agent_loop_stopped"
	PreApprovalRequest              = "pre_approval_request"
	PostApprovalResponse            = "post_approval_response"
	HumanInputRequest               = "on_human_input_request"
	HumanInputResolved              = "on_human_input_resolved"
	RoomMemberActivity              = "on_room_member_activity"
	PreTranscription                = "pre_transcription"
	KanbanTaskClaimed               = "kanban_task_claimed"
	KanbanTaskCompleted             = "kanban_task_completed"
	KanbanTaskBlocked               = "kanban_task_blocked"
	KanbanWorkerSpawned             = "on_kanban_worker_spawned"
	KanbanWorkerExited              = "on_kanban_worker_exited"
	KanbanWorkerStaleClaim          = "on_kanban_worker_stale_claim"
	KanbanTaskUpdated               = "on_kanban_task_updated"
	KanbanDispatchTick              = "on_kanban_dispatch_tick"
	GatewayPlatformEvent            = "gateway_platform_event"
	PreCommand                      = "pre_command"
	PostCompaction                  = "post_compaction"
)

// How the callbacks of one point are run
type Point struct {
	Name string
	// Callbacks are cut off past hooks.callback_timeout and left to finish on their own
	Bounded bool
	// A callback that fails or runs out of time blocks what the point guards
	FailClosed bool
	// Fired for each call of a tool, so a hook may name the tools it is for
	Tool bool
	// What it guards can be blocked by a hook's answer
	Blocking bool
	// A shell command has no way to give the answer the point takes
	NoShell bool
}

// Every point, by name
var points = func() map[string]Point {
	bounded := []string{PostToolCall, TransformTerminalOutput, TransformToolResult, TransformLLMOutput, PreLLMCall, PostLLMCall, PreAPIRequest,
		PostAPIRequest, APIRequestError, PreAuxiliaryCall, PostAuxiliaryCall, PreVerify, SessionStart, SessionEnd, PostGatewayAdmission, PreCommand,
		PostCompaction}
	all := []string{PreToolCall, PostToolCall, TransformTerminalOutput, TransformToolResult, TransformLLMOutput, PreLLMCall, PostLLMCall,
		StreamStart, StreamDelta, StreamEnd, InterimMessage, PreVerify, PreAPIRequest, PostAPIRequest, APIRequestError, PreAuxiliaryCall,
		PostAuxiliaryCall, TransformAPIErrorClassification, SessionStart, SessionEnd, SessionFinalize, SessionReset, SessionDelete, SkillLifecycle,
		SubagentStart, SubagentStop, PreGatewayDispatch, PostGatewayAdmission, AgentLoopStopped, PreApprovalRequest, PostApprovalResponse,
		HumanInputRequest, HumanInputResolved, RoomMemberActivity, PreTranscription, KanbanTaskClaimed, KanbanTaskCompleted, KanbanTaskBlocked,
		KanbanWorkerSpawned, KanbanWorkerExited, KanbanWorkerStaleClaim, KanbanTaskUpdated, KanbanDispatchTick, GatewayPlatformEvent, PreCommand,
		PostCompaction}
	out := map[string]Point{}
	for _, name := range all {
		out[name] = Point{Name: name, Bounded: slices.Contains(bounded, name), FailClosed: name == PreToolCall,
			Tool: name == PreToolCall || name == PostToolCall, Blocking: name == PreToolCall, NoShell: name == TransformAPIErrorClassification}
	}
	return out
}()

// Every point's name, sorted
func Points() []string {
	return slices.Sorted(maps.Keys(points))
}

// The point of the name, false for a name that is none
func PointOf(name string) (Point, bool) {
	point, ok := points[name]
	return point, ok
}

// Schema of the payloads observers read, which every firing but a gateway platform event names
const Schema = "icebath.observer.v1"

// What hooks a firing hands to its point's callbacks, one of the payload types of this package
type Event interface {
	// Name of the point it fires
	Point() string
}

// Event naming the call a callback runs for, a second run of the callback for the same call skipped while the first still runs
type called interface {
	call() string
}

// Directives a callback answers with
const (
	Block    = "block"
	Modify   = "modify"
	Approve  = "approve"
	Continue = "continue"
)

// What a callback hands back, nil for nothing
type Result struct {
	// block, modify, approve or continue; empty for none
	Action string
	// Why it blocked, what approval it asks for, what the agent is sent back with, or what the user is told
	Message string
	// Arguments a modify puts over those of the call
	Args map[string]any
	// Name the answer to an approve is remembered under, the tool's name when empty
	Rule string
	// Text the turn's message carries to the model, for pre_llm_call
	Context string
	// Text in place of what a transform point was given; nil leaves it
	Replace *string
	// Fields a hook sets over those of the event, for the points that take them, such as the model, language and prompt of pre_transcription
	Set map[string]string
	// How a failed request is to be treated, for transform_api_error_classification
	Class *Classification
}

// One callback set on a point
type Hook struct {
	Point string
	// What it is, as the log names it, such as shell_hook[pre_tool_call:./guard.sh]; one name for one callback, which its time limit and
	// failures are tracked by
	Name string
	// Plugin that set it, as the log names it when it hangs
	Owner string
	// On transform_llm_output: the reply is held back from the screen while it streams, only what the hook makes of it shown
	Hold bool
	Run  func(ctx context.Context, event Event) (*Result, error)
}

type Registrant interface {
	// Callbacks the registrant sets on the points for the profile of the settings, such as those its config names
	Hooks(cfg *config.Config) []Hook
}

// Registrant whose hooks need the user's consent before they run, such as the user's shell hooks: given it for every hook the profile's
// settings set, as a launch with --accept-hooks gives it
type Consenter interface {
	Accept(ctx context.Context, cfg *config.Config) error
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Hooks
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Hooks(cfg *config.Config) []Hook {
	var hooks []Hook
	for _, registrant := range r.registrants {
		hooks = append(hooks, registrant.Hooks(cfg)...)
	}
	return hooks
}

// Takes consent for every hook of the profile from every registrant that asks for it
func (r *Registry) Accept(ctx context.Context, cfg *config.Config) error {
	var errs []error
	for _, registrant := range r.registrants {
		if consenter, ok := registrant.(Consenter); ok {
			if err := consenter.Accept(ctx, cfg); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Host the registry is nested in, through which plugins fire hooks
func (r *Registry) Host() *Hooks {
	return r.host
}

// Lets the registrants that keep work going past a firing finish it, such as posts of events still queued, last registered first, as
// icebath exits
func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

// What runs the hooks: the callbacks set on a point, their time limits and failures as they ran so far
type Hooks struct {
	Registry
	mu sync.Mutex
	// Callbacks that timed out, skipped until the time given
	suppressed map[slot]time.Time
	// Callbacks running now for one call each
	running map[gate]*struct{}
	// Runs of a callback that timed out and still run
	abandoned map[slot]map[gate]bool
	// Failures logged already, repeats of which go to the debug log
	reported map[failure]bool
	streams  streams
}

// Callback of a point, as its time limit is tracked
type slot struct{ point, name string }

// One call a callback runs for
type gate struct {
	slot
	call  string
	named bool
}

type failure struct{ point, name, kind, message string }

const (
	// How long a callback that timed out is skipped
	suppression = 60 * time.Second
	// Runs of a timed out callback still going after which it is skipped as hung
	maxAbandoned = 3
	// What blocks a call whose pre_tool_call callback ran out of time
	timeoutBlock = "A plugin hook on pre_tool_call ran out of time or is still running, so the call is blocked."
	// Seconds hooks.callback_timeout takes when it holds no usable number, and the most it takes
	defaultTimeout = 30
	maxTimeout     = 600
)

func New(cfg *config.Config) *Hooks {
	h := &Hooks{suppressed: map[slot]time.Time{}, running: map[gate]*struct{}{}, abandoned: map[slot]map[gate]bool{}, reported: map[failure]bool{}}
	h.Registry.host = h
	h.streams.workers = map[slot]*worker{}
	hooksconfig.Register(cfg)
	return h
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (h *Hooks) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = h
	}
	h.Registry.Register(registrant)
}

// Whether hooks run for the profile: never in safe mode, which leaves every plugin and hook out
func off(cfg *config.Config) bool {
	return cfg == nil || cfg.GetBool(launchconfig.SafeMode)
}

// Callbacks set on the point for the profile, in the order they were registered
func (h *Hooks) set(cfg *config.Config, point string) []Hook {
	if h == nil || off(cfg) {
		return nil
	}
	var out []Hook
	for _, hook := range h.Registry.Hooks(cfg) {
		if hook.Point == point && hook.Run != nil {
			out = append(out, hook)
		}
	}
	return out
}

// Whether a callback is set on the point for the profile, so a caller builds a costly payload only when something reads it
func (h *Hooks) Has(cfg *config.Config, point string) bool {
	return len(h.set(cfg, point)) > 0
}

// Whether a callback rewrites the replies of the profile with the stream held back until it did
func (h *Hooks) Holds(cfg *config.Config) bool {
	return slices.ContainsFunc(h.set(cfg, TransformLLMOutput), func(hook Hook) bool { return hook.Hold })
}

// Takes consent for every hook the profile's settings set, as a launch with --accept-hooks gives it
func (h *Hooks) Accept(ctx context.Context, cfg *config.Config) error {
	if h == nil || off(cfg) {
		return nil
	}
	return h.Registry.Accept(ctx, cfg)
}

// Runs every callback of the event's point in turn and gives what they answered, in order
func (h *Hooks) Fire(ctx context.Context, cfg *config.Config, event Event) []Result {
	results, _ := h.fire(ctx, cfg, event)
	return results
}

// Runs the callbacks of the point, each its own copy of the event, under the point's policy: a bounded point's callbacks cut off past the
// time limit, a fail closed point's failures and timeouts answered as blocks. The results and how many callbacks failed
func (h *Hooks) fire(ctx context.Context, cfg *config.Config, event Event) ([]Result, int) {
	hooks := h.set(cfg, event.Point())
	if len(hooks) == 0 {
		return nil, 0
	}
	point := points[event.Point()]
	limit := timeout(cfg)
	var results []Result
	failed := 0
	for _, hook := range hooks {
		if ctx.Err() != nil {
			break
		}
		var result *Result
		var err error
		if (point.Bounded || point.FailClosed) && limit > 0 {
			var skipped bool
			if result, skipped, err = h.bounded(ctx, point, hook, event, limit); skipped {
				failed++
				if point.FailClosed {
					results = append(results, Result{Action: Block, Message: timeoutBlock})
				}
				continue
			}
		} else {
			result, err = run(ctx, hook, event)
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failed++
			h.report(point.Name, hook.Name, event, err)
			if point.FailClosed {
				results = append(results, Result{Action: Block, Message: fmt.Sprintf("The plugin hook %s on %s failed with %s, so the call is blocked: %s",
					hook.Name, point.Name, kind(err), strings.TrimSuffix(strs.Head(err.Error(), 200), ".")+".")})
			}
			continue
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, failed
}

// Longest a callback may take for the profile, zero for no limit; a value below zero or no number takes the default
func timeout(cfg *config.Config) time.Duration {
	seconds, err := cast.ToFloat64E(cfg.Get(hooksconfig.CallbackTimeout))
	if err != nil || seconds < 0 {
		seconds = defaultTimeout
	}
	if seconds = min(seconds, maxTimeout); seconds == 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// Runs the callback, a panic turned into its error
func run(ctx context.Context, hook Hook, event Event) (result *Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			result, err = nil, &Panic{Value: p}
		}
	}()
	return hook.Run(ctx, event)
}

// Panic of a callback, as its failure
type Panic struct{ Value any }

func (p *Panic) Error() string { return fmt.Sprint(p.Value) }

// Runs one callback on a worker of its own under the time limit: skipped while it is suppressed after a timeout, while it still runs for the
// same call, or once too many of its runs hang; a run past the limit is left to finish on its own and the callback suppressed for a while
func (h *Hooks) bounded(ctx context.Context, point Point, hook Hook, event Event, limit time.Duration) (*Result, bool, error) {
	key := slot{point: point.Name, name: hook.Name}
	g := gate{slot: key}
	if c, ok := event.(called); ok {
		g.call = c.call()
		g.named = g.call != ""
	}
	token := &struct{}{}
	h.mu.Lock()
	until, suppressed := h.suppressed[key]
	if h.running[g] != nil || suppressed && until.After(time.Now()) {
		h.mu.Unlock()
		slog.Warn(fmt.Sprintf("Hook '%s' callback %s skipped after previous timeout or while still running", point.Name, hook.Name))
		return nil, true, nil
	}
	if hung := len(h.abandoned[key]); hung >= maxAbandoned {
		h.mu.Unlock()
		slog.Warn(fmt.Sprintf("Hook '%s' callback %s (%s) skipped: %d abandoned worker(s) still running — the plugin is hung; fix or disable it "+
			"(retried when a worker finishes)", point.Name, hook.Name, hook.Owner, hung))
		return nil, true, nil
	}
	delete(h.suppressed, key)
	h.running[g] = token
	h.mu.Unlock()
	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := run(ctx, hook, event)
		h.mu.Lock()
		if h.running[g] == token {
			delete(h.running, g)
			if hung := h.abandoned[key]; hung != nil {
				delete(hung, g)
				if len(hung) == 0 {
					delete(h.abandoned, key)
				}
			}
		}
		h.mu.Unlock()
		done <- outcome{result, err}
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.result, false, o.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-timer.C:
	}
	h.mu.Lock()
	h.suppressed[key] = time.Now().Add(suppression)
	if h.running[g] == token {
		if h.abandoned[key] == nil {
			h.abandoned[key] = map[gate]bool{}
		}
		h.abandoned[key][g] = true
	}
	h.mu.Unlock()
	slog.Warn(fmt.Sprintf("Hook '%s' callback %s timed out after %gs — skipping", point.Name, hook.Name, limit.Seconds()))
	return nil, true, nil
}

// Logs a callback's failure once at warning level, each identical one after it at debug level
func (h *Hooks) report(point, name string, event Event, err error) {
	message := strs.Head(err.Error(), 200)
	key := failure{point: point, name: name, kind: kind(err), message: message}
	h.mu.Lock()
	seen := h.reported[key]
	h.reported[key] = true
	h.mu.Unlock()
	if seen {
		slog.Debug(fmt.Sprintf("Hook '%s' callback %s raised again: %v", point, name, err))
		return
	}
	provided := strings.Join(Fields(event), ", ")
	if provided == "" {
		provided = "no fields"
	}
	slog.Warn(fmt.Sprintf("Hook '%s' callback %s raised: %v (hook provides: %s; identical failures are logged at DEBUG from now on)", point, name, err,
		provided))
}

// Names of the fields of the event as it is written, sorted
func Fields(event Event) []string {
	return slices.Sorted(maps.Keys(Payload(event)))
}

// Event as plain JSON data, the way a hook outside icebath reads it: its fields by name, with the version of the observer schema every event
// but a gateway platform event names; nil for an event that cannot be written
func Payload(event Event) map[string]any {
	data, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil
	}
	if _, set := fields["telemetry_schema_version"]; !set && event.Point() != GatewayPlatformEvent {
		fields["telemetry_schema_version"] = Schema
	}
	return fields
}

// Kind of an error as its type names it, the package and pointer left off
func kind(err error) string {
	name := strings.TrimLeft(fmt.Sprintf("%T", err), "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}
