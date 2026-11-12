package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const pluginResumePhaseEnv = "ICEBATH_TEST_PLUGIN_RESUME_PHASE"

// Runs one fresh process phase of the plugin section resume case and exits before the suite
func dispatchPluginResumeChild() {
	phase := os.Getenv(pluginResumePhaseEnv)
	if phase == "" {
		return
	}
	out, err := pluginResumeChild(phase)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	fmt.Println(out)
	os.Exit(0)
}

// Restores or builds the prompt of a persisted session with a counting plugin section and reports the bytes
func pluginResumeChild(phase string) (string, error) {
	env := &tools.Env{Home: os.Getenv("ICEBATH_HOME")}
	ctx := tools.WithToolEnv(context.Background(), env)
	db, err := state.NewSessionDB(ctx, os.Getenv("TEST_DB"), false)
	if err != nil {
		return "", err
	}
	defer db.Close()
	sessionID := "resume-plugin-section"
	if err := db.EnsureSession(state.NewSession{ID: sessionID, Source: "cli", Model: "test/model"}); err != nil {
		return "", err
	}
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Provider: "openrouter",
		Platform: "cli", QuietMode: true, SkipContextFiles: true, SkipMemory: true, SessionID: sessionID, SessionDB: db, Env: env})
	if err != nil {
		return "", err
	}
	manager := plugins.GetPluginManager(ctx)
	manager.MarkDiscoveredForTests()
	pc := plugins.NewPluginContext(&plugins.Manifest{Name: "example-plugin", Key: "example-plugin", Source: "user"}, manager)
	callsPath := os.Getenv("TEST_CALLS")
	readCalls := func() int {
		raw, err := os.ReadFile(callsPath)
		if err != nil || len(raw) == 0 {
			return 0
		}
		n, _ := strconv.Atoi(string(raw))
		return n
	}
	render := func(map[string]any) string {
		_ = os.WriteFile(callsPath, []byte(strconv.Itoa(readCalls()+1)), 0o600)
		if phase == "first" {
			return "original bytes"
		}
		return "CHANGED"
	}
	if _, err := pc.RegisterSystemPromptSection("example.rules", render, plugins.WithSectionPosition("after_memory")); err != nil {
		return "", err
	}
	history := []map[string]any{}
	if phase != "first" {
		history = []map[string]any{{"role": "user", "content": "already persisted"}}
	}
	if err := restoreOrBuildSystemPrompt(ctx, a, nil, history); err != nil {
		return "", err
	}
	restored := a.cachedSystemPrompt
	report := map[string]any{"prompt_b64": base64.StdEncoding.EncodeToString([]byte(restored)), "calls": readCalls(),
		"rebuilt_equal": nil, "rebuilt_has_changed": nil, "calls_after_rebuild": nil}
	if phase != "first" {
		if err := InvalidateSystemPrompt(ctx, a); err != nil {
			return "", err
		}
		rebuilt, err := BuildSystemPrompt(ctx, a, nil)
		if err != nil {
			return "", err
		}
		report["rebuilt_equal"], report["rebuilt_has_changed"], report["calls_after_rebuild"] = rebuilt == restored, strings.Contains(rebuilt, "CHANGED"), readCalls()
	}
	raw, err := json.Marshal(report)
	return string(raw), err
}

type pluginResumeReport struct {
	PromptB64         string `json:"prompt_b64"`
	Calls             int    `json:"calls"`
	RebuiltEqual      *bool  `json:"rebuilt_equal"`
	RebuiltHasChanged *bool  `json:"rebuilt_has_changed"`
	CallsAfterRebuild *int   `json:"calls_after_rebuild"`
}

func TestFreshProcessResumeRestoresIdenticalFullPromptWithoutCallback(t *testing.T) {
	caseHome(t)
	t.Run("test_fresh_process_resume_restores_identical_full_prompt_without_callback", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("ICEBATH_HOME", tmp)
		var outputs []pluginResumeReport
		for _, phase := range []string{"first", "resume"} {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "ICEBATH_HOME="+filepath.Join(tmp, "icebath-home"), "TEST_DB="+filepath.Join(tmp, "state.db"),
				"TEST_CALLS="+filepath.Join(tmp, "calls.txt"), pluginResumePhaseEnv+"="+phase)
			out, err := cmd.Output()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				t.Fatalf("phase %s: %v\n%s%s", phase, err, out, exitErr.Stderr)
			} else if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			var report pluginResumeReport
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
				t.Fatalf("phase %s: %v in %q", phase, err, out)
			}
			outputs = append(outputs, report)
		}
		first, _ := base64.StdEncoding.DecodeString(outputs[0].PromptB64)
		resumed, _ := base64.StdEncoding.DecodeString(outputs[1].PromptB64)
		if string(first) != string(resumed) {
			at := 0
			for at < min(len(first), len(resumed)) && first[at] == resumed[at] {
				at++
			}
			t.Fatalf("prompt bytes differ at %d: first=%q resumed=%q", at, first[max(0, at-100):min(len(first), at+100)], resumed[max(0, at-100):min(len(resumed), at+100)])
		}
		if !strings.Contains(string(first), "original bytes") || strings.Contains(string(resumed), "CHANGED") {
			t.Fatalf("first %q resumed %q", first, resumed)
		}
		if outputs[0].Calls != 1 || outputs[1].Calls != 1 {
			t.Fatalf("calls %d %d", outputs[0].Calls, outputs[1].Calls)
		}
		r := outputs[1]
		if r.RebuiltEqual == nil || *r.RebuiltEqual || r.RebuiltHasChanged == nil || !*r.RebuiltHasChanged || r.CallsAfterRebuild == nil || *r.CallsAfterRebuild != 2 {
			t.Fatalf("rebuild %+v", r)
		}
	})
}
