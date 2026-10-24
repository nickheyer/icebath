// Ollama endpoint profile with model listing and runtime options
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"

	_ "github.com/nickheyer/icebath/pkg/protocol/ollama-chat"
)

const (
	Vendor  = "ollama"
	Name    = "ollama"
	BaseURL = "http://localhost:11434"
	HostEnv = "OLLAMA_HOST"
	// Model used when none is chosen
	DefaultModel = "llama3.2"
)

// Profile for a server at the given base URL, which may omit the scheme
func Profile(baseURL string) profiles.Profile {
	return profiles.Profile{Name: Name, Vendor: Vendor, Protocol: protocol.OllamaChat, BaseURL: Host(baseURL), Auth: profiles.AuthNone, Models: []string{DefaultModel}}
}

// Normalizes an OLLAMA_HOST style value into a base URL
func Host(value string) string {
	value = strings.TrimSuffix(strings.TrimSpace(value), "/")
	if value == "" {
		return BaseURL
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return value
}

func init() {
	profiles.Register(fromEnv(os.Getenv(HostEnv)))
	profiles.RegisterVendor(profiles.Vendor{Name: Vendor, Models: Models})
}

// Profile for the host named by the environment, falling back to the default host when the value is not a usable URL
func fromEnv(value string) profiles.Profile {
	p := Profile(value)
	if err := p.Validate(); err != nil {
		return Profile("")
	}
	return p
}

type tagList struct {
	Models []struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		ModifiedAt time.Time `json:"modified_at"`
		Details    *struct {
			Family        string `json:"family"`
			ParameterSize string `json:"parameter_size"`
		} `json:"details"`
	} `json:"models"`
}

// Lists the models pulled onto the server
func Models(ctx context.Context, p *provider.Provider) ([]profiles.Model, error) {
	var list tagList
	if err := profiles.GetJSON(ctx, p, "/api/tags", nil, &list); err != nil {
		return nil, err
	}
	out := make([]profiles.Model, 0, len(list.Models))
	for _, m := range list.Models {
		model := profiles.Model{ID: m.Name, Name: m.Name}
		if m.Details != nil && m.Details.Family != "" {
			model.Name = strings.TrimSpace(m.Details.Family + " " + m.Details.ParameterSize)
		}
		if !m.ModifiedAt.IsZero() {
			model.Created = m.ModifiedAt.Unix()
		}
		out = append(out, model)
	}
	return out, nil
}

// Request extras sizing the context window the server allocates
func ContextWindow(tokens int) map[string]json.RawMessage {
	return map[string]json.RawMessage{"options": json.RawMessage(fmt.Sprintf(`{"num_ctx":%d}`, tokens))}
}

// Request extras keeping the model loaded after the turn
func KeepAlive(d time.Duration) map[string]json.RawMessage {
	return map[string]json.RawMessage{"keep_alive": json.RawMessage(fmt.Sprintf("%q", d.String()))}
}
