package plugins

import ()

const postPlugin = `
import asyncio, json

def _probe(args, **kw):
    h = ctx_ref[0].post_message("first")
    out = {"repr": repr(h), "type": type(h).__name__, "ok": h.ok, "id": h.message_id, "error": h.error, "pending": h.pending}
    out["edit"] = h.edit("second")
    out["after"] = h.result
    async def later():
        h2 = await ctx_ref[0].apost_message("third")
        return {"repr": repr(h2), "aedit": await h2.aedit("fourth")}
    out["async"] = asyncio.run(later())
    return json.dumps(out)

ctx_ref = []

def register(ctx):
    ctx_ref.append(ctx)
    ctx.register_tool(name="postprobe", toolset="postprobe", schema={"name": "postprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=_probe)
`

const spawnPlugin = `
import json

CTX = []

async def _later():
    return CTX[0].post_message("from task").ok

def _probe(args, **kw):
    return json.dumps({"ok": CTX[0].spawn_task(_later()).result(10)})

def register(ctx):
    CTX.append(ctx)
    ctx.register_tool(name="spawnprobe", toolset="spawnprobe", schema={"name": "spawnprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=_probe)
`

type spawnSessionKey struct{}
