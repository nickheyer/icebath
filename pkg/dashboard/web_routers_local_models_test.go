package dashboard

import (
	"reflect"
	"testing"
)

func TestLocalModelsRoutesRequireAuth(t *testing.T) {
	unauth := newTestClient(t, dashboardTestApp(t))
	if resp := unauth.get("/api/local-models/status"); resp.Status != 401 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestLocalModelsRouterAnswersThroughTheApplication(t *testing.T) {
	c := newSessionClient(t)
	status := c.get("/api/local-models/status")
	if status.Status != 200 {
		t.Fatalf("status %d %s", status.Status, status.Body)
	}
	data := jsonObject(t, status)
	if _, ok := data["enabled"].(bool); !ok {
		t.Fatalf("enabled %v", data["enabled"])
	}
	if _, ok := data["models"].([]any); !ok {
		t.Fatalf("models %v", data["models"])
	}
	for _, tc := range []struct {
		method, target string
		body           any
		status         int
		want           string
	}{
		{"GET", "/api/local-models/jobs", nil, 200, `{"jobs":[]}`},
		{"GET", "/api/local-models/jobs/nope", nil, 404, `{"detail":"job not found"}`},
		{"POST", "/api/local-models/download", map[string]any{"model_id": "nope"}, 404, `{"detail":"unknown model nope"}`},
		{"POST", "/api/local-models/download/pause", map[string]any{"job_id": "nope"}, 404, `{"detail":"unknown download job"}`},
		{"POST", "/api/local-models/download/resume", map[string]any{"job_id": "nope"}, 404, `{"detail":"unknown download job"}`},
		{"DELETE", "/api/local-models/models/Doomed", nil, 404, `{"detail":"model not found"}`},
		{"POST", "/api/local-models/server", map[string]any{"action": "bogus"}, 400, `{"detail":"action must be 'stop' or 'start'"}`},
		{"POST", "/api/local-models/eject", map[string]any{"model_id": "x"}, 409, `{"detail":"local server is not running"}`},
		{"POST", "/api/local-models/activate", map[string]any{"model_id": "nope"}, 404, `{"detail":"nope is not downloaded"}`},
		{"GET", "/api/local-models/search?q=%20", nil, 200, `{"hits":[]}`},
		{"POST", "/api/local-models/download-browsed", map[string]any{"repo": "o/r", "paths": []any{"a.txt"}}, 422, `{"detail":"no .gguf files given"}`},
		{"POST", "/api/local-models/sideload", map[string]any{"path": "/nonexistent/x.bin"}, 422, `{"detail":"Pick a .gguf model file"}`},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			c.send(tc.method, tc.target, tc.body).expect(t, tc.status, tc.want)
		})
	}
}

func TestLocalModelsRouterValidatesBodiesAndQueries(t *testing.T) {
	c := newSessionClient(t)
	for _, tc := range []struct {
		method, target string
		body           any
		loc            []any
	}{
		{"POST", "/api/local-models/download", map[string]any{}, []any{"body", "model_id"}},
		{"POST", "/api/local-models/download/pause", map[string]any{}, []any{"body", "job_id"}},
		{"POST", "/api/local-models/server", map[string]any{}, []any{"body", "action"}},
		{"POST", "/api/local-models/download-browsed", map[string]any{"repo": "o/r"}, []any{"body", "paths"}},
		{"POST", "/api/local-models/sideload", map[string]any{}, []any{"body", "path"}},
		{"GET", "/api/local-models/search", nil, []any{"query", "q"}},
		{"GET", "/api/local-models/search/files", nil, []any{"query", "repo"}},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			resp := c.send(tc.method, tc.target, tc.body)
			if resp.Status != 422 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			detail := jsonObject(t, resp)["detail"].([]any)[0].(map[string]any)
			if detail["type"] != "missing" || !reflect.DeepEqual(detail["loc"], tc.loc) {
				t.Fatalf("detail %v", detail)
			}
		})
	}
}
