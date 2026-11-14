package provider

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

type auxActualRequest struct {
	Path    string
	Payload map[string]any
}

// Answers stream framed DNS queries on conn with loopback for host and NXDOMAIN for every other name
func auxServeLoopbackDNS(conn net.Conn, host string) {
	defer conn.Close()
	for {
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		var p dnsmessage.Parser
		head, err := p.Start(query)
		if err != nil {
			return
		}
		q, err := p.Question()
		if err != nil {
			return
		}
		match := strings.EqualFold(q.Name.String(), host+".")
		rcode := dnsmessage.RCodeNameError
		if match {
			rcode = dnsmessage.RCodeSuccess
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: head.ID, Response: true, Authoritative: true, RecursionAvailable: true, RCode: rcode})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		if match && q.Type == dnsmessage.TypeA {
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}})
		}
		msg, err := b.Finish()
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(msg)))
		if _, err := conn.Write(append(size[:], msg...)); err != nil {
			return
		}
	}
}

// Resolves host to 127.0.0.1 for every dial in the process, the getaddrinfo patch of the hermes fixture
func auxRedirectHostToLoopback(t *testing.T, host string) {
	t.Helper()
	orig := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go auxServeLoopbackDNS(server, host)
		return client, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = orig })
}

// Local chat completions endpoint reachable as 127.0.0.1 and api.actual.inc, recording every POST
func auxActualEndpoint(t *testing.T) (string, func() []auxActualRequest) {
	t.Helper()
	ctx := context.Background()
	ShutdownCachedClients(ctx)
	auxClearUnhealthy(t)
	auxRedirectHostToLoopback(t, "api.actual.inc")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,api.actual.inc")
	var mu sync.Mutex
	var requests []auxActualRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		requests = append(requests, auxActualRequest{Path: r.URL.Path, Payload: payload})
		mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if payload["model"] == "unavailable-model" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"The model is not supported with this account","type":"invalid_request_error","code":"unsupported_model"}}`)
			return
		}
		content := "The task is complete."
		if _, ok := payload["response_format"]; ok {
			content = `{"title":"Actual background routing"}`
		}
		choice := map[string]any{"index": 0, "finish_reason": "stop"}
		response := map[string]any{"id": "chatcmpl-background", "created": 1, "model": payload["model"], "object": "chat.completion",
			"choices": []any{choice}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
		if payload["stream"] == true {
			response["object"] = "chat.completion.chunk"
			choice["delta"] = map[string]any{"role": "assistant", "content": content}
			body, _ := json.Marshal(response)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
			return
		}
		choice["message"] = map[string]any{"role": "assistant", "content": content}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(func() {
		ShutdownCachedClients(ctx)
		srv.Close()
	})
	return srv.URL, func() []auxActualRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]auxActualRequest(nil), requests...)
	}
}

// First choice message content of an OpenAI shaped response map
func auxActualContent(t *testing.T, resp any) string {
	t.Helper()
	m, _ := resp.(map[string]any)
	var first map[string]any
	switch choices := m["choices"].(type) {
	case []any:
		if len(choices) > 0 {
			first, _ = choices[0].(map[string]any)
		}
	case []map[string]any:
		if len(choices) > 0 {
			first = choices[0]
		}
	}
	msg, _ := first["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

func auxActualUnsetenv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}

func auxActualMainRuntime(rt *Runtime) map[string]any {
	return map[string]any{"provider": rt.Provider, "base_url": rt.BaseURL, "api_key": rt.APIKey, "api_mode": string(rt.APIMode), "model": rt.Model}
}

func TestActualBackgroundTasksReachChatCompletions(t *testing.T) {
	for _, auxProvider := range []string{"auto", "actual", "aci", "custom", "custom:actual-relay"} {
		for _, hk := range []struct{ hosted, useAPIKey bool }{{false, false}, {false, true}, {true, true}} {
			for _, staleMode := range []string{"", "codex_responses"} {
				name := fmt.Sprintf("aux_provider=%s/hosted=%v/use_api_key=%v/stale_mode=%s", auxProvider, hk.hosted, hk.useAPIKey, staleMode)
				t.Run(name, func(t *testing.T) {
					baseURL, requests := auxActualEndpoint(t)
					if hk.hosted {
						baseURL = strings.Replace(baseURL, "127.0.0.1", "api.actual.inc", 1)
					}
					h := t.TempDir()
					t.Setenv("ICEBATH_HOME", h)
					auxActualUnsetenv(t, "ACTUAL_API_KEY", "ACTUAL_BASE_URL")
					if hk.useAPIKey {
						t.Setenv("ACTUAL_API_KEY", "actual-test-key")
						t.Setenv("ACTUAL_BASE_URL", "http://127.0.0.1:1")
					}
					auxModel := "test-model"
					if strings.HasPrefix(auxProvider, "custom") {
						auxModel = "override-model"
					}
					var apiMode any
					if staleMode != "" {
						apiMode = staleMode
					}
					relayBase := baseURL + "/v1"
					if staleMode != "" {
						relayBase = baseURL
					}
					aux := map[string]any{}
					for _, task := range []string{"compression", "title_generation", "session_search"} {
						entry := map[string]any{"provider": auxProvider, "model": auxModel, "api_mode": apiMode}
						if auxProvider == "custom" {
							entry["base_url"] = relayBase
						}
						aux[task] = entry
					}
					mainProvider := "actual"
					if auxProvider == "aci" {
						mainProvider = "aci"
					}
					transport := "chat_completions"
					if staleMode != "" {
						transport = staleMode
					}
					writeConfig(t, h, map[string]any{
						"model":     map[string]any{"provider": mainProvider, "default": "test-model", "base_url": baseURL},
						"auxiliary": aux,
						"providers": map[string]any{"actual-relay": map[string]any{"base_url": relayBase, "key_env": "ACTUAL_API_KEY", "transport": transport}},
					})
					ctx := context.Background()
					rt, err := ResolveRuntimeProvider(ctx, h, "actual", "", "", "")
					if err != nil {
						t.Fatal(err)
					}
					rt.Model = "test-model"
					if rt.APIMode != ModeChatCompletions {
						t.Fatalf("runtime api mode %q", rt.APIMode)
					}
					if staleMode != "" {
						rt.APIMode = APIMode(staleMode)
					}
					runtime := auxActualMainRuntime(rt)
					title, err := CallLLM(ctx, map[string]any{"task": "title_generation",
						"messages":   []any{map[string]any{"role": "system", "content": "Generate a title."}, map[string]any{"role": "user", "content": "Check the background routing"}},
						"max_tokens": 512, "temperature": nil, "timeout": 5.0, "main_runtime": runtime,
						"extra_body": map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
							"name": "session_title", "strict": true, "schema": map[string]any{"type": "object",
								"properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"}, "additionalProperties": false}}}},
						"reasoning_config": map[string]any{"enabled": false}})
					if err != nil {
						t.Fatal(err)
					}
					if got := auxActualContent(t, title); got != `{"title":"Actual background routing"}` {
						t.Fatalf("title content %q", got)
					}
					summary, err := CallLLM(ctx, map[string]any{"task": "compression",
						"main_runtime": map[string]any{"model": rt.Model, "provider": rt.Provider, "base_url": rt.BaseURL, "api_key": rt.APIKey, "api_mode": string(rt.APIMode)},
						"messages":     []any{map[string]any{"role": "user", "content": "Summarize this conversation."}}, "route_info": map[string]string{}})
					if err != nil {
						t.Fatal(err)
					}
					if got := auxActualContent(t, summary); got != "The task is complete." {
						t.Fatalf("summary content %q", got)
					}
					search, err := AsyncCallLLM(ctx, map[string]any{"task": "session_search",
						"messages": []any{map[string]any{"role": "user", "content": "Summarize the results."}}, "main_runtime": runtime, "timeout": 5.0})
					if err != nil {
						t.Fatal(err)
					}
					if got := auxActualContent(t, search); got != "The task is complete." {
						t.Fatalf("session search content %q", got)
					}
					got := requests()
					if len(got) != 3 {
						t.Fatalf("requests %v", got)
					}
					for _, req := range got {
						if req.Path != "/v1/chat/completions" || req.Payload["model"] != auxModel {
							t.Fatalf("request %s model %v, want /v1/chat/completions %s", req.Path, req.Payload["model"], auxModel)
						}
					}
				})
			}
		}
	}
}

func TestActualRuntimeTransitionsResolveChatCompletions(t *testing.T) {
	for _, row := range []struct {
		provider string
		hosted   bool
	}{{"actual", false}, {"aci", false}, {"custom", true}, {"custom:actual-relay", true}} {
		t.Run(fmt.Sprintf("provider=%s/hosted=%v", row.provider, row.hosted), func(t *testing.T) {
			baseURL, _ := auxActualEndpoint(t)
			if row.hosted {
				baseURL = strings.Replace(baseURL, "127.0.0.1", "api.actual.inc", 1)
			}
			baseURL += "/v1"
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			t.Setenv("ACTUAL_API_KEY", "actual-test-key")
			t.Setenv("OPENAI_API_KEY", "actual-test-key")
			writeConfig(t, h, map[string]any{
				"model":     map[string]any{"provider": row.provider, "default": "gpt-5.4", "base_url": baseURL, "api_mode": "codex_responses"},
				"providers": map[string]any{"actual-relay": map[string]any{"base_url": baseURL, "key_env": "ACTUAL_API_KEY", "transport": "codex_responses"}},
			})
			rt, err := ResolveRuntimeProvider(context.Background(), h, row.provider, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if rt.APIMode != ModeChatCompletions {
				t.Fatalf("an Actual route resolves %q, want chat_completions", rt.APIMode)
			}
		})
	}
}

func TestActualAuxiliaryFallbackReachesChatCompletions(t *testing.T) {
	for _, provider := range []string{"actual", "aci", "custom:actual-relay"} {
		for _, asyncMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("provider=%s/async_mode=%v", provider, asyncMode), func(t *testing.T) {
				localURL, requests := auxActualEndpoint(t)
				actualURL := strings.Replace(localURL, "127.0.0.1", "api.actual.inc", 1) + "/v1"
				h := t.TempDir()
				t.Setenv("ICEBATH_HOME", h)
				t.Setenv("ACTUAL_API_KEY", "actual-test-key")
				writeConfig(t, h, map[string]any{
					"model":     map[string]any{"provider": "actual", "default": "test-model", "base_url": actualURL},
					"providers": map[string]any{"actual-relay": map[string]any{"base_url": actualURL, "key_env": "ACTUAL_API_KEY", "transport": "codex_responses"}},
					"auxiliary": map[string]any{"session_search": map[string]any{"provider": "custom", "model": "unavailable-model", "base_url": localURL + "/v1",
						"fallback_chain": []any{map[string]any{"provider": provider, "model": "test-model", "api_mode": "codex_responses", "base_url": actualURL}}}},
				})
				kwargs := map[string]any{"task": "session_search", "messages": []any{map[string]any{"role": "user", "content": "Summarize the results."}}, "timeout": 5.0}
				call := CallLLM
				if asyncMode {
					call = AsyncCallLLM
				}
				resp, err := call(context.Background(), kwargs)
				if err != nil {
					t.Fatal(err)
				}
				if got := auxActualContent(t, resp); got != "The task is complete." {
					t.Fatalf("content %q", got)
				}
				got := requests()
				if len(got) < 2 || got[0].Payload["model"] != "unavailable-model" || got[len(got)-1].Payload["model"] != "test-model" {
					t.Fatalf("requests %v", got)
				}
				for _, req := range got {
					if req.Path != "/v1/chat/completions" {
						t.Fatalf("request reached %s", req.Path)
					}
				}
			})
		}
	}
}

// Forced Responses calls on Actual routes raise before any request from the sync and async auxiliary wrappers
func TestActualRejectsForcedResponsesBeforeHTTP(t *testing.T) {
	for _, row := range []struct {
		provider string
		hosted   bool
	}{{"actual", false}, {"aci", false}, {"custom", true}, {"custom:actual-relay", true}} {
		for _, sendSite := range []string{"auxiliary", "async_auxiliary"} {
			t.Run(fmt.Sprintf("provider=%s/hosted=%v/send_site=%s", row.provider, row.hosted, sendSite), func(t *testing.T) {
				baseURL, requests := auxActualEndpoint(t)
				if row.hosted {
					baseURL = strings.Replace(baseURL, "127.0.0.1", "api.actual.inc", 1)
				}
				baseURL += "/v1"
				h := t.TempDir()
				t.Setenv("ICEBATH_HOME", h)
				writeConfig(t, h, map[string]any{
					"model":     map[string]any{"provider": row.provider, "base_url": baseURL, "default": "test-model"},
					"providers": map[string]any{"actual-relay": map[string]any{"base_url": baseURL, "transport": "codex_responses"}},
				})
				ctx := context.Background()
				client, model, err := ResolveProviderClient(ctx, row.provider, "test-model", ProviderClientOptions{
					ExplicitBaseURL: baseURL, ExplicitAPIKey: "actual-test-key", APIMode: "codex_responses"})
				if err != nil || client == nil {
					t.Fatalf("resolve %v %v", client, err)
				}
				defer client.Close()
				real, ok := client.(auxResponsesClient)
				if !ok {
					t.Fatalf("resolved %T has no Responses surface", client)
				}
				var wrapper AuxClient = NewCodexAuxiliaryClient(real, model)
				if sendSite == "async_auxiliary" {
					wrapper = NewAsyncCodexAuxiliaryClient(wrapper.(*CodexAuxiliaryClient))
				}
				initial := requests()
				_, err = wrapper.CreateChatCompletion(ctx, map[string]any{"model": model,
					"messages": []any{map[string]any{"role": "user", "content": "Reply briefly."}}})
				var verr *ValueError
				if !errors.As(err, &verr) || !regexp.MustCompile(`Actual.*Chat Completions`).MatchString(err.Error()) {
					t.Fatalf("forced Responses call returned %v, want the Actual ValueError", err)
				}
				if got := requests(); len(got) != len(initial) {
					t.Fatalf("requests reached the endpoint %v", got)
				}
			})
		}
	}
}
