package acputil

import (
	"encoding/json"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// MCP server the client asks the agent to connect to: *StdioMCPServer, *HTTPMCPServer or *SSEMCPServer
type MCPServer interface{ isMCPServer() }

func (*StdioMCPServer) isMCPServer() {}
func (*HTTPMCPServer) isMCPServer()  {}
func (*SSEMCPServer) isMCPServer()   {}

// MCP server the agent starts as a command speaking over its standard input and output
type StdioMCPServer struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Args    []string         `json:"args"`
	Command string           `json:"command"`
	Env     []EnvVariable    `json:"env"`
	Name    string           `json:"name"`
}

// Fills the server from its JSON object, which must carry args, command, env and name
func (s *StdioMCPServer) UnmarshalJSON(data []byte) error {
	type plain StdioMCPServer
	return decodeObject(data, (*plain)(s), "StdioMCPServer", "args", "command", "env", "name")
}

// MCP server reached over streamable HTTP
type HTTPMCPServer struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Headers []HTTPHeader     `json:"headers"`
	Name    string           `json:"name"`
	URL     string           `json:"url"`
}

// JSON object of the server with type "http"
func (s HTTPMCPServer) MarshalJSON() ([]byte, error) {
	type plain HTTPMCPServer
	return withTag(plain(s), "type", "http")
}

// Fills the server from its JSON object, which must carry headers, name and url
func (s *HTTPMCPServer) UnmarshalJSON(data []byte) error {
	type plain HTTPMCPServer
	return decodeObject(data, (*plain)(s), "HTTPMCPServer", "headers", "name", "url")
}

// MCP server reached over server-sent events
type SSEMCPServer struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Headers []HTTPHeader     `json:"headers"`
	Name    string           `json:"name"`
	URL     string           `json:"url"`
}

// JSON object of the server with type "sse"
func (s SSEMCPServer) MarshalJSON() ([]byte, error) {
	type plain SSEMCPServer
	return withTag(plain(s), "type", "sse")
}

// Fills the server from its JSON object, which must carry headers, name and url
func (s *SSEMCPServer) UnmarshalJSON(data []byte) error {
	type plain SSEMCPServer
	return decodeObject(data, (*plain)(s), "SSEMCPServer", "headers", "name", "url")
}

// HTTP header sent to an MCP server
type HTTPHeader struct {
	Meta  *jsonutil.Object `json:"_meta,omitempty"`
	Name  string           `json:"name"`
	Value string           `json:"value"`
}

// Fills the header from its JSON object, which must carry name and value
func (h *HTTPHeader) UnmarshalJSON(data []byte) error {
	type plain HTTPHeader
	return decodeObject(data, (*plain)(h), "HTTPHeader", "name", "value")
}

// Environment variable of a command
type EnvVariable struct {
	Meta  *jsonutil.Object `json:"_meta,omitempty"`
	Name  string           `json:"name"`
	Value string           `json:"value"`
}

// Fills the variable from its JSON object, which must carry name and value
func (v *EnvVariable) UnmarshalJSON(data []byte) error {
	type plain EnvVariable
	return decodeObject(data, (*plain)(v), "EnvVariable", "name", "value")
}

// MCP server of a JSON object: http and sse by their type, a stdio server when the type is absent or stdio
func decodeMCPServer(data []byte) (MCPServer, error) {
	var server MCPServer
	switch kind, _ := tagOf(data, "type"); kind {
	case "", "stdio":
		server = new(StdioMCPServer)
	case "http":
		server = new(HTTPMCPServer)
	case "sse":
		server = new(SSEMCPServer)
	default:
		return nil, &UnknownVariantError{Union: "MCPServer", Key: "type", Value: kind}
	}
	if err := json.Unmarshal(data, server); err != nil {
		return nil, err
	}
	return server, nil
}
