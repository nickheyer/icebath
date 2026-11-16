package kanban

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// pyException is a Python exception of a named class this package has no dedicated type for
type pyException struct{ Class, Msg string }

func (e *pyException) Error() string      { return e.Msg }
func (e *pyException) PyTypeName() string { return e.Class }

// None for the unassign spellings none, dash and null
func noneProfile(value string) *string {
	switch pystr.Lower(value) {
	case "none", "-", "null":
		return nil
	}
	return &value
}

// Decoded --metadata JSON object with the exit code of a refusal
func (c *kanbanCLI) parseMetadataFlag(raw *string) (*pyjson.Object, int) {
	if raw == nil || *raw == "" {
		return nil, 0
	}
	decoded, err := pyjson.Loads(*raw)
	if err != nil {
		return nil, cliErr(c.errw, "kanban: --metadata: "+err.Error(), 2)
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, cliErr(c.errw, "kanban: --metadata: must be a JSON object", 2)
	}
	return obj, 0
}

// The --state-type and --state-name pair, both or neither, with the exit code of a refusal
func (c *kanbanCLI) runStateKwargs(cmd string) (stateType, stateName *string, rc int) {
	st, sn := nsOptStr(c.ns, "state_type"), nsOptStr(c.ns, "state_name")
	if (st == nil) != (sn == nil) {
		return nil, nil, cliErr(c.errw, fmt.Sprintf("kanban %s: pass both --state-type and --state-name, or omit both", cmd), 2)
	}
	return st, sn, 0
}

// argumentTypeError is the argparse ArgumentTypeError a malformed flag raises
type argumentTypeError struct{ msg string }

func (e *argumentTypeError) Error() string { return e.msg }

// Workspace kind and optional path of a --workspace value, both nil when omitted
func parseWorkspaceFlag(value *string) (*string, *string, error) {
	if value == nil || *value == "" {
		return nil, nil, nil
	}
	v := pystr.Strip(*value)
	if v == "scratch" || v == "worktree" {
		return &v, nil, nil
	}
	for _, pair := range [][2]string{{"dir:", "dir"}, {"worktree:", "worktree"}} {
		rest, ok := strings.CutPrefix(v, pair[0])
		if !ok {
			continue
		}
		path := pystr.Strip(rest)
		if path == "" {
			return nil, nil, &argumentTypeError{"--workspace " + pair[0] + " requires a path after the colon"}
		}
		kind, expanded := pair[1], pypath.Expanduser(path)
		return &kind, &expanded, nil
	}
	return nil, nil, &argumentTypeError{"unknown --workspace value " + pyrepr.Repr(*value) + ": use scratch, worktree, worktree:<path>, or dir:<path>"}
}

// Normalized optional --branch name, refusing blank, dash leading and whitespace names
func parseBranchFlag(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	branch := pystr.Strip(*value)
	if branch == "" {
		return nil, &argumentTypeError{"--branch requires a non-empty name"}
	}
	if strings.HasPrefix(branch, "-") {
		return nil, &argumentTypeError{"--branch must not start with '-'"}
	}
	for _, r := range branch {
		if pystr.IsSpace(r) {
			return nil, &argumentTypeError{"--branch must not contain whitespace"}
		}
	}
	return &branch, nil
}

// Whether a gateway will dispatch ready work for the home, with guidance when none will, failing open
func CheckDispatcherPresence(ctx context.Context, icebathHome string) (running bool, message string) {
	defer func() {
		if recover() != nil {
			running, message = true, ""
		}
	}()
	pid, probeError := gatewayDispatch().Liveness(ctx, icebathHome)
	if probeError {
		return true, ""
	}
	dispatchInGateway := true
	if section, ok := kanbanSection(ctx, false); ok {
		if v, present := section["dispatch_in_gateway"]; present {
			dispatchInGateway = pyrepr.Truthy(v)
		}
	}
	if pyrepr.Truthy(pid) && dispatchInGateway {
		return true, fmt.Sprintf("gateway pid=%s, dispatch enabled", pyrepr.Str(pid))
	}
	if pyrepr.Truthy(pid) {
		return false, "Gateway is running but kanban.dispatch_in_gateway=false in " +
			"config.yaml — the task will sit in 'ready' until you flip it " +
			"back on and restart the gateway, OR run the legacy " +
			"standalone daemon (`icebath kanban daemon --force`)."
	}
	return false, "No gateway is running — the task will sit in 'ready' until you " +
		"start it. Run:\n    icebath gateway start\n" +
		"The gateway hosts an embedded dispatcher (tick interval 60s by " +
		"default); your task will be picked up on the next tick after " +
		"the gateway comes up."
}

// Entry point of icebath kanban, returning a shell style exit code
func KanbanCommand(ctx context.Context, ns map[string]any, w, errw io.Writer) (int, error) {
	c := &kanbanCLI{ctx: ctx, ns: ns, w: w, errw: errw}
	action := nsStr(ns, "kanban_action")
	if action == "" {
		if parser, ok := ns["_kanban_parser"].(interface{ Help() error }); ok && parser != nil {
			return 0, parser.Help()
		}
		fmt.Fprintln(errw, "usage: icebath kanban <action> [options]\nRun 'icebath kanban --help' for the full list of actions.")
		return 0, nil
	}
	if c.isDelegatedChildCLIMutation() {
		return cliErr(errw, "kanban: delegate_task child contexts cannot mutate Kanban tasks via the CLI", 1), nil
	}
	if action == "boards" {
		return c.dispatchBoards()
	}
	if override := nsStr(ns, "board"); override != "" {
		normed, err := normalizeBoardSlug(&override)
		if err != nil {
			return cliErr(errw, "kanban: "+pyrepr.ExceptionMessage(err), 2), nil
		}
		if normed == "" {
			return cliErr(errw, "kanban: --board requires a slug", 2), nil
		}
		if normed != DefaultBoard && !BoardExists(&normed) {
			return cliErr(errw, fmt.Sprintf("kanban: board %s does not exist. Create it with `icebath kanban boards create %s`.", pyrepr.Repr(normed), normed), 1), nil
		}
		c.ctx = ScopedCurrentBoard(c.ctx, normed)
	}
	if action == "repair" {
		return c.cmdRepair()
	}
	if _, err := InitDB(c.ctx, "", nil); err != nil {
		return cliErr(errw, "kanban: could not initialize database: "+excText(err), 1), nil
	}
	handler, ok := c.handlers()[action]
	if !ok {
		return cliErr(errw, "kanban: unknown action "+pyrepr.Repr(action), 2), nil
	}
	rc, err := handler()
	if err != nil {
		if caughtByKanbanCommand(err) {
			return cliErr(errw, "kanban: "+excText(err), 1), nil
		}
		return 0, err
	}
	return rc, nil
}

// Whether an error is a ValueError, RuntimeError or PermissionError the command reports instead of raising
func caughtByKanbanCommand(err error) bool {
	var ve *ValueError
	var re *RuntimeError
	var pe *PermissionError
	if errors.As(err, &ve) || errors.As(err, &re) || errors.As(err, &pe) || errors.Is(err, fs.ErrPermission) {
		return true
	}
	switch pyrepr.ExceptionName(err) {
	case "ValueError", "RuntimeError", "PermissionError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError",
		"JSONDecodeError", "NotImplementedError", "RecursionError", "PythonFinalizationError":
		return true
	}
	return false
}

// Action handlers by name and alias
func (c *kanbanCLI) handlers() map[string]func() (int, error) {
	return map[string]func() (int, error){
		"init": c.cmdInit, "create": c.cmdCreate, "swarm": c.cmdSwarm,
		"list": c.cmdList, "ls": c.cmdList, "show": c.cmdShow,
		"assign": c.cmdAssign, "set-model": c.cmdSetModel,
		"reclaim": c.cmdReclaim, "reassign": c.cmdReassign,
		"diagnostics": c.cmdDiagnostics, "diag": c.cmdDiagnostics,
		"link": c.cmdLink, "unlink": c.cmdUnlink, "claim": c.cmdClaim,
		"comment": c.cmdComment, "attach": c.cmdAttach,
		"attachments": c.cmdAttachments, "attach-rm": c.cmdAttachRm,
		"complete": c.cmdComplete, "edit": c.cmdEdit, "block": c.cmdBlock,
		"schedule": c.cmdSchedule, "unblock": c.cmdUnblock,
		"request-review": c.cmdRequestReview, "request-changes": c.cmdRequestChanges,
		"reopen-review": c.cmdReopenReview, "promote": c.cmdPromote,
		"archive": c.cmdArchive, "tail": c.cmdTail, "dispatch": c.cmdDispatch,
		"daemon": c.cmdDaemon, "watch": c.cmdWatch, "stats": c.cmdStats,
		"log": c.cmdLog, "runs": c.cmdRuns, "heartbeat": c.cmdHeartbeat,
		"assignees": c.cmdAssignees, "notify-subscribe": c.cmdNotifySubscribe,
		"notify-list": c.cmdNotifyList, "notify-unsubscribe": c.cmdNotifyUnsubscribe,
		"context": c.cmdContext, "specify": c.cmdSpecify, "decompose": c.cmdDecompose,
		"gc": c.cmdGC,
	}
}

// Author name for an interactive CLI call, the current profile else user
func (c *kanbanCLI) profileAuthor() string {
	if name := profiles.CurrentProfileName(c.ctx); name != "" {
		return name
	}
	return "user"
}

var delegatedChildDeniedActions = map[string]bool{
	"init": true, "create": true, "swarm": true, "assign": true, "reclaim": true, "reassign": true, "link": true, "unlink": true,
	"claim": true, "comment": true, "attach": true, "attach-rm": true, "complete": true, "edit": true, "block": true,
	"schedule": true, "unblock": true, "promote": true, "archive": true, "dispatch": true, "daemon": true, "repair": true,
	"heartbeat": true, "notify-subscribe": true, "notify-unsubscribe": true, "specify": true, "decompose": true,
	"request-review": true, "request-changes": true, "reopen-review": true,
	"gc": true,
}

var delegatedChildDeniedBoardActions = map[string]bool{
	"create": true, "new": true, "rm": true, "remove": true, "delete": true, "switch": true, "use": true, "rename": true,
	"set-default-workdir": true, "import": true,
}

// Whether a delegated child context is attempting a mutating kanban action on a fenced board
func (c *kanbanCLI) isDelegatedChildCLIMutation() bool {
	action := nsStr(c.ns, "kanban_action")
	if action == "boards" {
		sub := nsStr(c.ns, "boards_action")
		if sub == "" {
			sub = "list"
		}
		if !delegatedChildDeniedBoardActions[sub] {
			return false
		}
	} else if !delegatedChildDeniedActions[action] {
		return false
	}
	if tools.KanbanPathIsFenced(c.ctx, KanbanHome()) {
		return true
	}
	dbPath, err := KanbanDBPath(c.ctx, nil)
	return err == nil && tools.KanbanPathIsFenced(c.ctx, dbPath)
}

// Joined free text words stripped, nil when none were given
func joinedWords(words []string) *string {
	if len(words) == 0 {
		return nil
	}
	s := pystr.Strip(strings.Join(words, " "))
	return &s
}

// Stripped value with empty as nil
func strippedOrNone(value *string) *string {
	if value == nil {
		return nil
	}
	s := pystr.Strip(*value)
	if s == "" {
		return nil
	}
	return &s
}

// Prints done with exit 0 or fail to stderr with exit 1
func (c *kanbanCLI) okOrErr(ok bool, fail, done string) int {
	if !ok {
		return cliErr(c.errw, fail, 1)
	}
	fmt.Fprintln(c.w, done)
	return 0
}

// Positional task id followed by the --ids extras
func (c *kanbanCLI) bulkIDs() []string {
	return append([]string{nsStr(c.ns, "task_id")}, nsStrs(c.ns, "ids")...)
}

// task_ids of the namespace with exit 1 when none were given
func (c *kanbanCLI) requireIDs() ([]string, int) {
	ids := nsStrs(c.ns, "task_ids")
	if len(ids) == 0 {
		return nil, cliErr(c.errw, "at least one task_id is required", 1)
	}
	return ids, 0
}

var durationUnits = map[byte]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}

// Seconds of a 30s, 5m, 2h, 1d or bare integer duration, nil for empty input
func parseDuration(val any) (*int64, error) {
	if val == nil || val == "" {
		return nil, nil
	}
	s := pystr.Lower(pystr.Strip(pyrepr.Str(val)))
	if n, ok := pyint.Parse(s); ok {
		v := int64(math.MaxInt64)
		if n.IsInt64() {
			v = n.Int64()
		}
		return &v, nil
	}
	if s == "" {
		return nil, &ValueError{"malformed duration " + pyrepr.Repr(val) + " (expected 30s, 5m, 2h, 1d, or a number)"}
	}
	unit, ok := durationUnits[s[len(s)-1]]
	if !ok {
		return nil, &ValueError{"malformed duration " + pyrepr.Repr(val) + " (expected 30s, 5m, 2h, 1d, or a number)"}
	}
	n, err := pyint.ParseFloat(s[:len(s)-1])
	if err != nil {
		return nil, &ValueError{"malformed duration " + pyrepr.Repr(val)}
	}
	total := n * unit
	if math.IsNaN(total) {
		return nil, &ValueError{"cannot convert float NaN to integer"}
	}
	if math.IsInf(total, 0) {
		return nil, &pyException{"OverflowError", "cannot convert float infinity to integer"}
	}
	v := int64(total)
	return &v, nil
}

// Creates the database and reports the profiles already addressable as assignees
func (c *kanbanCLI) cmdInit() (int, error) {
	path, err := InitDB(c.ctx, "", nil)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(c.w, "Kanban DB initialized at %s\n\n", path)
	if found := ListProfilesOnDisk(); len(found) > 0 {
		fmt.Fprintf(c.w, "Discovered %d profile(s) on disk; any of these can be an --assignee:\n", len(found))
		for _, name := range found {
			fmt.Fprintf(c.w, "  %s\n", name)
		}
	} else {
		fmt.Fprintln(c.w, "No profiles found under ~/.icebath/profiles/.\nCreate one with `icebath -p <name> setup` before assigning tasks.")
	}
	fmt.Fprintln(c.w, "\nNext step: start the gateway so ready tasks actually get picked up.\n"+
		"  icebath gateway start\n\n"+
		"The gateway hosts an embedded dispatcher that ticks every 60 seconds\n"+
		"by default (config: kanban.dispatch_interval_seconds). Without a\n"+
		"running gateway, tasks stay in 'ready' forever.")
	return 0, nil
}

// Records a worker heartbeat on a running task
func (c *kanbanCLI) cmdHeartbeat() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var ok bool
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		runID, err := workerRunIDFor(tid)
		if err != nil {
			return err
		}
		ok, err = HeartbeatWorkerNote(c.ctx, conn, tid, c.ns["note"], runID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return c.okOrErr(ok, "cannot heartbeat "+tid+" (not running?)", "Heartbeat recorded for "+tid), nil
}

// Lists known assignees with on disk presence and per status counts
func (c *kanbanCLI) cmdAssignees() (int, error) {
	var data []*pyjson.Object
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		data, err = KnownAssignees(c.ctx, conn)
		return err
	}); err != nil {
		return 0, err
	}
	list := make([]any, 0, len(data))
	for _, d := range data {
		list = append(list, d)
	}
	if done, err := jsonOut(c, list, false); done || err != nil {
		return 0, err
	}
	if len(data) == 0 {
		fmt.Fprintln(c.w, "(no assignees — create a profile with `icebath -p <name> setup`)")
		return 0, nil
	}
	fmt.Fprintf(c.w, "%-20s  %-8s  COUNTS\n", "NAME", "ON DISK")
	for _, entry := range data {
		onDisk := "no"
		if v, _ := entry.Get("on_disk"); pyrepr.Truthy(v) {
			onDisk = "yes"
		}
		counts := map[string]int64{}
		if v, _ := entry.Get("counts"); v != nil {
			if obj, ok := v.(*pyjson.Object); ok {
				counts = countsMap(obj)
			}
		}
		fmt.Fprintf(c.w, "%-20s  %-8s  %s\n", metaText(entry, "name"), onDisk, fmtCounts(counts, "(idle)"))
	}
	return 0, nil
}

// Body of --body or --body-file with the exit code of a refusal
func (c *kanbanCLI) createBody() (*string, int, error) {
	body, bodyFile := nsOptStr(c.ns, "body"), nsOptStr(c.ns, "body_file")
	if body != nil && bodyFile != nil {
		return nil, cliErr(c.errw, "kanban: --body and --body-file are mutually exclusive", 2), nil
	}
	if bodyFile == nil {
		return body, 0, nil
	}
	var raw []byte
	var err error
	if *bodyFile == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*bodyFile)
	}
	if err != nil {
		return nil, cliErr(c.errw, "kanban: --body-file: "+excText(err), 2), nil
	}
	var text string
	if *bodyFile == "-" {
		text = string(raw)
	} else if text, err = pystr.DecodeUTF8Sig(raw); err != nil {
		return nil, 0, err
	}
	return &text, 0, nil
}

// Creates one task from the create flags and warns when nothing will dispatch it
func (c *kanbanCLI) cmdCreate() (int, error) {
	body, rc, err := c.createBody()
	if err != nil || rc != 0 {
		return rc, err
	}
	wsKind, wsPath, err := parseWorkspaceFlag(nsOptStr(c.ns, "workspace"))
	var branch *string
	if err == nil {
		branch, err = parseBranchFlag(nsOptStr(c.ns, "branch"))
	}
	if err != nil {
		return cliErr(c.errw, "kanban: "+err.Error(), 2), nil
	}
	if branch != nil && (wsKind == nil || *wsKind != "worktree") {
		return cliErr(c.errw, "kanban: --branch is only valid with --workspace worktree", 2), nil
	}
	maxRuntime, err := parseDuration(c.ns["max_runtime"])
	if err != nil {
		var ve *ValueError
		if errors.As(err, &ve) {
			return cliErr(c.errw, "kanban: --max-runtime: "+err.Error(), 2), nil
		}
		return 0, err
	}
	maxRetries := nsOptInt(c.ns, "max_retries")
	if maxRetries != nil && *maxRetries < 1 {
		return cliErr(c.errw, fmt.Sprintf("kanban: --max-retries must be >= 1 (got %d); use 1 to trip on the first failure.", *maxRetries), 2), nil
	}
	createdBy := nsStr(c.ns, "created_by")
	if createdBy == "" {
		createdBy = c.profileAuthor()
	}
	var creatorTaskID *string
	if isDispatcherOwnedWorker(c.ctx, tools.ToolEnvFrom(c.ctx)) {
		if v, ok := os.LookupEnv("ICEBATH_KANBAN_TASK"); ok {
			creatorTaskID = &v
		}
	}
	initialStatus := "running"
	if v, ok := c.ns["initial_status"].(string); ok {
		initialStatus = v
	}
	var skills []string
	if s := nsStrs(c.ns, "skills"); len(s) > 0 {
		skills = s
	}
	priority := int64(0)
	if p := nsOptInt(c.ns, "priority"); p != nil {
		priority = *p
	}
	var task *Task
	var taskID string
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		taskID, err = CreateTask(c.ctx, conn, CreateTaskOptions{
			Title: nsStr(c.ns, "title"), Body: body, Assignee: nsOptStr(c.ns, "assignee"), CreatedBy: &createdBy,
			WorkspaceKind: wsKind, WorkspacePath: wsPath, BranchName: branch, ProjectID: nsOptStr(c.ns, "project"),
			Tenant: nsOptStr(c.ns, "tenant"), Priority: priority, Parents: nsStrs(c.ns, "parent"), Triage: nsBool(c.ns, "triage"),
			IdempotencyKey: nsOptStr(c.ns, "idempotency_key"), MaxRuntimeSeconds: maxRuntime, Skills: skills,
			MaxRetries: maxRetries, ModelOverride: nsOptStr(c.ns, "model_override"), ProviderOverride: nsOptStr(c.ns, "provider_override"),
			GoalMode: nsBool(c.ns, "goal_mode"), GoalMaxTurns: nsOptInt(c.ns, "goal_max_turns"),
			CompletionContract: nsOptStr(c.ns, "completion_contract"), InitialStatus: initialStatus, CreatorTaskID: creatorTaskID,
		})
		if err != nil {
			return err
		}
		task, err = GetTask(c.ctx, conn, taskID)
		return err
	}); err != nil {
		return 0, err
	}
	if nsBool(c.ns, "json") {
		return 0, printJSON(c.w, taskToDict(task), false)
	}
	assignee := "-"
	if task.Assignee != nil && *task.Assignee != "" {
		assignee = *task.Assignee
	}
	fmt.Fprintf(c.w, "Created %s  (%s, assignee=%s)\n", taskID, task.Status, assignee)
	if task.Status == "ready" && task.Assignee != nil && *task.Assignee != "" {
		if running, message := CheckDispatcherPresence(c.ctx, ""); !running && message != "" {
			fmt.Fprintf(c.errw, "\n⚠  %s\n", message)
		}
	}
	return 0, nil
}

// Creates a swarm graph from the --worker specs
func (c *kanbanCLI) cmdSwarm() (int, error) {
	var workers []SwarmWorkerSpec
	for _, raw := range nsStrs(c.ns, "worker") {
		spec, err := ParseWorkerArg(raw)
		if err != nil {
			return cliErr(c.errw, "kanban swarm: "+err.Error(), 2), nil
		}
		workers = append(workers, spec)
	}
	if len(workers) == 0 {
		return cliErr(c.errw, "kanban swarm: at least one --worker is required", 2), nil
	}
	o := DefaultSwarmOptions()
	o.Goal, o.Workers, o.VerifierAssignee, o.SynthesizerAssignee = nsStr(c.ns, "goal"), workers, nsStr(c.ns, "verifier"), nsStr(c.ns, "synthesizer")
	o.Tenant, o.IdempotencyKey = nsOptStr(c.ns, "tenant"), nsOptStr(c.ns, "idempotency_key")
	if o.CreatedBy = nsStr(c.ns, "created_by"); o.CreatedBy == "" {
		o.CreatedBy = c.profileAuthor()
	}
	if p := nsOptInt(c.ns, "priority"); p != nil {
		o.Priority = *p
	}
	var created *SwarmCreated
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		created, err = CreateSwarm(c.ctx, conn, o)
		return err
	}); err != nil {
		return 0, err
	}
	if nsBool(c.ns, "json") {
		return 0, printJSON(c.w, created.AsDict(), false)
	}
	fmt.Fprintf(c.w, "Swarm root: %s\nWorkers: %s\nVerifier: %s\nSynthesizer: %s\n", created.RootID, strings.Join(created.WorkerIDs, ", "), created.VerifierID, created.SynthesizerID)
	return 0, nil
}

// Lists tasks after a cheap readiness recompute, naming the board when several exist
func (c *kanbanCLI) cmdList() (int, error) {
	assignee := nsOptStr(c.ns, "assignee")
	if nsBool(c.ns, "mine") && (assignee == nil || *assignee == "") {
		me := c.profileAuthor()
		assignee = &me
	}
	var tasks []*Task
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		if _, err := RecomputeReady(c.ctx, conn, nil); err != nil {
			return err
		}
		var err error
		tasks, err = ListTasks(c.ctx, conn, ListTasksOptions{
			Assignee: assignee, Status: nsOptStr(c.ns, "status"), Tenant: nsOptStr(c.ns, "tenant"), SessionID: nsOptStr(c.ns, "session"),
			IncludeArchived: nsBool(c.ns, "archived"), OrderBy: nsOptStr(c.ns, "sort"),
			WorkflowTemplateID: nsOptStr(c.ns, "workflow_template_id"), CurrentStepKey: nsOptStr(c.ns, "current_step_key"),
		})
		return err
	}); err != nil {
		return 0, err
	}
	dicts := make([]any, 0, len(tasks))
	for _, t := range tasks {
		dicts = append(dicts, taskToDict(t))
	}
	if done, err := jsonOut(c, dicts, false); done || err != nil {
		return 0, err
	}
	if all, err := ListBoards(false); err == nil && len(all) > 1 {
		other := len(all) - 1
		plural := "s"
		if other == 1 {
			plural = ""
		}
		fmt.Fprintf(c.w, "Board: %s (%d other board%s — `icebath kanban boards list`)\n\n", GetCurrentBoard(c.ctx), other, plural)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(c.w, "(no matching tasks)")
		return 0, nil
	}
	for _, t := range tasks {
		fmt.Fprintln(c.w, fmtTaskLine(t))
	}
	return 0, nil
}

var severityMarkers = map[string]string{"warning": "⚠", "error": "!!", "critical": "!!!"}

// Prints diagnostics with their data and suggested actions
func (c *kanbanCLI) printDiagnostics(diags []*Diagnostic, indent string, withKind bool) {
	for _, d := range diags {
		head := d.Title
		if withKind {
			head = d.Kind + ": " + d.Title
		}
		marker, ok := severityMarkers[d.Severity]
		if !ok {
			marker = "?"
		}
		fmt.Fprintf(c.w, "%s%s [%s] %s\n", indent, marker, d.Severity, head)
		if d.Data != nil && d.Data.Len() > 0 {
			bits := make([]string, 0, d.Data.Len())
			for _, k := range d.Data.Keys {
				v, _ := d.Data.Get(k)
				if list, isList := v.([]any); isList {
					parts := make([]string, 0, len(list))
					for _, x := range list {
						parts = append(parts, pyrepr.Str(x))
					}
					bits = append(bits, k+"="+strings.Join(parts, ","))
				} else {
					bits = append(bits, k+"="+pyrepr.Str(v))
				}
			}
			fmt.Fprintf(c.w, "%s   data: %s\n", indent, strings.Join(bits, " | "))
		}
		for _, a := range d.Actions {
			if a.Suggested {
				fmt.Fprintf(c.w, "%s   → %s\n", indent, a.Label)
			}
		}
	}
}

// Prints a blank line, the title, then each line
func (c *kanbanCLI) printSection(title string, lines []string) {
	fmt.Fprintln(c.w)
	fmt.Fprintln(c.w, title)
	for _, line := range lines {
		fmt.Fprintln(c.w, line)
	}
}

// Events as rule inputs
func eventsAny(events []*Event) []any {
	out := make([]any, 0, len(events))
	for _, e := range events {
		out = append(out, e)
	}
	return out
}

// Runs as rule inputs
func runsAny(runs []*Run) []any {
	out := make([]any, 0, len(runs))
	for _, r := range runs {
		out = append(out, r)
	}
	return out
}

// Shows a task with its diagnostics, comments, events and runs, or as JSON
func (c *kanbanCLI) cmdShow() (int, error) {
	stateType, stateName, rc := c.runStateKwargs("show")
	if rc != 0 {
		return rc, nil
	}
	tid := nsStr(c.ns, "task_id")
	wantJSON := nsBool(c.ns, "json")
	var task *Task
	var comments []*Comment
	var events []*Event
	var parents, children []string
	var runs []*Run
	var latestSummary *string
	var graph *pyjson.Object
	missing := false
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		if task, err = GetTask(c.ctx, conn, tid); err != nil || task == nil {
			missing = err == nil
			return err
		}
		if comments, err = ListComments(c.ctx, conn, tid); err != nil {
			return err
		}
		if events, err = ListEvents(c.ctx, conn, tid); err != nil {
			return err
		}
		if parents, err = ParentIDs(c.ctx, conn, tid); err != nil {
			return err
		}
		if children, err = ChildIDs(c.ctx, conn, tid); err != nil {
			return err
		}
		if runs, err = ListRuns(c.ctx, conn, tid, true, stateType, stateName); err != nil {
			return err
		}
		if latestSummary, err = LatestSummary(c.ctx, conn, tid); err != nil {
			return err
		}
		if !wantJSON {
			graph, err = TaskGraphContext(c.ctx, conn, task.ID)
		}
		return err
	}); err != nil {
		return 0, err
	}
	if missing {
		return cliErr(c.errw, "no such task: "+tid, 1), nil
	}
	if wantJSON {
		commentDicts := make([]any, 0, len(comments))
		for _, cm := range comments {
			commentDicts = append(commentDicts, pyjson.ObjectOf("author", cm.Author, "body", cm.Body, "created_at", cm.CreatedAt))
		}
		eventDicts := make([]any, 0, len(events))
		for _, e := range events {
			eventDicts = append(eventDicts, objDict([]string{"kind", "payload", "created_at", "run_id"}, func(k string) any { return eventAttr(e, k) }))
		}
		runDicts := make([]any, 0, len(runs))
		for _, r := range runs {
			runDicts = append(runDicts, objDict(showRunFields, func(k string) any { return runAttr(r, k) }))
		}
		return 0, printJSON(c.w, pyjson.ObjectOf("task", taskToDict(task), "latest_summary", optStrValue(latestSummary),
			"parents", stringsAny(parents), "children", stringsAny(children), "comments", commentDicts, "events", eventDicts, "runs", runDicts), false)
	}
	field := func(label string, value any) { fmt.Fprintf(c.w, "  %-11s%s\n", label+":", pyrepr.Str(value)) }
	fmt.Fprintf(c.w, "Task %s: %s\n", task.ID, task.Title)
	field("status", task.Status)
	field("assignee", orDash(task.Assignee))
	if task.Tenant != nil && *task.Tenant != "" {
		field("tenant", *task.Tenant)
	}
	workspace := task.WorkspaceKind
	if task.WorkspacePath != nil && *task.WorkspacePath != "" {
		workspace += " @ " + *task.WorkspacePath
	}
	field("workspace", workspace)
	if task.BranchName != nil && *task.BranchName != "" {
		field("branch", *task.BranchName)
	}
	if len(task.Skills) > 0 {
		field("skills", strings.Join(task.Skills, ", "))
	}
	if task.ModelOverride != nil && *task.ModelOverride != "" {
		prov := ""
		if task.ProviderOverride != nil && *task.ProviderOverride != "" {
			prov = " (provider: " + *task.ProviderOverride + ")"
		}
		field("model", *task.ModelOverride+prov)
	}
	if task.MaxRetries != nil {
		fmt.Fprintf(c.w, "  max-retries: %d (task)\n", *task.MaxRetries)
	} else if cfgVal := c.kanbanConfig()["failure_limit"]; cfgVal != nil {
		n, err := pyint.Of(cfgVal)
		if err != nil {
			return 0, &ValueError{err.Error()}
		}
		if !n.IsInt64() || n.Int64() != DefaultFailureLimit {
			fmt.Fprintf(c.w, "  max-retries: %s (config kanban.failure_limit)\n", n.String())
		} else {
			fmt.Fprintf(c.w, "  max-retries: %d (default)\n", DefaultFailureLimit)
		}
	} else {
		fmt.Fprintf(c.w, "  max-retries: %d (default)\n", DefaultFailureLimit)
	}
	field("created", fmtTS(task.CreatedAt)+" by "+orDash(task.CreatedBy))
	var graphArg any
	if graph != nil {
		graphArg = graph
	}
	if diags := ComputeTaskDiagnostics(task, eventsAny(events), runsAny(runs), nil, nil, graphArg); len(diags) > 0 {
		fmt.Fprintf(c.w, "\n  Diagnostics (%d):\n", len(diags))
		c.printDiagnostics(diags, "    ", false)
	}
	if task.StartedAt != nil && *task.StartedAt != 0 {
		field("started", fmtTS(task.StartedAt))
	}
	if task.CompletedAt != nil && *task.CompletedAt != 0 {
		field("completed", fmtTS(task.CompletedAt))
	}
	if len(parents) > 0 {
		field("parents", strings.Join(parents, ", "))
	}
	if len(children) > 0 {
		field("children", strings.Join(children, ", "))
	}
	if task.Body != nil && *task.Body != "" {
		c.printSection("Body:", []string{*task.Body})
	}
	if task.Result != nil && *task.Result != "" {
		c.printSection("Result:", []string{*task.Result})
	} else if latestSummary != nil && *latestSummary != "" {
		c.printSection("Latest summary:", []string{*latestSummary})
	}
	if len(comments) > 0 {
		lines := make([]string, 0, len(comments))
		for _, cm := range comments {
			lines = append(lines, fmt.Sprintf("  [%s] %s: %s", fmtTS(&cm.CreatedAt), cm.Author, cm.Body))
		}
		c.printSection(fmt.Sprintf("Comments (%d):", len(comments)), lines)
	}
	if len(events) > 0 {
		tail := events
		if len(tail) > 20 {
			tail = tail[len(tail)-20:]
		}
		lines := make([]string, 0, len(tail))
		for _, e := range tail {
			run, pl := "", ""
			if e.RunID != nil && *e.RunID != 0 {
				run = fmt.Sprintf(" [run %d]", *e.RunID)
			}
			if pyrepr.Truthy(e.Payload) {
				pl = " " + pyrepr.Str(e.Payload)
			}
			lines = append(lines, fmt.Sprintf("  [%s]%s %s%s", fmtTS(&e.CreatedAt), run, e.Kind, pl))
		}
		c.printSection(fmt.Sprintf("Events (%d):", len(events)), lines)
	}
	if len(runs) > 0 {
		fmt.Fprintln(c.w)
		fmt.Fprintf(c.w, "Runs (%d):\n", len(runs))
		for _, r := range runs {
			el := "active"
			if r.EndedAt != nil && *r.EndedAt != 0 {
				el = fmt.Sprintf("%ds", max(0, *r.EndedAt-r.StartedAt))
			}
			outcome := "active"
			if r.Outcome != nil && *r.Outcome != "" {
				outcome = *r.Outcome
			} else if r.Status != "" {
				outcome = r.Status
			}
			fmt.Fprintf(c.w, "  #%-3d %-12s @%s  %s  %s\n", r.ID, outcome, orDash(r.Profile), el, fmtTS(&r.StartedAt))
			if r.Summary != nil && *r.Summary != "" {
				fmt.Fprintf(c.w, "        → %s\n", firstLineCut(*r.Summary, 160))
			}
			if r.Error != nil && *r.Error != "" {
				fmt.Fprintf(c.w, "        ! %s\n", firstLineCut(*r.Error, 160))
			}
		}
	}
	return 0, nil
}

// Value or a dash for nil or empty
func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// First line of text cut to n characters
func firstLineCut(text string, n int) string {
	lines := pystr.Splitlines(text, false)
	if len(lines) == 0 {
		return ""
	}
	return pystr.Slice(lines[0], 0, n)
}

// Assigns or unassigns a task
func (c *kanbanCLI) cmdAssign() (int, error) {
	tid := nsStr(c.ns, "task_id")
	profile := noneProfile(nsStr(c.ns, "profile"))
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = AssignTask(c.ctx, conn, tid, profile)
		return err
	}); err != nil {
		return 0, err
	}
	who := "(unassigned)"
	if profile != nil && *profile != "" {
		who = *profile
	}
	return c.okOrErr(ok, "no such task: "+tid, "Assigned "+tid+" to "+who), nil
}

// Sets or clears a task model and provider override
func (c *kanbanCLI) cmdSetModel() (int, error) {
	tid := nsStr(c.ns, "task_id")
	model := nsOptStr(c.ns, "model")
	if model != nil {
		switch pystr.Lower(*model) {
		case "none", "-", "null", "":
			model = nil
		}
	}
	prov := nsOptStr(c.ns, "provider")
	var ok bool
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = SetModelOverride(c.ctx, conn, tid, model, prov)
		return err
	})
	if err != nil {
		var ve *ValueError
		var re *RuntimeError
		if errors.As(err, &ve) || errors.As(err, &re) || pyrepr.ExceptionName(err) == "ValueError" {
			return cliErr(c.errw, "kanban: "+pyrepr.ExceptionMessage(err), 2), nil
		}
		return 0, err
	}
	if !ok {
		return cliErr(c.errw, "no such task: "+tid, 1), nil
	}
	if model != nil && *model != "" {
		label := *model
		if prov != nil && *prov != "" {
			label = *prov + ":" + *model
		}
		fmt.Fprintf(c.w, "Set model override on %s: %s (applies on next dispatch)\n", tid, label)
	} else {
		fmt.Fprintf(c.w, "Cleared model override on %s (worker uses its profile default)\n", tid)
	}
	return 0, nil
}

// Releases a running task's claim back to ready
func (c *kanbanCLI) cmdReclaim() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = ReclaimTask(c.ctx, conn, tid, nsOptStr(c.ns, "reason"), nil)
		return err
	}); err != nil {
		return 0, err
	}
	return c.okOrErr(ok, "cannot reclaim "+tid+" (not running or unknown id)", "Reclaimed "+tid), nil
}

// Reassigns a task, releasing its claim first with --reclaim
func (c *kanbanCLI) cmdReassign() (int, error) {
	tid := nsStr(c.ns, "task_id")
	profile := noneProfile(nsStr(c.ns, "profile"))
	reclaim := nsBool(c.ns, "reclaim")
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = ReassignTask(c.ctx, conn, tid, profile, reclaim, nsOptStr(c.ns, "reason"))
		return err
	}); err != nil {
		return 0, err
	}
	who := "(unassigned)"
	if profile != nil && *profile != "" {
		who = *profile
	}
	done := "Reassigned " + tid + " to " + who
	if reclaim {
		done += " (claim reclaimed)"
	}
	return c.okOrErr(ok, "cannot reassign "+tid+" (unknown id, or still running — pass --reclaim to release first)", done), nil
}

// Rows of a table for every task id ordered by id, an empty list for ids without rows
func rowsByTask(ctx context.Context, conn state.SQLiteConn, table string, ids []string) (map[string][]any, error) {
	by := make(map[string][]any, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		by[id] = []any{}
		args[i] = id
	}
	rows, err := QueryRows(ctx, conn, "SELECT * FROM "+table+" WHERE task_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+") ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		id := pyrepr.Str(row.Get("task_id"))
		by[id] = append(by[id], row)
	}
	return by, nil
}

// Lists active diagnostics for one task or the whole board through the dashboard rule engine
func (c *kanbanCLI) cmdDiagnostics() (int, error) {
	cfg, err := loadKanbanConfigTree(c.ctx, false)
	if err != nil {
		return 0, err
	}
	diagConfig, err := ConfigFromRuntimeConfig(map[string]any(cfg))
	if err != nil {
		return 0, err
	}
	var order []string
	byTask := map[string][]*Diagnostic{}
	meta := map[string]*pyjson.Object{}
	taskArg := nsStr(c.ns, "task")
	missing := false
	err = ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		if taskArg != "" {
			task, err := GetTask(c.ctx, conn, taskArg)
			if err != nil || task == nil {
				missing = err == nil
				return err
			}
			events, err := ListEvents(c.ctx, conn, taskArg)
			if err != nil {
				return err
			}
			runs, err := ListRuns(c.ctx, conn, taskArg, true, nil, nil)
			if err != nil {
				return err
			}
			graph, err := TaskGraphContext(c.ctx, conn, taskArg)
			if err != nil {
				return err
			}
			var graphArg any
			if graph != nil {
				graphArg = graph
			}
			order = []string{taskArg}
			byTask[taskArg] = ComputeTaskDiagnostics(task, eventsAny(events), runsAny(runs), nil, diagConfig, graphArg)
		} else {
			rows, err := QueryRows(c.ctx, conn, "SELECT * FROM tasks WHERE status != 'archived'")
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(rows))
			for _, r := range rows {
				ids = append(ids, pyrepr.Str(r.Get("id")))
			}
			if len(ids) > 0 {
				evBy, err := rowsByTask(c.ctx, conn, "task_events", ids)
				if err != nil {
					return err
				}
				runBy, err := rowsByTask(c.ctx, conn, "task_runs", ids)
				if err != nil {
					return err
				}
				graphBy, err := TaskGraphContexts(c.ctx, conn, ids)
				if err != nil {
					return err
				}
				for _, r := range rows {
					tid := pyrepr.Str(r.Get("id"))
					var graphArg any
					if g, ok := graphBy.Get(tid); ok && g != nil {
						graphArg = g
					}
					if dl := ComputeTaskDiagnostics(r, evBy[tid], runBy[tid], nil, diagConfig, graphArg); len(dl) > 0 {
						if _, seen := byTask[tid]; !seen {
							order = append(order, tid)
						}
						byTask[tid] = dl
					}
				}
			}
		}
		if sev := nsStr(c.ns, "severity"); sev != "" {
			floor := slices.Index(SeverityOrder, sev)
			var kept []string
			for _, tid := range order {
				var keep []*Diagnostic
				for _, d := range byTask[tid] {
					if slices.Index(SeverityOrder, d.Severity) >= floor {
						keep = append(keep, d)
					}
				}
				if len(keep) > 0 {
					byTask[tid] = keep
					kept = append(kept, tid)
				} else {
					delete(byTask, tid)
				}
			}
			order = kept
		}
		if len(order) > 0 {
			args := make([]any, len(order))
			for i, tid := range order {
				args[i] = tid
			}
			rows, err := QueryRows(c.ctx, conn, "SELECT id, title, status, assignee FROM tasks WHERE id IN ("+
				strings.TrimSuffix(strings.Repeat("?,", len(order)), ",")+")", args...)
			if err != nil {
				return err
			}
			for _, r := range rows {
				meta[pyrepr.Str(r.Get("id"))] = pyjson.ObjectOf("title", r.Get("title"), "status", r.Get("status"), "assignee", r.Get("assignee"))
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if missing {
		return cliErr(c.errw, "no such task: "+taskArg, 1), nil
	}
	allowlist := DispatchProfileAllowlistSummary(c.ctx)
	if nsBool(c.ns, "json") {
		payloadRows := make([]any, 0, len(order)+1)
		for _, tid := range order {
			row := pyjson.ObjectOf("task_id", tid)
			if m, ok := meta[tid]; ok {
				row.Update(m)
			}
			dicts := make([]any, 0, len(byTask[tid]))
			for _, d := range byTask[tid] {
				dicts = append(dicts, d.ToDict())
			}
			row.Set("diagnostics", dicts)
			payloadRows = append(payloadRows, row)
		}
		payloadRows = append(payloadRows, pyjson.ObjectOf("task_id", nil, "dispatch_profiles", allowlist, "diagnostics", []any{}))
		return 0, printJSON(c.w, payloadRows, false)
	}
	fmt.Fprintf(c.w, "kanban.dispatch_profiles: %s\n", allowlist)
	if len(order) == 0 {
		fmt.Fprintln(c.w, "No active diagnostics on this board.")
		return 0, nil
	}
	total := 0
	for _, tid := range order {
		total += len(byTask[tid])
	}
	fmt.Fprintf(c.w, "%d active diagnostic(s) across %d task(s):\n\n", total, len(order))
	for _, tid := range order {
		m := meta[tid]
		if m == nil {
			m = pyjson.NewObject()
		}
		text := func(key, def string) string {
			if v, _ := m.Get(key); pyrepr.Truthy(v) {
				return pyrepr.Str(v)
			}
			return def
		}
		fmt.Fprintf(c.w, "  %s  %-8s  @%-18s  %s\n", tid, text("status", "?"), text("assignee", "(unassigned)"), text("title", "(untitled)"))
		c.printDiagnostics(byTask[tid], "    ", true)
		fmt.Fprintln(c.w)
	}
	return 0, nil
}

// Links parent to child, noting when the ready child was gated back to the backlog
func (c *kanbanCLI) cmdLink() (int, error) {
	parent, child := nsStr(c.ns, "parent_id"), nsStr(c.ns, "child_id")
	var expected *int64
	if env, ok := os.LookupEnv("ICEBATH_KANBAN_TASK"); ok && child == env {
		var err error
		if expected, err = workerRunIDFor(child); err != nil {
			return 0, err
		}
	}
	var gated bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		gated, err = LinkTasks(c.ctx, conn, parent, child, expected)
		return err
	}); err != nil {
		return 0, err
	}
	fmt.Fprintf(c.w, "Linked %s -> %s\n", parent, child)
	if gated {
		fmt.Fprintf(c.w, "Note: %s was ready and is now todo — parent %s is not done yet. The ready -> running claim "+
			"re-checks parents, so the child only runs after the parent completes; use `icebath kanban unlink %s %s` to run it now.\n",
			child, parent, parent, child)
	}
	return 0, nil
}

// Removes a parent to child link
func (c *kanbanCLI) cmdUnlink() (int, error) {
	parent, child := nsStr(c.ns, "parent_id"), nsStr(c.ns, "child_id")
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = UnlinkTasks(c.ctx, conn, parent, child)
		return err
	}); err != nil {
		return 0, err
	}
	return c.okOrErr(ok, "No such link: "+parent+" -> "+child, "Unlinked "+parent+" -> "+child), nil
}

// Claims a ready task for manual work and resolves its workspace
func (c *kanbanCLI) cmdClaim() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var task *Task
	var workspace string
	refusal := ""
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		if task, err = ClaimTask(c.ctx, conn, tid, nsOptInt(c.ns, "ttl"), nil); err != nil {
			return err
		}
		if task == nil {
			existing, err := GetTask(c.ctx, conn, tid)
			if err != nil {
				return err
			}
			if existing == nil {
				refusal = "no such task: " + tid
			} else {
				lock := "(none)"
				if existing.ClaimLock != nil && *existing.ClaimLock != "" {
					lock = *existing.ClaimLock
				}
				refusal = fmt.Sprintf("cannot claim %s: status=%s lock=%s", tid, existing.Status, lock)
			}
			return nil
		}
		if workspace, err = ResolveWorkspace(c.ctx, task, nil); err != nil {
			return err
		}
		return SetWorkspacePath(c.ctx, conn, task.ID, workspace)
	}); err != nil {
		return 0, err
	}
	if refusal != "" {
		return cliErr(c.errw, refusal, 1), nil
	}
	fmt.Fprintf(c.w, "Claimed %s\nWorkspace: %s\n", task.ID, workspace)
	return 0, nil
}

// Appends a comment, trimmed to --max-len with a marker
func (c *kanbanCLI) cmdComment() (int, error) {
	tid := nsStr(c.ns, "task_id")
	body := pystr.Strip(strings.Join(nsStrs(c.ns, "text"), " "))
	if maxLen := nsOptInt(c.ns, "max_len"); maxLen != nil {
		if *maxLen < 1 {
			return cliErr(c.errw, "kanban: --max-len must be positive", 2), nil
		}
		if int64(pystr.Len(body)) > *maxLen {
			suffix := fmt.Sprintf("\n\n[trimmed to %d chars by --max-len]", *maxLen)
			body = pystr.Rstrip(pystr.Slice(body, 0, int(max(0, *maxLen-int64(pystr.Len(suffix)))))) + suffix
		}
	}
	author := nsStr(c.ns, "author")
	if author == "" {
		author = c.profileAuthor()
	}
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		_, err := AddComment(c.ctx, conn, tid, author, body)
		return err
	}); err != nil {
		return 0, err
	}
	fmt.Fprintf(c.w, "Comment added to %s\n", tid)
	return 0, nil
}

// Attaches a local file through the shared capped attachment path
func (c *kanbanCLI) cmdAttach() (int, error) {
	tid := nsStr(c.ns, "task_id")
	if _, err := workerRunIDFor(tid); err != nil {
		return 0, err
	}
	src := pypath.PathStr(pypath.Expanduser(nsStr(c.ns, "path")))
	if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
		return cliErr(c.errw, "kanban: no such file: "+src, 1), nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	name := nsStr(c.ns, "name")
	if name == "" {
		name = filepath.Base(src)
	}
	contentType := nsOptStr(c.ns, "content_type")
	if contentType == nil || *contentType == "" {
		contentType = nil
		if guessed, _, err := pymimetypes.GuessType(name, true); err == nil && guessed != "" {
			contentType = &guessed
		}
	}
	uploadedBy := nsStr(c.ns, "author")
	if uploadedBy == "" {
		uploadedBy = c.profileAuthor()
	}
	var attID int64
	err = ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		attID, err = StoreAttachmentBytes(c.ctx, conn, tid, name, data, StoreAttachmentOptions{ContentType: contentType, UploadedBy: &uploadedBy})
		return err
	})
	if err != nil {
		var tooLarge *AttachmentTooLarge
		if errors.As(err, &tooLarge) {
			return cliErr(c.errw, "kanban: "+tooLarge.Error(), 1), nil
		}
		return 0, err
	}
	fmt.Fprintf(c.w, "Attached %s to %s (attachment %d, %d bytes)\n", name, tid, attID, len(data))
	return 0, nil
}

// Lists a task's attachments
func (c *kanbanCLI) cmdAttachments() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var atts []*Attachment
	missing := false
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		task, err := GetTask(c.ctx, conn, tid)
		if err != nil || task == nil {
			missing = err == nil
			return err
		}
		atts, err = ListAttachments(c.ctx, conn, tid)
		return err
	}); err != nil {
		return 0, err
	}
	if missing {
		return cliErr(c.errw, "no such task: "+tid, 1), nil
	}
	dicts := make([]any, 0, len(atts))
	for _, a := range atts {
		dicts = append(dicts, objDict(cliAttachmentFields, func(k string) any { return attachmentAttr(a, k) }))
	}
	if done, err := jsonOut(c, dicts, true); done || err != nil {
		return 0, err
	}
	if len(atts) == 0 {
		fmt.Fprintf(c.w, "No attachments on %s\n", tid)
		return 0, nil
	}
	fmt.Fprintf(c.w, "Attachments on %s:\n", tid)
	for _, a := range atts {
		fmt.Fprintf(c.w, "  [%d] %s  (%d bytes, %s, by %s)\n", a.ID, a.Filename, a.Size, orDash(a.ContentType), orDash(a.UploadedBy))
		fmt.Fprintf(c.w, "        %s\n", a.StoredPath)
	}
	return 0, nil
}

// Deletes one attachment row and its file
func (c *kanbanCLI) cmdAttachRm() (int, error) {
	id := int64(0)
	if n := nsOptInt(c.ns, "attachment_id"); n != nil {
		id = *n
	}
	var removed *Attachment
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		removed, err = DeleteAttachment(c.ctx, conn, id)
		return err
	}); err != nil {
		return 0, err
	}
	if removed == nil {
		return cliErr(c.errw, fmt.Sprintf("no such attachment: %d", id), 1), nil
	}
	fmt.Fprintf(c.w, "Deleted attachment %d (%s) from %s\n", id, removed.Filename, removed.TaskID)
	return 0, nil
}

// Run id a dispatched worker proves ownership with, refusing a worker scoped to another task
func workerRunIDFor(taskID string) (*int64, error) {
	envTID := os.Getenv("ICEBATH_KANBAN_TASK")
	if envTID != "" && envTID != taskID {
		return nil, &ValueError{fmt.Sprintf("worker is scoped to task %s; refusing to mutate %s", envTID, taskID)}
	}
	raw := os.Getenv("ICEBATH_KANBAN_RUN_ID")
	if envTID != taskID || raw == "" {
		return nil, nil
	}
	n, ok := pyint.Parse(raw)
	if !ok || !n.IsInt64() {
		return nil, nil
	}
	v := n.Int64()
	return &v, nil
}

// Goal judge verdict for a terminal handoff, done with no reason when the judge allows it or is unavailable
func (c *kanbanCLI) goalModeHandoffRejection(task *Task, evidence string) (string, *string) {
	if task == nil || !task.GoalMode {
		return "done", nil
	}
	if !goalJudgeAvailableFn(c.ctx) {
		return "done", nil
	}
	verdict, reason, transportFailed := "done", "", false
	func() {
		defer func() {
			if r := recover(); r != nil {
				kanbanToolsLog.Warn(fmt.Sprintf("goal judge check failed, allowing lifecycle handoff: %v", r), "exc_info", fmt.Sprint(r))
			}
		}()
		ctx := c.ctx
		if provider.PortalAffinityScope(ctx) == "" {
			ctx = provider.WithPortalAffinityScope(ctx, "kanban:"+task.ID)
		}
		body := ""
		if task.Body != nil {
			body = *task.Body
		}
		result := judgeGoalFn(ctx, pystr.Strip(task.Title+"\n\n"+body), pystr.Strip(evidence), goals.JudgeOptions{})
		verdict, reason, transportFailed = result.Verdict, result.Reason, result.TransportFailed
	}()
	if transportFailed {
		kanbanToolsLog.Warn(fmt.Sprintf("goal judge unreachable (%s), allowing lifecycle handoff", reason))
		return "done", nil
	}
	if verdict == "done" {
		return verdict, nil
	}
	return verdict, &reason
}

// Goal judge gate shared by complete and request-review, the error line or nil to allow
func (c *kanbanCLI) goalGateError(conn state.SQLiteConn, tid, evidence, handoff, blockedHint, continueHint string) (*string, error) {
	task, err := GetTask(c.ctx, conn, tid)
	if err != nil {
		return nil, err
	}
	verdict, rejection := c.goalModeHandoffRejection(task, evidence)
	if verdict == "blocked" {
		msg := fmt.Sprintf("kanban: goal %s of %s rejected: judge ruled the goal unachievable — %s. %s", handoff, tid, pyrepr.Str(optStrValue(rejection)), blockedHint)
		return &msg, nil
	}
	if rejection != nil {
		msg := fmt.Sprintf("kanban: goal %s of %s rejected by judge: %s. %s", handoff, tid, *rejection, continueHint)
		return &msg, nil
	}
	return nil, nil
}

// Completes one or more tasks, naming the refusal of each failure
func (c *kanbanCLI) cmdComplete() (int, error) {
	ids, rc := c.requireIDs()
	if rc != 0 {
		return rc, nil
	}
	summary, rawMeta := nsOptStr(c.ns, "summary"), nsOptStr(c.ns, "metadata")
	if len(ids) > 1 && ((summary != nil && *summary != "") || (rawMeta != nil && *rawMeta != "")) {
		return cliErr(c.errw, "kanban: --summary / --metadata are per-task and can't be used "+
			"with multiple ids (would apply the same handoff to every task). "+
			"Complete tasks one at a time, or drop the flags for the bulk close.", 2), nil
	}
	metadata, rc := c.parseMetadataFlag(rawMeta)
	if rc != 0 {
		return rc, nil
	}
	result := nsOptStr(c.ns, "result")
	evidence := ""
	if summary != nil && *summary != "" {
		evidence = *summary
	} else if result != nil {
		evidence = *result
	}
	failMsg := map[string]string{}
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		op := func(tid string) (bool, error) {
			gateErr, err := c.goalGateError(conn, tid, pystr.Strip(evidence), "completion",
				"Re-scope with kanban edit, or record the block with kanban block instead of completing.",
				"Provide evidence matching the task's acceptance criteria.")
			if err != nil {
				return false, err
			}
			if gateErr != nil {
				failMsg[tid] = *gateErr
				return false, nil
			}
			failMsg[tid] = "cannot complete " + tid + " (unknown id or terminal state)"
			runID, err := workerRunIDFor(tid)
			if err != nil {
				return false, err
			}
			done, err := CompleteTask(c.ctx, conn, tid, CompleteTaskOptions{Result: result, Summary: summary, Metadata: metadata, ExpectedRunID: runID, Force: nsBool(c.ns, "force")})
			if err != nil {
				var live *LiveClaimError
				var empty *EmptyCompletionError
				switch {
				case errors.As(err, &live):
					failMsg[tid] = fmt.Sprintf("cannot complete %s: a live worker is running it. Wait for the "+
						"worker, `icebath kanban reclaim %s` to release it, or re-run with "+
						"--force to close its run and complete anyway.", tid, tid)
					return false, nil
				case errors.As(err, &empty):
					failMsg[tid] = fmt.Sprintf("cannot complete %s: %s. Pass --result/--summary "+
						"describing what was done (an empty completion is not evidence).", tid, empty.Error())
					return false, nil
				}
				return false, err
			}
			if !done {
				blockers, err := UnsatisfiedParents(c.ctx, conn, tid)
				if err != nil {
					return false, err
				}
				if len(blockers) > 0 {
					parts := make([]string, 0, len(blockers))
					for _, b := range blockers {
						parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Status))
					}
					failMsg[tid] = fmt.Sprintf("cannot complete %s: unsatisfied parent dependencies: %s; "+
						"complete the parents first, or `icebath kanban unlink <parent> %s`.", tid, strings.Join(parts, ", "), tid)
				}
			}
			return done, nil
		}
		var err error
		code, err = bulkApply(c, ids, op, func(tid string) string { return "Completed " + tid }, func(tid string) string { return failMsg[tid] })
		return err
	})
	return code, err
}

// Edits title, body, priority or a done task's result
func (c *kanbanCLI) cmdEdit() (int, error) {
	tid := nsStr(c.ns, "task_id")
	result, rawMeta, summary := nsOptStr(c.ns, "result"), nsOptStr(c.ns, "metadata"), nsOptStr(c.ns, "summary")
	title, body, priority := nsOptStr(c.ns, "title"), nsOptStr(c.ns, "body"), nsOptInt(c.ns, "priority")
	if result == nil && (summary != nil || rawMeta != nil) {
		return cliErr(c.errw, "kanban edit: --summary and --metadata require --result", 2), nil
	}
	if title == nil && body == nil && priority == nil && result == nil {
		return cliErr(c.errw, "kanban edit: provide --title, --body, --priority, or --result", 2), nil
	}
	metadata, rc := c.parseMetadataFlag(rawMeta)
	if rc != 0 {
		return rc, nil
	}
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = EditTask(c.ctx, conn, tid, EditTaskOptions{Title: title, Body: body, Priority: priority, Result: result, Summary: summary, Metadata: metadata})
		return err
	}); err != nil {
		return 0, err
	}
	return c.okOrErr(ok, "cannot edit "+tid+" (unknown id, or --result used on a task that is not done)", "Edited "+tid), nil
}

// Wraps a per task op so a reason is first recorded as a prefixed comment
func commented(ctx context.Context, conn state.SQLiteConn, reason *string, author, prefix string, op func(string) (bool, error)) func(string) (bool, error) {
	return func(tid string) (bool, error) {
		if reason != nil && *reason != "" {
			if _, err := AddComment(ctx, conn, tid, author, prefix+": "+*reason); err != nil {
				return false, err
			}
		}
		return op(tid)
	}
}

// Text suffix of an optional reason
func reasonSuffix(reason *string) string {
	if reason != nil && *reason != "" {
		return ": " + *reason
	}
	return ""
}

// Blocks tasks, reporting where each landed
func (c *kanbanCLI) cmdBlock() (int, error) {
	reason := joinedWords(nsStrs(c.ns, "reason"))
	kind := nsOptStr(c.ns, "kind")
	author := c.profileAuthor()
	ids := c.bulkIDs()
	suffix := reasonSuffix(reason)
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var landErr error
		okMsg := func(tid string) string {
			landed, err := GetTask(c.ctx, conn, tid)
			if err != nil {
				landErr = err
			}
			where := "blocked"
			if landed != nil {
				where = landed.Status
			}
			switch {
			case where == "todo":
				return tid + " → todo (dependency wait)" + suffix
			case kind != nil && *kind == "dependency" && where == "blocked":
				return "Blocked " + tid + " as needs_input (no open parent to wait on)" + suffix
			case where == "triage":
				blockKind := kind
				if landed != nil {
					blockKind = landed.BlockKind
				}
				verdict := "orchestration attention needed"
				if blockKind != nil && *blockKind == "needs_input" {
					verdict = "needs a human decision"
				}
				return tid + " → triage (unblock loop detected — " + verdict + ")" + suffix
			}
			return "Blocked " + tid + suffix
		}
		op := commented(c.ctx, conn, reason, author, "BLOCKED", func(tid string) (bool, error) {
			runID, err := workerRunIDFor(tid)
			if err != nil {
				return false, err
			}
			return BlockTaskWithKind(c.ctx, conn, tid, reason, kind, runID)
		})
		var err error
		code, err = bulkApply(c, ids, op, okMsg, func(tid string) string { return "cannot block " + tid })
		if err == nil {
			err = landErr
		}
		return err
	})
	return code, err
}

// Parks tasks in scheduled with an optional reason comment
func (c *kanbanCLI) cmdSchedule() (int, error) {
	reason := joinedWords(nsStrs(c.ns, "reason"))
	author := c.profileAuthor()
	ids := c.bulkIDs()
	suffix := reasonSuffix(reason)
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		op := commented(c.ctx, conn, reason, author, "SCHEDULED", func(tid string) (bool, error) {
			runID, err := workerRunIDFor(tid)
			if err != nil {
				return false, err
			}
			return ScheduleTask(c.ctx, conn, tid, reason, runID)
		})
		var err error
		code, err = bulkApply(c, ids, op, func(tid string) string { return "Scheduled " + tid + suffix }, func(tid string) string { return "cannot schedule " + tid })
		return err
	})
	return code, err
}

// Unblocks tasks from an orchestrator, refused inside a worker
func (c *kanbanCLI) cmdUnblock() (int, error) {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		return cliErr(c.errw, "kanban unblock is orchestrator-only; workers must hand off their assigned task", 1), nil
	}
	ids, rc := c.requireIDs()
	if rc != 0 {
		return rc, nil
	}
	reason := strippedOrNone(nsOptStr(c.ns, "reason"))
	author := ""
	if reason != nil {
		author = c.profileAuthor()
	}
	suffix := reasonSuffix(reason)
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		op := commented(c.ctx, conn, reason, author, "UNBLOCK", func(tid string) (bool, error) { return UnblockTask(c.ctx, conn, tid) })
		var err error
		code, err = bulkApply(c, ids, op, func(tid string) string { return "Unblocked " + tid + suffix },
			func(tid string) string { return "cannot unblock " + tid + " (not blocked/scheduled?)" })
		return err
	})
	return code, err
}

// Moves a task into review behind the goal judge gate
func (c *kanbanCLI) cmdRequestReview() (int, error) {
	tid := nsStr(c.ns, "task_id")
	summary := strippedOrNone(nsOptStr(c.ns, "summary"))
	metadata, rc := c.parseMetadataFlag(nsOptStr(c.ns, "metadata"))
	if rc != 0 {
		return rc, nil
	}
	evidence := ""
	if summary != nil {
		evidence = *summary
	}
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		gateErr, err := c.goalGateError(conn, tid, evidence, "review handoff",
			"Record the block with kanban block instead of requesting review.",
			"Provide acceptance evidence matching the task.")
		if err != nil {
			return err
		}
		if gateErr != nil {
			code = cliErr(c.errw, *gateErr, 1)
			return nil
		}
		runID, err := workerRunIDFor(tid)
		if err != nil {
			return err
		}
		ok, reason, err := RequestReview(c.ctx, conn, tid, RequestReviewOptions{Summary: summary, Metadata: metadata,
			Reviewer: nsOptStr(c.ns, "reviewer"), ExpectedRunID: runID, Force: nsBool(c.ns, "force")})
		if err != nil {
			return err
		}
		if !ok {
			if reason == "" {
				reason = "not running/ready?"
			}
			code = cliErr(c.errw, "cannot request review for "+tid+": "+reason, 1)
			return nil
		}
		run, err := LatestRun(c.ctx, conn, tid)
		if err != nil {
			return err
		}
		line := "Requested review for " + tid
		if run != nil && run.Summary != nil && *run.Summary != "" {
			line += ": " + *run.Summary
		}
		fmt.Fprintln(c.w, line)
		return nil
	})
	return code, err
}

// Returns an active review to its implementer with a reason
func (c *kanbanCLI) cmdRequestChanges() (int, error) {
	tid := nsStr(c.ns, "task_id")
	reason := pystr.Strip(strings.Join(nsStrs(c.ns, "reason"), " "))
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		runID, err := workerRunIDFor(tid)
		if err != nil {
			return err
		}
		ok, detail, err := RequestChanges(c.ctx, conn, tid, reason, runID)
		if err != nil {
			return err
		}
		if !ok {
			if detail == "" {
				detail = "invalid review state"
			}
			code = cliErr(c.errw, "cannot request changes for "+tid+": "+detail, 1)
			return nil
		}
		line := "Requested changes for " + tid
		if detail != "" {
			line += "; routed to " + detail
		}
		fmt.Fprintln(c.w, line)
		return nil
	})
	return code, err
}

// Reopens reviews back to ready with an optional redacted changes requested comment
func (c *kanbanCLI) cmdReopenReview() (int, error) {
	ids, rc := c.requireIDs()
	if rc != 0 {
		return rc, nil
	}
	var reason *string
	if raw := nsOptStr(c.ns, "reason"); raw != nil {
		if s := pystr.Strip(pyrepr.Str(redactReviewValue(pystr.Strip(*raw)))); s != "" {
			reason = &s
		}
	}
	author := ""
	if reason != nil {
		author = c.profileAuthor()
	}
	if author == "" {
		author = "operator"
	}
	suffix := reasonSuffix(reason)
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		op := func(tid string) (bool, error) {
			ok, err := ReopenReviewTask(c.ctx, conn, tid)
			if err != nil || !ok {
				return false, err
			}
			if reason != nil {
				if _, err := AddComment(c.ctx, conn, tid, author, "CHANGES REQUESTED: "+*reason); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		var err error
		code, err = bulkApply(c, ids, op, func(tid string) string { return "Reopened " + tid + suffix },
			func(tid string) string { return "cannot reopen " + tid + " (not in review?)" })
		return err
	})
	return code, err
}

// Promotes tasks to ready, reporting each result as text or JSON
func (c *kanbanCLI) cmdPromote() (int, error) {
	reason := joinedWords(nsStrs(c.ns, "reason"))
	author := c.profileAuthor()
	var ids []string
	for _, id := range c.bulkIDs() {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	dryRun := nsBool(c.ns, "dry_run")
	var results []*pyjson.Object
	failed := 0
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		for _, tid := range ids {
			ok, errText, err := PromoteTask(c.ctx, conn, tid, author, reason, dryRun)
			if err != nil {
				return err
			}
			var errValue any
			if errText != "" {
				errValue = errText
			}
			if !ok {
				failed++
			}
			results = append(results, pyjson.ObjectOf("task_id", tid, "promoted", ok, "dry_run", dryRun, "reason", optStrValue(reason), "error", errValue))
		}
		return nil
	}); err != nil {
		return 0, err
	}
	rc := 0
	if failed > 0 {
		rc = 1
	}
	if nsBool(c.ns, "json") {
		var out any = results[0]
		if len(results) != 1 {
			list := make([]any, 0, len(results))
			for _, r := range results {
				list = append(list, r)
			}
			out = list
		}
		return rc, printJSON(c.w, out, false)
	}
	tag, label := "", "Promoted"
	if dryRun {
		tag, label = " (dry)", "Would promote"
	}
	suffix := reasonSuffix(reason)
	for _, r := range results {
		tid, _ := r.Get("task_id")
		if promoted, _ := r.Get("promoted"); promoted == true {
			fmt.Fprintf(c.w, "%s %s -> ready%s%s\n", label, tid, tag, suffix)
		} else {
			errValue, _ := r.Get("error")
			fmt.Fprintf(c.errw, "cannot promote %s: %s\n", tid, pyrepr.Str(errValue))
		}
	}
	return rc, nil
}

// Archives tasks, or with --rm deletes already archived ones
func (c *kanbanCLI) cmdArchive() (int, error) {
	ids, purgeIDs := nsStrs(c.ns, "task_ids"), nsStrs(c.ns, "purge_ids")
	if len(ids) > 0 && len(purgeIDs) > 0 {
		return cliErr(c.errw, "choose either task_ids to archive or --rm archived task_ids", 1), nil
	}
	if len(ids) == 0 && len(purgeIDs) == 0 {
		return cliErr(c.errw, "at least one task_id is required", 1), nil
	}
	var code int
	err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		if len(purgeIDs) > 0 {
			code, err = bulkApply(c, purgeIDs, func(tid string) (bool, error) { return DeleteArchivedTask(c.ctx, conn, tid) },
				func(tid string) string { return "Deleted " + tid }, func(tid string) string { return "cannot delete " + tid + " (must already be archived)" })
			return err
		}
		code, err = bulkApply(c, ids, func(tid string) (bool, error) { return ArchiveTask(c.ctx, conn, tid, nil) },
			func(tid string) string { return "Archived " + tid }, func(tid string) string { return "cannot archive " + tid })
		return err
	})
	return code, err
}

// Per status and per assignee counts with the oldest ready age
func (c *kanbanCLI) cmdStats() (int, error) {
	var stats *pyjson.Object
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		stats, err = BoardStats(c.ctx, conn)
		return err
	}); err != nil {
		return 0, err
	}
	if done, err := jsonOut(c, stats, false); done || err != nil {
		return 0, err
	}
	byStatus, _ := stats.GetOr("by_status", pyjson.NewObject()).(*pyjson.Object)
	fmt.Fprintln(c.w, "By status:")
	for _, k := range DefaultWorkflow.Keys() {
		fmt.Fprintf(c.w, "  %-8s  %s\n", k, pyrepr.Str(byStatus.GetOr(k, 0)))
	}
	if byAssignee, _ := stats.GetOr("by_assignee", nil).(*pyjson.Object); byAssignee != nil && byAssignee.Len() > 0 {
		fmt.Fprintln(c.w, "\nBy assignee:")
		names := slices.Clone(byAssignee.Keys)
		slices.Sort(names)
		for _, who := range names {
			counts, _ := byAssignee.GetOr(who, nil).(*pyjson.Object)
			fmt.Fprintf(c.w, "  %-20s  %s\n", who, fmtCounts(countsMap(counts), ""))
		}
	}
	if age := stats.GetOr("oldest_ready_age_seconds", nil); age != nil {
		n, err := pyint.Of(age)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(c.w, "\nOldest ready task age: %ss\n", n.String())
	}
	return 0, nil
}

// Subscribes a gateway source to a task's terminal events
func (c *kanbanCLI) cmdNotifySubscribe() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var deliveryMetadata any
	meta := pyjson.NewObject()
	for _, key := range []string{"parent_chat_id", "guild_id"} {
		if v := c.ns[key]; pyrepr.Truthy(v) {
			meta.Set(key, v)
		}
	}
	if meta.Len() > 0 {
		deliveryMetadata = meta
	}
	notifier := nsStr(c.ns, "notifier_profile")
	if notifier == "" {
		notifier = c.profileAuthor()
	}
	missing := false
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		task, err := GetTask(c.ctx, conn, tid)
		if err != nil || task == nil {
			missing = err == nil
			return err
		}
		return AddNotifySub(c.ctx, conn, NotifySubOptions{
			TaskID: tid, Platform: nsStr(c.ns, "platform"), ChatID: nsStr(c.ns, "chat_id"), ChatType: nsOptStr(c.ns, "chat_type"),
			ThreadID: nsOptStr(c.ns, "thread_id"), UserID: nsOptStr(c.ns, "user_id"), UserIDAlt: nsOptStr(c.ns, "user_id_alt"),
			NotifierProfile: &notifier, DeliveryMode: nsOptStr(c.ns, "delivery_mode"), DeliveryMetadata: deliveryMetadata,
		})
	}); err != nil {
		return 0, err
	}
	if missing {
		return cliErr(c.errw, "no such task: "+tid, 1), nil
	}
	thread := ""
	if t := nsStr(c.ns, "thread_id"); t != "" {
		thread = ":" + t
	}
	fmt.Fprintf(c.w, "Subscribed %s:%s%s to %s\n", nsStr(c.ns, "platform"), nsStr(c.ns, "chat_id"), thread, tid)
	return 0, nil
}

// Lists notification subscriptions, of one task when given
func (c *kanbanCLI) cmdNotifyList() (int, error) {
	var subs []*pyjson.Object
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		subs, err = ListNotifySubs(c.ctx, conn, nsOptStr(c.ns, "task_id"), NotifyOwnerScope{})
		return err
	}); err != nil {
		return 0, err
	}
	list := make([]any, 0, len(subs))
	for _, s := range subs {
		list = append(list, s)
	}
	if done, err := jsonOut(c, list, false); done || err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		fmt.Fprintln(c.w, "(no subscriptions)")
		return 0, nil
	}
	for _, s := range subs {
		get := func(k string) any { return s.GetOr(k, nil) }
		thr := ""
		if v := get("thread_id"); pyrepr.Truthy(v) {
			thr = ":" + pyrepr.Str(v)
		}
		dmode, ctype := "notify", "dm"
		if v := get("delivery_mode"); pyrepr.Truthy(v) {
			dmode = pyrepr.Str(v)
		}
		if v := get("chat_type"); pyrepr.Truthy(v) {
			ctype = pyrepr.Str(v)
		}
		extras := ""
		if v := get("notifier_profile"); pyrepr.Truthy(v) {
			extras += "  owner=" + pyrepr.Str(v)
		}
		if ctype != "dm" {
			extras += "  chat_type=" + ctype
		}
		if v := get("user_id_alt"); pyrepr.Truthy(v) {
			extras += "  user_id_alt=" + pyrepr.Str(v)
		}
		if dmode != "notify" {
			extras += "  mode=" + dmode
		}
		fmt.Fprintf(c.w, "  %-10s  %s:%s%s  (since event %s)%s\n", pyrepr.Str(get("task_id")), pyrepr.Str(get("platform")),
			pyrepr.Str(get("chat_id")), thr, pyrepr.Str(get("last_event_id")), extras)
	}
	return 0, nil
}

// Removes one notification subscription
func (c *kanbanCLI) cmdNotifyUnsubscribe() (int, error) {
	tid := nsStr(c.ns, "task_id")
	var ok bool
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		ok, err = RemoveNotifySub(c.ctx, conn, tid, nsStr(c.ns, "platform"), nsStr(c.ns, "chat_id"), nsOptStr(c.ns, "thread_id"))
		return err
	}); err != nil {
		return 0, err
	}
	return c.okOrErr(ok, "(no such subscription)", "Unsubscribed from "+tid), nil
}

// Prints a task's worker log, its tail with --tail
func (c *kanbanCLI) cmdLog() (int, error) {
	tid := nsStr(c.ns, "task_id")
	content, err := ReadWorkerLog(c.ctx, tid, nsOptInt(c.ns, "tail"), nil)
	if err != nil {
		return 0, err
	}
	if content == nil {
		return cliErr(c.errw, "(no log for "+tid+" — task may not have spawned yet)", 1), nil
	}
	io.WriteString(c.w, *content)
	if !strings.HasSuffix(*content, "\n") {
		io.WriteString(c.w, "\n")
	}
	return 0, nil
}

// Attempt history of a task as a table or JSON
func (c *kanbanCLI) cmdRuns() (int, error) {
	stateType, stateName, rc := c.runStateKwargs("runs")
	if rc != 0 {
		return rc, nil
	}
	tid := nsStr(c.ns, "task_id")
	var runs []*Run
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		runs, err = ListRuns(c.ctx, conn, tid, true, stateType, stateName)
		return err
	}); err != nil {
		return 0, err
	}
	dicts := make([]any, 0, len(runs))
	for _, r := range runs {
		dicts = append(dicts, objDict(runsRunFields, func(k string) any { return runAttr(r, k) }))
	}
	if done, err := jsonOut(c, dicts, false); done || err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		fmt.Fprintf(c.w, "(no runs yet for %s)\n", tid)
		return 0, nil
	}
	fmt.Fprintf(c.w, "%-3s  %-12s  %-16s  %8s  STARTED\n", "#", "OUTCOME", "PROFILE", "ELAPSED")
	for i, r := range runs {
		end := nowUnix()
		if r.EndedAt != nil && *r.EndedAt != 0 {
			end = *r.EndedAt
		}
		elapsed := max(0, end-r.StartedAt)
		var el string
		switch {
		case elapsed < 60:
			el = fmt.Sprintf("%ds", elapsed)
		case elapsed < 3600:
			el = fmt.Sprintf("%dm", elapsed/60)
		default:
			el = fmt.Sprintf("%.1fh", float64(elapsed)/3600)
		}
		outcome := r.Status
		if r.Outcome != nil && *r.Outcome != "" {
			outcome = *r.Outcome
		} else if r.EndedAt == nil || *r.EndedAt == 0 {
			outcome = "(running)"
		}
		fmt.Fprintf(c.w, "%3d  %-12s  %-16s  %8s  %s\n", i+1, outcome, orDash(r.Profile), el, fmtTS(&r.StartedAt))
		if r.Summary != nil && *r.Summary != "" {
			fmt.Fprintf(c.w, "     → %s\n", firstLineCut(*r.Summary, 100))
		}
		if r.Error != nil && *r.Error != "" {
			fmt.Fprintf(c.w, "     ✖ %s\n", pystr.Slice(*r.Error, 0, 100))
		}
	}
	return 0, nil
}

// Prints the full worker context of a task
func (c *kanbanCLI) cmdContext() (int, error) {
	var text string
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		var err error
		text, err = BuildWorkerContext(c.ctx, conn, nsStr(c.ns, "task_id"))
		return err
	}); err != nil {
		return 0, err
	}
	fmt.Fprintln(c.w, text)
	return 0, nil
}

// triageOutcome is one specify or decompose result as the sweep reports it
type triageOutcome struct {
	taskID string
	ok     bool
	reason string
	fields *pyjson.Object
	human  string
}

// Shared specify and decompose driver over one task id or every triage task with --all
func (c *kanbanCLI) runTriageSweep(verb, jsonKey string, listIDs func(*string) ([]string, error), runOne func(tid, author string) (*triageOutcome, error)) (int, error) {
	allFlag := nsBool(c.ns, "all_triage")
	author := nsStr(c.ns, "author")
	if author == "" {
		author = c.profileAuthor()
	}
	wantJSON := nsBool(c.ns, "json")
	tenant := nsOptStr(c.ns, "tenant")
	taskID := nsStr(c.ns, "task_id")
	if taskID != "" && allFlag {
		return cliErr(c.errw, "kanban: pass either a task id OR --all, not both", 2), nil
	}
	var ids []string
	switch {
	case allFlag:
		var err error
		if ids, err = listIDs(tenant); err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			if wantJSON {
				text, err := pyjson.Dumps(pyjson.ObjectOf(jsonKey, 0, "total", 0))
				if err != nil {
					return 0, err
				}
				fmt.Fprintln(c.w, text)
			} else {
				suffix := ""
				if tenant != nil && *tenant != "" {
					suffix = " for tenant " + pyrepr.Repr(*tenant)
				}
				fmt.Fprintln(c.w, "No triage tasks"+suffix+".")
			}
			return 0, nil
		}
	case taskID != "":
		ids = []string{taskID}
	default:
		return cliErr(c.errw, "kanban: "+verb+" requires a task id or --all", 2), nil
	}
	okCount := 0
	for _, tid := range ids {
		outcome, err := runOne(tid, author)
		if err != nil {
			return 0, err
		}
		if outcome.ok {
			okCount++
		}
		switch {
		case wantJSON:
			text, err := pyjson.Dumps(outcome.fields)
			if err != nil {
				return 0, err
			}
			fmt.Fprintln(c.w, text)
		case outcome.ok:
			fmt.Fprintln(c.w, outcome.human)
		default:
			fmt.Fprintf(c.errw, "kanban: %s %s: %s\n", verb, outcome.taskID, outcome.reason)
		}
	}
	if !allFlag {
		if okCount == 1 {
			return 0, nil
		}
		return 1, nil
	}
	if okCount > 0 {
		return 0, nil
	}
	return 1, nil
}

// Retitle note of an outcome with a new title
func retitledSuffix(newTitle *string) string {
	if newTitle != nil && *newTitle != "" {
		return " — retitled: " + pyrepr.Repr(*newTitle)
	}
	return ""
}

// Specifies a triage task or all of them and promotes each to the backlog
func (c *kanbanCLI) cmdSpecify() (int, error) {
	return c.runTriageSweep("specify", "specified", func(tenant *string) ([]string, error) { return SpecifyListTriageIDs(c.ctx, tenant) },
		func(tid, author string) (*triageOutcome, error) {
			o, err := SpecifyTask(c.ctx, tid, &author, nil)
			if err != nil {
				return nil, err
			}
			return &triageOutcome{o.TaskID, o.OK, o.Reason,
				pyjson.ObjectOf("task_id", o.TaskID, "ok", o.OK, "reason", o.Reason, "new_title", optStrValue(o.NewTitle)),
				"Specified " + o.TaskID + " → todo" + retitledSuffix(o.NewTitle)}, nil
		})
}

// Human line of a successful decomposition
func decomposeOKLine(o *DecomposeOutcome) string {
	if o.Fanout && len(o.ChildIDs) > 0 {
		return fmt.Sprintf("Decomposed %s → %d children (%s); root promoted to todo", o.TaskID, len(o.ChildIDs), strings.Join(o.ChildIDs, ", "))
	}
	return "Specified " + o.TaskID + " → todo (no fanout)" + retitledSuffix(o.NewTitle)
}

// Fans a triage task or all of them out into routed child tasks
func (c *kanbanCLI) cmdDecompose() (int, error) {
	return c.runTriageSweep("decompose", "decomposed", func(tenant *string) ([]string, error) { return DecomposeListTriageIDs(c.ctx, tenant) },
		func(tid, author string) (*triageOutcome, error) {
			o, err := DecomposeTask(c.ctx, tid, &author, nil)
			if err != nil {
				return nil, err
			}
			var childIDs any
			if o.ChildIDs != nil {
				childIDs = stringsAny(o.ChildIDs)
			}
			return &triageOutcome{o.TaskID, o.OK, o.Reason,
				pyjson.ObjectOf("task_id", o.TaskID, "ok", o.OK, "reason", o.Reason, "fanout", o.Fanout, "child_ids", childIDs, "new_title", optStrValue(o.NewTitle)),
				decomposeOKLine(o)}, nil
		})
}

// Current epoch seconds
func nowUnix() int64 { return time.Now().Unix() }
