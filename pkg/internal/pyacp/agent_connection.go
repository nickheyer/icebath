package pyacp

import (
	"context"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Default per line limit of the stdio reader, 50 MB for multimodal frames
const DefaultStdioBufferLimitBytes = 50 * 1024 * 1024

// Agent side of an ACP connection, the client facing calls an agent makes
type AgentSideConnection struct {
	conn *Connection
}

// Connection serving agent over the given streams, calling its connect hook when present
func NewAgentSideConnection(agent any, writer io.Writer, reader io.Reader, listening, useUnstableProtocol bool, limit int) *AgentSideConnection {
	router := BuildAgentRouter(agent, useUnstableProtocol)
	c := &AgentSideConnection{}
	c.conn = NewConnection(router.Call, writer, reader, listening, limit)
	if hook, ok := agent.(Connector); ok {
		hook.OnConnect(c)
	}
	return c
}

// Underlying JSON-RPC connection
func (c *AgentSideConnection) Conn() *Connection { return c.conn }

// Receives frames until the client closes its side
func (c *AgentSideConnection) Listen() error { return c.conn.MainLoop() }

func metaOrNil(meta *pyjson.Object) *pyjson.Object {
	if meta == nil || meta.Len() == 0 {
		return nil
	}
	return meta
}

// Sends a session/update notification
func (c *AgentSideConnection) SessionUpdate(ctx context.Context, sessionID string, update SessionUpdate, meta *pyjson.Object) error {
	n := &SessionNotification{SessionID: sessionID, Update: update, Meta: metaOrNil(meta)}
	return c.conn.SendNotification(ctx, ClientMethods["session_update"], Dump(n, SerializeParams))
}

func requestModel[T any](ctx context.Context, c *Connection, method string, params any) (*T, error) {
	resp, err := c.SendRequest(ctx, method, Dump(params, SerializeParams))
	if err != nil {
		return nil, err
	}
	return Validate[T](resp)
}

func requestOptionalModel[T any](ctx context.Context, c *Connection, method string, params any) (*T, error) {
	resp, err := c.SendRequest(ctx, method, Dump(params, SerializeParams))
	if err != nil {
		return nil, err
	}
	if _, ok := resp.(*pyjson.Object); !ok {
		return nil, nil
	}
	return Validate[T](resp)
}

// Asks the client to pick one of the permission options for a tool call
func (c *AgentSideConnection) RequestPermission(ctx context.Context, options []PermissionOption, sessionID string, toolCall *ToolCallUpdate, meta *pyjson.Object) (*RequestPermissionResponse, error) {
	req := &RequestPermissionRequest{Options: options, SessionID: sessionID, ToolCall: *toolCall, Meta: metaOrNil(meta)}
	return requestModel[RequestPermissionResponse](ctx, c.conn, ClientMethods["session_request_permission"], req)
}

// Reads a text file through the client
func (c *AgentSideConnection) ReadTextFile(ctx context.Context, path, sessionID string, limit, line *int, meta *pyjson.Object) (*ReadTextFileResponse, error) {
	req := &ReadTextFileRequest{Path: path, SessionID: sessionID, Limit: limit, Line: line, Meta: metaOrNil(meta)}
	return requestModel[ReadTextFileResponse](ctx, c.conn, ClientMethods["fs_read_text_file"], req)
}

// Writes a text file through the client
func (c *AgentSideConnection) WriteTextFile(ctx context.Context, content, path, sessionID string, meta *pyjson.Object) (*WriteTextFileResponse, error) {
	req := &WriteTextFileRequest{Content: content, Path: path, SessionID: sessionID, Meta: metaOrNil(meta)}
	return requestOptionalModel[WriteTextFileResponse](ctx, c.conn, ClientMethods["fs_write_text_file"], req)
}

// Creates a client terminal running command
func (c *AgentSideConnection) CreateTerminal(ctx context.Context, req *CreateTerminalRequest) (*CreateTerminalResponse, error) {
	cp := *req
	cp.Meta = metaOrNil(cp.Meta)
	return requestModel[CreateTerminalResponse](ctx, c.conn, ClientMethods["terminal_create"], &cp)
}

// Current output of a client terminal
func (c *AgentSideConnection) TerminalOutput(ctx context.Context, sessionID, terminalID string, meta *pyjson.Object) (*TerminalOutputResponse, error) {
	req := &TerminalOutputRequest{SessionID: sessionID, TerminalID: terminalID, Meta: metaOrNil(meta)}
	return requestModel[TerminalOutputResponse](ctx, c.conn, ClientMethods["terminal_output"], req)
}

// Releases a client terminal
func (c *AgentSideConnection) ReleaseTerminal(ctx context.Context, sessionID, terminalID string, meta *pyjson.Object) (*ReleaseTerminalResponse, error) {
	req := &ReleaseTerminalRequest{SessionID: sessionID, TerminalID: terminalID, Meta: metaOrNil(meta)}
	return requestOptionalModel[ReleaseTerminalResponse](ctx, c.conn, ClientMethods["terminal_release"], req)
}

// Waits for a client terminal to exit
func (c *AgentSideConnection) WaitForTerminalExit(ctx context.Context, sessionID, terminalID string, meta *pyjson.Object) (*WaitForTerminalExitResponse, error) {
	req := &WaitForTerminalExitRequest{SessionID: sessionID, TerminalID: terminalID, Meta: metaOrNil(meta)}
	return requestModel[WaitForTerminalExitResponse](ctx, c.conn, ClientMethods["terminal_wait_for_exit"], req)
}

// Kills a client terminal
func (c *AgentSideConnection) KillTerminal(ctx context.Context, sessionID, terminalID string, meta *pyjson.Object) (*KillTerminalResponse, error) {
	req := &KillTerminalRequest{SessionID: sessionID, TerminalID: terminalID, Meta: metaOrNil(meta)}
	return requestOptionalModel[KillTerminalResponse](ctx, c.conn, ClientMethods["terminal_kill"], req)
}

// Sends an underscore prefixed extension request
func (c *AgentSideConnection) ExtMethod(ctx context.Context, method string, params *pyjson.Object) (any, error) {
	return c.conn.SendRequest(ctx, "_"+method, params)
}

// Sends an underscore prefixed extension notification
func (c *AgentSideConnection) ExtNotification(ctx context.Context, method string, params *pyjson.Object) error {
	return c.conn.SendNotification(ctx, "_"+method, params)
}

// Closes the underlying connection
func (c *AgentSideConnection) Close() error { return c.conn.Close() }

// Serves agent over the streams, stdin and stdout when both are nil, until the client disconnects
func RunAgent(agent any, input io.Writer, output io.Reader, useUnstableProtocol bool, limit int) error {
	if input == nil && output == nil {
		output, input = StdioStreams()
	}
	conn := NewAgentSideConnection(agent, input, output, false, useUnstableProtocol, limit)
	return conn.Listen()
}

// Reader of stdin and writer of stdout for a stdio connection
func StdioStreams() (io.Reader, io.Writer) {
	return os.Stdin, os.Stdout
}
