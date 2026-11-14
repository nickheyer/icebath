package file

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/tool"
)

type searchRequestArgs struct {
	pattern    string
	target     string
	path       string
	fileGlob   string
	limit      int
	offset     int
	outputMode string
	context    int
	order      string
}

func (t *Tools) resolvedMatchPath(ctx context.Context, path, taskID string) string {
	return t.resolvedOrRaw(ctx, path, taskID)
}

func (t *Tools) filterReadBlocked(ctx context.Context, result *SearchResult, taskID string) (int, error) {
	omitted := 0
	allowed := func(path string) (bool, error) {
		blocked, err := t.safety.ReadBlockError(ctx, t.resolvedMatchPath(ctx, path, taskID))
		if err != nil {
			return false, err
		}
		if blocked != "" {
			omitted++
			return false, nil
		}
		return true, nil
	}
	if len(result.Matches) > 0 {
		kept := make([]SearchMatch, 0, len(result.Matches))
		for _, m := range result.Matches {
			ok, err := allowed(m.Path)
			if err != nil {
				return 0, err
			}
			if ok {
				kept = append(kept, m)
			}
		}
		result.Matches = kept
	}
	if len(result.Files) > 0 {
		kept := make([]string, 0, len(result.Files))
		for _, f := range result.Files {
			ok, err := allowed(f)
			if err != nil {
				return 0, err
			}
			if ok {
				kept = append(kept, f)
			}
		}
		result.Files = kept
	}
	if len(result.Counts) > 0 {
		kept := make([]PathCount, 0, len(result.Counts))
		for _, count := range result.Counts {
			ok, err := allowed(count.Path)
			if err != nil {
				return 0, err
			}
			if ok {
				kept = append(kept, count)
			}
		}
		result.Counts = kept
	}
	return omitted, nil
}

func (t *Tools) searchFiles(ctx context.Context, c *tool.Call, req searchRequestArgs) (tool.Result, error) {
	taskID := taskOf(c)
	offset, limit := SearchPage(req.offset, req.limit)

	loopKey := strings.Join([]string{"search", req.pattern, req.target, req.path, req.fileGlob,
		strconv.Itoa(limit), strconv.Itoa(offset), req.order}, keySeparator)
	count := 0
	t.tasks.tracker.Update(taskID, func(reads *TaskReads) {
		count = reads.Bump(loopKey)
	})
	if count >= 4 {
		return tool.Failure(
			"BLOCKED: You have run this exact search "+strconv.Itoa(count)+" times in a row. "+
				"The results have NOT changed. You already have this information. "+
				"STOP re-searching and proceed with your task.",
			tool.Field{Key: "pattern", Value: validText(req.pattern)},
			tool.Field{Key: "already_searched", Value: count},
			tool.Field{Key: tool.GuardrailRefusalKey, Value: true}), nil
	}

	if msg := NTNamespaceError(req.path, "Search"); msg != "" {
		return tool.Failure(msg), nil
	}
	resolvedRoot := t.resolvedOrRaw(ctx, req.path, taskID)
	blocked, err := t.safety.ReadBlockError(ctx, resolvedRoot)
	if err != nil {
		return tool.Result{}, err
	}
	if blocked != "" {
		return tool.Failure(validText(blocked)), nil
	}
	if cached, ok := t.tasks.tracker.CheckNotFound("search", resolvedRoot, taskID); ok {
		return tool.Result{Text: cached}, nil
	}

	ops, err := t.ops(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	result, err := ops.Search(ctx, SearchQuery{
		Pattern: req.pattern, Path: req.path, Target: req.target, FileGlob: req.fileGlob,
		Limit: limit, Offset: offset, OutputMode: req.outputMode, Context: req.context, Order: req.order,
	})
	if err != nil {
		return tool.Result{}, err
	}
	omitted, err := t.filterReadBlocked(ctx, &result, taskID)
	if err != nil {
		return tool.Result{}, err
	}
	for i := range result.Matches {
		if m := &result.Matches[i]; m.Content != "" {
			m.Content = t.redactRead(ctx, m.Content, t.resolvedMatchPath(ctx, m.Path, taskID))
		}
	}
	fields := fieldList(result.Fields(true))
	if omitted > 0 {
		fields.set("_omitted", strconv.Itoa(omitted)+" result(s) omitted because they target credential, "+
			"token, cache, or secret-bearing environment files.")
	}
	if strings.HasPrefix(result.Error, "Path not found:") {
		t.tasks.tracker.RecordNotFound("search", resolvedRoot, taskID, encode(fields).Text)
	}
	if count >= 3 {
		fields.set("_warning", "You have run this exact search "+strconv.Itoa(count)+" times consecutively. "+
			"The results have not changed. Use the information you already have.")
	}
	if result.Truncated {
		fields.set("_hint", "Results truncated. Use offset="+strconv.Itoa(offset+limit)+" to see more, "+
			"or narrow with a more specific pattern or file_glob.")
	}
	return encode(fields), nil
}
