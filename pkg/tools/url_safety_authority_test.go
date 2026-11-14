package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

var errAuthorityNoDNS = errors.New("Name or service not known")

// Swaps the resolver for the test, every lookup answered by fn
func authorityResolver(t *testing.T, fn func(host string) ([]netip.Addr, error)) {
	t.Helper()
	prev := lookupHostIPs
	lookupHostIPs = func(_ context.Context, host string) ([]netip.Addr, error) { return fn(host) }
	t.Cleanup(func() { lookupHostIPs = prev })
}

// Resolver answering every host with the given addresses
func authorityResolvesTo(t *testing.T, ips ...string) {
	authorityResolver(t, func(string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			out = append(out, netip.MustParseAddr(ip))
		}
		return out, nil
	})
}

// Resolver failing every lookup like a gaierror
func authorityDNSFails(t *testing.T) {
	authorityResolver(t, func(string) ([]netip.Addr, error) { return nil, errAuthorityNoDNS })
}

// Clears every proxy variable, optionally setting HTTPS_PROXY afterwards
func authorityProxy(t *testing.T, proxy string) {
	for _, v := range proxyEnvVars {
		t.Setenv(v, "")
	}
	if proxy != "" {
		t.Setenv("HTTPS_PROXY", proxy)
	}
}

func authorityEnv(t *testing.T, security map[string]any) *Env {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "")
	tree := config.Tree{}
	if security != nil {
		tree["security"] = security
	}
	return &Env{Home: t.TempDir(), Config: tree}
}

var authorityAmbiguous = []string{
	"http://127.0.0.1:8080\\@1.1.1.1", "http://127.0.0.1:8080\\\\@1.1.1.1", "http://127.0.0.1\\@1.1.1.1/",
	"http://1.1.1.1\\.127.0.0.1/", "http://\\127.0.0.1/", "http://%31%32%37.0.0.1/",
	"http://127.0.0.1:8080\t@1.1.1.1", "http://127.0.0.1:8080\n@1.1.1.1", "http://127.0.0.1:8080\r@1.1.1.1",
	"http://127.0.0.1:8080\x00@1.1.1.1", "http://exa\tmple.com/", "https://example.com/a\nb", "http://exa mple.com/",
	"http://1.1.1.1:99999/", "http://1.1.1.1:80x/",
}

var authorityUserinfo = []string{
	"http://127.0.0.1:8080%5C@1.1.1.1", "http://user:pass@1.1.1.1/", "http://1.1.1.1@1.1.1.1/", "https://@example.com/",
}

var authorityLegitimate = []string{
	"https://example.com/image.png", "https://example.com/a\\b?q=c\\d", "https://example.com/%5C@x",
	"https://example.com/p@q?x=@y#frag@z", "https://example.com/r?next=http://127.0.0.1\\@x",
	"https://example.com/path%20with%20spaces?q=a%40b", "https://example.com:8443/x", "http://[2606:4700:4700::1111]:8080/",
	"HTTPS://EXAMPLE.COM/", "  https://example.com/  ",
}

func TestIsSafeURLRefusesAmbiguousAuthority(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityResolvesTo(t, "1.1.1.1")
	for _, raw := range append(append([]string{}, authorityAmbiguous...), authorityUserinfo...) {
		if isSafeURL(env, raw) {
			t.Errorf("isSafeURL(%q) allowed", raw)
		}
	}
	for _, raw := range authorityLegitimate {
		if !isSafeURL(env, raw) {
			t.Errorf("isSafeURL(%q) refused", raw)
		}
	}
	if isSafeURL(env, "http://127.0.0.1:8080\\@1.1.1.1") {
		t.Error("async variant of the reported url allowed")
	}
}

func TestIsSafeURLLogOmitsUserinfoCredentials(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityResolvesTo(t, "1.1.1.1")
	var buf bytes.Buffer
	env.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if isSafeURL(env, "https://alice:hunter2@example.com/") {
		t.Fatal("userinfo url allowed")
	}
	if !strings.Contains(buf.String(), "userinfo") || strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestAlwaysBlockedFloorRefusesAmbiguousAuthority(t *testing.T) {
	authorityResolvesTo(t, "1.1.1.1")
	for _, raw := range []string{
		"http://169.254.169.254\\@1.1.1.1/latest/meta-data/", "http:\\\\169.254.169.254\\latest",
		"http:///169.254.169.254/latest", "http://%31%36%39.254.169.254/", "http://169.254.169.254\t@1.1.1.1/",
	} {
		if !isAlwaysBlockedURL(nil, raw) {
			t.Errorf("floor passed %q", raw)
		}
	}
	for _, raw := range []string{"https://user:pass@example.com/", "https://example.com/a\\b", "about:blank", "data:text/plain,line1\nline2"} {
		if isAlwaysBlockedURL(nil, raw) {
			t.Errorf("floor blocked %q", raw)
		}
	}
}

// Resolver of the floor tests, localhost only
func authorityLocalhostOnly(t *testing.T) {
	authorityResolver(t, func(host string) ([]netip.Addr, error) {
		if host == "localhost" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errAuthorityNoDNS
	})
}

func TestFloorAcceptsTemplatedURLLiterals(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityLocalhostOnly(t)
	for _, raw := range []string{"http://localhost:{port}/", "http://localhost:%d/", "http://localhost:${port}/api",
		"http://{host}:8080/", "http://%s/", "http://user:pass@localhost:{port}/"} {
		if isAlwaysBlockedURL(env, raw) {
			t.Errorf("floor blocked placeholder %q", raw)
		}
	}
	for _, raw := range []string{"http://localhost:{port}/", "http://localhost:%d/"} {
		if isSafeURL(env, raw) {
			t.Errorf("isSafeURL allowed %q", raw)
		}
	}
}

func TestFloorJudgesPercentEncodedHostDecoded(t *testing.T) {
	authorityLocalhostOnly(t)
	for _, raw := range []string{"http://%31%36%39.254.169.254/", "HTTP://%31%36%39.254.169.254:80/latest",
		"http://metadata.google.%69nternal/computeMetadata/v1/"} {
		if !isAlwaysBlockedURL(nil, raw) {
			t.Errorf("decoded metadata host passed %q", raw)
		}
	}
	for _, raw := range []string{"http://%6c%6f%63%61%6c%68%6f%73%74/", "http://exa%20mple.com/"} {
		if isAlwaysBlockedURL(nil, raw) {
			t.Errorf("decoded ordinary host blocked %q", raw)
		}
	}
}

func TestTrailingControlCannotHideMetadataHost(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityLocalhostOnly(t)
	authorityProxy(t, "http://proxy.invalid:3128")
	for _, control := range []string{"\x00", "\x01", "\x08", "\x1b", "\x1f", "\x7f"} {
		if !isAlwaysBlockedURL(env, "http://169.254.169.254"+control) {
			t.Errorf("floor passed trailing %q", control)
		}
		if isSafeURL(env, "http://169.254.169.254"+control) {
			t.Errorf("isSafeURL allowed trailing %q", control)
		}
	}
	authorityResolvesTo(t, "93.184.216.34")
	for _, raw := range []string{"  https://example.com/  ", "https://example.com/\n", "\thttps://example.com/\r\n"} {
		if isAlwaysBlockedURL(env, raw) || !isSafeURL(env, raw) {
			t.Errorf("surrounding whitespace refused %q", raw)
		}
	}
}

func TestIDNADotFormsCannotHideMetadataHost(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityLocalhostOnly(t)
	authorityProxy(t, "http://proxy.invalid:3128")
	for _, raw := range []string{"http://169.254.169.254。/", "http://169.254.169.254．/latest/meta-data/",
		"http://169.254.169.254｡/", "http://169。254。169。254/", "http://１６９.254.169.254。/"} {
		if !isAlwaysBlockedURL(env, raw) {
			t.Errorf("floor passed %q", raw)
		}
		if isSafeURL(env, raw) {
			t.Errorf("isSafeURL allowed %q", raw)
		}
	}
	if isAlwaysBlockedURL(env, "https://example。com。/") || !isSafeURL(env, "https://example。com。/") {
		t.Error("public name with ideographic dots refused")
	}
}

func TestIsSafeURLCases(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityProxy(t, "")
	authorityResolvesTo(t, "93.184.216.34")
	if !isSafeURL(env, "https://example.com/image.png") {
		t.Error("public url refused")
	}
	for _, raw := range []string{"ftp://example.com/file.txt", "example.com/path", "http://", ""} {
		if isSafeURL(env, raw) {
			t.Errorf("unusable %q allowed", raw)
		}
	}
	for _, c := range [][2]string{{"127.0.0.1", "http://localhost:8080/secret"}, {"10.0.0.1", "http://internal-service.local/api"}, {"::1", "http://[::1]:8080/"}} {
		authorityResolvesTo(t, c[0])
		if isSafeURL(env, c[1]) {
			t.Errorf("private %q allowed", c[1])
		}
	}
	authorityDNSFails(t)
	if isSafeURL(env, "https://nonexistent.example.com") {
		t.Error("dns failure allowed")
	}
}

func TestProxyEnvironmentDNSDelegation(t *testing.T) {
	env := authorityEnv(t, nil)
	authorityProxy(t, "http://host.docker.internal:9090")
	authorityDNSFails(t)
	if !isSafeURL(env, "https://api.openai.com/v1/models") {
		t.Error("dns failure refused with a proxy")
	}
	if isSafeURL(env, "http://metadata.google.internal/computeMetadata/v1/") || isSafeURL(env, "http://169.254.169.254/latest/meta-data/") {
		t.Error("metadata allowed with a proxy")
	}
	authorityProxy(t, "")
	authorityResolvesTo(t, "fe80::1%eth0")
	if isSafeURL(env, "http://[fe80::1%eth0]/") {
		t.Error("scoped link local allowed")
	}
	if isSafeURL(env, "http://[::1/") || isSafeURL(env, "http://[evil.com]/") {
		t.Error("urlsplit ValueError allowed")
	}
	authorityResolvesTo(t, "198.18.0.23")
	if isSafeURL(env, "https://example.com/file.jpg") {
		t.Error("benchmark address allowed")
	}
	for raw, want := range map[string]bool{"https://multimedia.nt.qq.com.cn/download?id=123": true,
		"https://sub.multimedia.nt.qq.com.cn/download?id=123": false, "http://multimedia.nt.qq.com.cn/download?id=123": false} {
		if got := isSafeURL(env, raw); got != want {
			t.Errorf("isSafeURL(%q) = %v", raw, got)
		}
	}
}

func TestAllowPrivateURLsIntegration(t *testing.T) {
	env := authorityEnv(t, nil)
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	for _, c := range [][2]string{{"192.168.1.1", "http://router.local"}, {"198.18.23.183", "https://nousresearch.com"}} {
		authorityResolvesTo(t, c[0])
		if !isSafeURL(env, c[1]) {
			t.Errorf("toggle refused %q", c[1])
		}
	}
	for _, c := range [][2]string{{"fd00:ec2::254", "http://[fd00:ec2::254]/latest/"}, {"100.100.100.200", "http://100.100.100.200/latest/meta-data/"}} {
		authorityResolvesTo(t, c[0])
		if isSafeURL(env, c[1]) {
			t.Errorf("metadata allowed with the toggle %q", c[1])
		}
	}
	if isSafeURL(env, "http://metadata.google.internal/computeMetadata/v1/") {
		t.Error("metadata hostname allowed with the toggle")
	}
	authorityProxy(t, "")
	authorityDNSFails(t)
	if isSafeURL(env, "https://nonexistent.example.com") {
		t.Error("dns failure allowed with the toggle")
	}
}

func TestIsAlwaysBlockedURLCases(t *testing.T) {
	env := authorityEnv(t, nil)
	for _, raw := range []string{"http://169.254.42.1/", "http://100.100.100.200/latest/meta-data/"} {
		if !isAlwaysBlockedURL(env, raw) {
			t.Errorf("imds literal passed %q", raw)
		}
	}
	authorityDNSFails(t)
	if !isAlwaysBlockedURL(env, "http://metadata.google.internal/") || isAlwaysBlockedURL(env, "http://nonexistent.example.com/") {
		t.Error("floor without dns")
	}
	authorityResolvesTo(t, "::ffff:169.254.169.254%eth0")
	if !isAlwaysBlockedURL(env, "http://attacker-controlled.example.com/") {
		t.Error("scoped mapped imds passed")
	}
	authorityResolvesTo(t, "93.184.216.34")
	for _, raw := range []string{"https://example.com/path", "http://127.0.0.1:8080/", "http://100.64.0.1/", "not a url at all", "http://[::1/"} {
		if isAlwaysBlockedURL(env, raw) {
			t.Errorf("floor blocked %q", raw)
		}
	}
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	if !isAlwaysBlockedURL(env, "http://169.254.169.254/") {
		t.Error("toggle opened the floor")
	}
}

func TestIPv4WrappedMetadataSSRF(t *testing.T) {
	env := authorityEnv(t, nil)
	for _, c := range [][2]string{{"::ffff:169.254.169.254", "http://aws-metadata.internal/"}, {"::ffff:100.100.100.200", "http://aliyun-metadata.internal/"}} {
		authorityResolvesTo(t, c[0])
		if isSafeURL(env, c[1]) {
			t.Errorf("mapped metadata allowed %q", c[1])
		}
	}
	authorityResolvesTo(t, "::ffff:0:a9fe:a9fe")
	if !isAlwaysBlockedURL(env, "http://evil.example/") || isSafeURL(env, "http://evil.example/") {
		t.Error("translated metadata wrapper passed")
	}
	if !isAlwaysBlockedURL(env, "http://[::ffff:0:a9fe:a9fe]/") {
		t.Error("literal translated wrapper passed")
	}
	declared := authorityEnv(t, map[string]any{"fake_ip_ranges": []any{"198.18.0.0/15"}})
	authorityResolvesTo(t, "::ffff:0:c612:58")
	if !isSafeURL(declared, "https://example.com/") {
		t.Error("declared translated sentinel refused")
	}
	if ips, err := resolvedHTTPConnectIPs(context.Background(), declared, "example.com", "https"); err != nil || len(ips) != 1 || ips[0] != "::ffff:0:c612:58" {
		t.Errorf("connect ips %v %v", ips, err)
	}
}

func TestDeclaredFakeIPSentinelRanges(t *testing.T) {
	env := authorityEnv(t, map[string]any{"fake_ip_ranges": []any{"198.18.0.0/15"}})
	authorityResolvesTo(t, "198.18.1.125")
	if !isSafeURL(env, "https://example.com/") {
		t.Error("declared sentinel refused")
	}
	authorityResolvesTo(t, "192.168.99.99")
	if isSafeURL(env, "https://example.com/") {
		t.Error("undeclared private allowed")
	}
	authorityResolvesTo(t, "169.254.169.254")
	if isSafeURL(env, "http://example.com/") {
		t.Error("metadata allowed under a declaration")
	}
	authorityResolvesTo(t, "198.18.0.23")
	if isSafeURL(authorityEnv(t, nil), "https://example.com/file.jpg") {
		t.Error("sentinel allowed without a declaration")
	}
	for _, c := range [][2]string{{"10.0.0.0/8", "10.0.0.5"}, {"127.0.0.0/8", "127.0.0.1"}, {"100.64.0.0/10", "100.64.0.1"},
		{"fc00::/7", "fd00::1"}, {"0.0.0.0/0", "192.168.1.1"}, {"::/0", "::1"}} {
		e := authorityEnv(t, map[string]any{"fake_ip_ranges": []any{c[0], "198.18.0.0/15"}})
		authorityResolvesTo(t, c[1])
		if isSafeURL(e, "https://example.com/") {
			t.Errorf("declaration %s excused %s", c[0], c[1])
		}
		authorityResolvesTo(t, "198.18.1.125")
		if !isSafeURL(e, "https://example.com/") {
			t.Errorf("sibling declaration broken by %s", c[0])
		}
	}
}

func TestAuthorityAmbiguityExportCarriesTheURLSplitError(t *testing.T) {
	for _, c := range []struct {
		raw    string
		floor  bool
		reason string
		err    string
	}{
		{"http://127.0.0.1:8080\\@1.1.1.1", true, "backslash in authority", ""},
		{"http://user:pass@1.1.1.1/", false, "userinfo in authority", ""},
		{"http://user:pass@1.1.1.1/", true, "", ""},
		{"http://1.1.1.1:80x/", false, "invalid port", ""},
		{"\x01http://example.com/", true, "control character in URL", ""},
		{"about:blank", false, "", ""},
		{"http://[::1/", true, "", "Invalid IPv6 URL"},
		{"http://[1.2.3.4]/", false, "", "An IPv4 address cannot be in brackets"},
		{"https://[::g]/", true, "", "'::g' does not appear to be an IPv4 or IPv6 address"},
		{"http://ex℀mple.com/", false, "", "netloc 'ex℀mple.com' contains invalid characters under NFKC normalization"},
	} {
		reason, err := AuthorityAmbiguity(c.raw, c.floor)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if reason != c.reason || got != c.err {
			t.Errorf("AuthorityAmbiguity(%q, %v) = %q, %q", c.raw, c.floor, reason, got)
		}
	}
}

func TestURLHostnameExportCarriesTheURLSplitError(t *testing.T) {
	for _, c := range []struct {
		raw    string
		decode bool
		host   string
		err    string
	}{
		{"  HTTP://Example.COM./x  ", false, "example.com", ""},
		{"http://%31%36%39.254.169.254/", true, "169.254.169.254", ""},
		{"http://%31%36%39.254.169.254/", false, "%31%36%39.254.169.254", ""},
		{"http://a:b:c/", false, "a", ""},
		{"http://169.254.169.254。/", false, "169.254.169.254", ""},
		{"http://[FE80::1%Eth0]:8/", false, "fe80::1%eth0", ""},
		{"http://[::1]x/", false, "", "Invalid IPv6 URL"},
		{"http://a[::1]/", true, "", "Invalid IPv6 URL"},
	} {
		host, err := URLHostname(c.raw, c.decode)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if host != c.host || got != c.err {
			t.Errorf("URLHostname(%q, %v) = %q, %q", c.raw, c.decode, host, got)
		}
	}
}

func TestURLSafetyErrorsFollowThePythonExceptClauses(t *testing.T) {
	var buf bytes.Buffer
	env := authorityEnv(t, nil)
	env.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if isAlwaysBlockedURL(env, "http://[::1/") {
		t.Fatal("floor blocked an unparseable url")
	}
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "is_always_blocked_url error for http://[::1/: Invalid IPv6 URL") {
		t.Fatalf("floor log %q", buf.String())
	}
	buf.Reset()
	if isSafeURL(env, "http://[::1/") {
		t.Fatal("unparseable url allowed")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "URL safety check error for http://[::1/: Invalid IPv6 URL") {
		t.Fatalf("safety log %q", buf.String())
	}
}
