package providers

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type nvidiaProfile struct {
	base
}

func newNvidia() Profile {
	return &nvidiaProfile{base{spec: Spec{
		Name: "nvidia", Aliases: []string{"nvidia-nim", "nim", "build-nvidia", "nemotron"}, EnvVars: []string{"NVIDIA_API_KEY"},
		DisplayName: "NVIDIA NIM", Description: "NVIDIA NIM — accelerated inference", SignupURL: "https://build.nvidia.com/",
		FallbackModels: []string{"nvidia/llama-3.1-nemotron-70b-instruct", "nvidia/llama-3.3-70b-instruct"},
		BaseURL:        "https://integrate.api.nvidia.com/v1", DefaultMaxTokens: 16384,
	}}}
}

func nvidiaNeedsStrip(m *wire.Message) bool {
	if m.Role != wire.RoleTool {
		return false
	}
	if m.Name != "" || m.ToolName != "" {
		return true
	}
	return slices.ContainsFunc(m.Members(), func(mem wire.Member) bool { return mem.Key == "name" || mem.Key == "tool_name" })
}

func (p *nvidiaProfile) PrepareMessages(ctx context.Context, msgs []wire.Message) ([]wire.Message, error) {
	if !slices.ContainsFunc(msgs, func(m wire.Message) bool { return nvidiaNeedsStrip(&m) }) {
		return msgs, nil
	}
	out := make([]wire.Message, len(msgs))
	for i := range msgs {
		if !nvidiaNeedsStrip(&msgs[i]) {
			out[i] = msgs[i]
			continue
		}
		m := msgs[i].Clone()
		m.Name, m.ToolName = "", ""
		out[i] = m
	}
	return out, nil
}
