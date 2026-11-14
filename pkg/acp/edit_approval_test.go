package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func dispatchTool(t *testing.T, ctx context.Context, name string, args map[string]any, taskID string) *pyjson.Object {
	t.Helper()
	raw := tools.HandleFunctionCall(ctx, &tools.Call{Name: name, Args: args, TaskID: taskID})
	out, err := pyjson.LoadsDict(raw)
	if err != nil {
		t.Fatalf("result %q", raw)
	}
	return out
}

func TestAcpPermissionToolCallUsesEditKindAndDiffContent(t *testing.T) {
	args := map[string]any{"path": "demo.txt", "content": "new\n"}
	tc := BuildACPEditToolCall(&tools.EditProposal{ToolName: "write_file", Path: "demo.txt", OldText: strPtr("old\n"), NewText: "new\n", Arguments: args})
	if *tc.Kind != "edit" || *tc.Status != "pending" || len(tc.Content) != 1 {
		t.Fatalf("tool call %#v", tc)
	}
	raw := tc.RawInput.(*pyjson.Object)
	if dget(raw, "tool") != "write_file" || !pyjson.Equal(pyjson.Plain(dget(raw, "arguments")), args) {
		t.Fatalf("raw %v", raw)
	}
	d := tc.Content[0].(*pyacp.FileEditToolCallContent)
	if d.Path != "demo.txt" || *d.OldText != "old\n" || d.NewText != "new\n" {
		t.Fatalf("diff %#v", d)
	}
}

func strPtr(s string) *string { return &s }

func TestRequesterExceptionDeniesAndDoesNotMutate(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "sample.txt")
	os.WriteFile(target, []byte("before\n"), 0o644)
	ctx := SetEditApprovalRequester(context.Background(), func(context.Context, *tools.Env, *tools.EditProposal) (bool, error) {
		return false, errors.New("zed disconnected")
	})
	res := dispatchTool(t, ctx, "write_file", map[string]any{"path": target, "content": "after\n"}, "acp-edit-exception")
	if e, _ := dget(res, "error").(string); !strings.Contains(e, "Edit approval denied") {
		t.Fatalf("result %v", res)
	}
	if b, _ := os.ReadFile(target); string(b) != "before\n" {
		t.Fatalf("mutated %q", b)
	}
}

func TestPatchReplaceRejectionDoesNotMutate(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "sample.txt")
	os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644)
	ctx := SetEditApprovalRequester(context.Background(), func(context.Context, *tools.Env, *tools.EditProposal) (bool, error) { return false, nil })
	res := dispatchTool(t, ctx, "patch", map[string]any{"mode": "replace", "path": target, "old_string": "beta\n", "new_string": "gamma\n"}, "acp-patch-reject")
	if e, _ := dget(res, "error").(string); !strings.Contains(e, "Edit approval denied") {
		t.Fatalf("result %v", res)
	}
	if b, _ := os.ReadFile(target); string(b) != "alpha\nbeta\n" {
		t.Fatalf("mutated %q", b)
	}
}

func TestWorkspaceAutoApprovalAllowsWorkspaceAndTmpButNotSensitive(t *testing.T) {
	tmp := t.TempDir()
	tmpFile := filepath.Join(os.TempDir(), "icebath-acp-auto-approve-test.txt")
	if !tools.ShouldAutoApproveEdit(&tools.EditProposal{ToolName: "write_file", Path: filepath.Join(tmp, "src.py"), NewText: "x"}, "workspace_session", tmp) ||
		!tools.ShouldAutoApproveEdit(&tools.EditProposal{ToolName: "write_file", Path: tmpFile, NewText: "x"}, "workspace_session", tmp) ||
		tools.ShouldAutoApproveEdit(&tools.EditProposal{ToolName: "write_file", Path: filepath.Join(tmp, ".env"), NewText: "SECRET=x"}, "session", tmp) {
		t.Fatal("auto approval")
	}
}

func TestMultifileV4aPatchChecksEveryRealPathNotTheJoinedDisplayString(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	build := func(patch string) *tools.EditProposal {
		p, err := tools.BuildEditProposal(nil, "patch", map[string]any{"mode": "patch", "patch": patch})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := build("*** Update File: .env\n@@\n+SECRET=x\n*** Update File: src/ok.py\n@@\n+ok\n")
	if tools.ShouldAutoApproveEdit(p, "session", tmp) || tools.ShouldAutoApproveEdit(p, "workspace_session", tmp) {
		t.Fatal("hidden env approved")
	}
	userHome, _ := home.UserHome()
	escape := filepath.Join(userHome, ".icebath-acp-escape-test.txt")
	p = build("*** Update File: " + tmp + "/src/ok.py\n@@\n+ok\n*** Update File: " + escape + "\n@@\n+evil\n")
	if tools.ShouldAutoApproveEdit(p, "workspace_session", tmp) {
		t.Fatal("escape approved")
	}
	p = build("*** Update File: " + tmp + "/a.py\n@@\n+a\n*** Update File: " + tmp + "/b.py\n@@\n+b\n")
	if !tools.ShouldAutoApproveEdit(p, "workspace_session", tmp) {
		t.Fatal("safe refused")
	}
}

func TestMultifileV4aEnvWriteReachesPermissionPromptE2e(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	envTarget, okTarget := filepath.Join(tmp, ".env"), filepath.Join(tmp, "ok.py")
	patch := "*** Update File: " + envTarget + "\n@@\n+SECRET=x\n*** Update File: " + okTarget + "\n@@\n+ok\n"
	var prompted []string
	ctx := SetEditApprovalRequester(context.Background(), func(_ context.Context, _ *tools.Env, p *tools.EditProposal) (bool, error) {
		if tools.ShouldAutoApproveEdit(p, "session", tmp) {
			return true, nil
		}
		prompted = append(prompted, p.Path)
		return false, nil
	})
	res := dispatchTool(t, ctx, "patch", map[string]any{"mode": "patch", "patch": patch}, "acp-v4a-e2e")
	e, _ := dget(res, "error").(string)
	if len(prompted) == 0 || !strings.Contains(strings.ToLower(e), "denied") {
		t.Fatalf("prompted %v result %v", prompted, res)
	}
	if _, err := os.Stat(envTarget); err == nil {
		t.Fatal("env written")
	}
	if _, err := os.Stat(okTarget); err == nil {
		t.Fatal("ok written")
	}
}
