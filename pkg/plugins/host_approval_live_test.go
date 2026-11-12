package plugins

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

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

func TestLiveInProcessPluginApprovalTransportPresentsRestoredRequests(t *testing.T) {
	installLiveServices(t)
	h, m := liveHomeIsolated(t, liveInterpreters(t)["system"], map[string]string{"approvalprobe": approvalPlugin}, IsolationInProcess)
	if e := loadedError(m, "approvalprobe"); e != "" {
		t.Fatalf("approvalprobe failed to load: %s", e)
	}
	m.mu.RLock()
	acme, liar := m.approvalTransports["acme-approve"], m.approvalTransports["liar"]
	m.mu.RUnlock()
	if acme == nil || liar == nil || acme.PluginID != "approvalprobe" {
		t.Fatalf("transports %+v %+v", acme, liar)
	}
	req := tools.ApprovalRequest{SchemaVersion: 1, RequestID: "r1", Digest: "d1", Command: "rm -rf build", Description: "delete",
		PatternKey: "rm", PatternKeys: []string{"rm"}, Surface: "cli", TimeoutSeconds: 300, AllowedChoices: []string{"once", "session", "deny"}}
	decision, err := acme.Present(context.Background(), req)
	if err != nil || decision != (tools.ApprovalTransportDecision{RequestID: "r1", RequestDigest: "d1", Choice: "session"}) {
		t.Fatalf("acme decision = %+v %v", decision, err)
	}
	if decision, err := liar.Present(context.Background(), req); err != nil || decision != (tools.ApprovalTransportDecision{}) {
		t.Fatalf("a dict answer was taken as a decision: %+v %v", decision, err)
	}
	var got struct {
		Errors [][]string `json:"errors"`
		Seen   [][]any    `json:"seen"`
	}
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "approvalprobe", nil)), &got); err != nil {
		t.Fatal(err)
	}
	wantErrors := [][]string{{"ValueError", "approval transport name 'builtin' is reserved"},
		{"ValueError", "approval transport name must match [a-z0-9][a-z0-9_-]{0,63}"},
		{"TypeError", "approval transport present_fn must be callable"},
		{"ValueError", "approval transport 'acme-approve' is already registered by 'approvalprobe'"}}
	if len(got.Errors) != len(wantErrors) {
		t.Fatalf("errors = %v", got.Errors)
	}
	for i, want := range wantErrors {
		if got.Errors[i][0] != want[0] || got.Errors[i][1] != want[1] {
			t.Fatalf("errors = %v", got.Errors)
		}
	}
	if len(got.Seen) != 1 || got.Seen[0][0] != "ApprovalRequest" || got.Seen[0][1] != "rm -rf build" || got.Seen[0][3] != "tuple" || got.Seen[0][4] != true {
		t.Fatalf("seen = %v", got.Seen)
	}
}
