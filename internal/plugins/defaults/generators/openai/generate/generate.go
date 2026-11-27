package generate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/generation"
	openaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// One picture model of OpenAI as image_gen.openai.model names it, the API model it calls and the quality it asks for
type tier struct {
	id, title, speed, strengths string
	api                         string
	// Quality asked for, none when the model is one the settings named that no tier is
	quality string
}

// GPT Image 2 tiers, which the Codex sign-in draws with too
var tiers = []tier{
	{id: "gpt-image-2-low", title: "GPT Image 2 Low", speed: "About 15 seconds", strengths: "Fast tries at the lowest cost", api: "gpt-image-2", quality: "low"},
	{id: "gpt-image-2-medium", title: "GPT Image 2 Medium", speed: "About 40 seconds", strengths: "Balanced, and the default", api: "gpt-image-2", quality: "medium"},
	{id: "gpt-image-2-high", title: "GPT Image 2 High", speed: "About 2 minutes", strengths: "The most faithful, follows prompts most closely", api: "gpt-image-2", quality: "high"},
}

const defaultTier = "gpt-image-2-medium"

// GPT Image 2 tiers, then every quality of GPT Image 2.5 Flare and Sunburst
var models = func() []tier {
	out := slices.Clone(tiers)
	for _, variant := range []struct{ id, name, speed, strengths string }{
		{"gpt-image-2.5-flare", "Flare", "Fast", "Everyday images and edits"},
		{"gpt-image-2.5-sunburst", "Sunburst", "Slower", "Precise images and edits"},
	} {
		for _, quality := range []string{"auto", "low", "medium", "high", "xhigh", "max"} {
			id := variant.id + "-" + quality
			if quality == "auto" {
				id = variant.id
			}
			out = append(out, tier{id: id, title: fmt.Sprintf("GPT Image 2.5 %s %s", variant.name, strings.ToUpper(quality[:1])+quality[1:]), speed: variant.speed,
				strengths: variant.strengths, api: variant.id, quality: quality})
		}
	}
	return out
}()

// Tier of the list the first of the names names, else one the provider's own setting names that no tier is when passthrough, else the
// default
func pick(list []tier, own string, passthrough bool, names ...string) tier {
	for _, name := range append([]string{own}, names...) {
		if i := slices.IndexFunc(list, func(t tier) bool { return t.id == strings.TrimSpace(name) }); i >= 0 && strings.TrimSpace(name) != "" {
			return list[i]
		}
	}
	if own = strings.TrimSpace(own); passthrough && own != "" {
		return tier{id: own, title: own, api: own}
	}
	return list[slices.IndexFunc(list, func(t tier) bool { return t.id == defaultTier })]
}

// Picker rows of the tiers, every price varying
func rows(list []tier) []generation.Model {
	out := make([]generation.Model, len(list))
	for i, t := range list {
		out[i] = generation.Model{ID: t.id, Title: t.title, Speed: t.speed, Strengths: t.strengths, Price: "Varies"}
	}
	return out
}

type services struct {
	providers *provider.Registry
	creds     *credentials.Registry
}

func Register(registry *generation.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(services{providers, creds})
}

func (s services) Services() []generation.Service {
	edits := func(*config.Config) generation.ImageTakes { return generation.ImageTakes{Edits: true, References: 16} }
	fixed := func(context.Context, *config.Config) string { return defaultTier }
	return []generation.Service{
		{
			Name:  openaiconfig.Name,
			Title: "OpenAI",
			Ready: func(cfg *config.Config) bool { return s.endpoint(cfg).APIKey != "" },
			Setup: generation.Setup{Badge: "paid", Prompt: "OpenAI API key", Link: "https://platform.openai.com/api-keys", Provider: "openai-api"},
			Images: &generation.Images{
				About:    "Images and edits from OpenAI, needs an OpenAI API key",
				Settings: []string{openaiconfig.Model, openaiconfig.Provider, openaiconfig.BaseURL, openaiconfig.KeyEnv},
				Model:    openaiconfig.Model,
				Models:   func(context.Context, *config.Config) []generation.Model { return rows(models) },
				Default:  fixed,
				Takes:    edits,
				Generate: s.draw,
			},
		},
		{
			Name:  openaiconfig.Codex,
			Title: "OpenAI via ChatGPT",
			Ready: func(cfg *config.Config) bool { _, ok := s.codex(cfg); return ok },
			Setup: generation.Setup{Badge: "free", Provider: "openai-codex"},
			Images: &generation.Images{
				About:    "Images from OpenAI with your ChatGPT sign-in, no API key needed",
				Settings: []string{openaiconfig.CodexModel},
				Model:    openaiconfig.CodexModel,
				Models:   func(context.Context, *config.Config) []generation.Model { return rows(tiers) },
				Default:  fixed,
				Takes:    edits,
				Generate: s.drawCodex,
			},
		},
	}
}
