package acpclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("ACPCLIENT_FAKE"); mode != "" {
		os.Exit(runFakeAgent(mode))
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	config.RegisterConfigFacade(config.FacadeHooks{CoreDeclaredEnvNames: config.StaticCoreEnvNames})
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
	if tools.RegisteredAdapterSecretEnv == nil {
		tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	}
	os.Exit(m.Run())
}

// Agent CLI behaviours the test binary plays when re-executed with ACPCLIENT_FAKE set
func runFakeAgent(mode string) int {
	in := bufio.NewReader(os.Stdin)
	reply := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	readRequest := func() map[string]any {
		line, err := in.ReadString('\n')
		if err != nil {
			os.Exit(4)
		}
		var req map[string]any
		_ = json.Unmarshal([]byte(line), &req)
		return req
	}
	switch mode {
	case "server":
		for {
			line, err := in.ReadString('\n')
			if err != nil {
				return 0
			}
			var req map[string]any
			_ = json.Unmarshal([]byte(line), &req)
			result := map[string]any{}
			switch req["method"] {
			case "initialize":
				result = map[string]any{"protocolVersion": 1}
			case "session/new":
				result = map[string]any{"sessionId": "s1"}
			case "session/prompt":
				time.Sleep(400 * time.Millisecond)
				reply(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "done"}}}})
				result = map[string]any{"stopReason": "end_turn"}
			}
			reply(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
		}
	case "crash":
		exe, _ := os.Executable()
		child := exec.Command(exe)
		child.Env = append(os.Environ(), "ACPCLIENT_FAKE=crash-child")
		child.Stderr = os.Stderr
		_ = child.Start()
		return 3
	case "crash-child":
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "fatal: agent segfaulted")
		return 0
	case "stdin-close", "deaf":
		_ = os.Stdin.Close()
		f, _ := os.Create(os.Getenv("ACPCLIENT_FAKE_MARK"))
		_ = f.Close()
		if mode == "deaf" {
			time.Sleep(120 * time.Second)
			return 0
		}
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "fatal: agent cannot start")
		return 2
	case "fs-request":
		initialize := readRequest()
		reply(map[string]any{"jsonrpc": "2.0", "id": initialize["id"], "result": map[string]any{"protocolVersion": 1}})
		session := readRequest()
		reply(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "fs/read_text_file", "params": map[string]any{"path": os.Getenv("ACPCLIENT_FAKE_TARGET")}})
		fileResponse := readRequest()
		if code, _ := fileResponse["error"].(map[string]any)["code"].(float64); code != -32601 {
			return 5
		}
		reply(map[string]any{"jsonrpc": "2.0", "id": session["id"], "result": map[string]any{"sessionId": "catalog-session",
			"configOptions": []any{map[string]any{"id": "model", "options": []any{map[string]any{"value": "model-sol"}}}}}})
		_, _ = in.ReadString('\n')
		return 0
	}
	return 1
}
