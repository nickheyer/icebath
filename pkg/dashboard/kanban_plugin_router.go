package dashboard

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Kanban dashboard manifest, script bundle and stylesheet packed as one archive
//
//go:embed kanban_dashboard.tar.zst
var kanbanDashboardArchive []byte

// Seconds between two polls of the task events table
const kanbanEventPollSeconds = 0.3

func init() {
	assets, err := fs.Sub(tarzst.NewFS(kanbanDashboardArchive, ""), "dashboard")
	if err != nil {
		panic(err)
	}
	manifest, err := fs.ReadFile(assets, "manifest.json")
	if err != nil {
		panic(err)
	}
	plugins.RegisterBundledDashboard(plugins.BundledDashboard{Name: kanbanPluginName, Manifest: manifest, Assets: assets})
	compiledPluginRouters[kanbanPluginName] = kanbanPluginRouter
}

// Body of a call as its validated object
func kanbanBody(c *Call, name string) *pyjson.Object { return c.Object(name) }

// Every kanban dashboard route under the plugin api prefix in declaration order
func kanbanPluginRouter() *APIRouter {
	r := NewAPIRouter("/api/plugins/" + kanbanPluginName)
	taskID := PathParam("task_id", Str)
	optBoard := QueryParam("board", optStr, nil)
	runID := PathParam("run_id", Int)
	r.Get("/board", &Endpoint{
		Name: "get_board_endpoint",
		Params: []Param{QueryParam("tenant", optStr, nil), QueryParam("include_archived", Bool, false), kanbanBoardQuery,
			QueryParam("workflow_template_id", optStr, nil), QueryParam("current_step_key", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			board := c.Str("board")
			if board == "" {
				board = kanban.GetCurrentBoard(ctx)
			}
			return kanbanReadBoard.call(ctx, c.Value("tenant"), c.Bool("include_archived"), board,
				c.Value("workflow_template_id"), c.Value("current_step_key"))
		},
	})
	r.Get("/tasks/{task_id}", &Endpoint{
		Name:   "get_task",
		Params: []Param{taskID, optBoard, QueryParam("run_state_type", optStr, nil), QueryParam("run_state_name", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanGetTask(ctx, c.Str("task_id"), kanbanOpt(c, "board"), kanbanOpt(c, "run_state_type"), kanbanOpt(c, "run_state_name"))
		},
	})
	r.Post("/tasks", &Endpoint{
		Name: "create_task", Params: []Param{optBoard}, Body: []BodyParam{Body("payload", kanbanCreateTaskModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanCreateTask(ctx, kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/tasks/{task_id}/attachments", &Endpoint{
		Name: "list_task_attachments", Params: []Param{taskID, optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanListTaskAttachments(ctx, c.Str("task_id"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/attachments", &Endpoint{
		Name: "upload_task_attachment", Params: []Param{taskID, optBoard},
		Body: []BodyParam{FileField("file"), FormFieldDefault("uploaded_by", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			file, _ := c.Value("file").(*UploadFile)
			return kanbanUploadTaskAttachment(ctx, c.Str("task_id"), file, kanbanOpt(c, "board"), kanbanOpt(c, "uploaded_by"))
		},
	})
	r.Get("/attachments/{attachment_id}", &Endpoint{
		Name: "download_attachment", Params: []Param{PathParam("attachment_id", Int), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDownloadAttachment(ctx, c.Int("attachment_id"), kanbanOpt(c, "board"))
		},
	})
	r.Delete("/attachments/{attachment_id}", &Endpoint{
		Name: "remove_attachment", Params: []Param{PathParam("attachment_id", Int), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanRemoveAttachment(ctx, c.Int("attachment_id"), kanbanOpt(c, "board"))
		},
	})
	r.Patch("/tasks/{task_id}", &Endpoint{
		Name: "update_task", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanUpdateTaskModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanUpdateTask(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Delete("/tasks/{task_id}", &Endpoint{
		Name: "delete_task", Params: []Param{taskID, optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDeleteTask(ctx, c.Str("task_id"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/comments", &Endpoint{
		Name: "add_comment", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanCommentModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanAddComment(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/links", &Endpoint{
		Name: "add_link", Params: []Param{optBoard}, Body: []BodyParam{Body("payload", kanbanLinkModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanAddLink(ctx, kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Delete("/links", &Endpoint{
		Name: "delete_link", Params: []Param{RequiredQuery("parent_id", Str), RequiredQuery("child_id", Str), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDeleteLink(ctx, c.Str("parent_id"), c.Str("child_id"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/bulk", &Endpoint{
		Name: "bulk_update", Params: []Param{optBoard}, Body: []BodyParam{Body("payload", kanbanBulkTaskModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanBulkUpdate(ctx, kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/diagnostics", &Endpoint{
		Name: "list_diagnostics", Params: []Param{kanbanBoardQuery, QueryParam("severity", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanListDiagnostics(ctx, kanbanOpt(c, "board"), kanbanOpt(c, "severity"))
		},
	})
	r.Get("/workers/active", &Endpoint{
		Name: "list_active_workers", Params: []Param{kanbanBoardQuery},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanListActiveWorkers(ctx, kanbanOpt(c, "board"))
		},
	})
	r.Get("/runs/{run_id}", &Endpoint{
		Name: "get_run_endpoint", Params: []Param{runID, kanbanBoardQuery},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanGetRun(ctx, c.Int("run_id"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/runs/{run_id}/inspect", &Endpoint{
		Name: "inspect_run_endpoint", Params: []Param{runID, kanbanBoardQuery},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanInspectRun(ctx, c.Int("run_id"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/runs/{run_id}/terminate", &Endpoint{
		Name: "terminate_run_endpoint", Params: []Param{runID, kanbanBoardQuery},
		Body: []BodyParam{Body("payload", kanbanTerminateRunModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanTerminateRun(ctx, c.Int("run_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/reclaim", &Endpoint{
		Name: "reclaim_task_endpoint", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanReasonModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanReclaimTask(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/specify", &Endpoint{
		Name: "specify_task_endpoint", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanSpecifyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanSpecifyTask(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/reassign", &Endpoint{
		Name: "reassign_task_endpoint", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanReassignModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanReassignTask(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/estimate", &Endpoint{
		Name: "estimate_text_endpoint", Body: []BodyParam{Body("payload", kanbanEstimateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			payload := kanbanBody(c, "payload")
			title, _ := payload.GetOr("title", "").(string)
			return kanbanRunEstimate(ctx, title, kanbanField(payload, "body"), nil)
		},
	})
	r.Post("/tasks/{task_id}/estimate", &Endpoint{
		Name: "estimate_task_endpoint", Params: []Param{taskID, optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanEstimateTask(ctx, c.Str("task_id"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/config", &Endpoint{
		Name:   "get_config",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanGetConfig(ctx) },
	})
	r.Get("/home-channels", &Endpoint{
		Name: "get_home_channels", Params: []Param{QueryParam("task_id", optStr, nil), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanGetHomeChannels(ctx, kanbanOpt(c, "task_id"), kanbanOpt(c, "board"))
		},
	})
	r.Post("/tasks/{task_id}/home-subscribe/{platform}", &Endpoint{
		Name: "subscribe_home", Params: []Param{taskID, PathParam("platform", Str), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanSubscribeHome(ctx, c.Str("task_id"), c.Str("platform"), kanbanOpt(c, "board"))
		},
	})
	r.Delete("/tasks/{task_id}/home-subscribe/{platform}", &Endpoint{
		Name: "unsubscribe_home", Params: []Param{taskID, PathParam("platform", Str), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanUnsubscribeHome(ctx, c.Str("task_id"), c.Str("platform"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/stats", &Endpoint{
		Name: "get_stats", Params: []Param{optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanGetStats(ctx, kanbanOpt(c, "board")) },
	})
	r.Get("/assignees", &Endpoint{
		Name: "get_assignees", Params: []Param{optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanGetAssignees(ctx, kanbanOpt(c, "board")) },
	})
	r.Get("/tasks/{task_id}/log", &Endpoint{
		Name:   "get_task_log",
		Params: []Param{taskID, QueryParam("tail", Optional(IntRange(Bound(1), Bound(2_000_000))), nil), optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			var tail *int64
			if n, ok := c.Value("tail").(int64); ok {
				tail = &n
			}
			return kanbanGetTaskLog(ctx, c.Str("task_id"), tail, kanbanOpt(c, "board"))
		},
	})
	r.Post("/dispatch", &Endpoint{
		Name: "dispatch",
		Params: []Param{QueryParam("dry_run", Bool, false),
			{Name: "max_n", Alias: "max", In: "query", Type: Int, Default: int64(8)}, optBoard},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDispatch(ctx, c.Bool("dry_run"), c.Int("max_n"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/model-options", &Endpoint{
		Name:   "model_options",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanModelOptions(ctx) },
	})
	r.Get("/projects", &Endpoint{
		Name:   "list_kanban_projects",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanListProjects(ctx) },
	})
	r.Get("/boards", &Endpoint{
		Name: "list_boards", Params: []Param{QueryParam("include_archived", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanListBoards(ctx, c.Bool("include_archived"))
		},
	})
	r.Post("/boards", &Endpoint{
		Name: "create_board_endpoint", Body: []BodyParam{Body("payload", kanbanCreateBoardModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanCreateBoard(ctx, kanbanBody(c, "payload"))
		},
	})
	r.Patch("/boards/{slug}", &Endpoint{
		Name: "rename_board", Params: []Param{PathParam("slug", Str)}, Body: []BodyParam{Body("payload", kanbanRenameBoardModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanRenameBoard(ctx, c.Str("slug"), kanbanBody(c, "payload"))
		},
	})
	r.Delete("/boards/{slug}", &Endpoint{
		Name: "delete_board", Params: []Param{PathParam("slug", Str), QueryParam("delete", Bool, false)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDeleteBoard(ctx, c.Str("slug"), c.Bool("delete"))
		},
	})
	r.Post("/boards/{slug}/export", &Endpoint{
		Name: "export_board_endpoint", Params: []Param{PathParam("slug", Str)}, Body: []BodyParam{Body("body", kanbanExportBoardModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanExportBoard(ctx, c.Str("slug"), kanbanBody(c, "body"))
		},
	})
	r.Post("/boards/import", &Endpoint{
		Name: "import_board_endpoint", Body: []BodyParam{Body("body", kanbanImportBoardModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanImportBoard(ctx, kanbanBody(c, "body")) },
	})
	r.Post("/boards/{slug}/switch", &Endpoint{
		Name: "switch_board", Params: []Param{PathParam("slug", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanSwitchBoard(ctx, c.Str("slug")) },
	})
	r.Get("/profiles", &Endpoint{
		Name:   "list_profile_roster",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanListProfileRoster(ctx) },
	})
	r.Patch("/profiles/{profile_name}", &Endpoint{
		Name: "update_profile_description", Params: []Param{PathParam("profile_name", Str)},
		Body: []BodyParam{Body("payload", kanbanDescribeModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanUpdateProfileDescription(ctx, c.Str("profile_name"), kanbanBody(c, "payload"))
		},
	})
	r.Post("/profiles/{profile_name}/describe-auto", &Endpoint{
		Name: "auto_describe_profile", Params: []Param{PathParam("profile_name", Str)},
		Body: []BodyParam{Body("payload", kanbanDescribeAutoModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanAutoDescribeProfile(ctx, c.Str("profile_name"), kanbanBody(c, "payload"))
		},
	})
	r.Post("/tasks/{task_id}/decompose", &Endpoint{
		Name: "decompose_task_endpoint", Params: []Param{taskID, optBoard}, Body: []BodyParam{Body("payload", kanbanDecomposeModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanDecomposeTask(ctx, c.Str("task_id"), kanbanBody(c, "payload"), kanbanOpt(c, "board"))
		},
	})
	r.Get("/workflow", &Endpoint{
		Name:   "get_workflow",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanban.DefaultWorkflow.ToDict(), nil },
	})
	r.Get("/orchestration", &Endpoint{
		Name:   "get_orchestration_settings",
		Handle: func(ctx context.Context, c *Call) (any, error) { return kanbanGetOrchestrationSettings(ctx) },
	})
	r.Put("/orchestration", &Endpoint{
		Name: "set_orchestration_settings", Body: []BodyParam{Body("payload", kanbanOrchestrationModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return kanbanSetOrchestrationSettings(ctx, kanbanBody(c, "payload"))
		},
	})
	r.WebSocket("/events", &WSEndpoint{Name: "stream_events", Handle: kanbanStreamEvents})
	return r
}

// Event cursor a client sent, false when it sent none or one that is no integer
func kanbanSinceParam(ws *WebSocket) (int64, bool) {
	raw, ok := ws.QueryParams().Lookup("since")
	if !ok || pystr.Strip(raw) == "" {
		return 0, false
	}
	n, ok := pyint.Parse(raw)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return max(0, n.Int64()), true
}

// Normalised board of a socket query value, nil for none or a malformed one
func kanbanWSBoard(raw string) *string {
	if raw == "" {
		return nil
	}
	normed, err := kanban.NormalizeBoardSlug(&raw)
	if err != nil || normed == "" {
		return nil
	}
	return &normed
}

// The normalised slug when it names a live board, nil for an unknown one
func kanbanWSBoardLive(normed *string) *string {
	if normed == nil || *normed == "" || *normed == kanban.DefaultBoard {
		return normed
	}
	if kanban.BoardExists(normed) {
		return normed
	}
	return nil
}

// Streams new task events of one board to an authorised socket until it disconnects
func kanbanStreamEvents(ctx context.Context, c *Call) error {
	ws := c.WebSocket
	authorized, err := wsAuthOK(ctx, ws, false, nil)
	if err != nil {
		return err
	}
	if !authorized {
		return ws.Close(ctx, 1008, "")
	}
	if err := ws.Accept(ctx, nil, nil); err != nil {
		return err
	}
	rawBoard := kanbanWSBoard(ws.QueryParams().Get("board"))
	board := kanbanWSBoardLive(rawBoard)
	if rawBoard != nil && board == nil {
		return ws.Close(ctx, 1008, "")
	}
	tail := &kanbanEventTail{board: board}
	defer tail.shutdown()
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	gone := make(chan error, 1)
	go func() {
		for {
			msg, err := ws.Receive(rctx)
			if err != nil {
				gone <- err
				return
			}
			if msg.Type == "websocket.disconnect" {
				gone <- nil
				return
			}
		}
	}()
	failed := func(err error) error {
		var disconnect *WebSocketDisconnect
		if errors.As(err, &disconnect) || errors.Is(err, context.Canceled) {
			return nil
		}
		kanbanLog.Warn(fmt.Sprintf("Kanban event stream error: %s", pyExcStr(err)))
		_ = ws.Close(ctx, 1000, "")
		return nil
	}
	cursor, since := kanbanSinceParam(ws)
	if !since {
		if cursor, err = tail.latest(ctx); err != nil {
			return failed(err)
		}
	}
	ticker := time.NewTicker(time.Duration(kanbanEventPollSeconds * float64(time.Second)))
	defer ticker.Stop()
	for {
		select {
		case err := <-gone:
			if err != nil {
				return failed(err)
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		var events []any
		if cursor, events, err = tail.poll(ctx, cursor); err != nil {
			return failed(err)
		}
		if len(events) > 0 {
			if err := ws.SendJSON(ctx, pyjson.ObjectOf("events", events, "cursor", cursor), false); err != nil {
				return failed(err)
			}
		}
	}
}
