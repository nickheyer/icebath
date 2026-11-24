//go:build !icebath_core

package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway/platforms/feishu/feishuclient"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const feishuRawContentURI = "/open-apis/docx/v1/documents/:document_id/raw_content"

var feishuDocReadSchema = Dict(
	"name", "feishu_doc_read",
	"description", "Read the full content of a Feishu/Lark document as plain text. "+
		"Useful when you need more context beyond the quoted text in a comment.",
	"parameters", Dict(
		"type", "object",
		"properties", Dict(
			"doc_token", Dict(
				"type", "string",
				"description", "The document token (from the document URL or comment context).",
			),
		),
		"required", []any{"doc_token"},
	),
)

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
	return strings.TrimSpace(s)
}

// Plain text content of a Feishu document through the raw content API of the comment turn client
func handleFeishuDocRead(ctx context.Context, call *Call) string {
	docToken := feishuArgStrip(pythonCallArgs(call), "doc_token")
	if docToken == "" {
		return toolErrorJSON("doc_token is required", nil)
	}
	client := feishuclient.From(ctx)
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

// Feishu tools exist once the platform holds its app credentials, the state in which the reference adapter installs lark_oapi
func feishuToolsReady(env *Env) bool {
	return webHasEnv(env, "FEISHU_APP_ID") && webHasEnv(env, "FEISHU_APP_SECRET")
}

func init() {
	Register(Def{Name: "feishu_doc_read", Toolset: "feishu_doc", Schema: feishuDocReadSchema, Handler: handleFeishuDocRead,
		Check: feishuToolsReady, RequiresEnv: []string{}, Description: "Read Feishu document content", Emoji: "\U0001f4c4"})
}
