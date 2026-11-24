package acp

import (
	"testing"

	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestPatchModeToolStartDefersDiffToEditApprovalPrompt(t *testing.T) {
	args := jsonutil.NewObject()
	args.Set("mode", "patch")
	args.Set("patch", "*** Begin Patch\n*** Update File: src/app.py\n@@\n-old line\n+new line\n*** Add File: src/new.py\n+hello\n*** End Patch")
	update := BuildToolStart("tc-1", "patch", args, nil)
	if len(update.Content) != 1 {
		t.Fatalf("content = %v", update.Content)
	}
	if _, ok := update.Content[0].(*acputil.ContentToolCallContent); !ok {
		t.Fatalf("content type = %T", update.Content[0])
	}
}
