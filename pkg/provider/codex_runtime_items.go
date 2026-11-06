package provider

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var codexToolItemTypes = map[string]bool{"commandExecution": true, "fileChange": true, "mcpToolCall": true, "dynamicToolCall": true, "webSearch": true}

// Text delta notification methods and the agent stream hook each one feeds
var codexTextDeltaMethods = [][2]string{
	{"item/agentMessage/delta", "stream"},
	{"item/reasoning/delta", "reasoning"},
	{"item/reasoning/summaryDelta", "reasoning"},
	{"item/reasoning/textDelta", "reasoning"},
	{"item/reasoning/summaryTextDelta", "reasoning"},
}

// Notifications that prove a turn is alive, every text delta plus the tool output streams
var codexProgressDeltaMethods = func() map[string]bool {
	out := map[string]bool{"item/commandExecution/outputDelta": true, "item/fileChange/outputDelta": true}
	for _, m := range codexTextDeltaMethods {
		out[m[0]] = true
	}
	return out
}()

var codexProgressItemTypes = func() map[string]bool {
	out := map[string]bool{"agentMessage": true, "reasoning": true}
	for k := range codexToolItemTypes {
		out[k] = true
	}
	return out
}()

var codexStaticToolNames = map[string]string{"commandExecution": "exec_command", "fileChange": "apply_patch", "webSearch": "web_search"}

var codexStableIDPrefixes = map[string]string{"commandExecution": "exec", "fileChange": "apply_patch"}

var codexMCPLikeItemTypes = map[string]bool{"mcpToolCall": true, "dynamicToolCall": true}

var codexPreviewFields = map[string]string{"commandExecution": "command", "webSearch": "query"}

// Non empty text a delta notification carries, else empty
func codexDeltaText(params any) string {
	text, _ := cxOr(cxGet(params, "delta"), cxGet(params, "text")).(string)
	return text
}

func codexItemChanges(item any) []any {
	var out []any
	for _, c := range cxList(cxOr(cxGet(item, "changes"), []any{})) {
		if cxIsObject(c) {
			out = append(out, c)
		}
	}
	return out
}

// Synthetic tool name of a codex item, the icebath tools server prefix stripped
func codexItemToToolName(item any) string {
	itemType := cxStr(item, "type", "")
	switch itemType {
	case "mcpToolCall":
		server, tool := cxStr(item, "server", "mcp"), cxStr(item, "tool", "unknown")
		if server == CodexToolsMCPServerName {
			return tool
		}
		return "mcp." + server + "." + tool
	case "dynamicToolCall":
		return cxStr(item, "tool", "dynamic")
	}
	if name := codexStaticToolNames[itemType]; name != "" {
		return name
	}
	if itemType != "" {
		return itemType
	}
	return "unknown"
}

// Arguments the tool started callback receives, the projector shapes
func codexItemToArgs(item any) any {
	itemType := cxStr(item, "type", "")
	switch {
	case itemType == "commandExecution":
		return map[string]any{"command": cxStr(item, "command", ""), "cwd": cxStr(item, "cwd", "")}
	case itemType == "fileChange":
		changes := []any{}
		for _, c := range codexItemChanges(item) {
			changes = append(changes, map[string]any{"kind": cxStr(cxOr(cxGet(c, "kind"), map[string]any{}), "type", "update"), "path": cxStr(c, "path", "")})
		}
		return map[string]any{"changes": changes}
	case codexMCPLikeItemTypes[itemType]:
		return codexDictArgs(cxGet(item, "arguments"))
	case itemType == "webSearch":
		return map[string]any{"query": cxStr(item, "query", "")}
	}
	return map[string]any{}
}

// Short preview for the tool started bubble, nil when nothing useful
func codexItemToPreview(item any) any {
	itemType := cxStr(item, "type", "")
	if field, ok := codexPreviewFields[itemType]; ok {
		if s := pystr.Slice(cxStr(item, field, ""), 0, 120); s != "" {
			return s
		}
		return nil
	}
	if itemType == "fileChange" {
		var paths []string
		for _, c := range codexItemChanges(item) {
			if p := cxGet(c, "path"); cxTruthy(p) {
				paths = append(paths, cxStr(c, "path", ""))
			}
		}
		if len(paths) == 0 {
			return nil
		}
		text := strings.Join(paths[:min(3, len(paths))], ", ")
		if len(paths) > 3 {
			text += fmt.Sprintf(", +%d more", len(paths)-3)
		}
		return text
	}
	if codexMCPLikeItemTypes[itemType] {
		args := cxOr(cxGet(item, "arguments"), map[string]any{})
		if cxIsObject(args) && cxTruthy(args) {
			if text, err := cxDumps(args); err == nil {
				return pystr.Slice(text, 0, 120)
			}
		}
	}
	return nil
}

// Display text and error flag of a completed tool item for live tool cards
func codexItemCompletionPayload(item any) (string, bool) {
	switch cxStr(item, "type", "") {
	case "commandExecution":
		out, exitCode := cxStr(item, "aggregatedOutput", ""), cxGet(item, "exitCode")
		isError := exitCode != nil && !codexIsZero(exitCode)
		if isError {
			return fmt.Sprintf("[exit %v]\n%s", exitCode, out), true
		}
		return out, false
	case "fileChange":
		status := cxStr(item, "status", "unknown")
		n := len(cxList(cxOr(cxGet(item, "changes"), []any{})))
		return fmt.Sprintf("apply_patch status=%s, %d change(s)", status, n), !(status == "completed" || status == "applied" || status == "success")
	case "mcpToolCall":
		if errVal := cxGet(item, "error"); cxTruthy(errVal) {
			return "[error] " + cxDumpsCut(errVal, 1000), true
		}
		if result := cxGet(item, "result"); result != nil {
			return cxDumpsCut(result, 4000), false
		}
		return "", false
	case "dynamicToolCall":
		contentItems := cxOr(cxGet(item, "contentItems"), []any{})
		success := any(true)
		if cxHas(item, "success") {
			success = cxGet(item, "success")
		}
		if list, ok := contentItems.([]any); ok && len(list) > 0 {
			return cxDumpsCut(list, 4000), !cxTruthy(success)
		}
		return "success=" + pyrepr.Str(success), !cxTruthy(success)
	}
	return "", false
}

// Deterministic tool call id the projector gives the same item, so live cards match stored history
func codexStableCallID(item any, name string) string {
	itemType := cxStr(item, "type", "")
	tool := cxStr(item, "tool", "unknown")
	prefix := ""
	switch itemType {
	case "mcpToolCall":
		prefix = "mcp__" + cxStr(item, "server", "mcp") + "__" + tool
	case "dynamicToolCall":
		prefix = "dyn_" + tool
	default:
		prefix = name
		if p, ok := codexStableIDPrefixes[itemType]; ok {
			prefix = p
		}
	}
	return codexDeterministicCallID(prefix, cxStr(item, "id", ""))
}

// Python equality with zero for a decoded JSON number or bool
func codexIsZero(v any) bool {
	if b, ok := v.(bool); ok {
		return !b
	}
	if n, ok := pyrepr.Int(v); ok {
		return n.Sign() == 0
	}
	switch f := v.(type) {
	case float64:
		return f == 0
	case float32:
		return f == 0
	}
	return false
}

// Unwritten functions of the codex runtime, switch and plugin migration modules, each named for its Python function
var _ = []any{
	codexPruneZeroEventRetryPayload,
	codexModelProvider,
	codexWireModel,
	codexTurnEffort,
	codexTurnServiceTier,
	ensureCodexSession,
	persistProjectedMessages,
	transformCodexReply,
	RunCodexAppServerTurn,
	CodexRuntimeParseArgs,
	GetCurrentCodexRuntime,
	GetConfiguredCodexBinary,
	SetCodexRuntime,
	CheckCodexBinaryOK,
	codexRuntimeMigrationLines,
	ApplyCodexRuntime,
	codexMigrationStrMap,
	codexTranslateOneServer,
	codexEscapeTOMLString,
	codexFormatTOMLValue,
	codexQuoteTOMLKey,
	RenderCodexTOMLSection,
	codexInsertManagedBlockAtTopLevel,
	codexStripUnmanagedPluginTables,
	codexUnmanagedMCPServerNames,
	codexLooksLikeTableHeader,
	codexStripExistingManagedBlock,
	codexQueryCodexPlugins,
	codexLooksLikeTestTempdir,
	codexBuildToolsMCPEntry,
	codexWriteAtomic,
	MigrateCodexRuntimePlugins,
}
