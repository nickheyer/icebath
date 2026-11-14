package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	commandCodeBase      = "https://api.commandcode.ai/provider/v1"
	commandCodeModelsURL = commandCodeBase + "/models"
)

type commandCodeProfile struct {
	base
	d         Deps
	anthropic bool
}

func newCommandCode(d Deps, anthropic bool) Profile {
	if anthropic {
		return &commandCodeProfile{d: d, anthropic: true, base: base{spec: Spec{
			Name: "commandcode-anthropic", Aliases: []string{"commandcode-claude"}, Mode: wire.ModeAnthropic,
			EnvVars: []string{"COMMANDCODE_API_KEY", "COMMANDCODE_ANTHROPIC_BASE_URL"}, DisplayName: "CommandCode (Anthropic)",
			Description: "CommandCode — Claude models via Anthropic Messages API", SignupURL: "https://commandcode.ai/",
			BaseURL: commandCodeBase, ModelsURL: commandCodeModelsURL,
			FallbackModels:  []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"},
			DefaultAuxModel: "claude-haiku-4-5-20251001",
		}}}
	}
	return &commandCodeProfile{d: d, base: base{spec: Spec{
		Name: "commandcode", Aliases: []string{"commandcode-chat"}, Mode: wire.ModeChat,
		EnvVars: []string{"COMMANDCODE_API_KEY", "COMMANDCODE_BASE_URL"}, DisplayName: "CommandCode",
		Description: "CommandCode — 20+ models via OpenAI-compatible API", SignupURL: "https://commandcode.ai/",
		BaseURL: commandCodeBase, ModelsURL: commandCodeModelsURL,
		FallbackModels: []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "Qwen/Qwen3.7-Max", "Qwen/Qwen3.6-Plus",
			"moonshotai/Kimi-K2.6", "zai-org/GLM-5.1", "MiniMaxAI/MiniMax-M2.7", "stepfun/Step-3.5-Flash",
			"xiaomi/mimo-v2.5-pro", "google/gemini-3.5-flash", "gpt-5.5"},
		DefaultAuxModel: "deepseek/deepseek-v4-flash",
	}}}
}

func (p *commandCodeProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	callerBase := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	target := commandCodeModelsURL
	if callerBase != "" && callerBase != commandCodeBase {
		target = callerBase + "/models"
	}
	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", p.d.Net.UserAgent())
	data, err := p.d.Net.getJSON(ctx, target, listTimeout(in), header)
	if err != nil {
		slog.Debug(fmt.Sprintf("fetch_models(commandcode): %v", err))
		return nil, false
	}
	models := []string{}
	for _, id := range idsOf(data.Get("data").Elems()) {
		if !p.anthropic || strings.HasPrefix(id, "claude-") {
			models = append(models, id)
		}
	}
	return models, true
}

func (p *commandCodeProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	m := strings.TrimSpace(f.Model)
	if !strings.HasPrefix(strings.ToLower(m), "deepseek/") {
		return Extras{}, nil
	}
	native, ok := p.d.Registry.Get(ctx, "deepseek")
	if !ok {
		return Extras{}, nil
	}
	extender, ok := native.(ArgsExtender)
	if !ok {
		return Extras{}, nil
	}
	_, bare, _ := strings.Cut(m, "/")
	f.Model = bare
	return extender.APIExtras(ctx, f)
}
