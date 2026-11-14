package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPickedModelRunsTheQuickstartJobAndReportsHowItEnded(t *testing.T) {
	for _, outcome := range []string{"done", "error"} {
		t.Run(outcome, func(t *testing.T) {
			lmTestHome(t, ".icebath")
			saved := localFlowDeps
			t.Cleanup(func() { localFlowDeps = saved })
			row := map[string]any{"id": "tiny", "display_name": "Tiny", "fits": true, "needs_engine": false, "recommended": true,
				"downloaded": false, "size_label": "1.0 GB", "fit_summary": "runs fully on your GPU"}
			localFlowDeps.hardware = func(context.Context) (map[string]any, error) {
				return map[string]any{"gpu_name": "GPU", "vram_label": "8.0 GB"}, nil
			}
			localFlowDeps.status = func(context.Context) (map[string]any, error) {
				return map[string]any{"runtime_installed": true, "tag": "b1", "runtime_backend": "cpu", "models": []any{}, "active_model_id": nil}, nil
			}
			localFlowDeps.catalog = func(context.Context) (map[string]any, error) {
				return map[string]any{"models": []any{row}}, nil
			}
			var started []string
			localFlowDeps.quickstart = func(ctx context.Context, modelID, profile string) (map[string]any, error) {
				started = append(started, modelID)
				job := lmNewJob("quickstart", "Tiny", modelID)
				jobID := lmJobID(job)
				run := func(ctx context.Context) error {
					if err := lmStep(job, "downloading", "Downloading Tiny"); err != nil {
						return err
					}
					if outcome == "error" {
						return errors.New("disk full")
					}
					return lmFinish(job, "Tiny is ready — new chats use it")
				}
				lmSpawnJob(ctx, job, "test-quickstart", run, lmSpawnOptions{resumable: true})
				return map[string]any{"job_id": jobID}, nil
			}
			localFlowDeps.promptChoice = func(out io.Writer, labels []string, def int, title string) (int, bool) { return def, true }
			localFlowDeps.promptYesNo = func(question string, def bool, description string) bool { return true }
			var out bytes.Buffer
			if err := modelFlowLocal(&ModelFlowRun{Ctx: context.Background(), Out: &out, Config: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if !reflect.DeepEqual(started, []string{"tiny"}) {
				t.Fatalf("started %v", started)
			}
			if outcome == "done" {
				if !strings.Contains(text, "✓ Tiny is ready") || !strings.Contains(text, "Default model set to: Tiny") {
					t.Fatalf("output %q", text)
				}
			} else if !strings.Contains(text, "✗ disk full") || strings.Contains(text, "Default model set to") {
				t.Fatalf("output %q", text)
			}
		})
	}
}
