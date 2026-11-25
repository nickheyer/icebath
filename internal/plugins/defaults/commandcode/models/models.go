package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	commandcodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/commandcode/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct {
	name   string
	prefix string
}

func Register(registry *models.Registry) {
	registry.Register(catalog{name: commandcodeconfig.ChatName})
	registry.Register(catalog{name: commandcodeconfig.AnthropicName, prefix: "claude-"})
}

func (c catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Provider != c.name {
		return nil, false
	}
	data, err := models.GetJSON(ctx, target(route.BaseURL), 8*time.Second, map[string]string{"Accept": "application/json", "User-Agent": "icebath-cli/dev"})
	if err != nil {
		return nil, false
	}
	document, ok := data.(map[string]any)
	if !ok {
		return nil, false
	}
	items, _ := document["data"].([]any)
	ids := []string{}
	for _, item := range items {
		row, _ := item.(map[string]any)
		if id, present := row["id"]; present {
			if name := scalar(id); strings.HasPrefix(name, c.prefix) {
				ids = append(ids, name)
			}
		}
	}
	return ids, true
}

func target(base string) string {
	caller := strings.TrimRight(strings.TrimSpace(base), "/")
	if caller != "" && caller != commandcodeconfig.Endpoint {
		return caller + "/models"
	}
	return commandcodeconfig.ModelsURL
}

func scalar(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}
