package plugins

import (
	"encoding/json"
	"testing"
)

const llmProbePlugin = `
import asyncio, json
from agent.plugin_llm import PluginLlmTextInput, PluginLlmTrustError
S = {"type": "object", "properties": {}}

def register(ctx):
    async def probe(args, **kw):
        plain = ctx.llm.complete([{"role": "user", "content": "hi"}], max_tokens=16, purpose="probe")
        structured = await ctx.llm.acomplete_structured(instructions="Extract x", input=[PluginLlmTextInput(text="data")], json_mode=True)
        try:
            ctx.llm.complete([{"role": "user", "content": "hi"}], provider="anthropic")
            denied = None
        except PluginLlmTrustError as exc:
            denied = [isinstance(exc, PermissionError), str(exc)]
        return json.dumps({"awaitable": asyncio.iscoroutinefunction(ctx.llm.acomplete),
                           "sync": asyncio.iscoroutinefunction(ctx.llm.complete),
                           "text": plain.text, "tokens": plain.usage.total_tokens, "provider": plain.provider,
                           "parsed": structured.parsed, "content_type": structured.content_type, "denied": denied})
    ctx.register_tool(name="llm_probe", toolset="llmprobe", schema={"name": "llm_probe",
        "description": "d", "parameters": S}, handler=probe, is_async=True)
`

// test_plugin_host.py LLM facade case: a hosted plugin reaches ctx.llm with coroutine siblings and the trust gate
func TestLiveHostedPluginLlmFacade(t *testing.T) {
	installLiveServices(t)
	client := &fakeAuxClient{response: fakeLlmResponse("{\"x\": 42}", 4, 6), mainP: "openrouter", mainM: "openai/gpt-4o"}
	withClient := *installedServices()
	withClient.PluginLLM = client
	InstallServices(&withClient)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"llmprobe": llmProbePlugin})
	if e := loadedError(m, "llmprobe"); e != "" {
		t.Fatalf("load error %q", e)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "llm_probe", map[string]any{})), &got); err != nil {
		t.Fatal(err)
	}
	denied, _ := got["denied"].([]any)
	if got["awaitable"] != true || got["sync"] != false || got["text"] != "{\"x\": 42}" || got["tokens"] != float64(10) || got["provider"] != "openrouter" ||
		got["content_type"] != "json" || got["parsed"].(map[string]any)["x"] != float64(42) || len(denied) != 2 || denied[0] != true ||
		denied[1] != "Plugin 'llmprobe' cannot override the provider (set plugins.entries.llmprobe.llm.allow_provider_override to true to allow)." {
		t.Fatalf("probe %v", got)
	}
	if len(client.seen) != 2 || client.async[0] || !client.async[1] || *client.seen[0].MaxTokens != 16 {
		t.Fatalf("client calls %v %v", client.seen, client.async)
	}
}
