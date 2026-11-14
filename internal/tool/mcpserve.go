package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const MCPServerName = "icebath-tools"

const mcpInstructions = "icebath's tool surface, exposed for use inside a Codex session. Use these for capabilities Codex's built-in toolset doesn't cover: web search/extract, subagent delegation, vision, image generation, persistent memory, skills, and cross-session search."

func MCPExposedTools() []string {
	return []string{
		"web_search", "web_extract",
		"vision_analyze", "image_generate", "skill_view", "skills_list", "text_to_speech",
		"kanban_complete", "kanban_block", "kanban_schedule", "kanban_request_review",
		"kanban_request_changes", "kanban_comment",
		"kanban_heartbeat", "kanban_show", "kanban_list",
		"kanban_create", "kanban_unblock", "kanban_link",
	}
}

func mcpProtocolVersions() []string {
	return []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
}

type mcpTool struct {
	name        string
	description string
	schema      json.RawMessage
	props       map[string]bool
	required    []string
}

type mcpServer struct {
	d        *Dispatcher
	template Call
	tools    []mcpTool
	out      io.Writer
	writeMu  sync.Mutex
	callsMu  sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func exposedSchema(params json.RawMessage) (json.RawMessage, map[string]bool, []string) {
	n, err := parseNode(params)
	if err != nil || !n.isObj() {
		n = newObj().with("type", newStr("object")).with("properties", newObj())
	}
	props := map[string]bool{}
	if p := n.get("properties"); p.isObj() {
		kept := newObj()
		for i, k := range p.keys {
			if strings.HasPrefix(k, "_") {
				continue
			}
			kept.set(k, p.vals[i])
			props[k] = true
		}
		n.set("properties", kept)
	}
	var required []string
	if req := n.get("required"); req.isArr() {
		kept := newArr()
		for _, r := range req.arr {
			if s, ok := r.text(); ok && props[s] {
				kept.arr = append(kept.arr, r)
				required = append(required, s)
			}
		}
		n.set("required", kept)
	}
	return n.compact(), props, required
}

func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, d *Dispatcher, template Call, names []string) error {
	defs, err := d.r.Definitions(ctx, Selection{Scope: template.Scope}, DefinitionOptions{})
	if err != nil {
		return err
	}
	byName := map[string]wire.Tool{}
	for _, t := range defs.Tools {
		byName[t.Name] = t
	}
	s := &mcpServer{d: d, template: template, out: out, inflight: map[string]context.CancelFunc{}}
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			slog.Debug(fmt.Sprintf("skipping %s — not registered in this icebath process", name))
			continue
		}
		description := t.Description
		if description == "" {
			description = "icebath " + name + " tool"
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		schema, props, required := exposedSchema(params)
		s.tools = append(s.tools, mcpTool{name: name, description: description, schema: schema, props: props, required: required})
	}
	slog.Info(fmt.Sprintf("%s MCP server registered %d/%d tools", MCPServerName, len(s.tools), len(names)))
	return s.run(ctx, in)
}

func (s *mcpServer) run(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.wg.Wait()
	}()
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(in)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			if err := s.handle(ctx, line); err != nil {
				return err
			}
		}
	}
}

func (s *mcpServer) write(v json.RawMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.out.Write(append(slices.Clone(v), '\n'))
	return err
}

func (s *mcpServer) respond(id json.RawMessage, result json.RawMessage) error {
	return s.write(newObj().with("jsonrpc", newStr("2.0")).with("id", nodeOf(id)).with("result", nodeOf(result)).compact())
}

func (s *mcpServer) fail(id json.RawMessage, code int, message string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	errObj := newObj().with("code", newNum(fmt.Sprint(code))).with("message", newStr(message))
	return s.write(newObj().with("jsonrpc", newStr("2.0")).with("id", nodeOf(id)).with("error", errObj).compact())
}

func (s *mcpServer) handle(ctx context.Context, line []byte) error {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return s.fail(nil, -32700, "Parse error: "+err.Error())
	}
	notification := len(msg.ID) == 0 || string(msg.ID) == "null"
	if msg.Method == "" {
		return nil
	}
	if notification {
		if msg.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				s.callsMu.Lock()
				if c := s.inflight[string(p.RequestID)]; c != nil {
					c()
				}
				s.callsMu.Unlock()
			}
		}
		return nil
	}
	switch msg.Method {
	case "initialize":
		return s.respond(msg.ID, s.initialize(msg.Params))
	case "ping":
		return s.respond(msg.ID, json.RawMessage("{}"))
	case "tools/list":
		return s.respond(msg.ID, s.list())
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return s.fail(msg.ID, -32602, "Invalid params: "+err.Error())
		}
		idx := slices.IndexFunc(s.tools, func(t mcpTool) bool { return t.name == p.Name })
		if idx < 0 {
			return s.fail(msg.ID, -32602, "Unknown tool: "+p.Name)
		}
		callCtx, cancel := context.WithCancel(ctx)
		key := string(msg.ID)
		s.callsMu.Lock()
		s.inflight[key] = cancel
		s.callsMu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.callsMu.Lock()
				delete(s.inflight, key)
				s.callsMu.Unlock()
				cancel()
			}()
			result := s.call(callCtx, s.tools[idx], p.Arguments)
			if callCtx.Err() != nil && ctx.Err() == nil {
				return
			}
			if err := s.respond(msg.ID, result); err != nil {
				slog.Warn(fmt.Sprintf("%s MCP server could not write a tool result: %v", MCPServerName, err))
			}
		}()
		return nil
	}
	return s.fail(msg.ID, -32601, "Method not found: "+msg.Method)
}

func (s *mcpServer) initialize(params json.RawMessage) json.RawMessage {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	versions := mcpProtocolVersions()
	version := versions[0]
	if slices.Contains(versions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	serverVersion := "devel"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		serverVersion = info.Main.Version
	}
	return newObj().
		with("protocolVersion", newStr(version)).
		with("capabilities", newObj().with("tools", newObj().with("listChanged", newBool(false)))).
		with("serverInfo", newObj().with("name", newStr(MCPServerName)).with("version", newStr(serverVersion))).
		with("instructions", newStr(mcpInstructions)).compact()
}

func (s *mcpServer) list() json.RawMessage {
	tools := newArr()
	outputSchema := newObj().with("type", newStr("object")).
		with("properties", newObj().with("result", newObj().with("type", newStr("string")))).
		with("required", newArr(newStr("result")))
	for _, t := range s.tools {
		schema, _ := parseNode(t.schema)
		tools.arr = append(tools.arr, newObj().with("name", newStr(t.name)).with("description", newStr(t.description)).
			with("inputSchema", schema).with("outputSchema", outputSchema))
	}
	return newObj().with("tools", tools).compact()
}

func toolCallResult(text string, isError bool) json.RawMessage {
	r := newObj().with("content", newArr(newObj().with("type", newStr("text")).with("text", newStr(text))))
	if !isError {
		r.set("structuredContent", newObj().with("result", newStr(text)))
	}
	return r.with("isError", newBool(isError)).compact()
}

func (s *mcpServer) call(ctx context.Context, t mcpTool, raw json.RawMessage) json.RawMessage {
	args := Args{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		parsed, err := parseArgs(string(raw))
		if err != nil {
			return toolCallResult(fmt.Sprintf("Error executing tool %s: arguments must be a JSON object: %v", t.name, err), true)
		}
		for k, v := range parsed {
			if t.props[k] && v != nil {
				args[k] = v
			}
		}
	}
	var missing []string
	for _, r := range t.required {
		if _, ok := args[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return toolCallResult(fmt.Sprintf("Error executing tool %s: missing required argument(s): %s", t.name, strings.Join(missing, ", ")), true)
	}
	c := s.template
	c.Name = t.name
	c.Arguments = string(raw)
	res := s.d.Dispatch(ctx, &c, args, DispatchFlags{})
	return toolCallResult(res.Plain(), false)
}
