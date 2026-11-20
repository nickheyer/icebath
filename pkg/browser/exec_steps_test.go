package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBrowser is a DevTools endpoint with tabs, a scripted page and a record of every command it was sent
type fakeBrowser struct {
	mu    sync.Mutex
	url   string
	tabs  []TargetInfo
	calls []string
	conns int
}

func (b *fakeBrowser) record(method string, params json.RawMessage) {
	b.mu.Lock()
	b.calls = append(b.calls, method+" "+string(params))
	b.mu.Unlock()
}

func (b *fakeBrowser) sent(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, call := range b.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

// The value the scripted page gives an expression, an exception description when it throws
func (b *fakeBrowser) evaluate(expr string) (any, string) {
	switch {
	case expr == "document.readyState":
		return "complete", ""
	case strings.Contains(expr, "location.href"):
		return `{"url":"https://shop.example/","title":"Shop","w":1280,"h":720}`, ""
	case expr == "document.visibilityState":
		return "hidden", ""
	case expr == "return 7":
		return nil, "SyntaxError: Illegal return statement"
	case expr == "(function(){return 7})()":
		return 7, ""
	case expr == "boom()":
		return nil, "ReferenceError: boom is not defined"
	case strings.Contains(expr, "e.focus()"):
		return true, ""
	case strings.Contains(expr, "document.title"):
		return nil, ""
	}
	return expr, ""
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	b := &fakeBrowser{tabs: []TargetInfo{{TargetID: "T1", Type: "page", Title: "Start", URL: "https://start.example/"}}}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 40, 20)), nil); err != nil {
		t.Fatal(err)
	}
	shot := base64.StdEncoding.EncodeToString(buf.Bytes())
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		b.mu.Lock()
		b.conns++
		b.mu.Unlock()
		for {
			op, payload, err := s.readMessage()
			if err != nil || op == opClose {
				return
			}
			var req struct {
				ID        int64           `json:"id"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
				SessionID string          `json:"sessionId"`
			}
			if json.Unmarshal(payload, &req) != nil {
				continue
			}
			b.record(req.Method, req.Params)
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			var result any = map[string]any{}
			switch req.Method {
			case "Target.getTargets":
				b.mu.Lock()
				result = map[string]any{"targetInfos": append([]TargetInfo(nil), b.tabs...)}
				b.mu.Unlock()
			case "Target.createTarget":
				b.mu.Lock()
				id := fmt.Sprintf("T%d", len(b.tabs)+1)
				b.tabs = append(b.tabs, TargetInfo{TargetID: id, Type: "page", URL: "about:blank"})
				b.mu.Unlock()
				result = map[string]any{"targetId": id}
			case "Target.getTargetInfo":
				b.mu.Lock()
				for _, tab := range b.tabs {
					if tab.TargetID == p["targetId"] {
						result = map[string]any{"targetInfo": tab}
					}
				}
				b.mu.Unlock()
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S-" + p["targetId"].(string)}
			case "Page.navigate":
				b.mu.Lock()
				for i := range b.tabs {
					if "S-"+b.tabs[i].TargetID == req.SessionID {
						b.tabs[i].URL = p["url"].(string)
					}
				}
				b.mu.Unlock()
				result = map[string]any{"frameId": "F1"}
			case "Runtime.evaluate":
				value, thrown := b.evaluate(p["expression"].(string))
				if p["expression"] == "0/0" {
					result = map[string]any{"result": map[string]any{"type": "number", "unserializableValue": "NaN", "description": "NaN"}}
				} else if thrown != "" {
					result = map[string]any{"result": map[string]any{"type": "object", "subtype": "error", "description": thrown},
						"exceptionDetails": map[string]any{"text": "Uncaught", "lineNumber": 0, "columnNumber": 1}}
				} else {
					result = map[string]any{"result": map[string]any{"value": value}}
				}
			case "Browser.getVersion":
				result = map[string]any{"userAgent": "Mozilla/5.0 (X11; Linux x86_64) Chrome/140"}
			case "Page.captureScreenshot":
				if p["format"] == "png" {
					continue
				}
				result = map[string]any{"data": shot}
			case "DOM.getDocument":
				result = map[string]any{"root": map[string]any{"nodeId": 1}}
			case "DOM.querySelector":
				result = map[string]any{"nodeId": 9}
			case "Fake.dialog":
				_ = s.writeText(`{"method":"Page.javascriptDialogOpening","sessionId":"` + req.SessionID + `","params":{"type":"confirm","message":"Leave?"}}`)
				result = map[string]any{"n": 41}
			}
			reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
			_ = s.writeText(string(reply))
		}
	})
	t.Cleanup(srv.Close)
	b.url = wsURL
	return b
}

// Runs code under one environment and interpreter for the whole test, its sessions closed when the test ends
func codeRunner(t *testing.T) func(route map[string]string, workspace, code string, ownTab bool) execProc {
	t.Helper()
	run := timedCodeRunner(t)
	return func(route map[string]string, workspace, code string, ownTab bool) execProc {
		return run(route, workspace, code, ownTab, 20)
	}
}

// Runs code with its own timeout under one environment and interpreter for the whole test
func timedCodeRunner(t *testing.T) func(route map[string]string, workspace, code string, ownTab bool, timeout int) execProc {
	t.Helper()
	env := testEnv(t)
	python, err := execPython(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeExecSessions("") })
	return func(route map[string]string, workspace, code string, ownTab bool, timeout int) execProc {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return execRunCode(ctx, env, execRun{python: python, childEnv: execChildEnv(env, route, workspace), route: route, code: code,
			workspace: workspace, timeout: timeout, activate: route[cloudBrowserSentinel] == activateAlways, ownTab: ownTab})
	}
}

func TestExecHelpersDriveTheBrowserOverDevTools(t *testing.T) {
	b := newFakeBrowser(t)
	pin(t, &pngDeadline, 50*time.Millisecond)
	shots := t.TempDir()
	t.Setenv("BH_TMP_DIR", shots)
	workspace := t.TempDir()
	code := `# Driving the fake shop
print(new_tab("https://shop.example/"))
print(wait_for_load(timeout=2))
print(json.dumps(page_info(), sort_keys=True))
print(js("return 7"))
click_at_xy(10, 20)
fill_input("#q", "Ab")
print(capture_screenshot(max_dim=10))
print(cdp("Fake.dialog")["n"] + 1)
print(json.dumps(page_info(), sort_keys=True))
upload_file("input[type=file]", "/tmp/a.txt")
print([t["targetId"] for t in list_tabs()])
print(js("0/0") != js("0/0"))
try:
    js("boom()")
except RuntimeError as e:
    print("raised", e)
try:
    capture_screenshot("/no/such/dir/x.png")
except FileNotFoundError as e:
    print("missing", e.filename)
`
	out := codeRunner(t)(map[string]string{"BU_CDP_WS": b.url, cloudBrowserSentinel: activateAlways}, workspace, code, false)
	shot := filepath.Join(shots, "shot.png")
	want := strings.Join([]string{"T2", "True", `{"h": 720, "title": "Shop", "url": "https://shop.example/", "w": 1280}`, "7",
		shot, "42", `{"dialog": {"message": "Leave?", "type": "confirm"}}`, "['T1', 'T2']", "True",
		"raised JavaScript evaluation failed at line 0, column 1: ReferenceError: boom is not defined; expression: boom()",
		"missing /no/such/dir/x.png"}, "\n") + "\n"
	if out.returnCode != 0 || out.stdout != want {
		t.Fatalf("exit %d\nstdout\n%s\nstderr\n%s", out.returnCode, out.stdout, out.stderr)
	}
	data, err := os.ReadFile(shot)
	if cfg, format, derr := image.DecodeConfig(bytes.NewReader(data)); err != nil || derr != nil || format != "png" || cfg.Width != 10 {
		t.Errorf("screenshot %v %v %s %+v", err, derr, format, cfg)
	}
	if got := b.sent("Target.activateTarget"); len(got) != 3 || !strings.Contains(got[0], "T2") || !strings.Contains(got[2], "T2") {
		t.Errorf("activations %v", got)
	}
	if got := b.sent("Page.captureScreenshot"); len(got) != 4 || !strings.Contains(got[0], `"png"`) || !strings.Contains(got[1], `"quality":90`) {
		t.Errorf("captures %v", got)
	}
	keys := b.sent("Input.dispatchKeyEvent")
	var typed []string
	for _, call := range keys {
		var p struct {
			Type      string `json:"type"`
			Key       string `json:"key"`
			Code      string `json:"code"`
			Modifiers int    `json:"modifiers"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(call, "Input.dispatchKeyEvent ")), &p)
		typed = append(typed, fmt.Sprintf("%s %s %s %d", p.Type, p.Key, p.Code, p.Modifiers))
	}
	wantKeys := []string{"rawKeyDown a KeyA 2", "keyUp a KeyA 2", "keyDown Backspace Backspace 0", "keyUp Backspace Backspace 0",
		"keyDown A KeyA 8", "char A KeyA 8", "keyUp A KeyA 8", "keyDown b KeyB 0", "char b KeyB 0", "keyUp b KeyB 0"}
	if strings.Join(typed, "|") != strings.Join(wantKeys, "|") {
		t.Errorf("keys %v", typed)
	}
	if got := b.sent("Input.dispatchMouseEvent"); len(got) != 2 || !strings.Contains(got[0], `"x":10`) || !strings.Contains(got[0], `"clickCount":1`) {
		t.Errorf("clicks %v", got)
	}
	if got := b.sent("DOM.setFileInputFiles"); len(got) != 1 || !strings.Contains(got[0], `["/tmp/a.txt"]`) || !strings.Contains(got[0], `"nodeId":9`) {
		t.Errorf("upload %v", got)
	}
}

func TestExecHelpersKeepANamedSessionOnItsOwnTabAcrossCalls(t *testing.T) {
	b := newFakeBrowser(t)
	route := map[string]string{"BU_CDP_WS": b.url, "BU_NAME": "r7k2"}
	run := codeRunner(t)
	code := `print(current_tab()["targetId"], NAME)`
	first := run(route, t.TempDir(), code, true)
	second := run(route, t.TempDir(), code, true)
	shared := run(map[string]string{"BU_CDP_WS": b.url}, t.TempDir(), code, false)
	if first.stdout != "T2 r7k2\n" || first.stdout != second.stdout || shared.stdout != "T1 default\n" {
		t.Fatalf("named %q then %q, shared %q", first.stdout+first.stderr, second.stdout, shared.stdout+shared.stderr)
	}
	if created := b.sent("Target.createTarget"); len(created) != 1 || b.conns != 2 {
		t.Fatalf("created %v over %d connections", created, b.conns)
	}
}

func TestExecHelpersFollowTheRouteToANewEndpointAndStopOnTimeout(t *testing.T) {
	a, b := newFakeBrowser(t), newFakeBrowser(t)
	timed := timedCodeRunner(t)
	run := func(route map[string]string, workspace, code string, ownTab bool) execProc {
		return timed(route, workspace, code, ownTab, 20)
	}
	for _, browser := range []*fakeBrowser{a, b, a} {
		if out := run(map[string]string{"BU_CDP_WS": browser.url}, t.TempDir(), `goto_url("https://x.example/")`, false); out.returnCode != 0 {
			t.Fatal(out.stderr)
		}
	}
	if len(a.sent("Page.navigate")) != 2 || len(b.sent("Page.navigate")) != 1 || a.conns != 2 || b.conns != 1 {
		t.Fatalf("navigations %d and %d over %d and %d connections", len(a.sent("Page.navigate")), len(b.sent("Page.navigate")), a.conns, b.conns)
	}
	started := time.Now()
	out := timed(map[string]string{"BU_CDP_WS": a.url}, "", "print(page_info()['title'], flush=True)\nwait(30)", false, 2)
	if !out.timedOut || out.stdout != "Shop\n" || time.Since(started) > 15*time.Second {
		t.Fatalf("%+v after %s", out, time.Since(started))
	}
	if out := run(map[string]string{}, t.TempDir(), "print(1)", false); out.returnCode != 1 || !strings.Contains(out.stderr, "no DevTools endpoint") {
		t.Fatalf("%+v", out)
	}
	if out := run(map[string]string{"BU_CDP_WS": a.url}, t.TempDir(), "raise SystemExit(3)", false); out.returnCode != 3 {
		t.Fatalf("%+v", out)
	}
	h := &helperRun{ctx: context.Background(), s: &execSession{}}
	if reply := string(h.answer([]byte(`{"helper":"fly","args":{}}`))); reply != `{"error":"'fly' is not a browser helper","type":"RuntimeError"}` {
		t.Fatalf("reply %s", reply)
	}
}
