package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Temp home whose delegation block is the given yaml, bound as the tool env of the returned context
func approvalConfigContext(t *testing.T, block string) context.Context {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	body := "delegation: {}\n"
	if block != "" {
		body = "delegation:\n" + block
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return WithToolEnv(context.Background(), &Env{Home: h})
}

func sameFunc(a, b func(command, description string, kwargs map[string]any) string) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// test_delegate.py TestSubagentApprovalCallback
func TestSubagentApprovalCallbackAutoDenyReturnsDeny(t *testing.T) {
	if got := subagentAutoDeny("rm -rf /tmp/x", "dangerous", nil); got != "deny" {
		t.Fatalf("got %q", got)
	}
}

func TestSubagentApprovalCallbackGetterDefaultsToDeny(t *testing.T) {
	if !sameFunc(SubagentApprovalCallback(approvalConfigContext(t, "")), subagentAutoDeny) {
		t.Fatal("default callback is not the auto deny")
	}
}

func TestSubagentApprovalCallbackGetterTrueIsApprove(t *testing.T) {
	if !sameFunc(SubagentApprovalCallback(approvalConfigContext(t, "  subagent_auto_approve: true\n")), subagentAutoApprove) {
		t.Fatal("subagent_auto_approve true is not the auto approve")
	}
}
