package plugins

import ()

const approvalPlugin = `
import asyncio, dataclasses, json, sys, types

# Stands in for the upstream hermes_cli.approval_transport module the way a hermes test installs a module double
sys.modules.setdefault("hermes_cli", types.ModuleType("hermes_cli"))
_module = types.ModuleType("hermes_cli.approval_transport")
@dataclasses.dataclass(frozen=True)
class ApprovalDecision:
    request_id: str
    request_digest: str
    choice: str
@dataclasses.dataclass(frozen=True)
class ApprovalRequest:
    schema_version: int
    request_id: str
    digest: str
    command: str
    description: str
    pattern_key: str
    pattern_keys: tuple
    surface: str
    timeout_seconds: float
    allowed_choices: tuple
    def respond(self, choice):
        return ApprovalDecision(self.request_id, self.digest, choice)
_module.ApprovalRequest, _module.ApprovalDecision = ApprovalRequest, ApprovalDecision
sys.modules["hermes_cli.approval_transport"] = _module

ERRORS = []
SEEN = []

async def _present(request):
    await asyncio.sleep(0)
    SEEN.append([type(request).__name__, request.command, list(request.allowed_choices), type(request.pattern_keys).__name__, hash(request) is not None])
    return request.respond("session")

def register(ctx):
    for name, fn, exc in (("builtin", _present, ValueError), ("Bad Name!", _present, ValueError), ("ok-name", 42, TypeError)):
        try:
            ctx.register_approval_transport(name, fn)
        except exc as e:
            ERRORS.append([type(e).__name__, str(e)])
    ctx.register_approval_transport("Acme-Approve", _present)
    try:
        ctx.register_approval_transport("acme-approve", _present)
    except ValueError as e:
        ERRORS.append([type(e).__name__, str(e)])
    ctx.register_approval_transport("liar", lambda request: {"request_id": request.request_id, "request_digest": request.digest, "choice": "once"})
    ctx.register_tool(name="approvalprobe", toolset="approvalprobe", schema={"name": "approvalprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=lambda args, **kw: json.dumps({"errors": ERRORS, "seen": SEEN}))
`
