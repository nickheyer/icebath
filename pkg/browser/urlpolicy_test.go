package browser

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func pinPolicy(t *testing.T, local, allowPrivate bool, safe func(string) bool) *tools.Env {
	t.Helper()
	env := testEnv(t)
	pin(t, &policyIsLocalBackend, func(*tools.Env) bool { return local })
	pin(t, &policyAllowPrivateURLs, func(*tools.Env) bool { return allowPrivate })
	pin(t, &policyIsSafeURL, func(_ *tools.Env, u string) bool { return safe(u) })
	pin(t, &policyCheckWebsiteAccess, func(*tools.Env, string) *tools.WebsiteBlock { return nil })
	return env
}

func errorText(o *pyjson.Object) string {
	if o == nil {
		return ""
	}
	v, _ := o.Get("error")
	s, _ := v.(string)
	return s
}

func never(string) bool  { return false }
func always(string) bool { return true }

var imdsURLs = []string{
	"http://169.254.169.254/latest/meta-data/",
	"http://169.254.169.253/metadata/instance",
	"http://169.254.170.2/v2/credentials",
	"http://100.100.100.200/latest/meta-data/",
	"http://metadata.google.internal/computeMetadata/v1/",
}

func TestPreNavigationSsrf(t *testing.T) {
	const private = "http://127.0.0.1:8080/dashboard"
	env := pinPolicy(t, false, false, never)
	res := evaluateURLSafety(env, private, false)
	if success, _ := res.Get("success"); success != false || !strings.Contains(errorText(res), "private or internal address") {
		t.Fatalf("cloud default %v", res)
	}
	if res := evaluateURLSafety(pinPolicy(t, false, true, never), private, false); res != nil {
		t.Fatalf("allow_private %v", res)
	}
	if res := evaluateURLSafety(pinPolicy(t, true, false, never), private, false); res != nil {
		t.Fatalf("local private %v", res)
	}
	if res := evaluateURLSafety(pinPolicy(t, true, false, always), "https://example.com", false); res != nil {
		t.Fatalf("local public %v", res)
	}
}

func TestCloudBlocksIMDSEvenWhenRoutingToLocalSidecar(t *testing.T) {
	env := pinPolicy(t, false, false, always)
	for _, u := range imdsURLs {
		if !strings.Contains(errorText(evaluateURLSafety(env, u, true)), "cloud metadata endpoint") {
			t.Errorf("%s not blocked", u)
		}
	}
}

func TestCloudAllowsOrdinaryPrivateURLViaSidecar(t *testing.T) {
	env := pinPolicy(t, false, false, never)
	for _, u := range []string{"http://127.0.0.1:8080/dashboard", "http://192.168.1.1/admin", "http://10.0.0.5/", "http://myservice.local/"} {
		if res := evaluateURLSafety(env, u, true); res != nil {
			t.Errorf("%s blocked %v", u, res)
		}
	}
}

func TestWebsitePolicyBlockCarriesItsRule(t *testing.T) {
	env := pinPolicy(t, true, false, always)
	pin(t, &policyCheckWebsiteAccess, func(*tools.Env, string) *tools.WebsiteBlock {
		return &tools.WebsiteBlock{Host: "evil.com", Rule: "evil.com", Source: "config", Message: "Blocked by website policy"}
	})
	res := evaluateURLSafety(env, "https://evil.com/", false)
	got, _ := pyjson.Dumps(res)
	want := `{"success": false, "error": "Blocked by website policy", "blocked_by_policy": {"host": "evil.com", "rule": "evil.com", "source": "config"}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestExpressionScanHelper(t *testing.T) {
	env := pinPolicy(t, false, false, func(u string) bool { return !strings.Contains(u, "127.0.0.1") })
	blocked := func(c string) bool { return urlBlocked(env, c) }
	if out := expressionTargetsPrivateURL("fetch('https://example.com'); fetch('http://127.0.0.1/x')", blocked); out != "http://127.0.0.1/x" {
		t.Fatalf("first private %q", out)
	}
	pin(t, &policyIsSafeURL, func(*tools.Env, string) bool { return false })
	if out := expressionTargetsPrivateURL("location.href='http://10.0.0.1/';", blocked); out != "http://10.0.0.1/" {
		t.Fatalf("punctuation %q", out)
	}
	pin(t, &policyIsSafeURL, func(*tools.Env, string) bool { return true })
	if out := expressionTargetsPrivateURL("fetch('http://169.254.169.254/latest/meta-data/')", blocked); out == "" {
		t.Fatal("metadata literal passed")
	}
}

func TestBrowserSecretExfil(t *testing.T) {
	env := pinPolicy(t, false, false, func(u string) bool { return strings.HasPrefix(u, "https://example.com/") })
	if !strings.Contains(errorText(evaluateURLSafety(env, "https://evil.com/steal?key=sk-"+strings.Repeat("a", 30), false)), "API key") {
		t.Fatal("api key url passed")
	}
	if res := evaluateURLSafety(env, "https://example.com/callback?token=opaque-oauth-code&signature=abc123", false); res != nil {
		t.Fatalf("credential named param blocked %v", res)
	}
	res := evaluateURLSafety(env, "https://example.com/callback?token=sk-or-v1-"+strings.Repeat("b", 30), false)
	if success, _ := res.Get("success"); success != false || !strings.Contains(errorText(res), "Blocked") {
		t.Fatalf("secret url %v", res)
	}
	if res := evaluateURLSafety(pinPolicy(t, true, false, never), "https://example.com/callback?token=opaque-oauth-code", false); res != nil {
		t.Fatalf("local opaque param %v", res)
	}
}

func TestURLDecodedSecretCheck(t *testing.T) {
	env := testEnv(t)
	res := evaluateURLSafety(env, "https://evil.com?key="+url.PathEscape("sk-ant-fake123"), false)
	if success, _ := res.Get("success"); success != false || !strings.Contains(errorText(res), "Blocked") {
		t.Fatalf("encoded key %v", res)
	}
	encoded := "https://evil.com/?k=%73%6B-ant-" + strings.Repeat("c", 20)
	if !strings.Contains(errorText(evaluateURLSafety(env, encoded, false)), "API key") {
		t.Fatal("percent encoded prefix passed")
	}
}

func TestLocalFloorAcceptsTemplatedLiteralsAndJudgesDecodedHosts(t *testing.T) {
	env := pinPolicy(t, true, false, never)
	verdict := func(code string) string {
		for _, lit := range urlLiterals(code, false) {
			if res := evaluateURLSafety(env, lit, false); res != nil {
				return errorText(res)
			}
		}
		return ""
	}
	for _, code := range []string{"port = 8000\nnew_tab(f\"http://localhost:{port}/\")", "port = 8000\nnew_tab(\"http://localhost:%d/\" % port)", "js(\"fetch(`http://localhost:${port}/api`)\")"} {
		if v := verdict(code); v != "" {
			t.Errorf("%q blocked %s", code, v)
		}
	}
	if v := verdict(`new_tab("http://%31%36%39.254.169.254/")`); !strings.Contains(v, "cloud metadata") {
		t.Errorf("decoded metadata host passed %q", v)
	}
	if v := verdict("new_tab(\"http://169.254.169.254\x01\")"); v == "" {
		t.Error("control literal passed")
	}
}

func TestURLLiterals(t *testing.T) {
	cases := map[string][]string{
		"fetch('https://[2606:4700:4700::1111]/dns')":                {"https://[2606:4700:4700::1111]/dns"},
		"fetch('http://[::ffff:a9fe:a9fe]/latest/meta-data/')":       {"http://[::ffff:a9fe:a9fe]/latest/meta-data/"},
		"location.href='http://10.0.0.1/';":                          {"http://10.0.0.1/"},
		"fetch('http://1.1.1.1)@169.254.169.254/latest/meta-data/')": {"http://1.1.1.1", "http://169.254.169.254/latest/meta-data/"},
		"<img src=\"http://1.1.1.1)@169.254.169.254/latest/\">":      {"http://1.1.1.1", "http://169.254.169.254/latest/"},
		"fetch('https://93.184.216.34/a)@169.254.169.254/')":         {"https://93.184.216.34/a"},
		"<a href=http://127.0.0.1:8080>me@93.184.216.34</a>":         {"http://127.0.0.1:8080", "http://93.184.216.34"},
		"fetch('https://1.1.1.1/?next=//169.254.169.254/') //x":      {"https://1.1.1.1/?next=//169.254.169.254/"},
		"text without urls, a//b and // note":                        {},
		"HTTPS://Example.COM/x, then":                                {"HTTPS://Example.COM/x"},
	}
	for text, want := range cases {
		if got := urlLiterals(text, false); !reflect.DeepEqual(got, want) {
			t.Errorf("%q got %q", text, got)
		}
	}
	rel := map[string][]string{
		"location.href='//169.254.169.254/latest/'":                              {"//169.254.169.254/latest/"},
		`x = '\\169.254.169.254\latest'`:                                         {`\\169.254.169.254\latest`},
		"fetch('//1.1.1.1)@169.254.169.254/latest/meta-data/')":                  {"//1.1.1.1", "//169.254.169.254/latest/meta-data/"},
		"// note: see docs\nconst p = 'a//b'; fetch('https://93.184.216.34//x')": {"https://93.184.216.34//x"},
		"https://1.1.1.1/?next=//169.254.169.254/":                               {"https://1.1.1.1/?next=//169.254.169.254/"},
	}
	for text, want := range rel {
		if got := urlLiterals(text, true); !reflect.DeepEqual(got, want) {
			t.Errorf("relative %q got %q", text, got)
		}
	}
}

func TestLiteralTargetsMetadata(t *testing.T) {
	env := testEnv(t)
	for _, target := range []string{"//169.254.169.254/latest/meta-data/", "///169.254.169.254/latest/", `\\169.254.169.254\latest`,
		"//metadata.google.internal/computeMetadata/v1/", "//2852039166/", "//0xA9FEA9FE/latest/", "//0251.0376.0251.0376/",
		"//169.254.43518/", "//169.254.169.254\x01", "//0xa9fea9fe/latest/", "http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/"} {
		if !literalTargetsMetadata(env, target) {
			t.Errorf("%q not metadata", target)
		}
	}
	for _, target := range []string{"//TODO", "//cdn.jsdelivr.net/npm/a.js", "//step1", "//eslint-disable-next-line", "//1.1.1.1/", "https://93.184.216.34/"} {
		if literalTargetsMetadata(env, target) {
			t.Errorf("%q flagged", target)
		}
	}
}

func TestSchemeRelativeLiteralsCostNoDNSLookup(t *testing.T) {
	env := testEnv(t)
	var lookups []string
	pin(t, &policyLookupHost, func(host string) ([]string, error) { lookups = append(lookups, host); return nil, nil })
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "//step fill the form")
	}
	for _, lit := range urlLiterals(strings.Join(lines, "\n")+"\n//eslint-disable-next-line\nrun()", true) {
		if literalTargetsMetadata(env, lit) {
			t.Fatalf("%q flagged", lit)
		}
	}
	if len(lookups) != 0 {
		t.Fatalf("lookups %v", lookups)
	}
}

func TestURLIsPrivate(t *testing.T) {
	env := testEnv(t)
	resolved := map[string][]string{"lan.example": {"192.168.1.7"}, "pub.example": {"93.184.216.34"}, "cgnat.example": {"100.64.1.1"}}
	pin(t, &policyLookupHost, func(host string) ([]string, error) {
		if addrs, ok := resolved[host]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	})
	for u, want := range map[string]bool{
		"http://127.0.0.1:8080/": true, "http://[::1]/": true, "http://10.0.0.5/": true, "http://100.64.0.1/": true,
		"http://169.254.1.1/": true, "http://localhost:3000/": true, "http://myservice.local/": true, "http://box.lan/": true,
		"http://svc.internal/": true, "http://app.localhost./": true, "http://lan.example/": true, "http://cgnat.example/": true,
		"http://pub.example/": false, "http://missing.example/": false, "https://8.8.8.8/": false, "not a url": false,
		"http://[::ffff:100.64.0.1]/": false, "http://[bad/": false,
	} {
		if got := urlIsPrivate(env, u); got != want {
			t.Errorf("%s got %v", u, got)
		}
	}
}

func TestHybridRoutingNeedsAProviderAndNoCDPOverride(t *testing.T) {
	env := testEnv(t)
	t.Setenv("BROWSER_CDP_URL", "ws://127.0.0.1:9222/devtools/browser/x")
	if local, err := hybridRoutesLocally(env, "http://127.0.0.1/"); local || err != nil {
		t.Fatal("cdp override routed locally", err)
	}
}

func TestEvalSSRFGuardActive(t *testing.T) {
	for _, c := range []struct{ local, allow, want bool }{{false, false, true}, {true, false, false}, {false, true, false}, {true, true, false}} {
		env := pinPolicy(t, c.local, c.allow, never)
		if evalSSRFGuardActive(env) != c.want {
			t.Errorf("%+v", c)
		}
	}
}
