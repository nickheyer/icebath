package credentials

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/config"
)

func TestCredentialTakesTheFirstSupportedGitHubToken(t *testing.T) {
	t.Setenv(githubcopilotconfig.CopilotTokenEnv, "ghp_classic")
	t.Setenv(githubcopilotconfig.GHTokenEnv, "")
	t.Setenv(githubcopilotconfig.GitHubTokenEnv, " gho_oauth ")
	cfg := config.New(t.TempDir())
	githubcopilotconfig.Register(cfg)
	s := source{config: cfg}

	credential, ok := s.Credential(githubcopilotconfig.CopilotName)
	if !ok || credential.APIKey != "gho_oauth" || credential.Source != githubcopilotconfig.GitHubTokenEnv {
		t.Fatalf("Credential() = %+v, %v, want the trimmed gho_ token from %s past the classic PAT", credential, ok, githubcopilotconfig.GitHubTokenEnv)
	}
	if _, ok := s.Credential(githubcopilotconfig.ACPName); ok {
		t.Fatal("Credential() answered for copilot-acp, whose CLI keeps its own login")
	}

	t.Setenv(githubcopilotconfig.GitHubTokenEnv, "ghp_other")
	t.Setenv(githubcopilotconfig.GHTokenEnv, "plain-token")
	if credential, ok := s.Credential(githubcopilotconfig.CopilotName); ok {
		t.Fatalf("Credential() = %+v, want nothing from unsupported tokens", credential)
	}

	if err := cfg.Set(githubcopilotconfig.CopilotAPIKey, "github_pat_fine"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	credential, ok = s.Credential(githubcopilotconfig.CopilotName)
	if !ok || credential.APIKey != "github_pat_fine" || credential.Source != "config" {
		t.Fatalf("Credential() = %+v, %v, want the configured token when every variable holds an unsupported one", credential, ok)
	}

	t.Setenv(githubcopilotconfig.CopilotTokenEnv, "ghu_app")
	credential, ok = s.Credential(githubcopilotconfig.CopilotName)
	if !ok || credential.APIKey != "ghu_app" || credential.Source != githubcopilotconfig.CopilotTokenEnv {
		t.Fatalf("Credential() = %+v, %v, want a supported variable ahead of the configured token", credential, ok)
	}
}
