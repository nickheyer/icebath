package kanban

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs one boards subcommand, list when none is named
func (c *kanbanCLI) dispatchBoards() (int, error) {
	sub := nsStr(c.ns, "boards_action")
	if sub == "" {
		sub = "list"
	}
	var handler func() (int, error)
	switch sub {
	case "list", "ls":
		handler = c.cmdBoardsList
	case "create", "new":
		handler = c.cmdBoardsCreate
	case "rm", "remove", "delete":
		handler = c.cmdBoardsRm
	case "switch", "use":
		handler = c.cmdBoardsSwitch
	case "show", "current":
		handler = c.cmdBoardsShow
	case "rename":
		handler = c.cmdBoardsRename
	case "set-default-workdir":
		handler = c.cmdBoardsSetDefaultWorkdir
	case "export":
		handler = c.cmdBoardsExport
	case "import":
		handler = c.cmdBoardsImport
	default:
		return cliErr(c.errw, "kanban boards: unknown action "+pyrepr.Repr(sub), 2), nil
	}
	return handler()
}

// Status to count mapping of a board, empty when it has no database or cannot be read
func (c *kanbanCLI) boardTaskCounts(slug string) *pyjson.Object {
	counts := pyjson.NewObject()
	path, err := KanbanDBPath(c.ctx, &slug)
	if err != nil {
		return counts
	}
	if _, err := os.Stat(path); err != nil {
		return counts
	}
	var rows []*Row
	if err := ConnectClosing(c.ctx, "", &slug, func(conn state.SQLiteConn) error {
		var err error
		rows, err = QueryRows(c.ctx, conn, "SELECT status, COUNT(*) AS n FROM tasks GROUP BY status")
		return err
	}); err != nil {
		return pyjson.NewObject()
	}
	for _, r := range rows {
		n, err := pyint.Of(r.Get("n"))
		if err != nil {
			return pyjson.NewObject()
		}
		counts.Set(pyrepr.Str(r.Get("status")), n.Int64())
	}
	return counts
}

// Count mapping as plain integers for fmtCounts
func countsMap(counts *pyjson.Object) map[string]int64 {
	out := map[string]int64{}
	for _, k := range counts.Keys {
		v, _ := counts.Get(k)
		n, _ := v.(int64)
		out[k] = n
	}
	return out
}

// Sum of a count mapping
func countsTotal(counts *pyjson.Object) int64 {
	var total int64
	for _, n := range countsMap(counts) {
		total += n
	}
	return total
}

// Normalized slug argument of a boards subcommand, with the exit code of a refusal
func (c *kanbanCLI) boardSlugArg(cmd string, mustExist bool) (string, int) {
	raw := c.ns["slug"]
	var slugArg *string
	if raw != nil {
		s := pyrepr.Str(raw)
		slugArg = &s
	}
	normed, err := normalizeBoardSlug(slugArg)
	if err != nil {
		return "", cliErr(c.errw, fmt.Sprintf("kanban boards %s: %s", cmd, pyrepr.ExceptionMessage(err)), 2)
	}
	if mustExist {
		if normed == "" || !BoardExists(&normed) {
			return "", cliErr(c.errw, fmt.Sprintf("kanban boards %s: board %s does not exist", cmd, pyrepr.Repr(raw)), 1)
		}
	} else if normed == "" {
		return "", cliErr(c.errw, fmt.Sprintf("kanban boards %s: slug is required", cmd), 2)
	}
	return normed, 0
}

// Metadata field of a board as display text, empty when absent
func metaText(meta *pyjson.Object, key string) string {
	v, ok := meta.Get(key)
	if !ok || v == nil {
		if ok {
			return "None"
		}
		return ""
	}
	return pyrepr.Str(v)
}

// Lists boards with their task counts, archived ones too with --all
func (c *kanbanCLI) cmdBoardsList() (int, error) {
	boards, err := ListBoards(nsBool(c.ns, "all"))
	if err != nil {
		return 0, err
	}
	current := GetCurrentBoard(c.ctx)
	list := make([]any, 0, len(boards))
	for _, b := range boards {
		slug := metaText(b, "slug")
		b.Set("is_current", slug == current)
		counts := c.boardTaskCounts(slug)
		b.Set("counts", counts)
		b.Set("total", countsTotal(counts))
		list = append(list, b)
	}
	if done, err := jsonOut(c, list, false); done || err != nil {
		return 0, err
	}
	if len(boards) == 0 {
		fmt.Fprintln(c.w, "(no boards — create one with `icebath kanban boards create <slug>`)")
		return 0, nil
	}
	fmt.Fprintf(c.w, "%-2s  %-24s  %-28s  COUNTS\n", "", "SLUG", "NAME")
	for _, b := range boards {
		marker := " "
		if cur, _ := b.Get("is_current"); cur == true {
			marker = "●"
		}
		name := ""
		if v, _ := b.Get("name"); pyrepr.Truthy(v) {
			name = pyrepr.Str(v)
		}
		if v, _ := b.Get("archived"); pyrepr.Truthy(v) {
			name += " [archived]"
		}
		counts, _ := b.Get("counts")
		fmt.Fprintf(c.w, "%-2s  %-24s  %-28s  %s\n", marker, metaText(b, "slug"), name, fmtCounts(countsMap(counts.(*pyjson.Object)), "(empty)"))
	}
	fmt.Fprintf(c.w, "\nCurrent board: %s\n", current)
	if len(boards) > 1 {
		fmt.Fprintln(c.w, "Switch boards with `icebath kanban boards switch <slug>`.")
	}
	return 0, nil
}

// Creates a board, switching to it with --switch
func (c *kanbanCLI) cmdBoardsCreate() (int, error) {
	normed, rc := c.boardSlugArg("create", false)
	if rc != 0 {
		return rc, nil
	}
	already := BoardExists(&normed) && normed != DefaultBoard
	meta, err := CreateBoard(c.ctx, normed, BoardCreateOptions{
		Name: nsOptStr(c.ns, "name"), Description: nsOptStr(c.ns, "description"), Icon: nsOptStr(c.ns, "icon"),
		Color: nsOptStr(c.ns, "color"), DefaultWorkdir: nsOptStr(c.ns, "default_workdir"),
	})
	if err != nil {
		return 0, err
	}
	verb := "created"
	if already {
		verb = "already exists"
	}
	slugValue, _ := meta.Get("slug")
	fmt.Fprintf(c.w, "Board %s %s.\n  Display name: %s\n  DB path:      %s\n", pyrepr.Repr(slugValue), verb, metaText(meta, "name"), metaText(meta, "db_path"))
	if nsBool(c.ns, "switch") {
		if _, err := SetCurrentBoard(c.ctx, pyrepr.Str(slugValue)); err != nil {
			return 0, err
		}
		fmt.Fprintf(c.w, "  Switched to %s.\n", pyrepr.Repr(slugValue))
	} else {
		fmt.Fprintf(c.w, "  Use `icebath kanban boards switch %s` to make it current.\n", pyrepr.Str(slugValue))
	}
	return 0, nil
}

// Archives a board, or deletes it with --delete or the delete alias
func (c *kanbanCLI) cmdBoardsRm() (int, error) {
	forceDelete := nsBool(c.ns, "delete") || nsStr(c.ns, "boards_action") == "delete"
	res, err := RemoveBoard(c.ctx, nsStr(c.ns, "slug"), !forceDelete)
	if err != nil {
		var ve *ValueError
		if errors.As(err, &ve) {
			return cliErr(c.errw, "kanban boards rm: "+pyrepr.ExceptionMessage(err), 1), nil
		}
		return 0, err
	}
	slugValue, _ := res.Get("slug")
	if action, _ := res.Get("action"); action == "archived" {
		fmt.Fprintf(c.w, "Board %s archived → %s\nRecover by moving the directory back to <root>/kanban/boards/<slug>/ "+
			"and deleting the archived tombstone board.json left in its place.\n", pyrepr.Repr(slugValue), metaText(res, "new_path"))
	} else {
		fmt.Fprintf(c.w, "Board %s deleted.\n", pyrepr.Repr(slugValue))
	}
	return 0, nil
}

// Makes an existing unarchived board current
func (c *kanbanCLI) cmdBoardsSwitch() (int, error) {
	normed, rc := c.boardSlugArg("switch", false)
	if rc != 0 {
		return rc, nil
	}
	if !BoardExists(&normed) {
		return cliErr(c.errw, fmt.Sprintf("kanban boards switch: board %s does not exist. Create it with `icebath kanban boards create %s`.",
			pyrepr.Repr(normed), normed), 1), nil
	}
	meta, err := ReadBoardMetadata(&normed)
	if err != nil {
		return 0, err
	}
	if v, _ := meta.Get("archived"); pyrepr.Truthy(v) {
		return cliErr(c.errw, fmt.Sprintf("kanban boards switch: board %s is archived.", pyrepr.Repr(normed)), 1), nil
	}
	if _, err := SetCurrentBoard(c.ctx, normed); err != nil {
		return 0, err
	}
	fmt.Fprintf(c.w, "Active board is now %s.\n", pyrepr.Repr(normed))
	return 0, nil
}

// Prints the current board with its name, description, database path and task counts
func (c *kanbanCLI) cmdBoardsShow() (int, error) {
	current := GetCurrentBoard(c.ctx)
	meta, err := ReadBoardMetadata(&current)
	if err != nil {
		return 0, err
	}
	counts := c.boardTaskCounts(current)
	fmt.Fprintf(c.w, "Current board: %s\n  Display name: %s\n", current, metaText(meta, "name"))
	if v, _ := meta.Get("description"); pyrepr.Truthy(v) {
		fmt.Fprintf(c.w, "  Description:  %s\n", pyrepr.Str(v))
	}
	suffix := ""
	if counts.Len() > 0 {
		suffix = " (" + fmtCounts(countsMap(counts), "") + ")"
	}
	fmt.Fprintf(c.w, "  DB path:      %s\n  Tasks:        %d total%s\n", metaText(meta, "db_path"), countsTotal(counts), suffix)
	return 0, nil
}

// Changes a board display name
func (c *kanbanCLI) cmdBoardsRename() (int, error) {
	normed, rc := c.boardSlugArg("rename", true)
	if rc != 0 {
		return rc, nil
	}
	meta, err := WriteBoardMetadata(c.ctx, &normed, BoardMetadataUpdate{Name: nsOptStr(c.ns, "name")})
	if err != nil {
		return 0, err
	}
	name, _ := meta.Get("name")
	fmt.Fprintf(c.w, "Board %s renamed to %s.\n", pyrepr.Repr(normed), pyrepr.Repr(name))
	return 0, nil
}

// Sets or clears a board default workspace path
func (c *kanbanCLI) cmdBoardsSetDefaultWorkdir() (int, error) {
	normed, rc := c.boardSlugArg("set-default-workdir", true)
	if rc != 0 {
		return rc, nil
	}
	meta, err := WriteBoardMetadata(c.ctx, &normed, BoardMetadataUpdate{DefaultWorkdir: nsOptStr(c.ns, "path")})
	if err != nil {
		return 0, err
	}
	if v, _ := meta.Get("default_workdir"); pyrepr.Truthy(v) {
		fmt.Fprintf(c.w, "Board %s default workdir set to %s.\n", pyrepr.Repr(normed), pyrepr.Repr(v))
	} else {
		fmt.Fprintf(c.w, "Board %s default workdir cleared.\n", pyrepr.Repr(normed))
	}
	return 0, nil
}

// Whether an error is an OSError or ValueError in Python terms
func isOSOrValueError(err error) bool {
	var ve *ValueError
	var fe *FileNotFoundError
	var ae *pyfs.ArchiveError
	return pypath.IsOSError(err) || errors.As(err, &ve) || errors.As(err, &fe) || errors.As(err, &ae)
}

// Human size of a byte count, B below 1024 then one decimal KB, MB, GB and TB
func formatBytes(n int64) string {
	size := float64(n)
	if size < 1024 {
		return fmt.Sprintf("%d B", int64(size))
	}
	for _, unit := range []string{"KB", "MB", "GB"} {
		size /= 1024.0
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", size/1024.0)
}

// Exports a board to a portable archive
func (c *kanbanCLI) cmdBoardsExport() (int, error) {
	slug := nsStr(c.ns, "slug")
	if slug == "" {
		slug = GetCurrentBoard(c.ctx)
	}
	output := nsStr(c.ns, "output")
	if output == "" {
		output = slug + ".tar.gz"
	}
	res, err := ExportBoard(c.ctx, &slug, output, !nsBool(c.ns, "no_attachments"), nsBool(c.ns, "include_logs"))
	if err != nil {
		if isOSOrValueError(err) {
			return cliErr(c.errw, "kanban boards export: "+excText(err), 1), nil
		}
		return 0, err
	}
	if done, err := jsonOut(c, res, false); done || err != nil {
		return 0, err
	}
	countsValue, _ := res.Get("counts")
	counts := countsValue.(*pyjson.Object)
	get := func(o *pyjson.Object, k string) any { v, _ := o.Get(k); return v }
	size, _ := get(res, "size").(int64)
	fmt.Fprintf(c.w, "Exported board %s → %s\n  Size:        %s\n  Tasks:       %s\n  Comments:    %s\n  Attachments: %s\nImport it with `icebath kanban boards import <archive>`.\n",
		pyrepr.Repr(get(res, "board")), pyrepr.Str(get(res, "archive")), formatBytes(size),
		pyrepr.Str(get(counts, "tasks")), pyrepr.Str(get(counts, "task_comments")), pyrepr.Str(get(counts, "attachment_files")))
	return 0, nil
}

// Imports a board archive as a new board
func (c *kanbanCLI) cmdBoardsImport() (int, error) {
	res, err := ImportBoard(c.ctx, nsStr(c.ns, "archive"), nsOptStr(c.ns, "as_slug"), nsBool(c.ns, "switch"))
	if err != nil {
		if isOSOrValueError(err) {
			return cliErr(c.errw, "kanban boards import: "+excText(err), 1), nil
		}
		return 0, err
	}
	if done, err := jsonOut(c, res, false); done || err != nil {
		return 0, err
	}
	get := func(k string) any { v, _ := res.Get(k); return v }
	fmt.Fprintf(c.w, "Imported board %s (%s).\n", pyrepr.Repr(get("board")), pyrepr.Str(get("name")))
	if get("renamed") == true {
		fmt.Fprintf(c.w, "  Renamed from %s — that slug was taken.\n", pyrepr.Repr(get("requested_board")))
	}
	counts := get("counts").(*pyjson.Object)
	tasks, _ := counts.Get("tasks")
	fmt.Fprintf(c.w, "  Path:  %s\n  Tasks: %s\n", pyrepr.Str(get("path")), pyrepr.Str(tasks))
	for _, w := range get("warnings").([]any) {
		fmt.Fprintf(c.w, "  Note:  %s\n", pyrepr.Str(w))
	}
	if get("activated") == true {
		fmt.Fprintf(c.w, "  Active board is now %s.\n", pyrepr.Repr(get("board")))
	} else {
		fmt.Fprintf(c.w, "  Switch to it with `icebath kanban boards switch %s`.\n", pyrepr.Str(get("board")))
	}
	return 0, nil
}
