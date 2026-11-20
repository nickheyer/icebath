package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChatPrintsTheCLIBuildValueErrorToStdoutAndExitsOne(t *testing.T) {
	root := t.TempDir()
	h, user := filepath.Join(root, "icebath"), filepath.Join(root, "home")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := "model:\n  provider: custom\n  default: local-model\n  base_url: http://127.0.0.1:9/v1\nagent:\n  max_turns: .nan\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	env := gatewayChildEnv(h, user, "OPENAI_API_KEY=loopback-only", runMainEnv+"=1")
	code, stdout, stderr := runCLIProgram(t, os.Args[0], env, 60*time.Second, "chat", "-q", "hi")
	if code != 1 || stdout != "Error: cannot convert float NaN to integer\n" || stderr != "" {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
}
