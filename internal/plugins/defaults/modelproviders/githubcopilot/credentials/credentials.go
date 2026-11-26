package credentials

import (
	"strings"

	"github.com/nickheyer/icebath/internal/credentials"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

var prefixes = []string{"gho_", "github_pat_", "ghu_"}

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// GitHub token of the account, else of the first variable that holds one Copilot takes
func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != githubcopilotconfig.CopilotName {
		return credentials.Credential{}, false
	}
	if found, ok := credentials.Lookup(account.Fields, providerconfig.APIKeyField); ok && supported(found.APIKey) {
		return found, true
	}
	for _, variable := range []string{githubcopilotconfig.CopilotTokenEnv, githubcopilotconfig.GHTokenEnv, githubcopilotconfig.GitHubTokenEnv} {
		if found, ok := credentials.FromEnv(variable); ok && supported(found.APIKey) {
			return found, true
		}
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
