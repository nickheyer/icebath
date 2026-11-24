package state

import (
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestClientRequestDropsOwnerStampedAndDerivedFields(t *testing.T) {
	cases := []struct {
		name, request string
		derived       any
		want          *jsonutil.Object
	}{
		{"pre provenance alias", `{"model":"gpt-x","model_requested":"fast","base_url":"https://a","desktop_local":true,"route_derived":["model"],"cwd":"/w"}`, nil,
			jsonutil.ObjectOf("model_requested", "fast", "cwd", "/w")},
		{"derived fields", `{"model":"gpt-x","model_requested":"fast","base_url":"https://a","api_mode":"chat","route_derived":["api_mode"]}`, []any{"api_mode"},
			jsonutil.ObjectOf("model_requested", "fast", "base_url", "https://a", "route_derived", []any{"api_mode"})},
		{"no alias", `{"model":"gpt-x","base_url":"https://a","desktop_local":false}`, nil,
			jsonutil.ObjectOf("model", "gpt-x", "base_url", "https://a")},
	}
	for _, c := range cases {
		got, err := ClientRequest(c.request, c.derived)
		if err != nil || !anyutil.Equal(got, c.want) {
			t.Fatalf("%s: %v %v", c.name, got, err)
		}
	}
	if _, err := ClientRequest(`[1]`, nil); err == nil {
		t.Fatal("a list request accepted")
	}
	if _, err := ClientRequest(`{"model_requested":"x"}`, int64(3)); err == nil {
		t.Fatal("a non iterable provenance accepted")
	}
}

func TestSameCreateRequestComparesWhatTheClientSent(t *testing.T) {
	cases := []struct {
		name, saved, retry string
		want               bool
	}{
		{"env rotation changed the resolved model", `{"model":"gpt-1","model_requested":"fast","base_url":"https://old","route_derived":["base_url"]}`,
			`{"model":"gpt-2","model_requested":"fast","base_url":"https://new","route_derived":["base_url"]}`, true},
		{"different alias", `{"model":"gpt-1","model_requested":"fast","route_derived":[]}`,
			`{"model":"gpt-1","model_requested":"slow","route_derived":[]}`, false},
		{"owner stamped desktop flag", `{"model":"m","desktop_local":true}`, `{"model":"m"}`, true},
		{"receipt from before model_requested", `{"model":"same"}`, `{"model":"same","model_requested":"same","route_derived":["base_url"]}`, true},
		{"changed cwd", `{"model":"m","cwd":"/a"}`, `{"model":"m","cwd":"/b"}`, false},
	}
	for _, c := range cases {
		got, err := SameCreateRequest(c.saved, c.retry)
		if err != nil || got != c.want {
			t.Fatalf("%s: %v %v", c.name, got, err)
		}
	}
}

func TestSameCreateRetryJudgesTheClientEnvByTheSavedRules(t *testing.T) {
	policy := jsonutil.ObjectOf("request_json", `{"model":"m"}`, "client_env_json", nil)
	if same, err := SameCreateRetry(policy, `{"model":"m"}`, jsonutil.NewObject()); err != nil || !same {
		t.Fatalf("plain retry %v %v", same, err)
	}
	if same, err := SameCreateRetry(policy, `{"model":"m"}`, jsonutil.ObjectOf("client_env", jsonutil.ObjectOf("A", "1"))); err != nil || same {
		t.Fatalf("retry adding a client env %v %v", same, err)
	}
	if same, err := SameCreateRetry(policy, `{"model":"other"}`, jsonutil.NewObject()); err != nil || same {
		t.Fatalf("changed request %v %v", same, err)
	}
	if _, err := SameCreateRetry(jsonutil.NewObject(), `{"model":"m"}`, jsonutil.NewObject()); err == nil {
		t.Fatal("policy without request_json accepted")
	}
}
