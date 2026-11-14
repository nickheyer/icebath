package dashboard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	kanbanReasonModel       = &Model{ClassName: "ReclaimBody", Fields: []ModelField{FieldDefault("reason", optStr, nil)}}
	kanbanTerminateRunModel = &Model{ClassName: "TerminateRunBody", Fields: []ModelField{FieldDefault("reason", optStr, nil)}}
	kanbanSpecifyModel      = &Model{ClassName: "SpecifyBody", Fields: []ModelField{FieldDefault("author", optStr, nil)}}
	kanbanReassignModel     = &Model{ClassName: "ReassignBody", Fields: []ModelField{
		FieldDefault("profile", optStr, nil), FieldDefault("reclaim_first", Bool, false), FieldDefault("reason", optStr, nil)}}
	kanbanEstimateModel = &Model{ClassName: "EstimateBody", Fields: []ModelField{
		FieldDefault("title", Str, ""), FieldDefault("body", optStr, nil)}}
)

const kanbanEstimateSystemPrompt = "You estimate how much work an autonomous coding agent will spend on a " +
	"kanban task. Given the task title and description, respond with STRICT " +
	"JSON only (no prose, no code fence):\n" +
	`{"est_tokens": <integer total tokens across the whole run>, ` +
	`"complexity": "S"|"M"|"L", ` +
	`"rationale": "<one short sentence>"}` + "\n" +
	"Base the token figure on a realistic multi-turn agent run (reading files, " +
	"tool calls, edits, retries) — not a single reply. S≈small/localized, " +
	"M≈multi-file, L≈broad or ambiguous. Be honest that this is a rough guess."

var kanbanEstimateBlobRE = pyre.MustCompile(`(?s)\{.*\}`)

// Tasks with an active diagnostic, highest severity first then most recently seen
func kanbanListDiagnostics(ctx context.Context, board, severity *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		order, byTask, err := kanban.ComputeTaskDiagnosticDicts(ctx, conn, nil, true)
		if err != nil {
			return nil, err
		}
		if severity != nil && *severity != "" && len(byTask) > 0 {
			var kept []string
			for _, tid := range order {
				var keep []any
				for _, raw := range byTask[tid] {
					d, _ := raw.(*pyjson.Object)
					var sev *string
					if s, ok := d.GetOr("severity", nil).(string); ok {
						sev = &s
					}
					if kanban.SeverityAtOrAbove(sev, severity) {
						keep = append(keep, raw)
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
		if len(byTask) == 0 {
			return pyjson.ObjectOf("diagnostics", []any{}, "count", int64(0)), nil
		}
		args := make([]any, 0, len(order))
		for _, id := range order {
			args = append(args, id)
		}
		taskRows, err := kanban.QueryRows(ctx, conn, "SELECT id, title, status, assignee FROM tasks WHERE id IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(order)), ",")+")", args...)
		if err != nil {
			return nil, err
		}
		rows := map[string]*kanban.Row{}
		for _, r := range taskRows {
			rows[pyrepr.Str(r.Get("id"))] = r
		}
		out := make([]*pyjson.Object, 0, len(order))
		for _, tid := range order {
			var title, status, assignee any
			if r := rows[tid]; r != nil {
				title, status, assignee = r.Get("title"), r.Get("status"), r.Get("assignee")
			}
			out = append(out, pyjson.ObjectOf("task_id", tid, "task_title", title, "task_status", status, "task_assignee", assignee,
				"diagnostics", byTask[tid]))
		}
		key := func(row *pyjson.Object) (int, int64) {
			first, _ := row.GetOr("diagnostics", nil).([]any)[0].(*pyjson.Object)
			idx := -1
			if sev, ok := first.GetOr("severity", nil).(string); ok {
				for i, name := range kanban.SeverityOrder {
					if name == sev {
						idx = i
					}
				}
			}
			return -idx, -kanbanInt(first.GetOr("last_seen_at", nil))
		}
		sort.SliceStable(out, func(i, j int) bool {
			si, li := key(out[i])
			sj, lj := key(out[j])
			if si != sj {
				return si < sj
			}
			return li < lj
		})
		count := int64(0)
		list := make([]any, 0, len(out))
		for _, row := range out {
			count += int64(len(row.GetOr("diagnostics", nil).([]any)))
			list = append(list, row)
		}
		return pyjson.ObjectOf("diagnostics", list, "count", count), nil
	})
}

// Every running worker, an open run with a worker pid whose task is running
func kanbanListActiveWorkers(ctx context.Context, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		rows, err := kanban.QueryRows(ctx, conn,
			"SELECT r.id AS run_id, r.task_id, t.title AS task_title, t.status AS task_status, "+
				"t.assignee AS task_assignee, r.profile, r.worker_pid, r.started_at, r.claim_lock, "+
				"r.claim_expires, r.last_heartbeat_at, r.max_runtime_seconds "+
				"FROM task_runs r JOIN tasks t ON t.id = r.task_id "+
				"WHERE r.ended_at IS NULL AND r.worker_pid IS NOT NULL AND t.status = 'running' "+
				"ORDER BY r.started_at ASC")
		if err != nil {
			return nil, err
		}
		workers := make([]any, 0, len(rows))
		for _, row := range rows {
			w := pyjson.NewObject()
			for _, col := range []string{"run_id", "task_id", "task_title", "task_status", "task_assignee", "profile", "worker_pid",
				"started_at", "claim_lock", "claim_expires", "last_heartbeat_at", "max_runtime_seconds"} {
				w.Set(col, row.Get(col))
			}
			workers = append(workers, w)
		}
		return pyjson.ObjectOf("workers", workers, "count", int64(len(workers)), "checked_at", time.Now().Unix()), nil
	})
}

// One run serialised like the runs of a task
func kanbanGetRun(ctx context.Context, runID int64, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		run, err := kanbanRequireRun(ctx, conn, runID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("run", kanban.RunAsDict(run)), nil
	})
}

// Value of one process attribute, nil when access to it is denied
func kanbanProcAttr[T any](value T, err error) (any, error) {
	if errors.Is(err, proc.ErrAccessDenied) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

// Live process stats of the worker of a run, a dead answer with its reason when there is none to read
func kanbanInspectRun(ctx context.Context, runID int64, board *string) (any, error) {
	found, err := kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		return kanbanRequireRun(ctx, conn, runID)
	})
	if err != nil {
		return nil, err
	}
	run := found.(*kanban.Run)
	dead := func(reason string, pid *int64) *pyjson.Object {
		out := pyjson.ObjectOf("run_id", runID, "alive", false)
		if pid != nil {
			out.Set("pid", *pid)
		}
		out.Set("reason", reason)
		return out
	}
	if run.EndedAt != nil {
		return dead("run already ended", nil), nil
	}
	if run.WorkerPID == nil {
		return dead("no worker_pid recorded", nil), nil
	}
	pid := *run.WorkerPID
	denied := pyjson.ObjectOf("run_id", runID, "alive", true, "pid", pid, "error", "access denied")
	exists, err := proc.PidExists(int(pid))
	if err != nil {
		return nil, err
	}
	if !exists {
		return dead("process not found", &pid), nil
	}
	attrs := make([]any, 6)
	reads := []func() (any, error){
		func() (any, error) { return kanbanProcAttr(proc.RSS(int(pid))) },
		func() (any, error) { return kanbanProcAttr(proc.VMS(int(pid))) },
		func() (any, error) { return kanbanProcAttr(proc.NumThreads(int(pid))) },
		func() (any, error) { return kanbanProcAttr(proc.StatusName(int(pid))) },
		func() (any, error) { return kanbanProcAttr(proc.Birth(int(pid))) },
		func() (any, error) {
			argv, err := proc.Cmdline(int(pid))
			return kanbanProcAttr(kanbanStrings(argv), err)
		},
	}
	for i, read := range reads {
		value, err := read()
		if errors.Is(err, proc.ErrNoSuchProcess) {
			return dead("process not found", &pid), nil
		}
		if err != nil {
			return nil, err
		}
		attrs[i] = value
	}
	var numFDs any
	if proc.NumFDsAvailable {
		n, err := proc.NumFDs(int(pid))
		switch {
		case errors.Is(err, proc.ErrNoSuchProcess):
			return dead("process not found", &pid), nil
		case errors.Is(err, proc.ErrAccessDenied), errors.Is(err, os.ErrPermission):
			return denied, nil
		case err != nil:
			return nil, err
		}
		numFDs = int64(n)
	}
	if n, ok := attrs[2].(int); ok {
		attrs[2] = int64(n)
	}
	rss, vms := attrs[0], attrs[1]
	if rss == nil || vms == nil {
		rss, vms = nil, nil
	}
	return pyjson.ObjectOf("run_id", runID, "alive", true, "pid", pid, "cpu_percent", 0.0,
		"memory_rss_bytes", rss, "memory_vms_bytes", vms, "num_threads", attrs[2], "num_fds", numFDs,
		"status", attrs[3], "create_time", attrs[4], "cmdline", attrs[5]), nil
}

// Terminates an in flight run through the reclaim path, a 409 when it ended or cannot be reclaimed
func kanbanTerminateRun(ctx context.Context, runID int64, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		run, err := kanbanRequireRun(ctx, conn, runID)
		if err != nil {
			return nil, err
		}
		if run.EndedAt != nil {
			return nil, kanbanConflict(fmt.Sprintf("run %d already ended", runID))
		}
		ok, err := kanban.ReclaimTask(ctx, conn, run.TaskID, kanbanField(payload, "reason"), nil)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, kanbanConflict(fmt.Sprintf("cannot terminate run %d: task %s is no longer in a reclaimable state", runID, run.TaskID))
		}
		return pyjson.ObjectOf("ok", true, "run_id", runID, "task_id", run.TaskID), nil
	})
}

// Releases an active worker claim without waiting out its lease
func kanbanReclaimTask(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		ok, err := kanban.ReclaimTask(ctx, conn, taskID, kanbanField(payload, "reason"), nil)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, kanbanConflict(fmt.Sprintf("cannot reclaim %s: not in a claimable state (not running, or unknown id)", taskID))
		}
		return pyjson.ObjectOf("ok", true, "task_id", taskID), nil
	})
}

// Fleshes out a triage task through the auxiliary model, a refusal answered inline
func kanbanSpecifyTask(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	pinned, err := kanbanBoardPinned(ctx, board)
	if err != nil {
		return nil, err
	}
	outcome, err := kanban.SpecifyTask(pinned, taskID, kanbanNonEmpty(kanbanText(kanbanField(payload, "author"))), nil)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", outcome.OK, "task_id", outcome.TaskID, "reason", outcome.Reason,
		"new_title", kanbanOptAny(outcome.NewTitle)), nil
}

// Reassigns a task to another profile, optionally reclaiming it first
func kanbanReassignTask(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		profile := kanbanNonEmpty(kanbanText(kanbanField(payload, "profile")))
		ok, err := kanban.ReassignTask(ctx, conn, taskID, profile, payload.GetOr("reclaim_first", false) == true, kanbanField(payload, "reason"))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, kanbanConflict(fmt.Sprintf("cannot reassign %s: unknown id, or still "+
				"running (pass reclaim_first=true to release the claim first)", taskID))
		}
		return pyjson.ObjectOf("ok", true, "task_id", taskID, "assignee", kanbanOptAny(profile)), nil
	})
}

// Stripped text clamped to n characters with an ellipsis
func kanbanCap(s *string, n int) string {
	text := pystr.Strip(kanbanText(s))
	if runes := []rune(text); len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return text
}

// Python int of a decoded JSON value, a caught false for a TypeError or ValueError and an error for an overflow
func kanbanPyInt(v any) (int64, bool, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	case int64:
		return x, true, nil
	case int:
		return int64(x), true, nil
	case *big.Int:
		return x.Int64(), true, nil
	case float64:
		if math.IsInf(x, 0) {
			return 0, false, errors.New("cannot convert float infinity to integer")
		}
		if math.IsNaN(x) {
			return 0, false, nil
		}
		return int64(math.Trunc(x)), true, nil
	case string:
		if n, ok := pyint.Parse(x); ok {
			return n.Int64(), true, nil
		}
	}
	return 0, false, nil
}

// Rough token and complexity read of a task from the auxiliary model, every failure answered inline
func kanbanRunEstimate(ctx context.Context, title string, body *string, taskID *string) (any, error) {
	refuse := func(reason string) (any, error) { return pyjson.ObjectOf("ok", false, "reason", reason), nil }
	if pystr.Strip(title) == "" {
		return refuse("a title is required to estimate")
	}
	description := kanbanCap(body, 4000)
	if description == "" {
		description = "(none)"
	}
	userMsg := fmt.Sprintf("Title: %s\n\nDescription:\n%s", kanbanCap(&title, 400), description)
	if provider.PortalAffinityScope(ctx) == "" {
		scope := "estimate"
		if taskID != nil && *taskID != "" {
			scope = *taskID
		}
		ctx = provider.WithPortalAffinityScope(ctx, "kanban:"+scope)
	}
	resp, err := provider.CallLLM(ctx, map[string]any{
		"task": "kanban_estimator",
		"messages": []any{map[string]any{"role": "system", "content": kanbanEstimateSystemPrompt},
			map[string]any{"role": "user", "content": userMsg}},
		"temperature": 0.0, "max_tokens": int64(300), "timeout": float64(60),
	})
	if err != nil {
		return refuse("LLM error: " + provider.AuxExcTypeName(err))
	}
	raw, model := "", any(nil)
	if m, ok := resp.(map[string]any); ok {
		model = m["model"]
		if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			msg, _ := choice["message"].(map[string]any)
			content, _ := msg["content"].(string)
			raw = pystr.Strip(content)
		}
	}
	text := raw
	if !strings.HasPrefix(pystr.Lstrip(raw), "{") {
		if m, err := kanbanEstimateBlobRE.FindStringMatch(raw); err == nil && m != nil {
			text = m.String()
		}
	}
	decoded, err := pyjson.Loads(text)
	parsed, isDict := decoded.(*pyjson.Object)
	if err != nil || !isDict || len(parsed.Keys) == 0 {
		return refuse("could not parse an estimate from the model")
	}
	estTokens := int64(0)
	if value := parsed.GetOr("est_tokens", nil); pyTruthy(value) {
		n, ok, err := kanbanPyInt(value)
		if err != nil {
			return nil, err
		}
		if ok {
			estTokens = n
		}
	}
	var complexity any
	if value := parsed.GetOr("complexity", nil); pyTruthy(value) {
		if c := pystr.Upper(pystr.Strip(pyrepr.Str(value))); c == "S" || c == "M" || c == "L" {
			complexity = c
		}
	}
	var rationale any
	if value := parsed.GetOr("rationale", nil); pyTruthy(value) {
		if r := pystr.Strip(pyrepr.Str(value)); r != "" {
			rationale = r
		}
	}
	return pyjson.ObjectOf("ok", true, "est_tokens", estTokens, "complexity", complexity, "rationale", rationale, "model", model), nil
}

// Estimate for an existing task
func kanbanEstimateTask(ctx context.Context, taskID string, board *string) (any, error) {
	found, err := kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		return kanbanRequireTask(ctx, conn, taskID)
	})
	if err != nil {
		return nil, err
	}
	task := found.(*kanban.Task)
	return kanbanRunEstimate(ctx, task.Title, task.Body, &taskID)
}

// Loaded config as a mapping, empty when it cannot be loaded
func kanbanLoadConfigOrEmpty(ctx context.Context) map[string]any {
	cfg, err := kanban.LoadConfigTree(ctx)
	if err != nil || cfg == nil {
		return map[string]any{}
	}
	return map[string]any(cfg)
}

// Mapping behind a config value, empty for a falsy one and an error for a truthy value that is no mapping
func kanbanConfigSection(v any) (map[string]any, error) {
	if !pyTruthy(v) {
		return map[string]any{}, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
}

// Truthiness of a config key that defaults when absent
func kanbanConfigBool(section map[string]any, key string, def bool) bool {
	if v, ok := section[key]; ok {
		return pyTruthy(v)
	}
	return def
}

// Kanban dashboard preferences of the dashboard.kanban config section
func kanbanGetConfig(ctx context.Context) (any, error) {
	dashboard, err := kanbanConfigSection(kanbanLoadConfigOrEmpty(ctx)["dashboard"])
	if err != nil {
		return nil, err
	}
	section, err := kanbanConfigSection(dashboard["kanban"])
	if err != nil {
		return nil, err
	}
	var tenant any = ""
	if v := section["default_tenant"]; pyTruthy(v) {
		tenant = v
	}
	return pyjson.ObjectOf("default_tenant", tenant, "lane_by_profile", kanbanConfigBool(section, "lane_by_profile", true),
		"include_archived_by_default", kanbanConfigBool(section, "include_archived_by_default", false),
		"render_markdown", kanbanConfigBool(section, "render_markdown", true)), nil
}

// Every platform with a home channel from the live gateway config, sorted by platform
func kanbanConfiguredHomeChannels(ctx context.Context) []*pyjson.Object {
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil || cfg == nil {
		return []*pyjson.Object{}
	}
	result := []*pyjson.Object{}
	for _, platform := range cfg.PlatformKeys() {
		pcfg := cfg.Platforms[platform]
		if pcfg == nil || pcfg.HomeChannel == nil {
			continue
		}
		var name any = "Home"
		if pyTruthy(pcfg.HomeChannel.Name) {
			name = pcfg.HomeChannel.Name
		}
		result = append(result, pyjson.ObjectOf("platform", string(platform), "chat_id", pcfg.HomeChannel.ChatID,
			"thread_id", kanbanText(pcfg.HomeChannel.ThreadID), "name", name))
	}
	sort.SliceStable(result, func(i, j int) bool {
		return pyrepr.Str(result[i].GetOr("platform", "")) < pyrepr.Str(result[j].GetOr("platform", ""))
	})
	return result
}

// Active profile name owning a notify subscription
func kanbanActiveProfileName(ctx context.Context) string {
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name
	}
	return "default"
}

// Home channel of a platform, a 404 with the detail when it has none
func kanbanHomeForPlatform(ctx context.Context, platform, detail string) (*pyjson.Object, error) {
	for _, h := range kanbanConfiguredHomeChannels(ctx) {
		if h.GetOr("platform", nil) == platform {
			return h, nil
		}
	}
	return nil, NewHTTPException(404, detail)
}

// Every platform home channel with whether the task is subscribed to it
func kanbanGetHomeChannels(ctx context.Context, taskID, board *string) (any, error) {
	homes := kanbanConfiguredHomeChannels(ctx)
	subscribed := map[[3]string]bool{}
	if taskID != nil && *taskID != "" {
		_, err := kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
			subs, err := kanban.ListNotifySubs(ctx, conn, taskID, kanban.NotifyOwnerScope{})
			if err != nil {
				return nil, err
			}
			text := func(sub *pyjson.Object, key string) string {
				if v := sub.GetOr(key, nil); pyTruthy(v) {
					return pyrepr.Str(v)
				}
				return ""
			}
			for _, sub := range subs {
				subscribed[[3]string{text(sub, "platform"), text(sub, "chat_id"), text(sub, "thread_id")}] = true
			}
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]any, 0, len(homes))
	for _, h := range homes {
		entry := h.Copy()
		entry.Set("subscribed", subscribed[[3]string{pyrepr.Str(h.GetOr("platform", "")), pyrepr.Str(h.GetOr("chat_id", "")),
			pyrepr.Str(h.GetOr("thread_id", ""))}])
		out = append(out, entry)
	}
	return pyjson.ObjectOf("home_channels", out), nil
}

// Subscribes a task to the home channel of a platform
func kanbanSubscribeHome(ctx context.Context, taskID, platform string, board *string) (any, error) {
	home, err := kanbanHomeForPlatform(ctx, platform, fmt.Sprintf("No home channel configured for platform %s. "+
		"Set one from the messenger via /sethome, or configure "+
		"gateway.platforms.%s.home_channel in config.yaml.", pyrepr.Repr(platform), platform))
	if err != nil {
		return nil, err
	}
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
			return nil, err
		}
		profile := kanbanActiveProfileName(ctx)
		if err := kanban.AddNotifySub(ctx, conn, kanban.NotifySubOptions{TaskID: taskID, Platform: platform,
			ChatID:   pyrepr.Str(home.GetOr("chat_id", "")),
			ThreadID: kanbanNonEmpty(pyrepr.Str(home.GetOr("thread_id", ""))), NotifierProfile: &profile}); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true, "task_id", taskID, "home_channel", home), nil
	})
}

// Removes the subscription of a task matching the home channel of a platform
func kanbanUnsubscribeHome(ctx context.Context, taskID, platform string, board *string) (any, error) {
	home, err := kanbanHomeForPlatform(ctx, platform, fmt.Sprintf("No home channel configured for platform %s.", pyrepr.Repr(platform)))
	if err != nil {
		return nil, err
	}
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanban.RemoveNotifySub(ctx, conn, taskID, platform, pyrepr.Str(home.GetOr("chat_id", "")),
			kanbanNonEmpty(pyrepr.Str(home.GetOr("thread_id", "")))); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true, "task_id", taskID, "home_channel", home), nil
	})
}

// Per status and per assignee counts with the oldest ready age
func kanbanGetStats(ctx context.Context, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		return kanban.BoardStats(ctx, conn)
	})
}

// Union of profiles on disk and assignees used on the board
func kanbanGetAssignees(ctx context.Context, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		known, err := kanban.KnownAssignees(ctx, conn)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(known))
		for _, entry := range known {
			out = append(out, entry)
		}
		return pyjson.ObjectOf("assignees", out), nil
	})
}

// Worker log of a task with its path and size, the tail capping the answered bytes
func kanbanGetTaskLog(ctx context.Context, taskID string, tail *int64, board *string) (any, error) {
	resolved, err := kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
			return nil, err
		}
		return board, nil
	})
	if err != nil {
		return nil, err
	}
	logBoard, _ := resolved.(*string)
	content, err := kanban.ReadWorkerLog(ctx, taskID, tail, logBoard)
	if err != nil {
		return nil, err
	}
	logPath, err := kanban.WorkerLogPath(ctx, taskID, logBoard)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if info, err := os.Stat(logPath); err == nil {
		size = info.Size()
	}
	return pyjson.ObjectOf("task_id", taskID, "path", logPath, "exists", content != nil, "size_bytes", size,
		"content", kanbanText(content), "truncated", tail != nil && *tail != 0 && size > *tail), nil
}

// One dispatcher pass on demand so the UI need not wait out the dispatcher tick
func kanbanDispatch(ctx context.Context, dryRun bool, maxN int64, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		opts := kanban.DefaultDispatchOptions()
		maxSpawn := int(maxN)
		opts.DryRun, opts.MaxSpawn, opts.Board = dryRun, &maxSpawn, board
		result, err := kanban.DispatchOnce(ctx, conn, opts)
		if err != nil {
			return nil, err
		}
		return kanban.DispatchResultAsDict(result), nil
	})
}

// Providers and curated models of the override dropdown, an empty catalog when the inventory fails
func kanbanModelOptions(ctx context.Context) (any, error) {
	empty := pyjson.ObjectOf("providers", []any{})
	pc, err := provider.LoadPickerContext(ctx)
	if err != nil {
		kanbanLog.Error("kanban model-options failed", "error", err)
		return empty, nil
	}
	payload, err := provider.BuildModelsPayload(ctx, pc, provider.ModelsPayloadOptions{ExplicitOnly: true, CanonicalOrder: true})
	if err != nil {
		kanbanLog.Error("kanban model-options failed", "error", err)
		return empty, nil
	}
	providers := []any{}
	rows, _ := payload["providers"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if !pyTruthy(row["models"]) {
			continue
		}
		slug, ok := row["slug"]
		if !ok {
			slug = ""
		}
		label := row["label"]
		if !pyTruthy(label) {
			label = slug
		}
		models, _ := row["models"].([]any)
		providers = append(providers, pyjson.ObjectOf("slug", slug, "label", label, "models", append([]any{}, models...)))
	}
	return pyjson.ObjectOf("providers", providers), nil
}
