package wire

// Keys read by a chat route that names none, by a local server, and by an aggregator
const (
	DefaultReasoningKeys = KeyReasoningContent
	LocalReasoningKeys   = KeyReasoningContent | KeyReasoning
	AllReasoningKeys     = KeyReasoningContent | KeyReasoning | KeyReasoningDetails
)

// Message member each reasoning key travels under, in the order the keys are tried
func (k ReasoningKeys) Names() []string {
	var names []string
	for _, pair := range [...]struct {
		key  ReasoningKeys
		name string
	}{{KeyReasoningContent, "reasoning_content"}, {KeyReasoning, "reasoning"}, {KeyReasoningDetails, "reasoning_details"}} {
		if k&pair.key != 0 {
			names = append(names, pair.name)
		}
	}
	return names
}

// Key set holding the keys the given message members travel under, unknown names ignored
func ReasoningKeysOf(names ...string) ReasoningKeys {
	var k ReasoningKeys
	for _, name := range names {
		switch name {
		case "reasoning_content":
			k |= KeyReasoningContent
		case "reasoning":
			k |= KeyReasoning
		case "reasoning_details":
			k |= KeyReasoningDetails
		}
	}
	return k
}

// Endpoint family that rejects a tool-call turn replayed without reasoning_content
type EchoRule struct {
	Family         string
	Providers      []string
	LowerProviders []string
	ModelParts     []string
	Hosts          []string
}

// Families that must have reasoning_content echoed back, in the order the first match is taken
func EchoRules() []EchoRule {
	return []EchoRule{
		{Family: "kimi", Providers: []string{"kimi-coding", "kimi-coding-cn"}, Hosts: []string{"api.kimi.com", "moonshot.ai", "moonshot.cn"}},
		{Family: "deepseek", LowerProviders: []string{"deepseek"}, ModelParts: []string{"deepseek"}, Hosts: []string{"api.deepseek.com"}},
		{Family: "mimo", LowerProviders: []string{"xiaomi"}, ModelParts: []string{"mimo"}, Hosts: []string{"api.xiaomimimo.com", "xiaomimimo.com"}},
		{Family: "missingno", ModelParts: []string{"stealth/missingno"}},
	}
}

// Reasoning keys the chat routes of a group of providers and hosts read
type CarrierRoute struct {
	Keys      ReasoningKeys
	Providers []string
	Hosts     []string
}

// Chat routes whose reasoning keys differ from the default, strict hosts first, the first match winning
func CarrierRoutes() []CarrierRoute {
	return []CarrierRoute{
		{Providers: []string{"mistral", "groq", "cerebras", "sambanova"},
			Hosts: []string{"mistral.ai", "groq.com", "cerebras.ai", "sambanova.ai", "hunyuan.cloud.tencent.com"}},
		{Keys: KeyReasoningContent, Providers: []string{"fireworks"}, Hosts: []string{"fireworks.ai"}},
		{Keys: AllReasoningKeys, Providers: []string{"openrouter", "kilocode", "ai-gateway", "portal"},
			Hosts: []string{"openrouter.ai", "kilo.ai", "ai-gateway.vercel.sh", "icebath.app"}},
		{Keys: KeyReasoningContent | KeyReasoningDetails, Providers: []string{"novita", "tencent-tokenhub", "minimax", "minimax-cn"},
			Hosts: []string{"novita.ai", "tokenhub.tencentmaas.com", "tokenhub-intl.tencentcloudmaas.com", "minimax.io", "minimaxi.com"}},
		{Keys: LocalReasoningKeys, Providers: []string{"ollama-cloud", "custom", "lmstudio", "upstage"}, Hosts: []string{"ollama.com", "upstage.ai"}},
	}
}
