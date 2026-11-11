package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

func sdkTypeErrorText(t *testing.T, err error) string {
	t.Helper()
	var builtin *plugins.HostBuiltinError
	if !errors.As(err, &builtin) || builtin.Type != "TypeError" {
		t.Fatalf("want TypeError, got %#v", err)
	}
	return builtin.Message
}

func TestServePluginSDKRejectsUnknownOp(t *testing.T) {
	_, err := ServePluginSDK(context.Background(), t.TempDir(), "nope", nil, nil)
	var builtin *plugins.HostBuiltinError
	if !errors.As(err, &builtin) || builtin.Type != "ValueError" || builtin.Message != "unknown plugin SDK call 'nope'" {
		t.Fatalf("%#v", err)
	}
}

func TestServePluginSDKBindsLikeAPythonSignature(t *testing.T) {
	ctx, h := context.Background(), t.TempDir()
	_, err := ServePluginSDK(ctx, h, "chat_catalog_ids", []any{1, 2}, nil)
	if got := sdkTypeErrorText(t, err); got != "chat_catalog_ids() takes 1 positional argument but 2 were given" {
		t.Fatal(got)
	}
	_, err = ServePluginSDK(ctx, h, "chat_catalog_ids", nil, nil)
	if got := sdkTypeErrorText(t, err); got != "chat_catalog_ids() missing 1 required positional argument: 'items'" {
		t.Fatal(got)
	}
	_, err = ServePluginSDK(ctx, h, "get_version_info", nil, map[string]any{"x": 1})
	if got := sdkTypeErrorText(t, err); got != "get_version_info() got an unexpected keyword argument 'x'" {
		t.Fatal(got)
	}
	_, err = ServePluginSDK(ctx, h, "chat_catalog_ids", []any{1}, map[string]any{"items": 2})
	if got := sdkTypeErrorText(t, err); got != "chat_catalog_ids() got multiple values for argument 'items'" {
		t.Fatal(got)
	}
	_, err = ServePluginSDK(ctx, h, "provider_media.save_url", []any{"image", "http://127.0.0.1/x"}, map[string]any{"prefix": "p"})
	want := "save_url() missing 8 required keyword-only arguments: 'timeout', 'max_bytes', 'chunk_size', 'content_types', 'url_extensions', 'default_extension', 'label', and 'empty_error'"
	if got := sdkTypeErrorText(t, err); got != want {
		t.Fatal(got)
	}
}

func TestServePluginSDKAnswersCatalogSecretSourceVersionAndPool(t *testing.T) {
	ctx, h := context.Background(), t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ids, err := ServePluginSDK(ctx, h, "chat_catalog_ids", []any{[]any{map[string]any{"id": "alpha"}, map[string]any{"id": "beta"}}}, nil)
	if err != nil || !slices.Equal(ids.([]any), []any{"alpha", "beta"}) {
		t.Fatalf("%v %v", ids, err)
	}
	suffix, err := ServePluginSDK(ctx, h, "format_secret_source_suffix", []any{"SOME_UNSET_KEY"}, nil)
	if err != nil || suffix != "" {
		t.Fatalf("%v %v", suffix, err)
	}
	info, err := ServePluginSDK(ctx, h, "get_version_info", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := info.(map[string]any)
	for _, key := range []string{"base_version", "derived_version", "distance", "commit", "branch", "source", "dirty", "commit_date", "distribution"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("missing %s in %v", key, record)
		}
	}
	if record["base_version"] == "" {
		t.Fatalf("empty base version %v", record)
	}
	pool, err := ServePluginSDK(ctx, h, "read_credential_pool", nil, nil)
	if err != nil || len(pool.(map[string]any)) != 0 {
		t.Fatalf("%v %v", pool, err)
	}
	rows, err := ServePluginSDK(ctx, h, "read_credential_pool", []any{"openrouter"}, nil)
	if err != nil || len(rows.([]any)) != 0 {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestServePluginSDKOpensCredentialedURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Echo", r.Method+" "+r.Header.Get("Authorization"))
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	ctx, h := context.Background(), t.TempDir()
	out, err := ServePluginSDK(ctx, h, "open_credentialed_url", []any{srv.URL + "/models"}, map[string]any{"method": "GET",
		"headers": []any{[]any{"Authorization", "Bearer k"}}, "data": nil, "timeout": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	reply := out.(map[string]any)
	if reply["status"] != 200 || reply["url"] != srv.URL+"/models" || string(reply["body"].([]byte)) != `{"data":[]}` {
		t.Fatalf("%v", reply)
	}
	if !slices.ContainsFunc(reply["headers"].([]any), func(pair any) bool {
		p := pair.([]any)
		return p[0] == "X-Echo" && p[1] == "GET Bearer k"
	}) {
		t.Fatalf("headers %v", reply["headers"])
	}
	_, err = ServePluginSDK(ctx, h, "open_credentialed_url", []any{srv.URL + "/missing"}, map[string]any{"timeout": 5.0})
	var httpErr *sdkHTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != 404 || httpErr.Error() != "HTTP Error 404: Not Found" {
		t.Fatalf("%#v", err)
	}
}
