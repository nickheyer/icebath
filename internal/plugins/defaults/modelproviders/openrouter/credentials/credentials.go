package credentials

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/credentials"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Prefix every OpenRouter key has
const keyPrefix = "sk-or-"

// Variable of an OpenAI key, which holds an OpenRouter one in setups made for the OpenAI SDK
const openAIKey = "OPENAI_API_KEY"

// An OpenRouter key: the account's, else OPENROUTER_API_KEY when it has the shape of one, else OPENAI_API_KEY when it does
type source struct {
	warned sync.Map
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{})
}

func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != openrouterconfig.Name {
		return credentials.Credential{}, false
	}
	if credential, ok := account.Lookup(providerconfig.APIKeyField); ok {
		return credential, true
	}
	for _, variable := range []string{openrouterconfig.APIKeyEnv, openAIKey} {
		credential, ok := account.Lookup("", variable)
		if !ok {
			continue
		}
		if strings.HasPrefix(credential.APIKey, keyPrefix) {
			return credential, true
		}
		if _, seen := s.warned.LoadOrStore(variable, true); !seen && variable == openrouterconfig.APIKeyEnv {
			slog.Warn(fmt.Sprintf("Ignoring %s for provider %q: value does not match the expected key prefix (%s). Falling back to the next credential "+
				"source. Fix or remove the malformed key to silence this warning.", variable, openrouterconfig.Name, keyPrefix))
		}
	}
	return credentials.Credential{}, false
}
