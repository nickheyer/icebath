package provider

// Profiles the bundled nvidia plugin registers
func nvidiaPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "nvidia", keys: []string{"NVIDIA_API_KEY"}, display: "NVIDIA NIM",
		desc: "NVIDIA NIM — accelerated inference", signup: "https://build.nvidia.com/",
		fallbacks: []string{"nvidia/llama-3.1-nemotron-70b-instruct", "nvidia/llama-3.3-70b-instruct"},
		base:      "https://integrate.api.nvidia.com/v1", maxTokens: 16384, authType: AuthTypeAPIKey},
		"nvidia-nim", "nim", "build-nvidia", "nemotron")
	p.PrepareMessagesHook = nvidiaPrepareMessages
	return []Profile{p}
}

// Tool message carrying a name or tool_name field the NIM schema rejects
func nvidiaNeedsStrip(msg map[string]any) bool {
	if msg == nil || msg["role"] != "tool" {
		return false
	}
	_, name := msg["name"]
	_, toolName := msg["tool_name"]
	return name || toolName
}

// Tool messages without name and tool_name, copying only the stripped ones and returning untouched input as is
func nvidiaPrepareMessages(messages []map[string]any) []map[string]any {
	needs := false
	for _, m := range messages {
		if nvidiaNeedsStrip(m) {
			needs = true
			break
		}
	}
	if !needs {
		return messages
	}
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		if !nvidiaNeedsStrip(m) {
			out[i] = m
			continue
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k != "name" && k != "tool_name" {
				c[k] = v
			}
		}
		out[i] = c
	}
	return out
}
