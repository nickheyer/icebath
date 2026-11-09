package mcp

import (
	"slices"
	"sync"
)

// Symbols this client carries for each SDK module the hermes import binds
var sdkModules = map[string][]string{
	"mcp":                        {"ClientSession", "StdioServerParameters"},
	"mcp.client.stdio":           {"stdio_client"},
	"mcp.client.streamable_http": {"streamable_http_client"},
	"mcp.client.session":         {"LATEST_HANDSHAKE_VERSION"},
	"mcp.client.sse":             {"sse_client"},
	"mcp.types": {"LATEST_PROTOCOL_VERSION", "METHOD_NOT_FOUND", "CreateMessageResult", "CreateMessageResultWithTools",
		"ErrorData", "SamplingCapability", "SamplingToolsCapability", "TextContent", "ToolUseContent", "ElicitRequestParams",
		"ElicitResult", "ServerNotification", "ToolListChangedNotification", "PromptListChangedNotification",
		"ResourceListChangedNotification"},
}

// ClientSession keyword arguments the session construction of this client honours
var clientSessionParams = []string{"read_stream", "write_stream", "read_timeout_seconds", "sampling_callback",
	"elicitation_callback", "logging_callback", "message_handler", "client_info"}

// SDK surface flags bound once by ensureMCPSDK, the gate a test closes by clearing mcpAvailable
var (
	sdkMu                    sync.Mutex
	sdkImportAttempted       bool
	mcpAvailable             = true
	mcpHTTPAvailable         bool
	mcpNewHTTP               bool
	mcpLegacyHTTP            bool
	sseAvailable             bool
	samplingTypes            bool
	elicitationTypes         bool
	notificationTypes        bool
	messageHandlerSupported  bool
	loggingCallbackSupported bool
)

// Python ImportError a transport raises while the SDK gate is closed
type sdkImportError struct{ msg string }

func (e *sdkImportError) Error() string { return e.msg }

// Reports whether this build carries every named symbol of an SDK module, logging missingMsg when one is absent
func importSDKNames(module string, names []string, missingMsg string) bool {
	have := sdkModules[module]
	for _, n := range names {
		if !slices.Contains(have, n) {
			if missingMsg != "" {
				mcpLogger().Debug(missingMsg)
			}
			return false
		}
	}
	return true
}

// Binds the SDK surface flags once and reports availability, false at once while the gate is closed
func ensureMCPSDK() bool {
	sdkMu.Lock()
	defer sdkMu.Unlock()
	if !mcpAvailable {
		return false
	}
	if sdkImportAttempted {
		return mcpAvailable
	}
	if importSDKNames("mcp", []string{"ClientSession", "StdioServerParameters"}, "") && importSDKNames("mcp.client.stdio", []string{"stdio_client"}, "") {
		mcpNewHTTP = importSDKNames("mcp.client.streamable_http", []string{"streamable_http_client"}, "")
		mcpLegacyHTTP = importSDKNames("mcp.client.streamable_http", []string{"streamablehttp_client"}, "")
		mcpHTTPAvailable = mcpNewHTTP || mcpLegacyHTTP
		importSDKNames("mcp.types", []string{"LATEST_PROTOCOL_VERSION"}, "mcp.types.LATEST_PROTOCOL_VERSION not available -- using fallback protocol version")
		importSDKNames("mcp.client.session", []string{"LATEST_HANDSHAKE_VERSION"}, "")
		sseAvailable = importSDKNames("mcp.client.sse", []string{"sse_client"}, "mcp.client.sse.sse_client not available -- SSE transport disabled")
		samplingTypes = importSDKNames("mcp.types", []string{"CreateMessageResult", "CreateMessageResultWithTools", "ErrorData",
			"SamplingCapability", "SamplingToolsCapability", "TextContent", "ToolUseContent"}, "MCP sampling types not available -- sampling disabled")
		elicitationTypes = importSDKNames("mcp.types", []string{"ElicitRequestParams", "ElicitResult"}, "MCP elicitation types not available -- elicitation disabled")
		notificationTypes = importSDKNames("mcp.types", []string{"ServerNotification", "ToolListChangedNotification",
			"PromptListChangedNotification", "ResourceListChangedNotification"}, "MCP notification types not available -- dynamic tool discovery disabled")
	} else {
		mcpAvailable = false
		mcpLogger().Debug("mcp package not installed -- MCP tool support disabled")
	}
	messageHandlerSupported = clientSessionAcceptsLocked("message_handler")
	if mcpAvailable && !messageHandlerSupported {
		mcpLogger().Debug("MCP SDK does not support message_handler -- dynamic tool discovery disabled")
	}
	loggingCallbackSupported = clientSessionAcceptsLocked("logging_callback")
	sdkImportAttempted = true
	return mcpAvailable
}

// Whether the session construction takes kwarg, false while the SDK gate is closed
func clientSessionAccepts(kwarg string) bool {
	sdkMu.Lock()
	defer sdkMu.Unlock()
	return clientSessionAcceptsLocked(kwarg)
}

func clientSessionAcceptsLocked(kwarg string) bool {
	return mcpAvailable && slices.Contains(clientSessionParams, kwarg)
}

// Whether the HTTP transport family is bound, read after ensureMCPSDK
func httpTransportAvailable() bool {
	sdkMu.Lock()
	defer sdkMu.Unlock()
	return mcpHTTPAvailable
}
