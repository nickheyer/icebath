package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
)

// Tool a server offers its clients
type ServerTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Text of the result; an error comes back to the client as a tool error
	Handler func(ctx context.Context, arguments map[string]any) (string, error)
}

// MCP server icebath serves: its tools, answered over a stream of newline framed JSON-RPC
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []ServerTool
}

// Serves the client on the other end of in and out until in ends or ctx is done
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	write := func(msg *Message) {
		data, err := json.Marshal(msg)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		out.Write(append(data, '\n'))
	}
	var calls sync.WaitGroup
	defer calls.Wait()
	inflight := map[string]context.CancelFunc{}
	var mu sync.Mutex
	reader := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := longLine(reader)
		if len(bytes.TrimSpace(line)) > 0 {
			msgs, derr := DecodeMessages(line)
			if derr != nil {
				write(NewErrorResponse(json.RawMessage("null"), CodeParseError, "Parse error"))
			}
			for _, msg := range msgs {
				switch {
				case msg.IsNotification() && msg.Method == MethodCancelled:
					var p struct {
						ID json.RawMessage `json:"requestId"`
					}
					if json.Unmarshal(msg.Params, &p) == nil {
						mu.Lock()
						if cancel := inflight[IDKey(p.ID)]; cancel != nil {
							cancel()
						}
						mu.Unlock()
					}
				case msg.IsRequest():
					callCtx, cancelCall := context.WithCancel(ctx)
					key := IDKey(msg.ID)
					mu.Lock()
					inflight[key] = cancelCall
					mu.Unlock()
					calls.Add(1)
					go func() {
						defer calls.Done()
						defer func() {
							mu.Lock()
							delete(inflight, key)
							mu.Unlock()
							cancelCall()
						}()
						if reply := s.answer(callCtx, msg); reply != nil && callCtx.Err() == nil {
							write(reply)
						}
					}()
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (s *Server) answer(ctx context.Context, msg *Message) *Message {
	switch msg.Method {
	case MethodInitialize:
		var params InitializeParams
		json.Unmarshal(msg.Params, &params)
		version := HandshakeVersion
		if handshakeVersion(params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return response(msg.ID, InitializeResult{ProtocolVersion: version, Capabilities: &ServerCapabilities{Tools: map[string]any{"listChanged": false}},
			ServerInfo: Implementation{Name: s.Name, Version: s.Version}, Instructions: s.Instructions})
	case MethodPing:
		return response(msg.ID, map[string]any{})
	case MethodToolsList:
		tools := make([]map[string]any, len(s.Tools))
		for i, tool := range s.Tools {
			tools[i] = map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema}
		}
		return response(msg.ID, map[string]any{"tools": tools})
	case MethodToolsCall:
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(msg.Params, &params) != nil {
			return NewErrorResponse(msg.ID, CodeInvalidParams, "Invalid request parameters")
		}
		at := slices.IndexFunc(s.Tools, func(tool ServerTool) bool { return tool.Name == params.Name })
		if at < 0 {
			return NewErrorResponse(msg.ID, CodeInvalidParams, "Unknown tool: "+params.Name)
		}
		if params.Arguments == nil {
			params.Arguments = map[string]any{}
		}
		text, err := s.Tools[at].Handler(ctx, params.Arguments)
		result := CallToolResult{Content: []*ContentBlock{{Type: "text", Text: text}}}
		if err != nil {
			result = CallToolResult{Content: []*ContentBlock{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		return response(msg.ID, result)
	}
	return NewErrorResponse(msg.ID, CodeMethodNotFound, "Method not found")
}

func response(id json.RawMessage, result any) *Message {
	reply, err := NewResponse(id, result)
	if err != nil {
		slog.Error("MCP server answer could not be encoded", "error", err)
		return NewErrorResponse(id, CodeInternalError, err.Error())
	}
	return reply
}
