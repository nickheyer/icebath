package plugins

import (
	"context"
	"fmt"
	"testing"
	"time"
)

const routeAuthAPI = `
import os
from fastapi import APIRouter, Request
from icebath_cli.web_deps import late

router = APIRouter()

@router.put("/config")
async def config(request: Request):
    late("_require_token")(request)
    return {"pid": os.getpid(), "saved": True,
            "credentials": [k for k in request.headers if k in
                {"authorization", "cookie", "x-icebath-session-token", "x-icebath-gateway-ticket"}]}
`

// Status of one asgi call whose authenticated param is an arbitrary wire value
func routeAuthRawStatus(t *testing.T, host *PluginHost, api string, marker any) string {
	t.Helper()
	ctx := context.Background()
	if _, err := host.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := host.call(ctx, "asgi", map[string]any{"plugin": "guard-probe", "dashboard_dir": api, "api_file": "plugin_api.py",
		"method": "PUT", "path": "/config", "query": "authenticated=true", "authenticated": marker,
		"headers": []any{[]any{"X-Hosted-Http-Authenticated", "true"}}, "body": wireEncode([]byte(`{"authenticated":true}`), nil)},
		nil, time.Minute, nil)
	if err != nil {
		t.Fatalf("asgi with authenticated %#v: %v", marker, err)
	}
	reply, _ := raw.(map[string]any)
	return fmt.Sprint(reply["status"])
}
