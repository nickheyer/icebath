package provider

import (
	"encoding/json"
	"testing"
)

func TestQueryOllamaNumCtxReturnsContextFromModelInfo(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	show := map[string]any{"model_info": map[string]any{"llama.context_length": 131072}, "parameters": ""}
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, show)
	ft.on("POST", "http://localhost:11434/api/show", 200, show)
	if n, ok := QueryOllamaNumCtx(ctx, "llama3.1:8b", "http://localhost:11434/v1", ""); !ok || n != 131072 {
		t.Fatalf("got %d %t", n, ok)
	}
}

func TestQueryOllamaNumCtxPrefersExplicitNumCtxFromModelfile(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434", "ollama")
	show := map[string]any{"model_info": map[string]any{"llama.context_length": 131072}, "parameters": "num_ctx 32768\ntemperature 0.7"}
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, show)
	ft.on("POST", "http://localhost:11434/api/show", 200, show)
	if n, ok := QueryOllamaNumCtx(ctx, "custom-model", "http://localhost:11434", ""); !ok || n != 32768 {
		t.Fatalf("got %d %t", n, ok)
	}
}

func TestQueryOllamaNumCtxStripsProviderPrefix(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	show := map[string]any{"model_info": map[string]any{"qwen2.context_length": 32768}, "parameters": ""}
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, show)
	ft.on("POST", "http://localhost:11434/api/show", 200, show)
	n, ok := QueryOllamaNumCtx(ctx, "local:qwen2.5:7b", "http://localhost:11434/v1", "")
	if !ok || n != 32768 {
		t.Fatalf("got %d %t", n, ok)
	}
	ft.mu.Lock()
	body := ft.calls[len(ft.calls)-1].Body
	ft.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil || payload["name"] != "qwen2.5:7b" {
		t.Fatalf("show payload %s %v", body, err)
	}
}

func TestQueryOllamaSupportsVisionReturnsTrueWhenCapabilitiesIncludeVision(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	show := map[string]any{"capabilities": []any{"completion", "vision"}}
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, show)
	ft.on("POST", "http://localhost:11434/api/show", 200, show)
	if supported, known := QueryOllamaSupportsVision(ctx, "gemma4:e2b", "http://localhost:11434/v1", ""); !known || !supported {
		t.Fatalf("got %t %t", supported, known)
	}
}

func TestQueryOllamaSupportsVisionFallsBackToModelInfoVisionBlockCount(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434", "ollama")
	show := map[string]any{"model_info": map[string]any{"gemma3.vision.block_count": 27}}
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, show)
	ft.on("POST", "http://localhost:11434/api/show", 200, show)
	if supported, known := QueryOllamaSupportsVision(ctx, "llava", "http://localhost:11434", ""); !known || !supported {
		t.Fatalf("got %t %t", supported, known)
	}
}

func TestQueryOllamaSupportsVisionReturnsNoneForNonOllamaServer(t *testing.T) {
	ctx, _, _ := metaTest(t)
	seedServerType("http://localhost:8000/v1", "vllm")
	if _, known := QueryOllamaSupportsVision(ctx, "llava", "http://localhost:8000/v1", ""); known {
		t.Fatal("a vllm server answers no Ollama vision verdict")
	}
}
