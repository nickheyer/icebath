package credentials

import (
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
)

var prefixes = []string{"gho_", "github_pat_", "ghu_"}

type source struct {
	config *config.Config
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	if name != githubcopilotconfig.CopilotName {
		return credentials.Credential{}, false
	}
	for _, variable := range []string{githubcopilotconfig.CopilotTokenEnv, githubcopilotconfig.GHTokenEnv, githubcopilotconfig.GitHubTokenEnv} {
		if token := strings.TrimSpace(os.Getenv(variable)); supported(token) {
			return credentials.Credential{APIKey: token, Source: variable}, true
		}
	}
	if token := strings.TrimSpace(s.config.GetString(githubcopilotconfig.CopilotAPIKey)); supported(token) {
		return credentials.Credential{APIKey: token, Source: "config"}, true
	}
	return credentials.Credential{}, false
}

func supported(token string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}
