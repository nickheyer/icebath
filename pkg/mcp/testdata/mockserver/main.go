package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

var (
	outMu   sync.Mutex
	out     = bufio.NewWriter(os.Stdout)
	waitMu  sync.Mutex
	waiters = map[string]chan message{}
	nextID  = 1000
)

// Writes one message as a newline framed JSON line
func send(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	outMu.Unlock()
}

// Appends a received method name to the record file when one is configured
func record(method string) {
	path := os.Getenv("MOCK_MCP_RECORD")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, method)
	f.Close()
}

// Sends a request to the client and waits for its answer
func ask(method string, params any) message {
	waitMu.Lock()
	nextID++
	id := strconv.Itoa(nextID)
	ch := make(chan message, 1)
	waiters[id] = ch
	waitMu.Unlock()
	send(map[string]any{"jsonrpc": "2.0", "id": nextID, "method": method, "params": params})
	return <-ch
}

func reply(id json.RawMessage, result any) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func fail(id json.RawMessage, code int, msg string) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func handle(m message) {
	var params map[string]any
	_ = json.Unmarshal(m.Params, &params)
	switch m.Method {
	case "initialize":
		if os.Getenv("MOCK_MCP_HANG_INIT") != "" {
			select {}
		}
		if code := os.Getenv("MOCK_MCP_INIT_ERROR"); code != "" {
			c, _ := strconv.Atoi(code)
			fail(m.ID, c, os.Getenv("MOCK_MCP_INIT_MESSAGE"))
			return
		}
		version, _ := params["protocolVersion"].(string)
		if v := os.Getenv("MOCK_MCP_VERSION"); v != "" {
			version = v
		}
		caps := map[string]any{"tools": map[string]any{"listChanged": true}}
		if os.Getenv("MOCK_MCP_NO_TOOLS") != "" {
			caps = map[string]any{"prompts": map[string]any{}}
		}
		if raw := os.Getenv("MOCK_MCP_CAPS"); raw != "" {
			caps = map[string]any{}
			_ = json.Unmarshal([]byte(raw), &caps)
		}
		reply(m.ID, map[string]any{"protocolVersion": version, "capabilities": caps,
			"serverInfo": map[string]any{"name": "mock", "version": "1"}, "clientCapabilities": params["capabilities"]})
	case "server/discover":
		if os.Getenv("MOCK_MCP_DISCOVER") == "" {
			fail(m.ID, -32601, "Method not found: server/discover")
			return
		}
		reply(m.ID, map[string]any{"protocolVersion": "2026-07-28", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "stateless", "version": "1"}})
	case "prompts/list":
		if os.Getenv("MOCK_MCP_CAPS") == "" {
			fail(m.ID, -32601, "Method not found")
			return
		}
		reply(m.ID, map[string]any{"prompts": []any{map[string]any{"name": "greet"}}})
	case "prompts/get":
		if os.Getenv("MOCK_MCP_CAPS") == "" {
			fail(m.ID, -32601, "Method not found")
			return
		}
		record(fmt.Sprintf("prompts/get %v", params["name"]))
		reply(m.ID, map[string]any{"messages": []any{}})
	case "resources/list":
		if os.Getenv("MOCK_MCP_CAPS") == "" {
			fail(m.ID, -32601, "Method not found")
			return
		}
		reply(m.ID, map[string]any{"resources": []any{map[string]any{"uri": "file:///a", "name": "a"}, map[string]any{"uri": "file:///b", "name": "b"}}})
	case "ping":
		reply(m.ID, map[string]any{})
	case "tools/list":
		if os.Getenv("MOCK_MCP_BAD_UTF8") != "" {
			raw := append([]byte(`{"jsonrpc":"2.0","id":`), m.ID...)
			raw = append(raw, `,"result":{"tools":[{"name":"mangled","description":"ok`...)
			raw = append(raw, 0xff, 0xe2, 0x82)
			raw = append(raw, `!","inputSchema":{"type":"object"}}]}}`...)
			outMu.Lock()
			out.Write(raw)
			out.WriteByte('\n')
			out.Flush()
			outMu.Unlock()
			return
		}
		pages, _ := strconv.Atoi(os.Getenv("MOCK_MCP_PAGES"))
		if pages == 0 {
			pages = 1
		}
		page := 0
		if c, ok := params["cursor"].(string); ok {
			page, _ = strconv.Atoi(c)
		}
		result := map[string]any{"tools": []any{map[string]any{"name": fmt.Sprintf("tool%d", page),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		if names := os.Getenv("MOCK_MCP_TOOLS"); names != "" && page == 0 {
			var list []any
			for _, n := range strings.Split(names, ",") {
				list = append(list, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result["tools"] = list
		}
		if page == 0 {
			result["ttlMs"] = 60000
			result["cacheScope"] = "user"
		}
		if os.Getenv("MOCK_MCP_FOREVER") != "" || page+1 < pages {
			result["nextCursor"] = strconv.Itoa(page + 1)
		}
		reply(m.ID, result)
	case "tools/call":
		name, _ := params["name"].(string)
		meta, _ := params["_meta"].(map[string]any)
		token, hasToken := meta["progressToken"]
		record(fmt.Sprintf("tools/call %s progress=%v", name, hasToken))
		switch name {
		case "progress", "slow":
			for i := 1; i <= 20 && hasToken; i++ {
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{
					"progressToken": token, "progress": i * 5, "total": 100, "message": fmt.Sprintf("stage %d", i)}})
			}
			reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}})
		case "log":
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{
				"level": "warning", "logger": "http", "data": map[string]any{"b": 1, "a": "rate limited"}}})
			reply(m.ID, map[string]any{"content": []any{}})
		case "changed":
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			reply(m.ID, map[string]any{"content": []any{}})
		case "ask":
			method, _ := params["arguments"].(map[string]any)["method"].(string)
			answer := ask(method, map[string]any{"messages": []any{}, "maxTokens": 5, "message": "hi"})
			body, _ := json.Marshal(map[string]any{"result": answer.Result, "error": answer.Error})
			reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}})
		case "hang":
			select {}
		case "env":
			body, _ := json.Marshal(environMap())
			reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}})
		default:
			reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": os.Getenv("OWNED_MARKER")}}})
		}
	default:
		fail(m.ID, -32601, "Method not found")
	}
}

// Current process environment as a name to value map
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<26)
	for in.Scan() {
		var m message
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			record(m.Method)
			go handle(m)
		case m.Method != "":
			record(m.Method)
		default:
			waitMu.Lock()
			ch := waiters[string(m.ID)]
			delete(waiters, string(m.ID))
			waitMu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}
