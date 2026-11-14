package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Home whose main model runs on the managed llama.cpp runtime with no server answering
func llamacppHome(t *testing.T, localRuntime map[string]any) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(provider.SwapDetectServerForTests(func([]int) (*provider.DetectedServer, error) { return nil, nil }))
	cfg, err := json.Marshal(map[string]any{"model": map[string]any{"provider": "llamacpp", "default": "local-qwen"}, "local_runtime": localRuntime})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// Resolution failure of the llama.cpp main model in a home
func localResolutionError(t *testing.T, h string) *provider.LocalRuntimeUnavailableError {
	t.Helper()
	_, err := provider.ResolveRuntimeProvider(context.Background(), h, "llamacpp", "local-qwen", "", "")
	var local *provider.LocalRuntimeUnavailableError
	if !errors.As(err, &local) {
		t.Fatalf("resolution answered %T %v", err, err)
	}
	return local
}

func TestChatTextNamesTheLocalServerAndNoDesktopOnlyPane(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[enabled], func(t *testing.T) {
			h := llamacppHome(t, map[string]any{"enabled": enabled})
			exc := localResolutionError(t, h)
			reply, reason, retryable := gatewayrun.ResolutionFailureReply(context.Background(), exc, gateway.PlatformTelegram)
			surface := agent.BuildErrorSurfaceFromResult(map[string]any{"failed": true, "error": exc.Error(),
				"failure_reason": reason, "failure_retryable": retryable}, "llamacpp", "")
			tui := turnErrorText(exc.Error(), surface, retryable)
			title := strings.Split(tui, "\n")[0]
			if !strings.Contains(title, "local model server") {
				t.Fatalf("title %q", title)
			}
			if strings.Contains(tui, "custom model endpoint") {
				t.Fatalf("tui text %q", tui)
			}
			for _, text := range []string{reply, tui} {
				if !strings.Contains(text, "local model server") || strings.Contains(text, "Settings") || !strings.Contains(text, "/model") {
					t.Fatalf("text %q", text)
				}
			}
		})
	}
}

func TestChatTextSuggestsRetryOnlyWhileTheServerCanComeUp(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[enabled], func(t *testing.T) {
			h := llamacppHome(t, map[string]any{"enabled": enabled})
			exc := localResolutionError(t, h)
			surface := agent.BuildErrorSurfaceFromResult(map[string]any{"failed": true, "error": exc.Error(),
				"failure_reason": exc.ErrorCode(), "failure_retryable": exc.Retryable}, "llamacpp", "")
			lines := strings.Split(turnErrorText(exc.Error(), surface, true), "\n")
			hint := lines[len(lines)-1]
			if strings.Contains(hint, "/retry") != enabled {
				t.Fatalf("hint %q with enabled=%v", hint, enabled)
			}
			if !enabled {
				if !strings.Contains(hint, "icebath model") || !strings.Contains(hint, "/model") {
					t.Fatalf("hint %q", hint)
				}
				if !strings.Contains(exc.Error(), "icebath model") {
					t.Fatalf("backend sentence %q", exc.Error())
				}
			}
		})
	}
}
