package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestASwallowedRefusalIsWhatTheUpdateReports(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	checkout := filepath.Join(tmp, "checkout")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(updaterecord.ResetCustodyRun())
	savedRetarget, savedPreflight, savedHangup, savedFinalize, savedImpl := updateRetarget, updatePreflight, updateInstallHangup, updateFinalizeOutput, updateRunImpl
	t.Cleanup(func() {
		updateRetarget, updatePreflight, updateInstallHangup, updateFinalizeOutput, updateRunImpl = savedRetarget, savedPreflight, savedHangup, savedFinalize, savedImpl
	})
	updateRetarget = func(context.Context, string) error { return nil }
	updatePreflight = func(context.Context, io.Writer, map[string]any) (bool, error) { return false, nil }
	updateInstallHangup = func(io.Writer, io.Writer, bool) *gateway.UpdateOutputState { return nil }
	updateFinalizeOutput = func(*gateway.UpdateOutputState) {}
	updateRunImpl = func(ctx context.Context, w io.Writer, _ map[string]any, _ bool) error {
		updaterecord.BeginUpdateReceipt(ctx, nil, "")
		_ = updaterecord.RefuseCustodyChild(ctx, []string{"git", "rev-parse"}, errors.New("[Errno 6] The handle is invalid"), false)
		fmt.Fprintln(w, "✗ Could not resolve HEAD")
		return &pm.SystemExit{Code: 1}
	}
	var out bytes.Buffer
	ctx := updaterecord.UpdateReceiptScope(context.Background())
	code, err := CmdUpdate(ctx, UpdateInvocation{Namespace: map[string]any{"gateway": false}, ProjectRoot: checkout, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	text := out.String()
	if !strings.Contains(text, "`icebath update` stopped: Windows would not put `git` in this update's process job") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "Nothing was changed") || !strings.Contains(text, "Run `icebath update` again from a regular terminal") {
		t.Fatal(text)
	}
	payload := updaterecord.ReadLatestReceipt(ctx)
	rawSteps, _ := payload["steps"].([]any)
	var steps []map[string]any
	for _, s := range rawSteps {
		if step, ok := s.(map[string]any); ok && step["name"] == "update_custody" {
			steps = append(steps, step)
		}
	}
	if len(steps) == 0 || steps[0]["ok"] != false || !strings.Contains(fmt.Sprint(steps[0]["detail"]), "rev-parse") {
		t.Fatalf("steps = %#v", rawSteps)
	}
}
