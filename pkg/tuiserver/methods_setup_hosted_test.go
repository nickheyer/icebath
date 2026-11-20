package tuiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Reply of one setup flow request, the result or the error object
func hostedCall(t *testing.T, method string, params map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	reply := pyjson.Plain(handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})).(map[string]any)
	result, _ := reply["result"].(map[string]any)
	failure, _ := reply["error"].(map[string]any)
	return result, failure
}

// Polls a flow until it asks a question or ends, collecting the lines it printed
func hostedNext(t *testing.T, job string, after *int, lines *[]string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, failure := hostedCall(t, "setup.hosted.poll", map[string]any{"job": job, "after": *after})
		if failure != nil {
			t.Fatalf("poll failed %v", failure)
		}
		for _, line := range res["lines"].([]any) {
			*lines = append(*lines, line.(string))
		}
		*after += len(res["lines"].([]any))
		if res["done"] == true || res["question"] != nil {
			return res
		}
	}
	t.Fatal("flow neither asked nor ended")
	return nil
}

func TestHostedSetupAsksEachQuestionAndSavesTheModel(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object": "list", "data": [{"id": "gateway-model", "object": "model"}]}`))
	}))
	defer server.Close()
	res, failure := hostedCall(t, "setup.hosted.start", map[string]any{"flow": "model", "entry": "custom"})
	if failure != nil {
		t.Fatalf("start %v", failure)
	}
	job := res["job"].(string)
	after, lines, kinds := 0, []string{}, []string{}
	for {
		res = hostedNext(t, job, &after, &lines)
		if res["done"] == true {
			break
		}
		q := res["question"].(map[string]any)
		kind, title := q["kind"].(string), q["title"].(string)
		kinds = append(kinds, kind)
		answer := map[string]any{"job": job, "question": q["id"]}
		switch {
		case strings.HasPrefix(title, "API base URL"):
			answer["text"] = server.URL + "/v1"
		case strings.HasPrefix(title, "API key"):
			answer["text"] = "sk-gateway-test"
		case kind == "choice":
			answer["index"] = q["default"]
		}
		if _, failure := hostedCall(t, "setup.hosted.answer", answer); failure != nil {
			t.Fatalf("answer %v", failure)
		}
		if _, failure := hostedCall(t, "setup.hosted.answer", answer); failure == nil {
			t.Fatal("a second answer to the same question was taken")
		}
	}
	if res["ok"] != true || res["model"] != "gateway-model" || res["provider"] != "custom" || res["changed"] != true {
		t.Fatalf("end %v lines %v", res, lines)
	}
	if len(kinds) < 3 || kinds[0] != "text" || kinds[1] != "secret" {
		t.Fatalf("questions %v", kinds)
	}
	cfg, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	if model := cfg["model"].(map[string]any); model["default"] != "gateway-model" || model["base_url"] != server.URL+"/v1" {
		t.Fatalf("model %v", model)
	}
	if env, err := os.ReadFile(home.EnvPath(h)); err != nil || !strings.Contains(string(env), "=sk-gateway-test") {
		t.Fatalf("env %q %v", env, err)
	}
	if _, failure := hostedCall(t, "setup.hosted.poll", map[string]any{"job": job, "after": 0}); failure == nil {
		t.Fatal("a finished job must be forgotten once its end is delivered")
	}
}

func TestHostedSetupCancelEndsTheWaitingQuestion(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	res, failure := hostedCall(t, "setup.hosted.start", map[string]any{"flow": "model"})
	if failure != nil {
		t.Fatalf("start %v", failure)
	}
	job := res["job"].(string)
	after, lines := 0, []string{}
	res = hostedNext(t, job, &after, &lines)
	q, _ := res["question"].(map[string]any)
	if q == nil || q["kind"] != "choice" || !strings.Contains(strings.Join(anyText(q["rows"]), "\n"), "Custom endpoint") {
		t.Fatalf("first question %v", res)
	}
	if _, failure := hostedCall(t, "setup.hosted.cancel", map[string]any{"job": job}); failure != nil {
		t.Fatalf("cancel %v", failure)
	}
	res = hostedNext(t, job, &after, &lines)
	if res["done"] != true || res["ok"] != false {
		t.Fatalf("cancelled flow %v", res)
	}
	for _, params := range []map[string]any{{"flow": "wizard"}, {"flow": "model", "entry": "no-such-row"}} {
		if _, failure := hostedCall(t, "setup.hosted.start", params); failure == nil {
			t.Fatalf("start %v was accepted", params)
		}
	}
}

// Text items of a decoded list
func anyText(v any) []string {
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, item := range list {
		out[i], _ = item.(string)
	}
	return out
}
