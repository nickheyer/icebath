// Nebu gateway profile on its OpenAI API, asked through its own headers to keep nothing and add nothing
package nebu

import (
	"net/http"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/openai"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/openai-completions"
)

const (
	Name    = "nebu"
	BaseURL = "http://127.0.0.1:8484/v1"
	KeyEnv  = "NEBU_AUTH_TOKEN"
	// Environment variable overriding the base URL
	BaseURLEnv = "NEBU_BASE_URL"
	// Header asking the gateway to keep no trace, event or seat of a request
	GhostHeader = "X-Nebu-Ghost"
	// Header asking the gateway to attach no tools of its own and run no guards
	BareHeader = "X-Nebu-Bare"
)

// Profile for a gateway at the default address, its models listed the way OpenAI lists them
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: openai.Vendor, Protocol: protocol.OpenAICompletions, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv), BaseURLEnv: BaseURLEnv, Ghost: flag(GhostHeader), Bare: flag(BareHeader)}
}

// Directive heard as the header set to 1
func flag(name string) *provider.Directive {
	return &provider.Directive{Header: http.Header{name: {"1"}}}
}

func init() {
	profiles.Register(Profile())
}
