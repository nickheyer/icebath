package tools

import (
	"context"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Handler that checks the client and the stripped required keys, then sends the request and wraps its data
func feishuCommentOp(keys []string, missingMsg, label, method, uri string, queries func(map[string]any) []larkQuery,
	body func(map[string]any, []string) any, flagSuccess bool) Handler {
	return func(ctx context.Context, call *Call) string {
		client := FeishuClient(ctx)
		args := pythonCallArgs(call)
		values := make([]string, len(keys))
		for i, k := range keys {
			values[i] = feishuArgStrip(args, k)
		}
		if client == nil {
			return toolErrorJSON("Feishu client not available", nil)
		}
		if slices.Contains(values, "") {
			return toolErrorJSON(missingMsg, nil)
		}
		paths := map[string]string{}
		for i, k := range keys {
			if k != "content" {
				paths[k] = values[i]
			}
		}
		var q []larkQuery
		if queries != nil {
			q = queries(args)
		}
		var b any
		if body != nil {
			b = body(args, values)
		}
		code, msg, data := larkCall(ctx, client, method, uri, paths, q, b)
		if !larkCodeOK(code) {
			return toolErrorJSON(fmt.Sprintf("%s failed: code=%s msg=%s", label, pyrepr.Str(code), pyrepr.Str(msg)), nil)
		}
		if flagSuccess {
			return ToolResult(Dict("success", true, "data", data))
		}
		return ToolResult(data)
	}
}

// File type argument, docx when absent or falsy
func feishuFileType(args map[string]any) any {
	if v, ok := args["file_type"]; ok && pyrepr.Truthy(v) {
		return v
	}
	return "docx"
}

// Query pairs of the listing endpoints with the page token last after any extra pairs
func feishuPagedQueries(args map[string]any, extra ...larkQuery) []larkQuery {
	var pageSize any = int64(100)
	if v, ok := args["page_size"]; ok {
		pageSize = v
	}
	queries := []larkQuery{{"file_type", feishuFileType(args)}, {"user_id_type", "open_id"}, {"page_size", pyrepr.Str(pageSize)}}
	queries = append(queries, extra...)
	var pageToken any = ""
	if v, ok := args["page_token"]; ok {
		pageToken = v
	}
	if pyrepr.Truthy(pageToken) {
		queries = append(queries, larkQuery{"page_token", pageToken})
	}
	return queries
}

const (
	feishuCommentsURI   = "/open-apis/drive/v1/files/:file_token/comments"
	feishuRepliesURI    = "/open-apis/drive/v1/files/:file_token/comments/:comment_id/replies"
	feishuAddCommentURI = "/open-apis/drive/v1/files/:file_token/new_comments"
)

func feishuFileTokenProp() map[string]any {
	return map[string]any{"type": "string", "description": "The document file token."}
}

func feishuFileTypeProp() map[string]any {
	return map[string]any{"type": "string", "description": "File type (default: docx).", "default": "docx"}
}

func feishuPageTokenProp() map[string]any {
	return map[string]any{"type": "string", "description": "Pagination token for next page."}
}

func feishuDriveSchema(name, description string, properties map[string]any, required ...any) map[string]any {
	return map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": required}}
}

var feishuDriveListCommentsSchema = feishuDriveSchema("feishu_drive_list_comments",
	"List comments on a Feishu document. "+
		"Use is_whole=true to list whole-document comments only.",
	map[string]any{
		"file_token": feishuFileTokenProp(),
		"file_type":  feishuFileTypeProp(),
		"is_whole": map[string]any{
			"type":        "boolean",
			"description": "If true, only return whole-document comments.",
			"default":     false,
		},
		"page_size": map[string]any{
			"type":        "integer",
			"description": "Number of comments per page (max 100).",
			"default":     100,
		},
		"page_token": feishuPageTokenProp(),
	}, "file_token")

var feishuDriveListRepliesSchema = feishuDriveSchema("feishu_drive_list_comment_replies",
	"List all replies in a comment thread on a Feishu document.",
	map[string]any{
		"file_token": feishuFileTokenProp(),
		"comment_id": map[string]any{
			"type":        "string",
			"description": "The comment ID to list replies for.",
		},
		"file_type": feishuFileTypeProp(),
		"page_size": map[string]any{
			"type":        "integer",
			"description": "Number of replies per page (max 100).",
			"default":     100,
		},
		"page_token": feishuPageTokenProp(),
	}, "file_token", "comment_id")

var feishuDriveReplySchema = feishuDriveSchema("feishu_drive_reply_comment",
	"Reply to a local comment thread on a Feishu document. "+
		"Use this for local (quoted-text) comments. "+
		"For whole-document comments, use feishu_drive_add_comment instead.",
	map[string]any{
		"file_token": feishuFileTokenProp(),
		"comment_id": map[string]any{
			"type":        "string",
			"description": "The comment ID to reply to.",
		},
		"content": map[string]any{
			"type":        "string",
			"description": "The reply text content (plain text only, no markdown).",
		},
		"file_type": feishuFileTypeProp(),
	}, "file_token", "comment_id", "content")

var feishuDriveAddCommentSchema = feishuDriveSchema("feishu_drive_add_comment",
	"Add a new whole-document comment on a Feishu document. "+
		"Use this for whole-document comments or as a fallback when "+
		"reply_comment fails with code 1069302.",
	map[string]any{
		"file_token": feishuFileTokenProp(),
		"content": map[string]any{
			"type":        "string",
			"description": "The comment text content (plain text only, no markdown).",
		},
		"file_type": feishuFileTypeProp(),
	}, "file_token", "content")

var handleFeishuListComments = feishuCommentOp([]string{"file_token"}, "file_token is required", "List comments", "GET", feishuCommentsURI,
	func(a map[string]any) []larkQuery {
		if pyrepr.Truthy(a["is_whole"]) {
			return feishuPagedQueries(a, larkQuery{"is_whole", "true"})
		}
		return feishuPagedQueries(a)
	}, nil, false)

var handleFeishuListReplies = feishuCommentOp([]string{"file_token", "comment_id"}, "file_token and comment_id are required", "List replies", "GET",
	feishuRepliesURI, func(a map[string]any) []larkQuery { return feishuPagedQueries(a) }, nil, false)

// Replies take the rich content elements text_run body with file_type as a query param
var handleFeishuReplyComment = feishuCommentOp([]string{"file_token", "comment_id", "content"}, "file_token, comment_id, and content are required",
	"Reply comment", "POST", feishuRepliesURI, func(a map[string]any) []larkQuery { return []larkQuery{{"file_type", feishuFileType(a)}} },
	func(a map[string]any, v []string) any {
		return map[string]any{"content": map[string]any{"elements": []any{map[string]any{"type": "text_run", "text_run": map[string]any{"text": v[len(v)-1]}}}}}
	}, true)

// New comments take the flat reply_elements text body with file_type in the body
var handleFeishuAddComment = feishuCommentOp([]string{"file_token", "content"}, "file_token and content are required", "Add comment", "POST",
	feishuAddCommentURI, nil,
	func(a map[string]any, v []string) any {
		return map[string]any{"file_type": feishuFileType(a), "reply_elements": []any{map[string]any{"type": "text", "text": v[len(v)-1]}}}
	}, true)

func init() {
	for _, t := range []struct {
		schema      map[string]any
		handler     Handler
		desc, emoji string
	}{
		{feishuDriveListCommentsSchema, handleFeishuListComments, "List document comments", "\U0001f4ac"},
		{feishuDriveListRepliesSchema, handleFeishuListReplies, "List comment replies", "\U0001f4ac"},
		{feishuDriveReplySchema, handleFeishuReplyComment, "Reply to a document comment", "✉️"},
		{feishuDriveAddCommentSchema, handleFeishuAddComment, "Add a whole-document comment", "✉️"},
	} {
		Register(Def{Name: t.schema["name"].(string), Toolset: "feishu_drive", Schema: t.schema, Handler: t.handler,
			RequiresEnv: []string{}, Description: t.desc, Emoji: t.emoji})
	}
}
