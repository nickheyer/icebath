package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestAFinishChildThatCannotStartAfterTheCommitIsOwedNotFailed(t *testing.T) {
	tmp := t.TempDir()
	served := filepath.Join(tmp, "home")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", served)
	savedLock, savedPrepare, savedPopen := takeoverAcquireLock, takeoverPrepare, takeoverPopen
	savedFinalize, savedStderr := takeoverFinalize, takeoverStderr
	t.Cleanup(func() {
		takeoverAcquireLock, takeoverPrepare, takeoverPopen = savedLock, savedPrepare, savedPopen
		takeoverFinalize, takeoverStderr = savedFinalize, savedStderr
	})
	takeoverAcquireLock = func(context.Context, string) (bool, *updaterecord.UpdateHolder, func(), error) {
		return true, nil, func() {}, nil
	}
	takeoverPrepare = func(context.Context, *pyjson.Object) (string, []string, error) {
		return os.Args[0], os.Environ(), nil
	}
	takeoverPopen = func(context.Context, []string, string, ChildOptions) (*exec.Cmd, error) {
		return nil, syscall.ENOMEM
	}
	var saved []int
	takeoverFinalize = func(ctx context.Context, code *int, reason string) string {
		saved = append(saved, *code)
		return updaterecord.FinalizePendingUpdateReceipt(ctx, code, reason)
	}
	stderr := &bytes.Buffer{}
	takeoverStderr = stderr
	root := filepath.Join(tmp, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	contextPath, result := filepath.Join(tmp, "context.json"), filepath.Join(tmp, "result.json")
	body, _ := json.Marshal(map[string]any{"root": root, "gateway_mode": true, "receipt": map[string]any{}})
	if err := os.WriteFile(contextPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	code, err := CmdUpdateTakeover(context.Background(), map[string]any{"context": contextPath, "result": result}, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("takeover %d %v", code, err)
	}

	if len(saved) != 1 || saved[0] != 0 {
		t.Fatalf("finalized %v", saved)
	}
	exitCode, err := os.ReadFile(filepath.Join(served, ".update_exit_code"))
	if err != nil || strings.TrimSpace(string(exitCode)) != "0" {
		t.Fatalf("exit code %q %v", exitCode, err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := json.Unmarshal(raw, &ack); err != nil || ack["resume_handled"] != false {
		t.Fatalf("acknowledgement %s %v", raw, err)
	}
	if !strings.Contains(stderr.String(), "finishing steps did not run") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
