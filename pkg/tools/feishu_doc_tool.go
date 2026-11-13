package tools

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const feishuRawContentURI = "/open-apis/docx/v1/documents/:document_id/raw_content"

var feishuDocReadSchema = map[string]any{
	"name": "feishu_doc_read",
	"description": "Read the full content of a Feishu/Lark document as plain text. " +
		"Useful when you need more context beyond the quoted text in a comment.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"doc_token": map[string]any{
				"type":        "string",
				"description": "The document token (from the document URL or comment context).",
			},
		},
		"required": []any{"doc_token"},
	},
}

// Stripped string argument, empty when absent, the AttributeError a non string value raises on strip
func feishuArgStrip(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, isStr := v.(string)
	if !isStr {
		panic(pyAttributeError(v, "strip"))
	}
	return pystr.Strip(s)
}

// Plain text content of a Feishu document through the raw content API of the comment turn client
func handleFeishuDocRead(ctx context.Context, call *Call) string {
	docToken := feishuArgStrip(pythonCallArgs(call), "doc_token")
	if docToken == "" {
		return toolErrorJSON("doc_token is required", nil)
	}
	client := FeishuClient(ctx)
	if client == nil {
		return toolErrorJSON("Feishu client not available (not in a Feishu comment context)", nil)
	}
	resp := larkRequest(ctx, client, buildLarkRequest("GET", feishuRawContentURI, map[string]string{"document_id": docToken}, nil, nil))
	if !larkCodeOK(resp.code) {
		return toolErrorJSON(fmt.Sprintf("Failed to read document: code=%s msg=%s", pyrepr.Str(resp.code), pyrepr.Str(resp.msg)), nil)
	}
	if body := larkRawBody(resp); body != nil {
		if data, ok := body.GetOr("data", pyjson.NewObject()).(*pyjson.Object); ok {
			return ToolResult(Dict("success", true, "content", data.GetOr("content", "")))
		}
	}
	return toolErrorJSON("No content returned from document API", nil)
}

func init() {
	Register(Def{Name: "feishu_doc_read", Toolset: "feishu_doc", Schema: feishuDocReadSchema, Handler: handleFeishuDocRead,
		RequiresEnv: []string{}, Description: "Read Feishu document content", Emoji: "\U0001f4c4"})
}
