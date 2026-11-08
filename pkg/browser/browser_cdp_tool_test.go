package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type cdpMockHandler func(params *pyjson.Object, sessionID string) (any, error)

// In process CDP endpoint answering each request through the handler registered for its method
type cdpMock struct {
	mu       sync.Mutex
	handlers map[string]cdpMockHandler
	msgs     []*pyjson.Object
	url      string
}

func newCDPMock(t *testing.T) *cdpMock {
	t.Helper()
	m := &cdpMock{handlers: map[string]cdpMockHandler{}}
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		for {
			op, payload, err := s.readMessage()
			if err != nil || op == opClose {
				return
			}
			decoded, err := pyjson.LoadsBytes(payload)
			if err != nil {
				return
			}
			msg := decoded.(*pyjson.Object)
			id, _ := msg.Get("id")
			method := objStr(msg, "method")
			sid := objStr(msg, "sessionId")
			m.mu.Lock()
			m.msgs = append(m.msgs, msg)
			fn := m.handlers[method]
			m.mu.Unlock()
			reply := pyjson.ObjectOf("id", id)
			if fn == nil {
				reply.Set("error", pyjson.ObjectOf("code", -32601, "message", "No handler for "+method))
			} else if res, err := fn(objChild(msg, "params"), sid); err != nil {
				reply.Set("error", pyjson.ObjectOf("code", -1, "message", err.Error()))
			} else {
				reply.Set("result", res)
			}
			if sid != "" {
				reply.Set("sessionId", sid)
			}
			text, _ := pyjson.Dumps(reply)
			if s.writeText(text) != nil {
				return
			}
		}
	})
	t.Cleanup(srv.Close)
	m.url = wsURL + "/devtools/browser/mock"
	pin(t, &resolveCDPEndpoint, func(*tools.Env) string { return m.url })
	return m
}

func (m *cdpMock) on(method string, fn cdpMockHandler) {
	m.mu.Lock()
	m.handlers[method] = fn
	m.mu.Unlock()
}

// Requests received for method
func (m *cdpMock) received(method string) []*pyjson.Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*pyjson.Object
	for _, msg := range m.msgs {
		if objStr(msg, "method") == method {
			out = append(out, msg)
		}
	}
	return out
}

func decodeResult(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("bad json %q: %v", text, err)
	}
	return v.(*pyjson.Object)
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		v = anyGet(v, k)
	}
	return v
}

func result(v any) func(*pyjson.Object, string) (any, error) {
	return func(*pyjson.Object, string) (any, error) { return v, nil }
}

func obj(pairs ...any) *pyjson.Object { return pyjson.ObjectOf(pairs...) }

func TestMissingMethodReturnsError(t *testing.T) {
	env := testEnv(t)
	out := decodeResult(t, browserCDP(t.Context(), env, "", nil, nil, nil, 30.0, ""))
	if !strings.Contains(strings.ToLower(objStr(out, "error")), "method") || anyGet(out, "cdp_docs") != cdpDocsURL {
		t.Fatalf("got %v", out)
	}
}

func TestNoEndpointReturnsHelpfulError(t *testing.T) {
	env := testEnv(t)
	pin(t, &resolveCDPEndpoint, func(*tools.Env) string { return "" })
	out := decodeResult(t, browserCDP(t.Context(), env, "Target.getTargets", nil, nil, nil, 30.0, ""))
	if !strings.Contains(objStr(out, "error"), "/browser connect") || anyGet(out, "cdp_docs") != cdpDocsURL {
		t.Fatalf("got %v", out)
	}
}

func TestWebsocketsMissingReturnsError(t *testing.T) {
	env := testEnv(t)
	pin(t, &cdpTransportAvailable, false)
	out := decodeResult(t, browserCDP(t.Context(), env, "Target.getTargets", nil, nil, nil, 30.0, ""))
	if !strings.Contains(strings.ToLower(objStr(out, "error")), "websockets") {
		t.Fatalf("got %v", out)
	}
}

func runCDP(t *testing.T, env *tools.Env, method string, params any, targetID any) *pyjson.Object {
	t.Helper()
	return decodeResult(t, browserCDP(t.Context(), env, method, params, targetID, nil, 30.0, ""))
}

func TestBrowserLevelRedactsSecretResult(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	fakeKey := "sk-" + "CDPSECRETRESULT1234567890"
	m.on("Runtime.evaluate", result(obj("result", obj("type", "string", "value", fakeKey))))
	out := runCDP(t, env, "Runtime.evaluate", nil, nil)
	serialized := dumpsUnicode(out)
	if anyGet(out, "success") != true || strings.Contains(serialized, "CDPSECRETRESULT") ||
		!strings.HasPrefix(dig(out, "result", "result", "value").(string), "sk-") {
		t.Fatalf("got %s", serialized)
	}
}

func TestScreenshotBase64PassesThroughUnredacted(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	shot := "iVBORw0KGgoAAAANSUhEUg+" + "gAAAA" + strings.Repeat("B", 60) + "=="
	m.on("Page.captureScreenshot", result(obj("data", shot)))
	out := runCDP(t, env, "Page.captureScreenshot", nil, nil)
	if anyGet(out, "success") != true || dig(out, "result", "data") != shot {
		t.Fatalf("got %v", dumpsUnicode(out))
	}
}

func TestPrintToPdfBase64PassesThroughUnredacted(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	pdf := "JVBERi0xLjcK/" + "gAAAA" + strings.Repeat("C", 60) + "="
	m.on("Page.printToPDF", result(obj("data", pdf)))
	out := runCDP(t, env, "Page.printToPDF", nil, nil)
	if anyGet(out, "success") != true || dig(out, "result", "data") != pdf {
		t.Fatalf("got %v", dumpsUnicode(out))
	}
}

func TestBinaryPayloadFlagKeepsSecretRedactionOffMethodList(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	fakeKey := "sk-" + "CDPSECRETSTILLREDACTED1234567890"
	m.on("Runtime.evaluate", result(obj("result", obj("type", "string", "value", fakeKey))))
	m.on("Page.captureScreenshot", result(obj("data", "gAAAA"+strings.Repeat("B", 60))))
	if text := dumpsUnicode(runCDP(t, env, "Runtime.evaluate", nil, nil)); strings.Contains(text, "CDPSECRETSTILLREDACTED") {
		t.Fatalf("leaked %s", text)
	}
	if got := dig(runCDP(t, env, "Page.captureScreenshot", nil, nil), "result", "data"); got != "gAAAA"+strings.Repeat("B", 60) {
		t.Fatalf("got %v", got)
	}
}

func TestBinaryPayloadFieldSiblingStringStillRedacted(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	fakeKey := "sk-" + "CDPSECRETSIBLING1234567890"
	shot := "iVBORw0KGgoAAAANSUhEUg+" + "gAAAA" + strings.Repeat("B", 60) + "=="
	m.on("Page.captureScreenshot", result(obj("data", shot, "note", fakeKey)))
	out := runCDP(t, env, "Page.captureScreenshot", nil, nil)
	text := dumpsUnicode(out)
	if anyGet(out, "success") != true || dig(out, "result", "data") != shot || strings.Contains(text, "CDPSECRETSIBLING") ||
		!strings.HasPrefix(dig(out, "result", "note").(string), "sk-") {
		t.Fatalf("got %s", text)
	}
}

func assertPassThrough(t *testing.T, method, field, payload string, extra ...any) {
	t.Helper()
	env := testEnv(t)
	m := newCDPMock(t)
	m.on(method, result(obj(append([]any{field, payload}, extra...)...)))
	out := runCDP(t, env, method, nil, nil)
	if anyGet(out, "success") != true || dig(out, "result", field) != payload {
		t.Fatalf("got %s", dumpsUnicode(out))
	}
}

func assertRedacted(t *testing.T, method string, res *pyjson.Object, marker string) {
	t.Helper()
	env := testEnv(t)
	m := newCDPMock(t)
	m.on(method, result(res))
	out := runCDP(t, env, method, nil, nil)
	if text := dumpsUnicode(out); anyGet(out, "success") != true || strings.Contains(text, marker) {
		t.Fatalf("got %s", text)
	}
}

func TestGetResponseBodyBase64DiscriminatorPassesThrough(t *testing.T) {
	assertPassThrough(t, "Network.getResponseBody", "body", "q9Z7"+"gAAAA"+strings.Repeat("B", 60)+"==", "base64Encoded", true)
}

func TestGetResponseBodyTextDiscriminatorStillRedacts(t *testing.T) {
	fakeKey := "sk-" + "CDPSECRETBODY1234567890"
	assertRedacted(t, "Network.getResponseBody", obj("body", "leak "+fakeKey+" here", "base64Encoded", false), "CDPSECRETBODY")
}

func TestIoReadBase64DiscriminatorPassesThrough(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	chunk := "AAA" + "gAAAA" + strings.Repeat("C", 60) + "="
	m.on("IO.read", result(obj("data", chunk, "base64Encoded", true, "eof", true)))
	out := runCDP(t, env, "IO.read", nil, nil)
	if anyGet(out, "success") != true || dig(out, "result", "data") != chunk || dig(out, "result", "eof") != true {
		t.Fatalf("got %s", dumpsUnicode(out))
	}
}

func TestFetchGetResponseBodyBase64DiscriminatorPassesThrough(t *testing.T) {
	assertPassThrough(t, "Fetch.getResponseBody", "body", "zz7+"+"gAAAA"+strings.Repeat("D", 60)+"==", "base64Encoded", true)
}

func TestRuntimeEvaluateSpoofedBase64FlagStillRedacts(t *testing.T) {
	fakeKey := "sk-" + "CDPSPOOFEDFLAG1234567890"
	assertRedacted(t, "Runtime.evaluate", obj("result", obj("type", "object", "value", obj("base64Encoded", true, "data", fakeKey))), "CDPSPOOFEDFLAG")
}

func TestStreamResourceContentUnflaggedBufferedDataPassesThrough(t *testing.T) {
	assertPassThrough(t, "Network.streamResourceContent", "bufferedData", "Q2FjaGU/"+"gAAAA"+strings.Repeat("E", 60)+"==")
}

func TestGetRequestPostDataFlaggedPassesThrough(t *testing.T) {
	assertPassThrough(t, "Network.getRequestPostData", "postData", "cG9zdA=="+"gAAAA"+strings.Repeat("F", 60)+"=", "base64Encoded", true)
}

func TestGetRequestPostDataUnflaggedStillRedacts(t *testing.T) {
	fakeKey := "sk-" + "CDPPOSTDATASECRET1234567890"
	assertRedacted(t, "Network.getRequestPostData", obj("postData", "leak "+fakeKey+" here", "base64Encoded", false), "CDPPOSTDATASECRET")
}

func TestNestedUnflaggedBinaryPathPassesThrough(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	fakeKey := "sk-" + "CDPNESTEDSECRET1234567890"
	body := "SUNBRQ/" + "gAAAA" + strings.Repeat("G", 60) + "=="
	m.on("CacheStorage.requestCachedResponse", result(obj("response", obj("url", "https://example.test/x", "body", body, "note", fakeKey))))
	out := runCDP(t, env, "CacheStorage.requestCachedResponse", nil, nil)
	if text := dumpsUnicode(out); anyGet(out, "success") != true || dig(out, "result", "response", "body") != body || strings.Contains(text, "CDPNESTEDSECRET") {
		t.Fatalf("got %s", text)
	}
}

const cdpPrivateURL = "http://169.254.169.254/latest/meta-data/"

// Fake transport recording every stateless call
func fakeCDPCalls(t *testing.T, reply *pyjson.Object) *int {
	t.Helper()
	calls := 0
	pin(t, &cdpCallFn, func(_ context.Context, _, _ string, _ any, _ any, _ float64) (*pyjson.Object, error) {
		calls++
		return reply, nil
	})
	return &calls
}

func TestRuntimeEvaluateBlockedWhenCurrentPageIsPrivate(t *testing.T) {
	env := testEnv(t)
	pin(t, &resolveCDPEndpoint, func(*tools.Env) string { return "ws://127.0.0.1:9222/devtools/browser/mock" })
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	pin(t, &currentPagePrivateURL, func(*tools.Env, string, any) string { return cdpPrivateURL })
	calls := fakeCDPCalls(t, obj("result", obj("value", "private data")))
	out := decodeResult(t, browserCDP(t.Context(), env, "Runtime.evaluate", obj("expression", "document.body.innerText"), nil, nil, 30.0, "task-1"))
	errText := objStr(out, "error")
	if !strings.Contains(errText, cdpPrivateURL) || !strings.Contains(errText, "private or internal address") || *calls != 0 {
		t.Fatalf("got %v calls=%d", errText, *calls)
	}
}

// Fake supervisor route recording its calls
func fakeSupervisorRoute(t *testing.T, reply string) *int {
	t.Helper()
	calls := 0
	pin(t, &browserCDPViaSupervisor, func(*tools.Env, string, string, string, any, any) string {
		calls++
		return reply
	})
	return &calls
}

func TestFrameIdRouteBlockedWhenCurrentPageIsPrivate(t *testing.T) {
	env := testEnv(t)
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	pin(t, &currentPagePrivateURL, func(*tools.Env, string, any) string { return cdpPrivateURL })
	calls := fakeSupervisorRoute(t, `{"success": true, "result": {"value": "private data"}}`)
	out := decodeResult(t, browserCDP(t.Context(), env, "Runtime.evaluate", obj("expression", "document.body.innerText"), nil, "frame-1", 30.0, "task-1"))
	errText := objStr(out, "error")
	if !strings.Contains(errText, cdpPrivateURL) || !strings.Contains(errText, "private or internal address") || *calls != 0 {
		t.Fatalf("got %v", errText)
	}
}

func TestFrameIdRouteAllowedWhenPageIsNotPrivate(t *testing.T) {
	env := testEnv(t)
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	pin(t, &currentPagePrivateURL, func(*tools.Env, string, any) string { return "" })
	calls := fakeSupervisorRoute(t, `{"success": true, "result": {"value": "ok"}}`)
	out := decodeResult(t, browserCDP(t.Context(), env, "Runtime.evaluate", obj("expression", "document.title"), nil, "frame-1", 30.0, "task-1"))
	if anyGet(out, "success") != true || *calls != 1 {
		t.Fatalf("got %v calls=%d", out, *calls)
	}
}

func TestPageNavigateToPrivateUrlBlockedBeforeCdp(t *testing.T) {
	env := testEnv(t)
	pin(t, &resolveCDPEndpoint, func(*tools.Env) string { return "ws://127.0.0.1:9222/devtools/browser/mock" })
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	calls := fakeCDPCalls(t, obj("frameId", "f"))
	out := decodeResult(t, browserCDP(t.Context(), env, "Page.navigate", obj("url", cdpPrivateURL), nil, nil, 30.0, "task-1"))
	if !strings.Contains(objStr(out, "error"), cdpPrivateURL) || *calls != 0 {
		t.Fatalf("got %v", out)
	}
}

func targets(infos ...*pyjson.Object) func(*pyjson.Object, string) (any, error) {
	list := make([]any, len(infos))
	for i, v := range infos {
		list[i] = v
	}
	return result(obj("targetInfos", list))
}

func TestPageUrlProbeReadsTheTargetOverTheSameEndpoint(t *testing.T) {
	for _, tc := range []struct {
		pageURL string
		blocked bool
	}{{cdpPrivateURL, true}, {"https://93.184.216.34/", false}} {
		t.Run(tc.pageURL, func(t *testing.T) {
			env := testEnv(t)
			m := newCDPMock(t)
			m.on("Target.getTargets", targets(obj("targetId", "other", "type", "page", "url", "https://example.org/"),
				obj("targetId", "tab-1", "type", "page", "url", tc.pageURL)))
			m.on("Target.attachToTarget", result(obj("sessionId", "s-1")))
			m.on("Runtime.evaluate", result(obj("result", obj("value", "page text"))))
			pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
			out := runCDP(t, env, "Runtime.evaluate", obj("expression", "document.body.innerText"), "tab-1")
			evaluated := m.received("Runtime.evaluate")
			if tc.blocked {
				errText := objStr(out, "error")
				if !strings.Contains(errText, cdpPrivateURL) || !strings.Contains(errText, "private or internal address") || len(evaluated) != 0 {
					t.Fatalf("got %v", out)
				}
			} else if anyGet(out, "success") != true || len(evaluated) != 1 {
				t.Fatalf("got %v", out)
			}
		})
	}
}

const cdpPublicPage = "https://93.184.216.34/"

func TestNonPageTargetFailsClosedWhileAnyPageIsPrivate(t *testing.T) {
	for _, tc := range []struct {
		targetID any
		pageURL  string
		blocked  bool
	}{{"worker-1", cdpPrivateURL, true}, {"bogus-id", cdpPrivateURL, true}, {nil, cdpPrivateURL, true}, {"worker-1", cdpPublicPage, false}} {
		t.Run(cdpPublicPage, func(t *testing.T) {
			env := testEnv(t)
			m := newCDPMock(t)
			m.on("Target.getTargets", targets(obj("targetId", "tab-public", "type", "page", "url", cdpPublicPage),
				obj("targetId", "tab-2", "type", "page", "url", tc.pageURL),
				obj("targetId", "newtab", "type", "page", "url", "chrome://newtab/"),
				obj("targetId", "worker-1", "type", "shared_worker", "url", "")))
			m.on("Target.attachToTarget", result(obj("sessionId", "s-1")))
			m.on("Runtime.evaluate", result(obj("result", obj("value", "page body"))))
			pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
			out := runCDP(t, env, "Runtime.evaluate", obj("expression", "fetch('/').then(r => r.text())"), tc.targetID)
			evaluated := m.received("Runtime.evaluate")
			if tc.blocked {
				if !strings.Contains(objStr(out, "error"), cdpPrivateURL) || len(evaluated) != 0 {
					t.Fatalf("%v: got %v", tc.targetID, out)
				}
			} else if anyGet(out, "success") != true || len(evaluated) != 1 {
				t.Fatalf("%v: got %v", tc.targetID, out)
			}
		})
	}
}

func TestFrameIdIsJudgedByItsOwnTargetNotTheFirstPage(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	m.on("Target.getTargets", targets(obj("targetId", "tab-public", "type", "page", "url", cdpPublicPage),
		obj("targetId", "tab-private", "type", "page", "url", cdpPrivateURL),
		obj("targetId", "frame-1", "type", "iframe", "url", "https://93.184.216.35/embed")))
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	calls := fakeSupervisorRoute(t, `{"success": true}`)
	out := decodeResult(t, browserCDP(t.Context(), env, "Runtime.evaluate", obj("expression", "document.title"), nil, "frame-1", 30.0, ""))
	if !strings.Contains(objStr(out, "error"), cdpPrivateURL) || *calls != 0 {
		t.Fatalf("got %v", out)
	}
}

func TestUnreadableTargetListFailsClosed(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	m.on("Target.getTargets", func(*pyjson.Object, string) (any, error) { return nil, errors.New("Target domain unavailable") })
	m.on("Runtime.evaluate", result(obj("result", obj("value", "page body"))))
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
	out := runCDP(t, env, "Runtime.evaluate", obj("expression", "document.title"), nil)
	if !strings.Contains(objStr(out, "error"), "could not read the browser's targets") || len(m.received("Runtime.evaluate")) != 0 {
		t.Fatalf("got %v", out)
	}
}

func TestTabManagementStaysUsableWhileATabIsPrivate(t *testing.T) {
	for _, tc := range []struct {
		method  string
		params  *pyjson.Object
		allowed bool
	}{
		{"Target.createTarget", obj("url", cdpPublicPage), true},
		{"Target.createTarget", obj("url", "http://127.0.0.1:3000/admin"), false},
		{"Target.createTarget", obj("url", cdpPrivateURL), false},
		{"Target.createTarget", obj("url", "about:blank"), true},
		{"Target.createTarget", obj("url", ""), true},
		{"Target.createTarget", obj(), true},
		{"Target.createTarget", obj("url", "file:///etc/passwd"), false},
		{"Target.createTarget", obj("url", "about:blank@127.0.0.1"), false},
		{"Target.createTarget", obj("url", "data:text/html,<script>fetch('http://127.0.0.1:3000/')</script>"), false},
		{"Target.activateTarget", obj("targetId", "tab-public"), true},
		{"Target.closeTarget", obj("targetId", "tab-public"), true},
		{"Network.getAllCookies", obj(), false},
		{"Browser.setDownloadBehavior", obj("behavior", "allow", "downloadPath", "/srv"), false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			env := testEnv(t)
			m := newCDPMock(t)
			m.on("Target.getTargets", targets(obj("targetId", "tab-public", "type", "page", "url", cdpPublicPage),
				obj("targetId", "tab-local", "type", "page", "url", "http://127.0.0.1:3000/")))
			reply := obj()
			if tc.method == "Target.createTarget" {
				reply = obj("targetId", "new-tab")
			}
			m.on(tc.method, result(reply))
			pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return true })
			out := runCDP(t, env, tc.method, tc.params, nil)
			sent := m.received(tc.method)
			if tc.allowed {
				if anyGet(out, "success") != true || len(sent) != 1 {
					t.Fatalf("%v: got %v", tc.params, out)
				}
			} else if !strings.Contains(objStr(out, "error"), "private or internal address") || len(sent) != 0 {
				t.Fatalf("%v: got %v", tc.params, out)
			}
		})
	}
}

func TestPrivateGuardInactiveDoesNotProbe(t *testing.T) {
	env := testEnv(t)
	m := newCDPMock(t)
	m.on("Runtime.evaluate", result(obj("result", obj("value", "ok"))))
	pin(t, &cdpEvalSSRFGuardActive, func(*tools.Env) bool { return false })
	pin(t, &currentPagePrivateURL, func(*tools.Env, string, any) string {
		t.Fatal("_current_page_private_url must not be probed")
		return ""
	})
	out := decodeResult(t, browserCDP(t.Context(), env, "Runtime.evaluate", obj("expression", "document.title"), nil, nil, 30.0, "task-1"))
	if anyGet(out, "success") != true || dig(out, "result", "result", "value") != "ok" {
		t.Fatalf("got %v", out)
	}
}

func TestCheckFnDoesNotProbeNetwork(t *testing.T) {
	env := testEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("check_fn must not perform network I/O")
	}))
	defer srv.Close()
	pin(t, &browserExecAvailable, func(*tools.Env) bool { return true })
	t.Setenv("BROWSER_CDP_URL", srv.URL)
	if !browserCDPCheck(env) {
		t.Fatal("check closed with a configured CDP url")
	}
}

func TestCheckFnFalseWhenBrowserRequirementsFail(t *testing.T) {
	env := testEnv(t)
	pin(t, &browserExecAvailable, func(*tools.Env) bool { return false })
	t.Setenv("BROWSER_CDP_URL", "ws://localhost:9222/devtools/browser/x")
	if browserCDPCheck(env) {
		t.Fatal("check open without the browser harness")
	}
}

const redactDepthSecret = "sk-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func nest(leaf any, levels int, asDict bool) any {
	for range levels {
		if asDict {
			leaf = obj("x", leaf)
		} else {
			leaf = []any{leaf}
		}
	}
	return leaf
}

func TestHostileDeepPageValueIsCutOffNotRaised(t *testing.T) {
	for _, asDict := range []bool{false, true} {
		blob, err := pyjson.Dumps(redactCDPOutput(nil, nest(redactDepthSecret, 2000, asDict), nil, nil, 0))
		if err != nil || strings.Contains(blob, redactDepthSecret) {
			t.Fatalf("as_dict=%v: err %v leaked=%v", asDict, err, strings.Contains(blob, redactDepthSecret))
		}
	}
}

func TestDeepestDomChromeReturnsIsNotTruncated(t *testing.T) {
	var node any = obj("nodeId", 1, "nodeName", "#text", "nodeValue", "hello")
	for i := range 150 {
		node = obj("nodeId", i+2, "nodeName", "DIV", "children", []any{node})
	}
	want, _ := pyjson.Dumps(node)
	got, _ := pyjson.Dumps(redactCDPOutput(nil, node, nil, nil, 0))
	if got != want {
		t.Fatal("deep DOM tree was altered")
	}
}

// Guard with the policy seams pinned to a local or remote backend and a public page open
func cdpGuard(t *testing.T, local, allowPrivate bool) func(method string, params any) string {
	t.Helper()
	env := testEnv(t)
	pin(t, &policyIsLocalBackend, func(*tools.Env) bool { return local })
	pin(t, &policyAllowPrivateURLs, func(*tools.Env) bool { return allowPrivate })
	pin(t, &policyCheckWebsiteAccess, func(*tools.Env, string) *tools.WebsiteBlock { return nil })
	pin(t, &currentPagePrivateURL, func(*tools.Env, string, any) string { return "" })
	return func(method string, params any) string {
		return browserCDPPrivateGuard(env, method, params, "ws://x", "T")
	}
}

func TestRawCdpKeepsTheMetadataFloorWhenThePrivateGuardIsOff(t *testing.T) {
	for _, tc := range []struct{ local, allowPrivate bool }{{true, false}, {false, true}} {
		guard := cdpGuard(t, tc.local, tc.allowPrivate)
		for _, method := range []string{"Page.navigate", "Target.createTarget", "Network.loadNetworkResource", "Fetch.continueRequest"} {
			if !strings.Contains(guard(method, obj("url", cdpPrivateURL)), "cloud metadata") || guard(method, obj("url", "http://192.168.1.5/")) != "" {
				t.Fatalf("%v %s", tc, method)
			}
		}
		for _, tmpl := range []string{"location.href='%s'", "fetch('%s').then(r => r.text())"} {
			if !strings.Contains(guard("Runtime.evaluate", obj("expression", strings.Replace(tmpl, "%s", cdpPrivateURL, 1))), "cloud metadata") ||
				guard("Runtime.evaluate", obj("expression", strings.Replace(tmpl, "%s", "http://192.168.1.5/", 1))) != "" {
				t.Fatalf("%v %s", tc, tmpl)
			}
		}
	}
}

func TestRawCdpUrlParamsAreScreenedEvenWhenTheOpenPageIsPublic(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		metadata := "http://metadata.google.internal/computeMetadata/v1/"
		calls := []struct {
			method string
			params *pyjson.Object
		}{
			{"Network.loadNetworkResource", obj("frameId", "F", "url", metadata, "options", obj())},
			{"Runtime.callFunctionOn", obj("functionDeclaration", "() => fetch('"+metadata+"')")},
			{"Page.addScriptToEvaluateOnNewDocument", obj("source", "fetch(`"+metadata+"`)")},
		}
		for _, c := range calls {
			if !strings.Contains(guard(c.method, c.params), "cloud metadata") {
				t.Fatalf("local=%v %s", local, c.method)
			}
		}
		if (guard("Network.loadNetworkResource", obj("frameId", "F", "url", "http://127.0.0.1:8080/", "options", obj())) == "") != local {
			t.Fatalf("local=%v loopback", local)
		}
		if guard("Network.loadNetworkResource", obj("frameId", "F", "url", "https://93.184.216.34/", "options", obj())) != "" {
			t.Fatalf("local=%v public", local)
		}
		for _, typed := range []string{metadata, "http://192.168.1.10/"} {
			if guard("Input.insertText", obj("text", typed)) != "" {
				t.Fatalf("local=%v typed %s", local, typed)
			}
		}
	}
}

func TestRawCdpParamsTheCallNeverFetchesAreNotScreened(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		calls := []struct {
			method string
			params *pyjson.Object
		}{
			{"Network.setBlockedURLs", obj("urls", []any{"https://*.doubleclick.net/*", "http://127.0.0.1/*", "http://169.254.169.254/*"})},
			{"Fetch.enable", obj("patterns", []any{obj("urlPattern", "https://*.example.invalid/*")})},
			{"Page.handleJavaScriptDialog", obj("accept", true, "promptText", "http://127.0.0.1:8080/")},
			{"Network.setCookie", obj("name", "a", "value", "b", "url", "http://localhost:3000/")},
		}
		for _, c := range calls {
			if got := guard(c.method, c.params); got != "" {
				t.Fatalf("local=%v %s: %s", local, c.method, got)
			}
		}
		src := obj("nodeId", 1, "name", "src", "value", cdpPrivateURL)
		if !strings.Contains(guard("DOM.setAttributeValue", src), "cloud metadata") {
			t.Fatalf("local=%v src", local)
		}
	}
}

func TestRawCdpReadsBracketedIpv6HostsWhole(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		if guard("Network.loadNetworkResource", obj("frameId", "F", "url", "https://[2606:4700:4700::1111]/dns", "options", obj())) != "" {
			t.Fatalf("local=%v public v6", local)
		}
		mapped := obj("functionDeclaration", "() => fetch('http://[::ffff:a9fe:a9fe]/latest/meta-data/')")
		if !strings.Contains(guard("Runtime.callFunctionOn", mapped), "cloud metadata") {
			t.Fatalf("local=%v mapped", local)
		}
	}
}

func TestRawCdpMetadataFloorReadsSchemeRelativeUrls(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		for _, target := range []string{"//169.254.169.254/latest/meta-data/", "///169.254.169.254/latest/", "\\\\169.254.169.254\\latest",
			"//metadata.google.internal/computeMetadata/v1/", "//2852039166/", "//0xA9FEA9FE/latest/",
			"//0251.0376.0251.0376/", "//169.254.43518/", "//169.254.169.254\x01"} {
			if !strings.Contains(guard("Runtime.evaluate", obj("expression", "location.href='"+target+"'")), "cloud metadata") {
				t.Fatalf("local=%v %q", local, target)
			}
		}
		if !strings.Contains(guard("Runtime.evaluate", obj("expression", "document.body.innerHTML = '<img src=//169.254.169.254/latest/>'")), "cloud metadata") {
			t.Fatalf("local=%v html", local)
		}
		if !strings.Contains(guard("DOM.setAttributeValue", obj("nodeId", 1, "name", "src", "value", "//0xa9fea9fe/latest/")), "cloud metadata") {
			t.Fatalf("local=%v src", local)
		}
		if guard("Runtime.evaluate", obj("expression", "// note: see docs\nconst p = 'a//b'; fetch('https://93.184.216.34//x')")) != "" {
			t.Fatalf("local=%v harmless", local)
		}
		if guard("Runtime.callFunctionOn", obj("functionDeclaration", "function(){ //TODO\n import('//cdn.jsdelivr.net/npm/a.js') }")) != "" {
			t.Fatalf("local=%v script", local)
		}
		for _, u := range []string{"https://1.1.1.1/?next=//169.254.169.254/", "https://[2606:4700:4700::1111]//169.254.169.254/x"} {
			if guard("Network.loadNetworkResource", obj("frameId", "F", "url", u)) != "" {
				t.Fatalf("local=%v %s", local, u)
			}
		}
	}
}

func TestRawCdpReadsTheHostAfterUserinfo(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		for _, userinfo := range []string{"1.1.1.1)", "1.1.1.1]", "1.1.1.1<", "1.1.1.1>", "1.1.1.1)@1.1.1.1]"} {
			for _, prefix := range []string{"http://", "//"} {
				target := prefix + userinfo + "@169.254.169.254/latest/meta-data/"
				if !strings.Contains(guard("Runtime.callFunctionOn", obj("functionDeclaration", "() => fetch('"+target+"')")), "cloud metadata") {
					t.Fatalf("local=%v %s", local, target)
				}
			}
		}
		html := obj("expression", "document.body.innerHTML = '<img src=\"http://1.1.1.1)@169.254.169.254/latest/\">'")
		if !strings.Contains(guard("Runtime.evaluate", html), "(http://169.254.169.254/latest/)") {
			t.Fatalf("local=%v html: %s", local, guard("Runtime.evaluate", html))
		}
		if !strings.Contains(guard("Network.loadNetworkResource", obj("frameId", "F", "url", "http://1.1.1.1]@169.254.169.254/latest/", "options", obj())), "cloud metadata") {
			t.Fatalf("local=%v raw", local)
		}
		if (guard("Runtime.callFunctionOn", obj("functionDeclaration", "() => fetch('http://1.1.1.1)@127.0.0.1:8080/')")) == "") != local {
			t.Fatalf("local=%v loopback", local)
		}
		href := obj("functionDeclaration", "() => el.innerHTML = '<a href=http://127.0.0.1:8080>me@93.184.216.34</a>'")
		if (guard("Runtime.callFunctionOn", href) == "") != local {
			t.Fatalf("local=%v href", local)
		}
		if guard("Network.loadNetworkResource", obj("frameId", "F", "url", "https://93.184.216.34/a)@169.254.169.254/", "options", obj())) != "" {
			t.Fatalf("local=%v path", local)
		}
	}
}

func TestRawCdpSchemeRelativeLiteralsCostNoDnsLookup(t *testing.T) {
	for _, local := range []bool{false, true} {
		guard := cdpGuard(t, local, false)
		var lookups []string
		pin(t, &policyLookupHost, func(host string) ([]string, error) { lookups = append(lookups, host); return nil, nil })
		safe := policyIsSafeURL
		pin(t, &policyIsSafeURL, func(env *tools.Env, u string) bool { lookups = append(lookups, u); return safe(env, u) })
		var script strings.Builder
		for i := range 20 {
			script.WriteString("//step" + strconv.Itoa(i) + " fill the form\n")
		}
		script.WriteString("//eslint-disable-next-line\nrun()")
		if guard("Runtime.evaluate", obj("expression", script.String())) != "" || len(lookups) != 0 {
			t.Fatalf("local=%v lookups %v", local, lookups)
		}
	}
}
