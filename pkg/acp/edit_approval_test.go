package acp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestAcpPermissionToolCallUsesEditKindAndDiffContent(t *testing.T) {
	args := map[string]any{"path": "demo.txt", "content": "new\n"}
	tc := BuildACPEditToolCall(&tools.EditProposal{ToolName: "write_file", Path: "demo.txt", OldText: strPtr("old\n"), NewText: "new\n", Arguments: args})
	if *tc.Kind != "edit" || *tc.Status != "pending" || len(tc.Content) != 1 {
		t.Fatalf("tool call %#v", tc)
	}
	raw := tc.RawInput.(*jsonutil.Object)
	if dget(raw, "tool") != "write_file" || !anyutil.Equal(jsonutil.Plain(dget(raw, "arguments")), args) {
		t.Fatalf("raw %v", raw)
	}
	d := tc.Content[0].(*acputil.FileEditToolCallContent)
	if d.Path != "demo.txt" || *d.OldText != "old\n" || d.NewText != "new\n" {
		t.Fatalf("diff %#v", d)
	}
}

func strPtr(s string) *string { return &s }

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
	escape := filepath.Join(filepath.VolumeName(tmp)+string(filepath.Separator), ".icebath-acp-escape-test.txt")
	p = build("*** Update File: " + tmp + "/src/ok.py\n@@\n+ok\n*** Update File: " + escape + "\n@@\n+evil\n")
	if tools.ShouldAutoApproveEdit(p, "workspace_session", tmp) {
		t.Fatal("escape approved")
	}
	p = build("*** Update File: " + tmp + "/a.py\n@@\n+a\n*** Update File: " + tmp + "/b.py\n@@\n+b\n")
	if !tools.ShouldAutoApproveEdit(p, "workspace_session", tmp) {
		t.Fatal("safe refused")
	}
}
