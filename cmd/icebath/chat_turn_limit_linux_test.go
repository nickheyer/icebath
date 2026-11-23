package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatRunsWithoutATurnLimitWhenTheStoredOneIsNotANumber(t *testing.T) {
	peer := newModelPeer(t)
	h, env := cliGatewayHomes(t, peer)
	configPath := filepath.Join(h, "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	stored := strings.TrimSuffix(strings.TrimSpace(string(config)), "}") + `, "agent": {"max_turns": .nan}}`
	if err := os.WriteFile(configPath, []byte(stored), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := cliProgramBinary(t)
	bootHome = h
	t.Cleanup(func() { stopCLIGateway(t, bin, h, env) })
	code, stdout, stderr := runCLIProgram(t, bin, env, 120*time.Second, "chat", "-q", "WS_SHARED please")
	if code != 0 || !strings.Contains(stdout, "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("chat -q = %d\nstdout:\n%s\nstderr:\n%s\n%s", code, stdout, stderr, homeFailureLines(h))
	}
	if !strings.Contains(stderr, "agent.max_turns") || !strings.Contains(stderr, "takes a whole number") {
		t.Fatalf("the value that is not a number was not reported:\n%s", stderr)
	}
}
