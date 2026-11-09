package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	delegateDefaultMaxIterations = 250
	childCompressionCapMin       = 16000
)

var (
	delegateRoles          = map[string]bool{"leaf": true, "orchestrator": true}
	delegateControlActions = map[string]bool{"list": true, "steer": true, "stop": true}
)

func delegateLog() *slog.Logger { return slog.Default().With("logger", "tools.delegate_tool") }

// Python ValueError of the delegation family, the refusal a caller turns into a tool error string
type delegateValueError struct{ msg string }

func (e *delegateValueError) Error() string { return e.msg }

// Live transcript writer of one task, the delegation_live_log LiveTranscriptWriter
type delegationLiveWriter interface {
	Path() string
	Finalize(entry *pyjson.Object) error
	observe(eventType any, toolName, preview, args any, kwargs map[string]any)
	flushStream()
}

// Steer and wake authority of the session that issued the delegate_task call
type delegationOrigin struct {
	WakeSID                string
	UISessionID            string
	OwnerTransport         any
	OwnerSessionRecord     any
	SessionHistoryDelivery bool
}

// One built child with its task position and task dict
type delegateChild struct {
	Index int
	Task  map[string]any
	Child *AIAgent
}

// Everything a batch run needs, built on the calling goroutine before any child runs
type delegateBatch struct {
	TaskList     []map[string]any
	Children     []delegateChild
	Parent       *AIAgent
	Creds        map[string]any
	Context      any
	TopRole      string
	MaxChildren  int
	LiveDelegID  string
	LiveWriters  []delegationLiveWriter
	LivePaths    []string
	Origin       delegationOrigin
	OverallStart time.Time
	LiveHome     string
	Refused      []*pyjson.Object
	Group        *string
	UnitID       string
}

// Construction inputs of one child, the _build_child_agent keywords over one task route
type childBuildSpec struct {
	TaskIndex                int
	Goal                     any
	Context                  any
	Model                    any
	MaxIterations            any
	TaskCount                int
	Parent                   *AIAgent
	Role                     string
	OverrideProvider         any
	OverrideBaseURL          any
	OverrideAPIKey           any
	OverrideAPIMode          any
	OverrideRequestOverrides any
	OverrideACPCommand       any
	OverrideACPArgs          any
	OverrideServiceTier      any
	RoutingCfg               map[string]any
	OverrideReasoningEffort  any
	ProfileHome              string
}

// Profile home of the live transcripts from the parent's session db path, empty for the ambient resolve
func parentLiveHome(parent *AIAgent) string {
	if parent.sessionDB != nil {
		if p := parent.sessionDB.Path(); p != "" {
			return filepath.Dir(p)
		}
		delegateLog().Debug("delegate_task: parent _session_db.db_path is '' (not a str/Path); " +
			"live-transcript home pinning skipped, falling back to ambient " +
			"ICEBATH_HOME resolve (transcripts may land in a different profile, #91996)")
		return ""
	}
	delegateLog().Warn("delegate_task: parent agent exposes no _session_db; live-transcript " +
		"home pinning skipped, falling back to ambient ICEBATH_HOME resolve " +
		"(transcripts may land in a different profile, #91996)")
	return ""
}

// Leaf or orchestrator, an empty or unknown role becoming leaf with a warning for the unknown one
func normalizeDelegateRole(r any) string {
	norm := "leaf"
	if pyTruthy(r) {
		s, ok := r.(string)
		if !ok {
			s = fmt.Sprint(r)
		}
		norm = strings.ToLower(pystr.Strip(s))
	}
	if !delegateRoles[norm] {
		delegateLog().Warn(fmt.Sprintf("Unknown delegate_task role=%s, coercing to 'leaf'", pyrepr.Repr(r)))
		return "leaf"
	}
	return norm
}

// A delegated child never keeps the 1h cache tier, any other setting stays as configured
func applyChildCacheTTL(child *AIAgent) {
	if child.cacheTTL == "1h" {
		child.cacheTTL = "5m"
	}
}

// Validated delegation compression cap, nil when unset or not a token count of at least 16000
func childCompressionCapTokens(raw any) (*int, error) {
	invalid := func() (*int, error) {
		delegateLog().Warn(fmt.Sprintf("delegation.compression_threshold_tokens=%s is not a token count >= %d; ignoring it "+
			"(children keep the ratio trigger).", pyrepr.Repr(raw), childCompressionCapMin))
		return nil, nil
	}
	var n int
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case bool:
		if !v {
			return nil, nil
		}
		return invalid()
	case int:
		n = v
	case int64:
		n = int(v)
	case uint64:
		n = int(v)
	case float64:
		if math.IsNaN(v) {
			return nil, &delegateValueError{"cannot convert float NaN to integer"}
		}
		if math.IsInf(v, 0) {
			return nil, errors.New("cannot convert float infinity to integer")
		}
		if v == 0 {
			return nil, nil
		}
		n = int(math.Trunc(v))
	default:
		return invalid()
	}
	if n == 0 {
		if _, isFloat := raw.(float64); !isFloat {
			return nil, nil
		}
	}
	if n < childCompressionCapMin {
		return invalid()
	}
	return &n, nil
}

// Lowers the child compressor trigger to the delegation cap, never raising an existing global cap
func applyChildCompressionCap(child *AIAgent, delegationCfg map[string]any) error {
	cc, ok := child.contextCompressor.(*ContextCompressor)
	if !ok || cc == nil {
		return nil
	}
	capTokens, err := childCompressionCapTokens(delegationCfg["compression_threshold_tokens"])
	if err != nil || capTokens == nil {
		return err
	}
	v := *capTokens
	if existing := cc.thresholdTokensCap; existing != nil && *existing > 0 {
		v = min(v, *existing)
	}
	cc.thresholdTokensCap = &v
	if cc.thresholdTokens != nil {
		cc.applyThresholdTokensCap()
	}
	return nil
}

// Child construction overrides of a resolved credential bundle and the config block owning its route
func credentialOverrides(creds, routingCfg map[string]any) childBuildSpec {
	return childBuildSpec{
		OverrideProvider: creds["provider"], OverrideBaseURL: creds["base_url"], OverrideAPIKey: creds["api_key"],
		OverrideAPIMode: creds["api_mode"], OverrideRequestOverrides: creds["request_overrides"],
		OverrideACPCommand: creds["command"], OverrideACPArgs: creds["args"], OverrideServiceTier: creds["service_tier"],
		RoutingCfg: routingCfg,
	}
}

// Child construction overrides of one task route with its reasoning effort and profile home
func routeOverrides(route *tools.TaskRoute) childBuildSpec {
	spec := credentialOverrides(route.Creds, route.RoutingCfg)
	spec.OverrideReasoningEffort, spec.ProfileHome = route.ReasoningEffort, route.ProfileHome
	return spec
}

// Builds every routable child in task order, a refusal string after closing the siblings already built
func buildDelegateChildren(ctx context.Context, taskList []map[string]any, taskSchemas []any, routes []*tools.TaskRoute,
	topRole string, maxIterations any, parent *AIAgent, liveDelegID string, liveWriters []delegationLiveWriter,
	taskImages [][]string) ([]delegateChild, string, error) {
	var children []delegateChild
	for i, t := range taskList {
		route := routes[i]
		if route.Error != "" {
			continue
		}
		var taskSchema any
		if i < len(taskSchemas) {
			taskSchema = taskSchemas[i]
		}
		childContext := t["context"]
		if taskSchema != nil {
			text, _ := childContext.(string)
			childContext = tools.AppendOutputContract(text, taskSchema)
		}
		role := t["role"]
		if !pyTruthy(role) {
			role = topRole
		}
		spec := routeOverrides(route)
		spec.TaskIndex, spec.Goal, spec.Context, spec.Model = i, t["goal"], childContext, route.Creds["model"]
		spec.MaxIterations, spec.TaskCount, spec.Parent, spec.Role = maxIterations, len(taskList), parent, normalizeDelegateRole(role)
		child, err := buildChildPreservingParentToolsFn(ctx, spec)
		if err != nil {
			var refusal *delegateValueError
			if !errors.As(err, &refusal) {
				return nil, "", err
			}
			for _, built := range children {
				detachChild(parent, built.Child)
				closeChild(built.Child, "Failed to close child after a refused sibling")
			}
			return nil, refusal.Error(), nil
		}
		tools.PinChildServiceTier(child, route.Creds)
		if taskSchema != nil {
			child.delegateOutputSchema = taskSchema
		}
		if i < len(taskImages) && len(taskImages[i]) > 0 {
			child.delegateImages = taskImages[i]
		}
		if i < len(liveWriters) && liveWriters[i] != nil {
			child.toolProgressCallback = wrapProgressCallback(child.toolProgressCallback, liveWriters[i])
			child.liveTranscriptPath = liveWriters[i].Path()
		}
		if report := route.Report(); report != nil {
			child.delegateRoute = report
		}
		if liveDelegID != "" {
			child.delegationID = liveDelegID
			if child.progressIdentityRef != nil {
				child.progressIdentityRef["delegation_id"] = liveDelegID
			}
		}
		children = append(children, delegateChild{Index: i, Task: t, Child: child})
	}
	return children, "", nil
}

// Per task routes, an internal caller's credentials block owning every task, the active provider otherwise
func routesFor(ctx context.Context, taskList []map[string]any, cfg, credentialsCfg map[string]any, parent *AIAgent) ([]*tools.TaskRoute, string, error) {
	if credentialsCfg == nil {
		return resolveTaskRoutes(ctx, taskList, cfg, parent)
	}
	route, err := routeForConfig(ctx, credentialsCfg, parent)
	if err != nil {
		var refusal *delegateValueError
		if errors.As(err, &refusal) {
			return nil, refusal.Error(), nil
		}
		return nil, "", err
	}
	routes := make([]*tools.TaskRoute, len(taskList))
	for i := range routes {
		routes[i] = route
	}
	return routes, "", nil
}

// Finalizes the transcripts of refused tasks and marks them settled in the manifest
func settleRefusedTranscripts(refused []*pyjson.Object, liveDelegID string, liveWriters []delegationLiveWriter, liveHome string) {
	for _, entry := range refused {
		raw, _ := entry.Get("task_index")
		idx, _ := raw.(int)
		if idx < len(liveWriters) && liveWriters[idx] != nil {
			if err := liveWriters[idx].Finalize(entry); err != nil {
				delegateLog().Debug("Live transcript finalize failed", "error", err)
			}
		}
	}
	updateManifestStatuses(liveDelegID, refused, liveHome, false)
}

// Charges the requested children against a one shot session's total, the refusal text once it is spent
func oneshotSpawnBudget(ctx context.Context, parent *AIAgent, requested int) string {
	if !tools.IsSingleQuerySession(parent.env) {
		return ""
	}
	limit := tools.OneshotMaxChildren(ctx)
	if limit <= 0 {
		return ""
	}
	for {
		spent := atomic.LoadInt64(&parent.oneshotChildrenSpawned)
		if int(spent)+requested > limit {
			return fmt.Sprintf("Delegation budget for this one-shot run is exhausted (%d/%d subagents used; "+
				"delegation.oneshot_max_children). Do the remaining work yourself in this session — reviewing "+
				"your own diff and running the tests inline is expected here, not a delegated review.", spent, limit)
		}
		if atomic.CompareAndSwapInt64(&parent.oneshotChildrenSpawned, spent, spent+int64(requested)) {
			return ""
		}
	}
}

func delegateToolError(msg string) string { return tools.ErrorResult(errors.New(msg)) }

// Spawns children for a goal or task batch, or runs a list, steer or stop control action
func delegateTask(ctx context.Context, parent *AIAgent, kwargs map[string]any) (string, error) {
	if parent == nil {
		return delegateToolError("delegate_task requires a parent agent context."), nil
	}
	action := kwargs["action"]
	actionText := ""
	if action != nil {
		s, ok := action.(string)
		if !ok {
			return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(action))
		}
		actionText = s
	}
	normalized := strings.ToLower(pystr.Strip(actionText))
	if delegateControlActions[normalized] {
		return handleDelegateControlAction(normalized, kwargs["subagent_id"], kwargs["message"], parent)
	}
	if normalized != "" && normalized != "spawn" {
		return delegateToolError(fmt.Sprintf("Unknown action '%s'. Use spawn (default), list, steer, or stop.", actionText)), nil
	}
	if isSpawnPaused() {
		return delegateToolError("Delegation spawning is paused. Clear the pause via the TUI " +
			"(`p` in /agents) or the `delegation.pause` RPC before retrying."), nil
	}
	topRole := normalizeDelegateRole(kwargs["role"])
	background := false
	if v := kwargs["background"]; v != nil {
		background = isTruthyValue(v, false)
	}
	depth := parent.delegateDepth
	maxSpawn := tools.MaxSpawnDepth(ctx)
	if depth >= maxSpawn {
		return delegateToolError(fmt.Sprintf("Delegation depth limit reached (depth=%d, max_spawn_depth=%d). Raise "+
			"delegation.max_spawn_depth in config.yaml if deeper nesting is required (no hard ceiling, but each level "+
			"multiplies API cost).", depth, maxSpawn)), nil
	}
	cfg := tools.LoadDelegationConfig(ctx)
	defaultMaxIter, ok := cfg["max_iterations"]
	if !ok {
		defaultMaxIter = delegateDefaultMaxIterations
	}
	if mi := kwargs["max_iterations"]; mi != nil && fmt.Sprint(mi) != fmt.Sprint(defaultMaxIter) {
		delegateLog().Debug(fmt.Sprintf("delegate_task: ignoring caller-supplied max_iterations=%v; using delegation.max_iterations=%v from config",
			mi, defaultMaxIter))
	}
	maxChildren := tools.MaxConcurrentChildren(ctx)
	taskList, msg, err := tools.NormalizeTaskList(kwargs["goal"], kwargs["context"], kwargs["tasks"], kwargs["output_schema"], topRole, maxChildren)
	if err != nil {
		return "", err
	}
	var taskSchemas []any
	var taskImages [][]string
	var routes []*tools.TaskRoute
	if msg == "" {
		taskSchemas, msg = tools.CoerceTaskSchemas(taskList, kwargs["output_schema"])
	}
	if msg == "" {
		taskImages, msg = tools.CoerceTaskImages(taskList, kwargs["images"])
	}
	if msg == "" {
		credentialsCfg, _ := kwargs["credentials_cfg"].(map[string]any)
		if routes, msg, err = routesFor(ctx, taskList, cfg, credentialsCfg, parent); err != nil {
			return "", err
		}
	}
	if msg != "" {
		return delegateToolError(msg), nil
	}
	var runnable []*tools.TaskRoute
	var refused []*pyjson.Object
	var refusals []string
	for i, r := range routes {
		if r.Error == "" {
			runnable = append(runnable, r)
			continue
		}
		refused = append(refused, r.RefusedEntry(i))
		if !slices.Contains(refusals, r.Error) {
			refusals = append(refusals, r.Error)
		}
	}
	if len(runnable) == 0 {
		return delegateToolError(strings.Join(refusals, " ")), nil
	}
	creds := runnable[0].Creds
	for _, r := range runnable {
		if !reflect.DeepEqual(r.Creds, runnable[0].Creds) {
			creds = map[string]any{}
			for k := range runnable[0].Creds {
				creds[k] = nil
			}
			break
		}
	}
	if refusal := oneshotSpawnBudget(ctx, parent, len(runnable)); refusal != "" {
		return delegateToolError(refusal), nil
	}
	overallStart := time.Now()
	liveHome := parentLiveHome(parent)
	liveDelegID, liveWriters, livePaths := createLiveTranscripts(ctx, taskList, kwargs["context"], creds["model"], creds["provider"], liveHome)
	if len(refused) > 0 {
		settleRefusedTranscripts(refused, liveDelegID, liveWriters, liveHome)
	}
	announceBatch(parent, len(taskList), liveDelegID)
	origin := captureOrigin(ctx)
	children, msg, err := buildDelegateChildren(ctx, taskList, taskSchemas, routes, topRole, defaultMaxIter, parent,
		liveDelegID, liveWriters, taskImages)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return delegateToolError(msg), nil
	}
	batch := &delegateBatch{
		TaskList: taskList, Children: children, Parent: parent, Creds: creds, Context: kwargs["context"], TopRole: topRole,
		MaxChildren: maxChildren, LiveDelegID: liveDelegID, LiveWriters: liveWriters, LivePaths: livePaths, Origin: origin,
		OverallStart: overallStart, LiveHome: liveHome, Refused: refused,
	}
	return runDelegateBatchFn(ctx, batch, background)
}

// Batch runner of delegate_task, swapped by tests that read the built batch
var runDelegateBatchFn = runDelegateBatch

// Whether the carried delegate_task is the joined variant so top level delegations join inside the call
func sessionJoinsDelegation(a *AIAgent) bool {
	if value, pinned := a.oneshotSession.Pinned(); pinned && value {
		return false
	}
	for _, t := range a.tools {
		m, _ := t.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn != nil && fn["name"] == "delegate_task" {
			description, _ := fn["description"].(string)
			return strings.Contains(description, tools.OneshotDescriptionDelivery)
		}
	}
	return false
}

// Routes the registry delegate_task handler onto the agent delegate entry point
func init() {
	tools.DelegateTask = func(ctx context.Context, env *tools.Env, req tools.DelegateTaskRequest) string {
		parent, _ := req.ParentAgent.(*AIAgent)
		kwargs := map[string]any{
			"goal": req.Goal, "context": req.Context, "tasks": req.Tasks, "max_iterations": req.MaxIterations,
			"role": req.Role, "output_schema": req.OutputSchema, "images": req.Images, "action": req.Action,
			"subagent_id": req.SubagentID, "message": req.Message,
		}
		if req.Background != nil {
			kwargs["background"] = *req.Background
		}
		if req.CredentialsCfg != nil {
			kwargs["credentials_cfg"] = req.CredentialsCfg
		}
		out, err := delegateTask(ctx, parent, kwargs)
		if err != nil {
			return tools.ErrorResult(err)
		}
		return out
	}
}
