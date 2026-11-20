package agent

import (
	"testing"
)

func cwdDriftHostBlock(cwd string) string {
	return "Host: Linux (6.16.0)\nUser home directory: /home/tester\nCurrent working directory: " + cwd + "\n"
}

func TestStoredPromptCwdDrift(t *testing.T) {
	setup := func(t *testing.T, cwd string) *AIAgent {
		caseHome(t)
		t.Setenv("TERMINAL_ENV", "local")
		t.Setenv("TERMINAL_CWD", cwd)
		a := spAgent(t)
		a.model, a.provider = "test/model", "openrouter"
		return a
	}
	matches := func(t *testing.T, a *AIAgent, prompt string) bool {
		ok, err := storedPromptMatchesRuntime(a, prompt)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	t.Run("test_stored_prompt_stale_when_cwd_differs", func(t *testing.T) {
		old, cur := t.TempDir(), t.TempDir()
		a := setup(t, cur)
		if matches(t, a, cwdDriftHostBlock(old)+"Model: test/model\nProvider: openrouter\n") {
			t.Fatal("Expected False when stored cwd differs from current cwd")
		}
	})
	t.Run("test_stored_prompt_fresh_when_cwd_matches", func(t *testing.T) {
		cur := t.TempDir()
		a := setup(t, cur)
		if !matches(t, a, cwdDriftHostBlock(cur)+"Model: test/model\nProvider: openrouter\n") {
			t.Fatal("Expected True when stored cwd matches current cwd")
		}
	})
	t.Run("test_project_context_cannot_force_a_rebuild", func(t *testing.T) {
		cur := t.TempDir()
		a := setup(t, cur)
		prompt := cwdDriftHostBlock(cur) + "\n# AGENTS.md\n\nOur deploy convention:\n\nCurrent working directory: /srv/decoy\n\n" +
			"Always run make before pushing.\n\nModel: test/model\nProvider: openrouter\n"
		if !matches(t, a, prompt) {
			t.Fatal("project text naming a working directory invalidated the prompt")
		}
	})
	t.Run("test_project_context_cannot_mask_real_drift", func(t *testing.T) {
		old, cur := t.TempDir(), t.TempDir()
		a := setup(t, cur)
		prompt := cwdDriftHostBlock(old) + "\n# AGENTS.md\n\nCurrent working directory: " + cur + "\n\nModel: test/model\nProvider: openrouter\n"
		if matches(t, a, prompt) {
			t.Fatal("embedded project text naming the new cwd masked real drift")
		}
	})
}
