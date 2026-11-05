// Mock language server the LSP tests drive, a Go port of the hermes test mock
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// Reads one Content-Length framed message the way the Python mock does
func mockRead(r *bufio.Reader) map[string]any {
	headers := map[string]string{}
	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return nil
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, _ := strings.Cut(line, ":")
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	n, _ := strconv.Atoi(headers["content-length"])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil
	}
	var msg map[string]any
	json.Unmarshal(body, &msg)
	return msg
}

func mockWrite(obj any) {
	body, _ := json.Marshal(obj)
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n", len(body))
	os.Stdout.Write(body)
}

func mockPublish(uri string, version any, diagnostics []any, withVersion bool) {
	params := map[string]any{"uri": uri, "diagnostics": diagnostics}
	if withVersion {
		params["version"] = version
	}
	mockWrite(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": params})
}

func envFloat(name string, fallback float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func sleepSeconds(f float64) { time.Sleep(time.Duration(f * float64(time.Second))) }

// Child of slow_tree that ignores SIGTERM and records its pid
func runSigtermIgnoringChild() {
	signal.Ignore(syscall.SIGTERM)
	os.WriteFile(os.Getenv("MOCK_LSP_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	time.Sleep(60 * time.Second)
}

// Applies a ranged edit at UTF-16 offsets like a real server
func applyUTF16Edit(text string, change map[string]any) string {
	lines := pySplitLinesKeep(text)
	offset := func(pos map[string]any) int {
		line := int(pos["line"].(float64))
		prefix := strings.Join(lines[:min(line, len(lines))], "")
		return len(utf16.Encode([]rune(prefix))) + int(pos["character"].(float64))
	}
	rng := change["range"].(map[string]any)
	start := offset(rng["start"].(map[string]any))
	end := offset(rng["end"].(map[string]any))
	enc := utf16.Encode([]rune(text))
	repl := utf16.Encode([]rune(change["text"].(string)))
	out := append(append(append([]uint16{}, enc[:start]...), repl...), enc[end:]...)
	return string(utf16.Decode(out))
}

// Python splitlines with line ends kept, for the mock's offset math
func pySplitLinesKeep(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			out = append(out, string(rs[start:i+1]))
			start = i + 1
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\x85', ' ', ' ':
			out = append(out, string(rs[start:i+1]))
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

func main() {
	if os.Getenv("LSP_TEST_ROLE") == "child" {
		runSigtermIgnoringChild()
		return
	}
	os.Exit(runMockServer())
}

// Speaks just enough LSP to drive a client through its lifecycle, selected by MOCK_LSP_SCRIPT
func runMockServer() int {
	script := os.Getenv("MOCK_LSP_SCRIPT")
	if script == "" {
		script = "clean"
	}
	documents := map[string]string{}
	if script == "slow_tree" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "LSP_TEST_ROLE=child")
		child.Start()
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		go func() { <-sigs; os.Exit(0) }()
	}
	if script == "oom_abort" {
		os.Stderr.WriteString("<oproject>:28982 ms: Mark-Compact 2041.4 (2055.6) -> 2038.4 (2058.4) MB\n" +
			"  1317.07 ms (average mu = 0.307, current mu = 0.134)\n" +
			"<--- Last few GCs --->\n" +
			"FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory\n")
		syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -ABRT $$"}, os.Environ())
		return 134
	}
	if script == "large_stderr" {
		os.Stderr.WriteString(strings.Repeat("X", 131072) + "\n")
		script = "clean"
	}
	if script == "oversized_stderr" {
		n, _ := strconv.Atoi(os.Getenv("MOCK_LSP_STDERR_BYTES"))
		os.Stderr.WriteString(strings.Repeat("X", n) + "\n")
		script = "clean"
	}
	errorDiag := []any{map[string]any{
		"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 5}},
		"severity": 1, "code": "MOCK001", "source": "mock-lsp", "message": "synthetic error from mock-lsp",
	}}
	in := bufio.NewReader(os.Stdin)
	for {
		msg := mockRead(in)
		if msg == nil {
			return 0
		}
		method, _ := msg["method"].(string)
		id, hasID := msg["id"]
		if hasID && method == "initialize" {
			if script == "init_error" {
				mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "bad init"}})
				return 0
			}
			if script == "slow" || script == "slow_tree" {
				time.Sleep(time.Second)
			}
			sync := 1
			if script == "incremental" {
				sync = 2
			}
			mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"capabilities": map[string]any{"textDocumentSync": sync,
					"diagnosticProvider": map[string]any{"interFileDependencies": false, "workspaceDiagnostics": false}},
				"serverInfo": map[string]any{"name": "mock-lsp", "version": "0.1"},
			}})
			if script == "crash" {
				return 0
			}
			continue
		}
		switch method {
		case "initialized", "workspace/didChangeConfiguration", "workspace/didChangeWatchedFiles":
			continue
		case "workspace/didChangeWorkspaceFolders":
			if p := os.Getenv("MOCK_LSP_FOLDERS_LOG"); p != "" {
				f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				b, _ := json.Marshal(msg["params"])
				f.Write(append(b, '\n'))
				f.Close()
			}
			continue
		case "textDocument/didOpen", "textDocument/didChange":
			params, _ := msg["params"].(map[string]any)
			td, _ := params["textDocument"].(map[string]any)
			uri, _ := td["uri"].(string)
			version := td["version"]
			isChange := method == "textDocument/didChange"
			if script == "incremental" {
				text := documents[uri]
				if !isChange {
					text, _ = td["text"].(string)
				}
				changes, _ := params["contentChanges"].([]any)
				for _, c := range changes {
					change := c.(map[string]any)
					if _, ranged := change["range"]; !ranged {
						text = change["text"].(string)
						continue
					}
					text = applyUTF16Edit(text, change)
				}
				documents[uri] = text
			}
			if !isChange && (script == "clean_eof" || script == "malformed_frame") {
				if script == "malformed_frame" {
					os.Stdout.WriteString("Content-Length: invalid\r\n\r\n")
				}
				os.Stdout.Close()
				for mockRead(in) != nil {
				}
				return 0
			}
			switch script {
			case "silent":
				continue
			case "stale":
				if !isChange {
					mockPublish(uri, version, errorDiag, true)
				}
				continue
			case "by_content":
				text, _ := td["text"].(string)
				if isChange {
					changes, _ := params["contentChanges"].([]any)
					last, _ := changes[len(changes)-1].(map[string]any)
					text, _ = last["text"].(string)
				}
				gate := os.Getenv("MOCK_LSP_GATE")
				deadline := time.Now().Add(60 * time.Second)
				for gate != "" && time.Now().Before(deadline) {
					if _, err := os.Stat(gate); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if gate == "" {
					sleepSeconds(envFloat("MOCK_LSP_PUSH_DELAY", 0.4))
				}
				diags := []any{}
				if strings.Contains(text, "bad") {
					diags = errorDiag
				}
				mockPublish(uri, version, diags, true)
				continue
			case "slow_push":
				if isChange {
					sleepSeconds(envFloat("MOCK_LSP_PUSH_DELAY", 1.0))
					mockPublish(uri, version, []any{}, true)
				} else {
					mockPublish(uri, version, errorDiag, true)
				}
				continue
			}
			diags := []any{}
			if script == "errors" {
				diags = errorDiag
			}
			if script == "versionless" && !isChange {
				diags = errorDiag
			}
			mockPublish(uri, version, diags, script != "versionless")
			continue
		case "textDocument/hover":
			if script == "incremental" {
				params := msg["params"].(map[string]any)
				uri := params["textDocument"].(map[string]any)["uri"].(string)
				mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"contents": map[string]any{"kind": "plaintext", "value": documents[uri]}}})
				continue
			}
		case "textDocument/diagnostic":
			switch script {
			case "stale", "slow_push", "versionless", "silent", "by_content":
				mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
			default:
				mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"kind": "full", "items": []any{}}})
			}
			continue
		case "textDocument/didSave":
			continue
		case "shutdown":
			mockWrite(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
			continue
		case "exit":
			return 0
		}
		if hasID {
			mockWrite(map[string]any{"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": "method not found: " + method}})
		}
	}
}
