package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
)

// Marker rooted workspace with optional package scripts and a saved verify recipe
func ledgerNudgeWorkspace(t *testing.T, scripts map[string]any, manifestRecipe map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath-home"))
	t.Setenv("ICEBATH_SESSION_ID", "")
	os.Unsetenv("ICEBATH_SESSION_ID")
	project := filepath.Join(dir, "project")
	pkg := map[string]any{}
	if len(scripts) > 0 {
		pkg = map[string]any{"scripts": scripts}
	}
	body, err := pyjson.Dumps(pkg)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, "package.json"), body)
	if manifestRecipe != nil {
		manifest, err := pyjson.Dumps(map[string]any{"version": 1, "recipe": manifestRecipe})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, verify.ManifestPath(project), manifest)
	}
	return project
}

// Nudge for one edited file of the session in the project
func editedProjectNudge(t *testing.T, project string) string {
	t.Helper()
	changed := filepath.Join(project, "src", "app.ts")
	markEdited(t, "s1", project, changed)
	nudge := stopNudge(t, "s1", changed)
	if nudge == "" {
		t.Fatal("no nudge for an unverified edit")
	}
	return nudge
}

func TestRecordVerifyRunMarksWorkspacePassed(t *testing.T) {
	project := ledgerNudgeWorkspace(t, nil, nil)
	event := recordVerify(t, project, "s1", true, "full", "all green")
	if event == nil || event["status"] != "passed" || event["kind"] != "verify" {
		t.Fatalf("event = %v", event)
	}
	status := ledgerStatus(t, "s1", project)
	evidence, _ := status["evidence"].(map[string]any)
	if status["status"] != "passed" || evidence["canonical_command"] != "icebath verify" {
		t.Fatalf("status = %v", status)
	}
}

func TestRecordVerifyRunRecordsFailure(t *testing.T) {
	project := ledgerNudgeWorkspace(t, nil, nil)
	recordVerify(t, project, "s1", false, "full", "boom")
	if got := ledgerStatus(t, "s1", project)["status"]; got != "failed" {
		t.Fatalf("status = %v", got)
	}
}

func TestNudgeMentionsHermesVerifyWhenRecipeHasStart(t *testing.T) {
	project := ledgerNudgeWorkspace(t, map[string]any{"test": "vitest", "dev": "vite"}, nil)
	nudge := editedProjectNudge(t, project)
	if !strings.Contains(nudge, "icebath verify --json") || !strings.Contains(nudge, "npm run test") {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeMentionsHermesVerifyWhenManifestExists(t *testing.T) {
	project := ledgerNudgeWorkspace(t, map[string]any{"test": "vitest"}, map[string]any{"name": "Fake", "test": []any{"echo ok"}})
	if nudge := editedProjectNudge(t, project); !strings.Contains(nudge, "icebath verify --json") {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeKeepsPlainWordingWithoutRecipeStart(t *testing.T) {
	project := ledgerNudgeWorkspace(t, map[string]any{"test": "vitest"}, nil)
	if nudge := editedProjectNudge(t, project); strings.Contains(nudge, "icebath verify") {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeRecipeDetectionFailureIsSilent(t *testing.T) {
	prev := verifyDetectRecipe
	verifyDetectRecipe = func(string) (*verify.Recipe, error) { return nil, errors.New("detector exploded") }
	t.Cleanup(func() { verifyDetectRecipe = prev })
	project := ledgerNudgeWorkspace(t, map[string]any{"test": "vitest", "dev": "vite"}, nil)
	if nudge := editedProjectNudge(t, project); strings.Contains(nudge, "icebath verify") {
		t.Fatalf("nudge = %q", nudge)
	}
}
