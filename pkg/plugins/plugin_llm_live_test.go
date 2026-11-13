package plugins

import ()

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
