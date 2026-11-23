//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var hubKanbanRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubKanbanRegistry, tuigateway.KanbanBoardView.Name(), 5180, rpcShape{}, kanbanBoardView)
	scopedRPC(hubKanbanRegistry, tuigateway.KanbanBoardCommand.Name(), 5180, rpcShape{}, kanbanCommandRPC(kanbanBoardVerbs, true))
	scopedRPC(hubKanbanRegistry, tuigateway.KanbanTaskShow.Name(), 5180, rpcShape{}, kanbanTaskShow)
	scopedRPC(hubKanbanRegistry, tuigateway.KanbanTaskCommand.Name(), 5180, rpcShape{}, kanbanCommandRPC(kanbanTaskVerbs, false))
	hubKanbanRegistry.install()
}

// Board verbs the board command runs, the ones that answer at once
var kanbanBoardVerbs = map[string]bool{"list": true, "create": true, "switch": true, "rename": true, "rm": true, "show": true, "set-default-workdir": true}

// Task verbs the task command runs, every one that answers at once: never tail, watch, daemon or dispatch, which follow the board until stopped
var kanbanTaskVerbs = map[string]bool{"create": true, "edit": true, "assign": true, "reassign": true, "comment": true, "complete": true,
	"block": true, "schedule": true, "unblock": true, "promote": true, "request-review": true, "request-changes": true, "reopen-review": true,
	"reopen": true, "archive": true, "link": true, "unlink": true, "set-model": true, "reclaim": true, "specify": true, "decompose": true,
	"show": true, "runs": true, "log": true, "context": true, "stats": true, "assignees": true, "diagnostics": true}

// Refusal of params that do not fit a kanban hub method
func kanbanParamsRefused(rid any, err error) map[string]any {
	return rpcErr(rid, 4180, err.Error(), nil)
}

// Board a request names, the current board when it names none
func kanbanBoardOf(ctx context.Context, board string) string {
	if board = strings.TrimSpace(board); board != "" {
		return board
	}
	return kanban.GetCurrentBoard(ctx)
}

// Text of a metadata field, empty when absent
func kanbanMetaText(meta *pyjson.Object, key string) string {
	if v, ok := meta.Get(key); ok && pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Number of an integer cell, zero for anything else
func kanbanInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// Tasks of a board by column, zero for an empty board or one with no database yet
func kanbanCounts(ctx context.Context, slug string) (map[string]int64, error) {
	counts := map[string]int64{}
	path, err := kanban.KanbanDBPath(ctx, &slug)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return counts, nil
	}
	err = kanban.ConnectClosing(ctx, "", &slug, func(conn state.SQLiteConn) error {
		rows, err := kanban.QueryRows(ctx, conn, "SELECT status, COUNT(*) AS n FROM tasks GROUP BY status")
		for _, r := range rows {
			counts[pyrepr.Str(r.Get("status"))] = kanbanInt(r.Get("n"))
		}
		return err
	})
	return counts, err
}

// Text an optional string holds, empty for nil
func kanbanText(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Epoch an optional stamp holds, zero for nil
func kanbanEpoch(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Row of a task for the board
func kanbanTaskRow(t *kanban.Task) tuigateway.KanbanTaskRow {
	return tuigateway.KanbanTaskRow{ID: t.ID, Title: t.Title, Body: kanbanText(t.Body), Status: t.Status, Assignee: kanbanText(t.Assignee),
		Priority: t.Priority, CreatedBy: kanbanText(t.CreatedBy), CreatedAt: kanbanEpoch(t.CreatedAt), StartedAt: kanbanEpoch(t.StartedAt),
		CompletedAt: kanbanEpoch(t.CompletedAt), Skills: t.Skills, Result: kanbanText(t.Result), LastError: kanbanText(t.LastFailureError),
		Model: kanbanText(t.ModelOverride), BlockKind: kanbanText(t.BlockKind)}
}

// One board in full with every board to switch to, its columns and its tasks
func kanbanBoardView(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.KanbanBoardView.Params(params)
	if err != nil {
		return kanbanParamsRefused(rid, err), nil
	}
	slug := kanbanBoardOf(ctx, p.Board)
	current := kanban.GetCurrentBoard(ctx)
	out := tuigateway.KanbanBoardResult{Board: slug, Boards: []tuigateway.KanbanBoard{}, Columns: []tuigateway.KanbanColumn{}, Tasks: []tuigateway.KanbanTaskRow{}}
	boards, err := kanban.ListBoards(false)
	if err != nil {
		return nil, err
	}
	for _, b := range boards {
		bslug := kanbanMetaText(b, "slug")
		counts, err := kanbanCounts(ctx, bslug)
		if err != nil {
			return nil, err
		}
		out.Boards = append(out.Boards, tuigateway.KanbanBoard{Slug: bslug, Name: kanbanMetaText(b, "name"), Description: kanbanMetaText(b, "description"),
			Icon: kanbanMetaText(b, "icon"), Current: bslug == current, Archived: kanbanMetaText(b, "archived") != "", Counts: counts})
	}
	for col := range kanban.DefaultWorkflow.All() {
		out.Columns = append(out.Columns, tuigateway.KanbanColumn{Key: col.Key, Label: col.Label, Icon: col.Icon})
	}
	if _, err := kanban.InitDB(ctx, "", &slug); err != nil {
		return nil, err
	}
	err = kanban.ConnectClosing(ctx, "", &slug, func(conn state.SQLiteConn) error {
		tasks, err := kanban.ListTasks(ctx, conn, kanban.ListTasksOptions{IncludeArchived: p.Archived})
		if err != nil {
			return err
		}
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, kanbanTaskRow(t))
		}
		known, err := kanban.KnownAssignees(ctx, conn)
		for _, a := range known {
			if name := kanbanMetaText(a, "name"); name != "" {
				out.Assignees = append(out.Assignees, name)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One task with its comments, attempts and links
func kanbanTaskShow(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.KanbanTaskShow.Params(params)
	if err != nil {
		return kanbanParamsRefused(rid, err), nil
	}
	slug := kanbanBoardOf(ctx, p.Board)
	if _, err := kanban.InitDB(ctx, "", &slug); err != nil {
		return nil, err
	}
	var out tuigateway.KanbanTaskResult
	found := false
	err = kanban.ConnectClosing(ctx, "", &slug, func(conn state.SQLiteConn) error {
		task, err := kanban.GetTask(ctx, conn, p.TaskID)
		if err != nil || task == nil {
			return err
		}
		found = true
		out.Task = kanbanTaskRow(task)
		comments, err := kanban.ListComments(ctx, conn, p.TaskID)
		if err != nil {
			return err
		}
		out.Comments = make([]tuigateway.KanbanComment, 0, len(comments))
		for _, c := range comments {
			out.Comments = append(out.Comments, tuigateway.KanbanComment{ID: c.ID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt})
		}
		runs, err := kanban.ListRuns(ctx, conn, p.TaskID, true, nil, nil)
		if err != nil {
			return err
		}
		out.Runs = make([]tuigateway.KanbanAttempt, 0, len(runs))
		for _, r := range runs {
			out.Runs = append(out.Runs, tuigateway.KanbanAttempt{ID: r.ID, Profile: kanbanText(r.Profile), Status: r.Status, Outcome: kanbanText(r.Outcome),
				Summary: kanbanText(r.Summary), Error: kanbanText(r.Error), StartedAt: r.StartedAt, EndedAt: kanbanEpoch(r.EndedAt)})
		}
		if out.Parents, err = kanban.ParentIDs(ctx, conn, p.TaskID); err != nil {
			return err
		}
		out.Children, err = kanban.ChildIDs(ctx, conn, p.TaskID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return rpcErr(rid, 4181, "no task "+p.TaskID+" on the board "+slug, nil), nil
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// First text that is not blank
func kanbanFirstText(texts ...string) string {
	for _, t := range texts {
		if strings.TrimSpace(t) != "" {
			return t
		}
	}
	return ""
}

// Runs a kanban command in this process the way icebath kanban does, a board verb after boards and a task verb on the named board,
// refusing a verb that follows the board until stopped
func kanbanCommandRPC(verbs map[string]bool, boards bool) rpcBody {
	return func(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
		p, err := tuigateway.KanbanTaskCommand.Params(params)
		if err != nil {
			return kanbanParamsRefused(rid, err), nil
		}
		if len(p.Args) == 0 || !verbs[p.Args[0]] {
			return rpcErr(rid, 4182, "the kanban screen does not run that command", nil), nil
		}
		args := slices.Clone(p.Args)
		switch {
		case boards:
			args = append([]string{"boards"}, args...)
		case strings.TrimSpace(p.Board) != "":
			args = append(args, "--board", strings.TrimSpace(p.Board))
		}
		if kanban.ParseSlash == nil {
			return nil, errors.New("the kanban command parser is not bound in this build")
		}
		parsed := kanban.ParseSlash(args)
		if !parsed.Parsed {
			why := kanbanFirstText(parsed.ArgError, strings.TrimSpace(parsed.Err), strings.TrimSpace(parsed.Out))
			if why == "" && parsed.Failure != nil {
				why = parsed.Failure.Error()
			}
			return ok(rid, tuigateway.Plain(tuigateway.KanbanCommandResult{Output: strings.TrimSpace(ansi.Strip(why))})), nil
		}
		var out, errw bytes.Buffer
		out.WriteString(parsed.Out)
		errw.WriteString(parsed.Err)
		rc, kerr := kanban.KanbanCommand(ctx, parsed.Namespace, &out, &errw)
		if kerr != nil {
			fmt.Fprintln(&errw, pyrepr.ExceptionMessage(kerr))
		}
		text := strings.TrimSpace(ansi.Strip(strings.TrimSpace(out.String()) + "\n" + strings.TrimSpace(errw.String())))
		failed := rc != 0 || kerr != nil
		return ok(rid, tuigateway.Plain(tuigateway.KanbanCommandResult{OK: !failed, Output: text})), nil
	}
}
