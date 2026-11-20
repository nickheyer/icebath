package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provider and gateway sources of the env blocklist that a running process installs at startup
func init() {
	config.RegisterConfigFacade(config.FacadeHooks{CoreDeclaredEnvNames: config.StaticCoreEnvNames})
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
	if tools.RegisteredAdapterSecretEnv == nil {
		tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	}
}

// Clears the skill registered and config passthrough allowlists around a test
func cleanPassthrough(t *testing.T) {
	t.Helper()
	tools.ClearEnvPassthrough()
	tools.ResetEnvPassthroughConfigCache()
	t.Cleanup(func() {
		tools.ClearEnvPassthrough()
		tools.ResetEnvPassthroughConfigCache()
	})
}

// Copies the embedded optional agentmail skill into a skills dir
func installAgentMail(t *testing.T, skills string) {
	t.Helper()
	src, err := fs.Sub(OptionalFS(), "email/agentmail")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(skills, "email", "agentmail"), src); err != nil {
		t.Fatal(err)
	}
}

func TestSkillViewRegistersAvailableEnvPassthrough(t *testing.T) {
	cleanPassthrough(t)
	withCapture(t, nil)
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "test-skill", "required_environment_variables:\n  - name: TENOR_API_KEY\n    prompt: Enter your Tenor API key\n", "", "")
	t.Setenv("TENOR_API_KEY", "test-value-123")
	if r := viewJSON(t, s, "test-skill", ""); r["success"] != true {
		t.Fatalf("result %v", r)
	}
	if !tools.IsEnvPassthrough(nil, "TENOR_API_KEY") {
		t.Fatal("available required env var not registered for passthrough")
	}
}

func TestSkillWithoutEnvVarsRegistersNothing(t *testing.T) {
	cleanPassthrough(t)
	withCapture(t, nil)
	s, skills, _ := viewScope(t)
	makeCmdSkill(t, skills, "simple-skill", "", "", "")
	if r := viewJSON(t, s, "simple-skill", ""); r["success"] != true {
		t.Fatalf("result %v", r)
	}
	if all := tools.AllPassthrough(nil); len(all) != 0 {
		t.Fatalf("passthrough %v", all)
	}
}

func TestAgentMailKeyRegisteredForSandboxWhenSet(t *testing.T) {
	cleanPassthrough(t)
	withCapture(t, nil)
	s, skills, _ := viewScope(t)
	installAgentMail(t, skills)
	t.Setenv("AGENTMAIL_API_KEY", "am_test_key_123")
	if r := viewJSON(t, s, "agentmail", ""); r["success"] != true {
		t.Fatalf("result %v", r)
	}
	if !tools.IsEnvPassthrough(nil, "AGENTMAIL_API_KEY") {
		t.Fatal("AGENTMAIL_API_KEY was set but not registered for sandbox passthrough")
	}
}

func TestAgentMailKeyIsOptionalSoSelfSignupStillWorks(t *testing.T) {
	cleanPassthrough(t)
	withCapture(t, nil)
	s, skills, _ := viewScope(t)
	installAgentMail(t, skills)
	unsetEnv(t, "AGENTMAIL_API_KEY")
	r := viewJSON(t, s, "agentmail", "")
	if r["success"] != true || r["setup_needed"] != false {
		t.Fatalf("result %v", r)
	}
	for _, m := range r["missing_required_environment_variables"].([]any) {
		if m == "AGENTMAIL_API_KEY" {
			t.Fatal("optional AGENTMAIL_API_KEY reported missing")
		}
	}
}
