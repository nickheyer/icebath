package gateway

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var proxyEnvNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "NO_PROXY", "no_proxy"}

func TestUTF16Len(t *testing.T) {
	if UTF16Len("hello") != 5 || UTF16Len("你好") != 2 || UTF16Len("😀") != 2 {
		t.Fatal("UTF-16 code unit counts are wrong")
	}
}

func TestPrefixWithinUTF16Limit(t *testing.T) {
	if got := prefixWithinUTF16Limit("hello", 10); got != "hello" {
		t.Fatalf("fits entirely, got %q", got)
	}
	got := prefixWithinUTF16Limit(strings.Repeat("😀", 10), 6)
	if got != "😀😀😀" || UTF16Len(got) != 6 {
		t.Fatalf("all emoji prefix %q", got)
	}
	runes := func(s string) int { return len([]rune(s)) }
	if FitPrefix("abcdef", 3, runes, "...") != "abc..." || FitPrefix("ab", 3, runes, "...") != "ab" {
		t.Fatal("FitPrefix keeps the suffix outside the budget")
	}
}

func TestSafeURLForLog(t *testing.T) {
	got := SafeURLForLog("https://user:pass@example.com/private/path/image.png?X-Amz-Signature=supersecret&token=abc#frag", 80)
	if got != "https://example.com/.../image.png" {
		t.Fatalf("strips query fragment and userinfo, got %q", got)
	}
	for _, c := range []struct {
		raw  string
		max  int
		want string
	}{
		{"https://example.com/", 80, "https://example.com"},
		{"https://example.com/dir/", 80, "https://example.com/..."},
		{"https://example.com/a", 3, "..."},
		{"https://example.com/a", 10, "https:/..."},
		{"https://example.com/a", 0, ""},
		{"", 80, ""},
		{"http://[::1/x", 6, "http:/"},
		{"no-scheme/path", 80, "no-scheme/path"},
	} {
		if got := SafeURLForLog(c.raw, c.max); got != c.want {
			t.Fatalf("SafeURLForLog(%q, %d) = %q", c.raw, c.max, got)
		}
	}
}

func TestProxyKwargsForAiohttp(t *testing.T) {
	sess, req, err := ProxyKwargsForAiohttp("http://proxy:8080")
	if err != nil || sess.Connector == nil || sess.Connector.Type != "http" || req != (ProxyKwargs{}) {
		t.Fatalf("an http proxy must ride a connector, got %+v %+v %v", sess, req, err)
	}
	if sess, req, err := ProxyKwargsForAiohttp(""); err != nil || sess != (ProxyKwargs{}) || req != (ProxyKwargs{}) {
		t.Fatal("no proxy means no settings")
	}
	if _, _, err := ProxyKwargsForAiohttp("https://proxy:8080"); err == nil || err.Error() != "Invalid scheme component: https" {
		t.Fatalf("an https proxy URL is refused by the connector, got %v", err)
	}
	bot, err := ProxyKwargsForBot("SOCKS5://u:p@Proxy.Example:1080")
	if err != nil || bot.Connector == nil || bot.Connector.Host != "proxy.example" || bot.Connector.Username != "u" || bot.Connector.Password != "p" {
		t.Fatalf("socks bot settings %+v %v", bot.Connector, err)
	}
	if bot, _ := ProxyKwargsForBot("http://h:1"); bot.Proxy != "http://h:1" || bot.Connector != nil {
		t.Fatal("an http bot proxy passes the URL")
	}
	if _, err := NewProxyConnector("socks5://h"); err == nil || err.Error() != "Invalid port component" {
		t.Fatalf("a missing port is refused, got %v", err)
	}
	if c, _ := NewProxyConnector("socks4://user@h:1080"); c.Username != "" {
		t.Fatal("a user without a password reads as no credentials")
	}
}

// Accepts one proxy connection, runs the handshake check and echoes what follows
func fakeProxy(t *testing.T, handshake func(conn net.Conn, br *bufio.Reader) error) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if handshake(conn, br) == nil {
			io.Copy(conn, br)
		}
	}()
	return ln.Addr().String()
}

func echoThrough(t *testing.T, c *ProxyConnector, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo %q %v", buf, err)
	}
}

func TestProxyConnectorTunnels(t *testing.T) {
	addr := fakeProxy(t, func(conn net.Conn, br *bufio.Reader) error {
		req, err := http.ReadRequest(br)
		if err != nil || req.Method != http.MethodConnect || req.Host != "api.example:443" || req.Header.Get("Proxy-Authorization") != "Basic dTpw" {
			conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			return errors.New("bad connect")
		}
		_, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		return err
	})
	c, err := NewProxyConnector("http://u:p@" + addr)
	if err != nil {
		t.Fatal(err)
	}
	echoThrough(t, c, "api.example:443")

	addr = fakeProxy(t, func(conn net.Conn, br *bufio.Reader) error {
		head := make([]byte, 8)
		io.ReadFull(br, head)
		user, _ := br.ReadString(0)
		host, _ := br.ReadString(0)
		if head[0] != 4 || head[1] != 1 || binary.BigEndian.Uint16(head[2:]) != 8443 || string(head[4:]) != "\x00\x00\x00\x01" || user != "\x00" || host != "api.example\x00" {
			conn.Write([]byte{0, 91, 0, 0, 0, 0, 0, 0})
			return errors.New("bad socks4a request")
		}
		_, err := conn.Write([]byte{0, 90, 0, 0, 0, 0, 0, 0})
		return err
	})
	c, err = NewProxyConnector("socks4://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	echoThrough(t, c, "api.example:8443")
}

func TestGatewayTrustEnvReadsConfig(t *testing.T) {
	for body, want := range map[string]bool{"gateway:\n  trust_env: false\n": false, "gateway:\n  trust_env: true\n": true, "gateway:\n  trust_env: \" Off \"\n": false, "{}\n": true} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", dir)
			os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600)
			ctx := context.Background()
			if GatewayTrustEnv(ctx) != want {
				t.Fatalf("trust_env = %v", !want)
			}
			clearEnv(t, proxyEnvNames...)
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
			if got, err := ResolveProxyURL(ctx, "", nil, ""); err != nil || (got != "") != want {
				t.Fatalf("env proxy honored = %q %v", got, err)
			}
			t.Setenv("X_PLATFORM_PROXY", "http://127.0.0.1:1080")
			if got, _ := ResolveProxyURL(ctx, "X_PLATFORM_PROXY", nil, ""); got != "http://127.0.0.1:1080" {
				t.Fatalf("the platform var always wins, got %q", got)
			}
			if got, _ := ResolveProxyURL(ctx, "", nil, " socks://cfg:1 "); got != "socks5://cfg:1" {
				t.Fatalf("the configured value is normalized, got %q", got)
			}
		})
	}
}

func TestResolveProxyURLMultiplexScope(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearEnv(t, proxyEnvNames...)
	t.Setenv("DISCORD_PROXY", "http://default-profile-proxy:8080")
	if got, _ := ResolveProxyURL(context.Background(), "DISCORD_PROXY", nil, ""); got != "http://default-profile-proxy:8080" {
		t.Fatalf("an unscoped default profile reads the env, got %q", got)
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	scoped := provider.WithSecretScope(context.Background(), map[string]string{"DISCORD_PROXY": "http://secondary-profile-proxy:9090"}, t.TempDir())
	if got, _ := ResolveProxyURL(scoped, "DISCORD_PROXY", nil, ""); got != "http://secondary-profile-proxy:9090" {
		t.Fatalf("a scoped profile uses its own value, got %q", got)
	}
	empty := provider.WithSecretScope(context.Background(), map[string]string{}, t.TempDir())
	if got, _ := ResolveProxyURL(empty, "DISCORD_PROXY", nil, ""); got != "" {
		t.Fatalf("a scoped profile never borrows the default value, got %q", got)
	}
}

func TestResolveProxyURLNoProxyBypass(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clearEnv(t, proxyEnvNames...)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	t.Setenv("NO_PROXY", "api.telegram.org")
	if got, _ := ResolveProxyURL(context.Background(), "", []string{"api.telegram.org"}, ""); got != "" {
		t.Fatalf("matching host bypasses, got %q", got)
	}
	t.Setenv("NO_PROXY", "149.154.160.0/20")
	if got, _ := ResolveProxyURL(context.Background(), "", []string{"149.154.167.220"}, ""); got != "" {
		t.Fatalf("CIDR target bypasses, got %q", got)
	}
	if !IsHostExcludedByNoProxy("149.154.167.220", nil) || IsHostExcludedByNoProxy("example.com", nil) {
		t.Fatal("no proxy host matching")
	}
	value := "example.com"
	if !IsHostExcludedByNoProxy("api.example.com", &value) {
		t.Fatal("an explicit NO_PROXY value overrides the environment")
	}
}

func TestMacOSProxyProbeCache(t *testing.T) {
	prevDarwin, prevNow, prevOut := macOSProxyIsDarwin, macOSProxyNow, scutilProxyOutput
	t.Cleanup(func() {
		macOSProxyIsDarwin, macOSProxyNow, scutilProxyOutput = prevDarwin, prevNow, prevOut
		ResetMacOSProxyCache()
	})
	macOSProxyIsDarwin = true
	ResetMacOSProxyCache()
	calls := 0
	scutilProxyOutput = func(context.Context) (string, error) {
		calls++
		return "<dictionary> {\n  HTTPEnable : 1\n  HTTPProxy : 10.0.0.1\n  HTTPPort : 3128\n}", nil
	}
	for range 10 {
		if got := detectMacOSSystemProxy(context.Background()); got != "http://10.0.0.1:3128" {
			t.Fatalf("probe = %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 scutil fork for 10 probes, got %d", calls)
	}
	ResetMacOSProxyCache()
	calls = 0
	clock := time.Unix(1000, 0)
	macOSProxyNow = func() time.Time { return clock }
	detectMacOSSystemProxy(context.Background())
	clock = clock.Add(macOSProxyTTL + time.Second)
	detectMacOSSystemProxy(context.Background())
	if calls != 2 {
		t.Fatalf("an expired TTL reads again, got %d forks", calls)
	}
	if got := parseScutilProxy("HTTPSEnable : 1\nHTTPSProxy : s.example\nHTTPSPort : 443\nHTTPEnable : 1\nHTTPProxy : h\nHTTPPort : 80"); got != "http://s.example:443" {
		t.Fatalf("HTTPS wins, got %q", got)
	}
}

func TestIsNetworkAccessible(t *testing.T) {
	ctx := context.Background()
	for host, want := range map[string]bool{"127.0.0.1": false, "::1": false, "::ffff:127.0.0.1": false, "::": true, "0.0.0.0": true, "10.0.0.1": true, "8.8.8.8": true} {
		if IsNetworkAccessible(ctx, host) != want {
			t.Fatalf("IsNetworkAccessible(%q) != %v", host, want)
		}
	}
	prev := lookupBindHost
	t.Cleanup(func() { lookupBindHost = prev })
	lookupBindHost = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("192.168.1.100")}}, nil
	}
	if !IsNetworkAccessible(ctx, "dual-host.local") {
		t.Fatal("a hostname with one non loopback answer is accessible")
	}
	lookupBindHost = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if IsNetworkAccessible(ctx, "localhost") {
		t.Fatal("a loopback only hostname is not accessible")
	}
	lookupBindHost = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") }
	if !IsNetworkAccessible(ctx, "nowhere.invalid") {
		t.Fatal("DNS failure fails closed")
	}
}

// Media fetch seams pointed at a plain client with the given safety rule
func mediaFetchSeams(t *testing.T, safe func(string) bool) *[]time.Duration {
	prevSafe, prevClient, prevSleep, prevIdle := mediaURLIsSafe, newMediaFetchClient, mediaRetrySleep, mediaFetchIdleTimeout
	t.Cleanup(func() {
		mediaURLIsSafe, newMediaFetchClient, mediaRetrySleep, mediaFetchIdleTimeout = prevSafe, prevClient, prevSleep, prevIdle
	})
	mediaURLIsSafe = func(_ *tools.Env, raw string) bool { return safe(raw) }
	newMediaFetchClient = func(*tools.Env) *http.Client { return &http.Client{} }
	sleeps := &[]time.Duration{}
	mediaRetrySleep = func(_ context.Context, d time.Duration) error { *sleeps = append(*sleeps, d); return nil }
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return sleeps
}

func TestCacheImageFromURLRetriesOnTimeoutThenSucceeds(t *testing.T) {
	sleeps := mediaFetchSeams(t, func(string) bool { return true })
	mediaFetchIdleTimeout = 100 * time.Millisecond
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Accept") != "image/*,*/*;q=0.8" {
			w.WriteHeader(400)
		}
		w.Write([]byte("\xff\xd8\xff image data"))
	}))
	defer srv.Close()
	path, err := CacheImageFromURL(context.Background(), srv.URL+"/img.jpg", ".jpg", 2)
	if err != nil || !strings.HasSuffix(path, ".jpg") || hits.Load() != 2 || len(*sleeps) != 1 || (*sleeps)[0] != 1500*time.Millisecond {
		t.Fatalf("path %q err %v hits %d sleeps %v", path, err, hits.Load(), *sleeps)
	}
}

func TestCacheAudioFromURLRetriesOnServerErrorThenSucceeds(t *testing.T) {
	sleeps := mediaFetchSeams(t, func(string) bool { return true })
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("audio data"))
	}))
	defer srv.Close()
	path, err := CacheAudioFromURL(context.Background(), srv.URL+"/a.ogg", ".ogg", 2)
	if err != nil || !strings.HasSuffix(path, ".ogg") || hits.Load() != 2 || len(*sleeps) != 1 {
		t.Fatalf("path %q err %v hits %d sleeps %v", path, err, hits.Load(), *sleeps)
	}
}

func TestCacheMediaFromURLRefusals(t *testing.T) {
	sleeps := mediaFetchSeams(t, func(raw string) bool { return !strings.Contains(raw, "169.254") })
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		case "/missing":
			w.WriteHeader(404)
		case "/busy":
			w.WriteHeader(429)
		default:
			w.Write([]byte(strings.Repeat("x", 100)))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	var valueErr *mediacache.ValueError
	_, err := CacheImageFromURL(ctx, srv.URL+"/redirect", ".jpg", 2)
	if !errors.As(err, &valueErr) || valueErr.Msg != "Blocked redirect to private/internal address: http://169.254.169.254/.../meta-data" {
		t.Fatalf("a private redirect is refused, got %v", err)
	}
	if _, err := CacheImageFromURL(ctx, "http://169.254.169.254/x?token=1", ".jpg", 2); err == nil || err.Error() != "Blocked unsafe URL (SSRF protection): http://169.254.169.254/.../x" {
		t.Fatalf("an unsafe URL is refused before any request, got %v", err)
	}
	hits.Store(0)
	var statusErr *httpStatusError
	if _, err := CacheImageFromURL(ctx, srv.URL+"/missing", ".jpg", 2); !errors.As(err, &statusErr) || statusErr.Code != 404 || hits.Load() != 1 || !strings.HasPrefix(err.Error(), "Client error '404 Not Found' for url") {
		t.Fatalf("a client error is raised without retry, got %v after %d hits", err, hits.Load())
	}
	hits.Store(0)
	*sleeps = nil
	if _, err := CacheImageFromURL(ctx, srv.URL+"/busy", ".jpg", 2); !errors.As(err, &statusErr) || statusErr.Code != 429 || hits.Load() != 3 || len(*sleeps) != 2 {
		t.Fatalf("a rate limit retries until the budget runs out, got %v after %d hits", err, hits.Load())
	}
	os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: 10\n"), 0o600)
	if _, err := CacheImageFromURL(ctx, srv.URL+"/big", ".jpg", 2); !errors.As(err, &valueErr) || !strings.Contains(valueErr.Msg, "Inbound image payload is too large (100 bytes > 10 bytes)") {
		t.Fatalf("the inbound cap rejects the declared size, got %v", err)
	}
}

func TestThreadMetadataAndReplyAnchor(t *testing.T) {
	s := func(v string) *string { return &v }
	src := &SessionSource{Platform: PlatformTelegram, ChatType: "dm", ThreadID: s("77"), MessageID: s("9"), Profile: s(" work ")}
	md := threadMetadataForSource(src, s("12"))
	if md["thread_id"] != "77" || md["direct_messages_topic_id"] != "77" || md["telegram_reply_to_message_id"] != "12" || md["telegram_dm_topic_reply_fallback"] != true || md["icebath_profile"] != "work" {
		t.Fatalf("telegram dm metadata %v", md)
	}
	if md := threadMetadataForSource(&SessionSource{Platform: PlatformTelegram, ChatType: "dm", ThreadID: s("1"), MessageID: s("9")}, s("")); md["telegram_reply_to_message_id"] != "9" || md["direct_messages_topic_id"] != nil {
		t.Fatalf("the general topic keeps no topic id and anchors on the source message, %v", md)
	}
	if md := threadMetadataForSource(&SessionSource{Platform: PlatformSlack, ScopeID: s("T1")}, nil); md["slack_team_id"] != "T1" || len(md) != 1 {
		t.Fatalf("slack workspace identity %v", md)
	}
	if threadMetadataForSource(&SessionSource{Platform: PlatformDiscord}, nil) != nil {
		t.Fatal("no thread means no metadata")
	}
	if m := markNotifyMetadata(nil); m["notify"] != true {
		t.Fatal("notify mark")
	}
	ev := NewMessageEvent("hi")
	ev.Source, ev.MessageID, ev.ReplyToMessageID = &SessionSource{Platform: PlatformTelegram, ChatType: "group", ThreadID: s("5")}, s("m1"), s("r1")
	if replyAnchorForEvent(ev) != nil {
		t.Fatal("forum topics route without a reply anchor")
	}
	ev.Source.ChatType = "dm"
	if a := replyAnchorForEvent(ev); a == nil || *a != "m1" {
		t.Fatal("telegram dm topics reply to the triggering message")
	}
	ev.Source = &SessionSource{Platform: PlatformFeishu, ThreadID: s("t")}
	if a := replyAnchorForEvent(ev); a == nil || *a != "r1" {
		t.Fatal("feishu threads anchor on the replied message")
	}
	ev.Source = &SessionSource{Platform: PlatformSlack}
	ev.RawMessage = map[string]any{"_icebath_no_thread_response": true}
	if replyAnchorForEvent(ev) != nil {
		t.Fatal("a slack reaction handoff is a new top level message")
	}
	ev.RawMessage, ev.Internal = nil, true
	if replyAnchorForEvent(ev) != nil {
		t.Fatal("a synthetic wake never anchors")
	}
	ev.ReplyAnchorOverride = s("o")
	if a := replyAnchorForEvent(ev); *a != "o" {
		t.Fatal("the override wins")
	}
}
