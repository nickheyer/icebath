package provider

import (
	"bufio"
	"context"
	"io"
	"maps"
	"net"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Loopback server answering every chat completion and recording each raw request head
func sdkHeadServer(t *testing.T) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	body := `{"id":"x","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	reply := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + itoaTest(len(body)) + "\r\n\r\n" + body
	heads := make(chan string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					var head strings.Builder
					length := 0
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						head.WriteString(line)
						if line == "\r\n" {
							break
						}
						if name, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ": "); ok && strings.EqualFold(name, "Content-Length") {
							for _, r := range value {
								length = length*10 + int(r-'0')
							}
						}
					}
					if _, err := io.CopyN(io.Discard, br, int64(length)); err != nil {
						return
					}
					heads <- head.String()
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), heads
}

func itoaTest(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoaTest(n/10) + string(rune('0'+n%10))
}

// Head net/http writes for a call carrying the given SDK header lines, PLATFORM standing for this host's X-Stainless lines: Host, User-Agent and Content-Length, then the rest by canonical name
func sdkGoldenHead(addr string, lines ...string) string {
	platform := []string{"X-Stainless-Lang: python", "X-Stainless-Package-Version: 2.54.0", "X-Stainless-OS: " + stainlessOS(),
		"X-Stainless-Arch: " + stainlessArch(), "X-Stainless-Runtime: CPython", "X-Stainless-Runtime-Version: " + pinnedPythonRuntimeVersion()}
	fields := map[string]string{"Accept-Encoding": "gzip, deflate", "Connection": "keep-alive"}
	for _, l := range lines {
		expanded := []string{l}
		if l == "PLATFORM" {
			expanded = platform
		}
		for _, line := range expanded {
			name, value, _ := strings.Cut(line, ": ")
			fields[textproto.CanonicalMIMEHeaderKey(name)] = value
		}
	}
	head := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + addr + "\r\nUser-Agent: " + fields["User-Agent"] + "\r\nContent-Length: 57\r\n"
	delete(fields, "User-Agent")
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		head += name + ": " + fields[name] + "\r\n"
	}
	return head + "\r\n"
}

func TestOpenAISDKWireHeadCarriesThePinnedSDKHeaders(t *testing.T) {
	unsetEnvForTest(t, "OPENAI_ORG_ID", "OPENAI_PROJECT_ID")
	addr, heads := sdkHeadServer(t)
	base := "http://" + addr + "/v1"
	zero := 0
	call := func(c *auxOpenAIClient, extra map[string]any) string {
		kwargs := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for k, v := range extra {
			kwargs[k] = v
		}
		if _, err := c.CreateChatCompletion(context.Background(), kwargs); err != nil {
			t.Fatal(err)
		}
		return <-heads
	}
	plain := newAuxOpenAIClient("sk-k", base, auxOpenAIOptions{MaxRetries: &zero})
	if got, want := call(plain, nil), sdkGoldenHead(addr, "Authorization: Bearer sk-k", "Accept: application/json", "Content-Type: application/json",
		"User-Agent: OpenAI/Python 2.54.0", "PLATFORM", "X-Stainless-Async: false", "x-stainless-retry-count: 0", "x-stainless-read-timeout: 600"); got != want {
		t.Fatalf("plain head\n%q\nwant\n%q", got, want)
	}
	custom := newAuxOpenAIClient("sk-k", base, auxOpenAIOptions{MaxRetries: &zero, Timeout: 1800.0,
		DefaultHeaders: map[string]string{"HTTP-Referer": "https://r", "X-Title": "T", "User-Agent": "IcebathAgent/1"}})
	extra := jsonutil.ObjectOf("x-grok-conv-id", "c1", "X-Title", "T2")
	if got, want := call(custom, map[string]any{"extra_headers": extra, "timeout": 30.0}), sdkGoldenHead(addr, "Authorization: Bearer sk-k",
		"Accept: application/json", "Content-Type: application/json", "User-Agent: IcebathAgent/1", "PLATFORM", "HTTP-Referer: https://r",
		"X-Title: T2", "X-Stainless-Async: false", "x-grok-conv-id: c1", "x-stainless-retry-count: 0", "x-stainless-read-timeout: 30.0"); got != want {
		t.Fatalf("custom head\n%q\nwant\n%q", got, want)
	}
	t.Setenv("OPENAI_ORG_ID", "org-1")
	t.Setenv("OPENAI_PROJECT_ID", "proj-1")
	omit := newAuxOpenAIClient("sk-k", base, auxOpenAIOptions{MaxRetries: &zero})
	if got, want := call(omit, map[string]any{"extra_headers": jsonutil.ObjectOf("X-Stainless-Retry-Count", "7", "OpenAI-Project", nil), "timeout": 30}),
		sdkGoldenHead(addr, "Authorization: Bearer sk-k", "Accept: application/json", "Content-Type: application/json", "User-Agent: OpenAI/Python 2.54.0",
			"PLATFORM", "X-Stainless-Async: false", "OpenAI-Organization: org-1", "X-Stainless-Retry-Count: 7", "x-stainless-read-timeout: 30"); got != want {
		t.Fatalf("omit head\n%q\nwant\n%q", got, want)
	}
	unsetEnvForTest(t, "OPENAI_ORG_ID", "OPENAI_PROJECT_ID")
	async := newAuxAsyncOpenAIClient("sk-k", base, auxOpenAIOptions{MaxRetries: &zero})
	if got, want := call(async.auxOpenAIClient, nil), sdkGoldenHead(addr, "Authorization: Bearer sk-k", "Accept: application/json",
		"Content-Type: application/json", "User-Agent: AsyncOpenAI/Python 2.54.0", "PLATFORM", "X-Stainless-Async: async:asyncio",
		"x-stainless-retry-count: 0", "x-stainless-read-timeout: 600"); got != want {
		t.Fatalf("async head\n%q\nwant\n%q", got, want)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestStainlessPlatformLabelsFollowTheSDKRules(t *testing.T) {
	if v := pinnedPythonRuntimeVersion(); v == "unknown" || strings.Contains(v, "+") || strings.Count(v, ".") != 2 {
		t.Fatalf("pinned runtime version %q", v)
	}
	if got := httpxHost(mustURL(t, "https://api.example.com:443/v1")); got != "api.example.com" {
		t.Fatalf("default https port kept %q", got)
	}
	if got := httpxHost(mustURL(t, "http://[::1]:80/v1")); got != "[::1]" {
		t.Fatalf("default http port kept %q", got)
	}
	if got := httpxHost(mustURL(t, "http://127.0.0.1:8080/v1")); got != "127.0.0.1:8080" {
		t.Fatalf("explicit port dropped %q", got)
	}
}
