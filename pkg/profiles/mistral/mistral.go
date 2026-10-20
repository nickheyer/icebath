// Mistral endpoint profile with model listing
package mistral

import (
	"context"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/mistral-conversations"
)

const (
	Vendor  = "mistral"
	Name    = "mistral"
	BaseURL = "https://api.mistral.ai"
	KeyEnv  = "MISTRAL_API_KEY"
)

// Profile for the public API authenticated with a bearer token
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.MistralConversations, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv)}
}

func init() {
	profiles.Register(Profile())
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

type modelList struct {
	Data []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Created          int64  `json:"created"`
		MaxContextLength int    `json:"max_context_length"`
		Capabilities     *struct {
			CompletionChat bool `json:"completion_chat"`
		} `json:"capabilities"`
	} `json:"data"`
}

// Lists the models that can chat
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var list modelList
	if err := profiles.GetJSON(ctx, p, "/v1/models", nil, &list); err != nil {
		return nil, err
	}
	var out []profiles.Model
	for _, m := range list.Data {
		if m.Capabilities != nil && !m.Capabilities.CompletionChat {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, profiles.Model{ID: m.ID, Name: name, Context: m.MaxContextLength, Created: m.Created})
	}
	return out, nil
}
