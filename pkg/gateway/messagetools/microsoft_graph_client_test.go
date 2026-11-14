package messagetools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Provider whose cached token never expires so no token request is made
func makeCachedProvider() *MicrosoftGraphTokenProvider {
	p := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"))
	p.cached.Store(&CachedAccessToken{AccessToken: "cached-token", ExpiresAt: 9_999_999_999, TokenType: "Bearer"})
	return p
}

func graphJSON(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGraphClientAttachesBearerTokenHeader(t *testing.T) {
	var auth []string
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			auth = append(auth, req.Header.Get("Authorization"))
			if req.URL.String() != "https://graph.microsoft.com/v1.0/me" || req.Header.Get("User-Agent") != "Icebath-Agent/graph-client" {
				t.Errorf("request %s %v", req.URL, req.Header)
			}
			return mockResponse(req, 200, `{"ok": true}`), nil
		}),
	})
	payload, err := client.GetJSON(context.Background(), "/me", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if graphJSON(t, payload) != `{"ok": true}` || len(auth) != 1 || auth[0] != "Bearer cached-token" {
		t.Fatalf("payload %v auth %v", payload, auth)
	}
}

func TestGraphClientRetriesOnRateLimitAndUsesRetryAfter(t *testing.T) {
	calls := 0
	var sleeps []float64
	two := 2
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return mockResponse(req, 429, `{"error": {"code": "TooManyRequests", "message": "slow down"}}`, "Retry-After", "3"), nil
			}
			return mockResponse(req, 200, `{"ok": true}`), nil
		}),
		Sleep:      func(ctx context.Context, seconds float64) error { sleeps = append(sleeps, seconds); return nil },
		MaxRetries: &two,
	})
	payload, err := client.GetJSON(context.Background(), "/me", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if graphJSON(t, payload) != `{"ok": true}` || calls != 2 || len(sleeps) != 1 || sleeps[0] != 3.0 {
		t.Fatalf("payload %v calls %d sleeps %v", payload, calls, sleeps)
	}
}

func TestGraphClientDownloadAcceptsStreamContentByDefault(t *testing.T) {
	var accept []string
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			accept = append(accept, req.Header.Get("Accept"))
			return mockResponse(req, 200, "recording-bytes", "content-type", "video/mp4"), nil
		}),
	})
	destination := filepath.Join(t.TempDir(), "nested", "recording.mp4")
	result, err := client.DownloadToFile(context.Background(), "/recordings/recording-1/content", destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(destination)
	if len(accept) != 1 || accept[0] != "*/*" || string(data) != "recording-bytes" || result["content_type"] != "video/mp4" {
		t.Fatalf("accept %v data %q result %v", accept, data, result)
	}
	if result["path"] != destination || result["size_bytes"] != int64(15) {
		t.Fatalf("result %v", result)
	}
	if _, err := os.Stat(destination + ".part"); !os.IsNotExist(err) {
		t.Fatalf("part file left behind: %v", err)
	}
}

func TestGraphClientInvalidJSONResponseRaisesClientError(t *testing.T) {
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			return mockResponse(req, 200, "not-json", "content-type", "application/json"), nil
		}),
	})
	_, err := client.GetJSON(context.Background(), "/me", map[string]any{"$top": int64(5)}, nil)
	var ce *MicrosoftGraphClientError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "Microsoft Graph response was not valid JSON for GET https://graph.microsoft.com/v1.0/me?%24top=5" {
		t.Fatalf("err = %q", err)
	}
}

func TestGraphAPIErrorTextAndClassMatching(t *testing.T) {
	bare := &MicrosoftGraphAPIError{StatusCode: 404}
	if bare.Error() != "Microsoft Graph API error 404 for  : " {
		t.Fatalf("bare = %q", bare.Error())
	}
	var ce *MicrosoftGraphClientError
	if !errors.As(error(bare), &ce) {
		t.Fatal("api error is not a client error")
	}
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			return mockResponse(req, 403, `{"error": {"code": "Forbidden", "message": "Insufficient privileges"}}`, "Retry-After", "7"), nil
		}),
	})
	_, err := client.PostJSON(context.Background(), "subscriptions", map[string]any{"a": "é"}, nil)
	var api *MicrosoftGraphAPIError
	if !errors.As(err, &api) || api.StatusCode != 403 || *api.RetryAfterSeconds != 7 ||
		err.Error() != "Microsoft Graph API error 403 for POST https://graph.microsoft.com/v1.0/subscriptions: Forbidden: Insufficient privileges" {
		t.Fatalf("err = %v", err)
	}
	if graphJSON(t, api.Payload) != `{"error": {"code": "Forbidden", "message": "Insufficient privileges"}}` {
		t.Fatalf("payload = %v", api.Payload)
	}
}

func TestGraphClientRefreshesTokenAfter401AndBacksOffOnServerErrors(t *testing.T) {
	tokens := 0
	provider := NewMicrosoftGraphTokenProvider(NewGraphCredentials("tenant", "client", "secret"), MicrosoftGraphTokenProviderOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			tokens++
			return mockResponse(req, 200, `{"access_token": "tok-`+string(rune('0'+tokens))+`", "expires_in": 3600}`), nil
		}),
	})
	var seen []string
	var sleeps []float64
	statuses := []int{401, 503, 200}
	client := NewMicrosoftGraphClient(provider, MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			seen = append(seen, req.Header.Get("Authorization")+" "+req.Header.Get("Content-Type")+" "+req.Header.Get("X-Extra"))
			body, _ := io.ReadAll(req.Body)
			if string(body) != `{"name":"é","n":[1,2]}` {
				t.Errorf("body = %q", body)
			}
			st := statuses[len(seen)-1]
			if st == 200 {
				return mockResponse(req, 204, ""), nil
			}
			return mockResponse(req, st, "oops"), nil
		}),
		Sleep: func(ctx context.Context, seconds float64) error { sleeps = append(sleeps, seconds); return nil },
	})
	body := pyjson.NewObject()
	body.Set("name", "é")
	body.Set("n", []any{int64(1), int64(2)})
	got, err := client.PatchJSON(context.Background(), "/subscriptions/1", body, map[string]string{"X-Extra": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if graphJSON(t, got) != `{}` || tokens != 2 || len(sleeps) != 2 || sleeps[0] != 0.5 || sleeps[1] != 1.0 {
		t.Fatalf("got %v tokens %d sleeps %v", got, tokens, sleeps)
	}
	want := []string{"Bearer tok-1 application/json yes", "Bearer tok-2 application/json yes", "Bearer tok-2 application/json yes"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("seen %v", seen)
	}
}

func TestGraphClientExhaustsRetriesOnTransportFailures(t *testing.T) {
	zero := 1
	attempts := 0
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("connection reset")
		}),
		Sleep:      func(ctx context.Context, seconds float64) error { return nil },
		MaxRetries: &zero,
	})
	_, err := client.GetJSON(context.Background(), "/me", nil, nil)
	var ce *MicrosoftGraphClientError
	if !errors.As(err, &ce) || attempts != 2 || !strings.HasPrefix(err.Error(), "Microsoft Graph request failed for GET https://graph.microsoft.com/v1.0/me: ") {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	statusClient := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) { return mockResponse(req, 400, "  bad  "), nil }),
	})
	_, err = statusClient.GetJSON(context.Background(), "/me", nil, nil)
	if err == nil || err.Error() != "Microsoft Graph API error 400 for GET https://graph.microsoft.com/v1.0/me: bad" {
		t.Fatalf("err = %v", err)
	}
}

func TestGraphClientCollectsPagesAndDeletes(t *testing.T) {
	var urls []string
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			urls = append(urls, req.Method+" "+req.URL.String())
			switch {
			case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/gone"):
				return mockResponse(req, 204, ""), nil
			case req.Method == http.MethodDelete:
				return mockResponse(req, 200, `{"status": "removed"}`), nil
			case req.URL.Query().Get("page") == "2":
				return mockResponse(req, 200, `{"value": [3]}`), nil
			case strings.HasSuffix(req.URL.Path, "/bad"):
				return mockResponse(req, 200, `[1]`), nil
			}
			return mockResponse(req, 200, `{"value": [1, 2], "@odata.nextLink": "https://graph.microsoft.com/v1.0/items?page=2"}`), nil
		}),
	})
	items, err := client.CollectPaginated(context.Background(), "items", map[string]any{"$top": int64(2), "flag": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if graphJSON(t, items) != `[1, 2, 3]` {
		t.Fatalf("items %v", items)
	}
	if urls[0] != "GET https://graph.microsoft.com/v1.0/items?%24top=2&flag=true" || urls[1] != "GET https://graph.microsoft.com/v1.0/items?page=2" {
		t.Fatalf("urls %v", urls)
	}
	if _, err := client.CollectPaginated(context.Background(), "/bad", nil, nil); err == nil ||
		err.Error() != "Expected paginated Graph response dict, got list." {
		t.Fatalf("err = %v", err)
	}
	gone, err := client.Delete(context.Background(), "/gone", nil)
	if err != nil || gone["deleted"] != true || gone["status_code"] != int64(204) {
		t.Fatalf("gone %v err %v", gone, err)
	}
	removed, err := client.Delete(context.Background(), "/subscriptions/1", nil)
	if err != nil || removed["status"] != "removed" {
		t.Fatalf("removed %v err %v", removed, err)
	}
}

func TestGraphClientDownloadErrorLeavesNoPartFile(t *testing.T) {
	client := NewMicrosoftGraphClient(makeCachedProvider(), MicrosoftGraphClientOptions{
		Transport: mockTransport(func(req *http.Request) (*http.Response, error) {
			return mockResponse(req, 404, `{"error": {"code": "itemNotFound", "message": "missing"}}`), nil
		}),
	})
	destination := filepath.Join(t.TempDir(), "rec.mp4")
	_, err := client.DownloadToFile(context.Background(), "https://example.test/rec", destination, nil)
	var api *MicrosoftGraphAPIError
	if !errors.As(err, &api) || api.StatusCode != 404 || err.Error() != "Microsoft Graph API error 404 for GET https://example.test/rec: itemNotFound: missing" {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists: %v", err)
	}
}
