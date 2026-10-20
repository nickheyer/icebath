// OpenAI endpoint profiles for the Responses and Chat Completions APIs with model listing
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/openai-completions"
	_ "github.com/nickheyer/icebath/pkg/protocol/openai-responses"
)

const (
	Vendor   = "openai"
	Name     = "openai"
	ChatName = "openai-chat"
	BaseURL  = "https://api.openai.com/v1"
	KeyEnv   = "OPENAI_API_KEY"
)

// Profile for the Responses API
func Profile() profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.OpenAIResponses, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv)}
}

// Profile for the Chat Completions API
func ChatProfile() profiles.Profile {
	return profiles.Profile{Name: ChatName, Vendor: Vendor, Protocol: protocol.OpenAICompletions, BaseURL: BaseURL, Auth: profiles.AuthBearer, Key: profiles.Env(KeyEnv)}
}

func init() {
	profiles.Register(Profile())
	profiles.Register(ChatProfile())
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

type modelList struct {
	Data []struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// Lists every model the endpoint serves
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var list modelList
	if err := profiles.GetJSON(ctx, p, "/models", nil, &list); err != nil {
		return nil, err
	}
	out := make([]profiles.Model, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, profiles.Model{ID: m.ID, Name: m.ID, Created: m.Created})
	}
	return out, nil
}

// Request extras setting the reasoning effort on the Responses API
func Reasoning(effort string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"reasoning": json.RawMessage(fmt.Sprintf(`{"effort":%q}`, effort))}
}

// Request extras setting the reasoning effort on the Chat Completions API
func ChatReasoning(effort string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"reasoning_effort": json.RawMessage(fmt.Sprintf("%q", effort))}
}

// Headers selecting an organization and project
func Organization(org, project string) http.Header {
	h := http.Header{}
	if org != "" {
		h.Set("OpenAI-Organization", org)
	}
	if project != "" {
		h.Set("OpenAI-Project", project)
	}
	return h
}
