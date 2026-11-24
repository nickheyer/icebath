package acputil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Agent end of a connection: serves the agent's methods and makes the calls an agent makes to its client
type AgentSideConnection struct {
	conn *Connection
}

// Connection serving agent over r and w, its OnConnect hook called before any frame is read
func NewAgentSideConnection(agent any, w io.Writer, r io.Reader, opts ...Option) *AgentSideConnection {
	return newAgentSideConnection(agent, w, r, newConfig(opts))
}

func newAgentSideConnection(agent any, w io.Writer, r io.Reader, cfg config) *AgentSideConnection {
	router := newAgentRouter(agent, cfg.unstable)
	c := &AgentSideConnection{conn: newConnection(router.handle, w, r, cfg)}
	if hook, ok := agent.(Connector); ok {
		hook.OnConnect(c)
	}
	return c
}

// Underlying JSON-RPC connection
func (c *AgentSideConnection) Conn() *Connection { return c.conn }

// Serves the client until its input ends, reading fails or ctx is done
func (c *AgentSideConnection) Listen(ctx context.Context) error { return c.conn.Serve(ctx) }

// Closes the underlying connection without waiting for running handlers, so a handler may call it
func (c *AgentSideConnection) Close() error { return c.conn.Close() }

// Meta to send, nil for an empty one so the member is left out
func metaOrNil(meta *jsonutil.Object) *jsonutil.Object {
	if meta.Len() == 0 {
		return nil
	}
	return meta
}

// Sends a request and decodes its result into a T; with optional set, a result that is not an object gives nil
func call[T any](ctx context.Context, conn *Connection, method string, params any, optional bool) (*T, error) {
	raw, err := conn.request(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if !isObject(raw) {
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("%s answered with %s instead of an object", method, describe(raw))
	}
	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("result of %s: %w", method, err)
	}
	return out, nil
}

// Sends a session/update notification
func (c *AgentSideConnection) SessionUpdate(ctx context.Context, sessionID string, update SessionUpdate, meta *jsonutil.Object) error {
	return c.conn.SendNotification(ctx, MethodSessionUpdate, &SessionNotification{Meta: metaOrNil(meta), SessionID: sessionID, Update: update})
}

// Asks the client to have the user choose one of the options for a tool call
func (c *AgentSideConnection) RequestPermission(ctx context.Context, options []PermissionOption, sessionID string, toolCall *ToolCallUpdate, meta *jsonutil.Object) (*RequestPermissionResponse, error) {
	if toolCall == nil {
		return nil, errors.New("acp permission request has no tool call")
	}
	req := &RequestPermissionRequest{Meta: metaOrNil(meta), Options: options, SessionID: sessionID, ToolCall: *toolCall}
	return call[RequestPermissionResponse](ctx, c.conn, MethodSessionRequestPermission, req, false)
}

// Reads a text file through the client, from line and at most limit lines when they are set
func (c *AgentSideConnection) ReadTextFile(ctx context.Context, path, sessionID string, limit, line *int, meta *jsonutil.Object) (*ReadTextFileResponse, error) {
	req := &ReadTextFileRequest{Meta: metaOrNil(meta), Limit: limit, Line: line, Path: path, SessionID: sessionID}
	return call[ReadTextFileResponse](ctx, c.conn, MethodFSReadTextFile, req, false)
}

// Writes a text file through the client, nil when its result is not an object
func (c *AgentSideConnection) WriteTextFile(ctx context.Context, content, path, sessionID string, meta *jsonutil.Object) (*WriteTextFileResponse, error) {
	req := &WriteTextFileRequest{Meta: metaOrNil(meta), Content: content, Path: path, SessionID: sessionID}
	return call[WriteTextFileResponse](ctx, c.conn, MethodFSWriteTextFile, req, true)
}

// Starts a command in a client terminal
func (c *AgentSideConnection) CreateTerminal(ctx context.Context, req *CreateTerminalRequest) (*CreateTerminalResponse, error) {
	if req == nil {
		return nil, errors.New("acp terminal request has no command")
	}
	sent := *req
	sent.Meta = metaOrNil(sent.Meta)
	return call[CreateTerminalResponse](ctx, c.conn, MethodTerminalCreate, &sent, false)
}

// Output of a client terminal so far
func (c *AgentSideConnection) TerminalOutput(ctx context.Context, sessionID, terminalID string, meta *jsonutil.Object) (*TerminalOutputResponse, error) {
	req := &TerminalOutputRequest{Meta: metaOrNil(meta), SessionID: sessionID, TerminalID: terminalID}
	return call[TerminalOutputResponse](ctx, c.conn, MethodTerminalOutput, req, false)
}

// Releases a client terminal, nil when its result is not an object
func (c *AgentSideConnection) ReleaseTerminal(ctx context.Context, sessionID, terminalID string, meta *jsonutil.Object) (*ReleaseTerminalResponse, error) {
	req := &ReleaseTerminalRequest{Meta: metaOrNil(meta), SessionID: sessionID, TerminalID: terminalID}
	return call[ReleaseTerminalResponse](ctx, c.conn, MethodTerminalRelease, req, true)
}

// Waits for the command of a client terminal to exit
func (c *AgentSideConnection) WaitForTerminalExit(ctx context.Context, sessionID, terminalID string, meta *jsonutil.Object) (*WaitForTerminalExitResponse, error) {
	req := &WaitForTerminalExitRequest{Meta: metaOrNil(meta), SessionID: sessionID, TerminalID: terminalID}
	return call[WaitForTerminalExitResponse](ctx, c.conn, MethodTerminalWaitForExit, req, false)
}

// Kills the command of a client terminal, nil when its result is not an object
func (c *AgentSideConnection) KillTerminal(ctx context.Context, sessionID, terminalID string, meta *jsonutil.Object) (*KillTerminalResponse, error) {
	req := &KillTerminalRequest{Meta: metaOrNil(meta), SessionID: sessionID, TerminalID: terminalID}
	return call[KillTerminalResponse](ctx, c.conn, MethodTerminalKill, req, true)
}

// Sends the extension request "_" + name and waits for its result, decoded with objects as *jsonutil.Object
func (c *AgentSideConnection) ExtMethod(ctx context.Context, name string, params any) (any, error) {
	return c.conn.SendRequest(ctx, "_"+name, params)
}

// Sends the extension notification "_" + name
func (c *AgentSideConnection) ExtNotification(ctx context.Context, name string, params any) error {
	return c.conn.SendNotification(ctx, "_"+name, params)
}

// Serves agent on w and r, standard output and input for nil ones, until the client's input ends, reading fails or ctx is done; then closes the connection, which cancels every handler's context, and waits up to the shutdown grace for running handlers to return: any still running after it are logged, named in the error and left to finish on their own
func RunAgent(ctx context.Context, agent any, w io.Writer, r io.Reader, opts ...Option) error {
	if w == nil {
		w = os.Stdout
	}
	if r == nil {
		r = os.Stdin
	}
	cfg := newConfig(opts)
	conn := newAgentSideConnection(agent, w, r, cfg)
	err := conn.Listen(ctx)
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.grace)
	defer cancel()
	if serr := conn.conn.Shutdown(grace); serr != nil {
		if err == nil {
			return serr
		}
		return errors.Join(err, serr)
	}
	return err
}
