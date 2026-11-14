//go:build windows

package selfupdate

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestResumeVerificationLiveDeadRelaunchIsNotReportedAsSuccess(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	vsSwap(t, &winRefreshLaunchers, func(context.Context, io.Writer) {})
	vsSwap(t, &winLaunchDetachedRestart, func(ctx context.Context, _ string, _ int) (bool, error) {
		dead := exec.Command("cmd", "/c", "exit 0")
		if err := dead.Run(); err != nil {
			return false, err
		}
		return gateway.SpawnGatewayRestartWatcher(ctx, dead.Process.Pid, []string{"cmd", "/c", "exit 0"}, nil, "")
	})
	token := map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 999999}, "unmapped_pids": []any{}, "unmapped": []any{}}
	var out bytes.Buffer
	err := resumeWindowsGatewaysAfterUpdate(context.Background(), &out, token)
	if err == nil || !strings.Contains(err.Error(), "not verified alive") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(out.String(), "✓ Restarting") || !strings.Contains(out.String(), "could not be verified") {
		t.Fatalf("the updater vouched for a gateway that is not alive:\n%s", out.String())
	}
}
