package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Hash checked wheels of the hermes lock closure of fastapi and httpx for the host Python, cached with the pm suite wheels
func routeAuthWheelhouse(t *testing.T, python string) string {
	t.Helper()
	script := "import json, sys, tomllib\nfrom packaging.tags import sys_tags\nfrom packaging.utils import parse_wheel_filename\n" +
		"lock = tomllib.loads(open(sys.argv[1], encoding='utf-8-sig').read())\npk = {p['name']: p for p in lock['package']}\n" +
		"tags = set(sys_tags())\npending, seen, out = ['fastapi', 'httpx'], set(), []\nwhile pending:\n    n = pending.pop()\n" +
		"    if n in seen:\n        continue\n    seen.add(n)\n    pending.extend(d['name'] for d in pk[n].get('dependencies', []))\n" +
		"    choices = [w for w in pk[n].get('wheels', []) if parse_wheel_filename(w['url'].rsplit('/', 1)[1])[3] & tags]\n" +
		"    if choices:\n        out.append([choices[0]['url'], choices[0]['hash']])\nprint(json.dumps(out))"
	cmd := exec.Command(python, "-I", "-c", script, filepath.Join(hermesReferenceTree, "uv.lock"))
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("wheel selection: %v", err)
	}
	var rows [][2]string
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(cacheRoot, "icebath-pm-tests", "wheels")
	if err := os.MkdirAll(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	wheelhouse := filepath.Join(t.TempDir(), "wheelhouse")
	if err := os.MkdirAll(wheelhouse, 0o777); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, row := range rows {
		name := row[0][strings.LastIndex(row[0], "/")+1:]
		cached := filepath.Join(cache, name)
		data, err := os.ReadFile(cached)
		if err != nil {
			resp, err := client.Get(row[0])
			if err != nil {
				t.Fatal(err)
			}
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != row[1] {
			t.Fatalf("%s hash mismatch", name)
		}
		if err := os.WriteFile(cached, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wheelhouse, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return wheelhouse
}

// Interpreter of a uv built offline venv holding fastapi and httpx, the dashboard API stack of the host child
func routeAuthPython(t *testing.T) string {
	t.Helper()
	uv, system := pubUV(t), pubPython(t)
	wheelhouse := routeAuthWheelhouse(t, system)
	venv := filepath.Join(t.TempDir(), "venv")
	env := append(os.Environ(), "UV_CACHE_DIR="+filepath.Join(t.TempDir(), "uv-cache"), "UV_OFFLINE=1", "VIRTUAL_ENV=")
	for _, args := range [][]string{
		{"venv", "--python", system, venv},
		{"pip", "install", "--python", filepath.Join(venv, "bin", "python"), "--offline", "--no-index", "--find-links", wheelhouse, "fastapi", "httpx"},
	} {
		cmd := exec.Command(uv, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("uv %s: %v\n%s", args[0], err, out)
		}
	}
	return filepath.Join(venv, "bin", "python")
}

// Plugin host of a bare home whose child runs under python
func routeAuthHost(t *testing.T, python string, h string) *PluginHost {
	t.Helper()
	installHostEnvHooks()
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	host, err := newPluginHost(NewManager(h))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	return host
}

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

func TestHostAdmissionDoesNotLeakOrComeFromHTTPInput(t *testing.T) {
	python := routeAuthPython(t)
	tmp := t.TempDir()
	api, h := filepath.Join(tmp, "api"), filepath.Join(tmp, "home")
	for _, d := range []string{api, h} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(api, "plugin_api.py"), routeAuthAPI)
	host := routeAuthHost(t, python, h)
	request := func(authenticated bool) string {
		reply, err := host.ASGIRequest(context.Background(), "guard-probe", api, "plugin_api.py", "PUT", "/config", "authenticated=true",
			[][2]string{{"X-Hosted-Http-Authenticated", "true"}}, []byte(`{"authenticated":true}`), time.Minute, authenticated)
		if err != nil {
			t.Fatalf("asgi authenticated=%v: %v", authenticated, err)
		}
		return fmt.Sprint(reply["status"])
	}
	if got := request(true); got != "200" {
		t.Fatalf("admitted request status %s", got)
	}
	for _, marker := range []any{false, nil, "true", 1} {
		if got := routeAuthRawStatus(t, host, api, marker); got != "401" {
			t.Fatalf("authenticated %#v gave status %s, want 401", marker, got)
		}
	}
	if got := request(false); got != "401" {
		t.Fatalf("default admission status %s, want 401", got)
	}
	if got := request(true); got != "200" {
		t.Fatalf("admission after refusals status %s, want 200", got)
	}
}

func TestAdmissionMatchesNormalGuardAndPreservesUnguardedRoutes(t *testing.T) {
	python := routeAuthPython(t)
	root, err := materializeHostChild()
	if err != nil {
		t.Fatal(err)
	}
	script := `
from fastapi import FastAPI, HTTPException
from starlette.requests import Request
from icebath_cli.plugin_host_http_auth import admitted_for_sensitive_route
from icebath_cli import web_server

app = FastAPI()
scope = {"type": "http", "method": "PUT", "path": "/config", "headers": [], "app": app}
assert not admitted_for_sensitive_route(Request(scope))
assert admitted_for_sensitive_route(Request({**scope, "headers": [
    (web_server._SESSION_HEADER_NAME.lower().encode(), web_server._SESSION_TOKEN.encode())]}))
app.state.auth_required = True
assert not admitted_for_sensitive_route(Request(scope))
assert admitted_for_sensitive_route(Request({**scope, "state": {"session": object()}}))
assert admitted_for_sensitive_route(Request({**scope, "state": {"native_http_principal": object()}}))
for marker in (None, False, "true", 1):
    try:
        web_server._require_token(Request({**scope, "state": {"hosted_http_authenticated": marker}}))
    except HTTPException as error:
        assert error.status_code == 401, error.status_code
    else:
        raise AssertionError(f"hosted marker {marker!r} was admitted")
print("guard ok")
`
	cmd := exec.Command(python, "-I", "-c", "import sys; sys.path.insert(0, sys.argv[1]); exec(sys.argv[2])", root, script)
	cmd.Env = append(os.Environ(), "ICEBATH_DASHBOARD_SESSION_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "guard ok" {
		t.Fatalf("child guard: %v\n%s", err, out)
	}
}
