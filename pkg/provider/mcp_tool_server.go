package provider

import (
	"io"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Worker threads anyio lends sync tool handlers, the cap on calls a concurrent server runs at once
const mcpToolServerWorkers = 40

// One keyword parameter of a tool signature, Default applying when HasDefault is set and None otherwise
type MCPToolParam struct {
	Name       string
	Type       string
	Required   bool
	Default    any
	HasDefault bool
}

// Stdio MCP server the codex tools bridge and icebath mcp-serve share
type MCPToolServer = CodexToolsServer

// Server with no tools yet, a concurrent one running every tool call on its own worker
func NewMCPToolServer(name, instructions string, in io.Reader, out io.Writer, concurrent bool) *MCPToolServer {
	return &CodexToolsServer{name: name, instructions: instructions, in: in, out: out, concurrent: concurrent,
		workers: make(chan struct{}, mcpToolServerWorkers)}
}

// Keyword parameters of a JSON schema, optional ones defaulting to None
func MCPToolParamsFromSchema(schema any) []MCPToolParam {
	sig, _ := codexToolsSignatureFromSchema(schema)
	out := make([]MCPToolParam, 0, len(sig.Params))
	for _, p := range sig.Params {
		out = append(out, MCPToolParam{Name: p.Name, Type: p.PyType, Required: p.Required})
	}
	return out
}

// Registers a tool after the existing ones, a nil inputSchema listing the schema its params derive
func (s *CodexToolsServer) AddTool(name, description string, params []MCPToolParam, inputSchema any, handler func(kwargs *jsonutil.Object) string) {
	sig := codexToolsSignature{ReturnAnnotation: "str"}
	for _, p := range params {
		sig.Params = append(sig.Params, codexToolsParam{Name: p.Name, PyType: p.Type, Required: p.Required, Default: p.Default, HasDefault: p.HasDefault})
	}
	s.tools = append(s.tools, &codexToolsTool{name: name, description: description, sig: sig, inputSchema: inputSchema, handler: handler})
}
