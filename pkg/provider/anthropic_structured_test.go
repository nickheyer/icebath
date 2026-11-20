package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Sends one request carrying the extra body and top level response format and returns the wire body
func anthCaptureBody(t *testing.T, extraBody map[string]any, responseFormat map[string]any, reasoning Reasoning) map[string]any {
	t.Helper()
	srv := newAnthServer(t)
	req := anthUserRequest("claude-sonnet-4-6", "hi")
	req.MaxTokens, req.ExtraBody, req.ResponseFormat, req.Reasoning = 64, extraBody, responseFormat, reasoning
	if _, err := srv.client(srv.route("", "sk-ant-api03-test")).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := srv.last().Body
	if _, has := body["response_format"]; has {
		t.Fatalf("raw response_format reached the wire: %v", body)
	}
	if eb, ok := body["extra_body"].(map[string]any); ok && eb["response_format"] != nil {
		t.Fatalf("raw response_format reached the wire extra_body: %v", body)
	}
	return body
}

func anthFormat(body map[string]any) any {
	cfg, _ := body["output_config"].(map[string]any)
	return cfg["format"]
}

func TestAnthropicJSONSchemaResponseFormatUsesNativeOutputConfig(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"}}
	body := anthCaptureBody(t, map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "thread_title", "schema": schema, "strict": false}}}, nil, Reasoning{})
	anthMustEqualJSON(t, anthFormat(body), map[string]any{"type": "json_schema", "schema": schema})
}

func TestAnthropicJSONObjectResponseFormatUsesPermissiveSchema(t *testing.T) {
	body := anthCaptureBody(t, map[string]any{"response_format": map[string]any{"type": "json_object"}}, nil, Reasoning{})
	anthMustEqualJSON(t, anthFormat(body), map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}})
}

func TestAnthropicResponseFormatMergesWithAdaptiveThinkingEffort(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	on := true
	body := anthCaptureBody(t, map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"schema": schema}}}, nil, Reasoning{Enabled: &on, Effort: "high"})
	anthMustEqualJSON(t, body["output_config"], map[string]any{"effort": "high", "format": map[string]any{"type": "json_schema", "schema": schema}})
	anthMustEqualJSON(t, body["thinking"], map[string]any{"type": "adaptive", "display": "summarized"})
}

func TestAnthropicUnrelatedExtraBodyKeysStillPassThrough(t *testing.T) {
	body := anthCaptureBody(t, map[string]any{"response_format": map[string]any{"type": "json_object"}, "metadata": map[string]any{"user_id": "thread-autotitle"}, "vendor_option": true}, nil, Reasoning{})
	anthMustEqualJSON(t, body["metadata"], map[string]any{"user_id": "thread-autotitle"})
	if body["vendor_option"] != true {
		t.Fatalf("vendor option lost: %v", body)
	}
}

func TestAnthropicTopLevelResponseFormatIsTranslatedNotDropped(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"}}
	body := anthCaptureBody(t, nil, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "session_title", "schema": schema}}, Reasoning{})
	anthMustEqualJSON(t, anthFormat(body), map[string]any{"type": "json_schema", "schema": schema})
}

func TestAnthropicExtraBodyResponseFormatWinsOverTopLevel(t *testing.T) {
	ebSchema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	topSchema := map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}
	body := anthCaptureBody(t, map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"schema": ebSchema}}},
		map[string]any{"type": "json_schema", "json_schema": map[string]any{"schema": topSchema}}, Reasoning{})
	anthMustEqualJSON(t, anthFormat(body), map[string]any{"type": "json_schema", "schema": ebSchema})
}

func anthInstallCLI(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Only the given executables answer the version probe, any other probed path fails the test
func anthCLIVersions(t *testing.T, prefixes []string, versions map[string]string) {
	t.Helper()
	prevPrefixes, prevRun := anthClaudeCodePrefixes, anthRunVersion
	anthClaudeCodePrefixes = prefixes
	anthRunVersion = func(path string) (string, error) {
		v, ok := versions[path]
		if !ok {
			t.Errorf("probed unexpected path %s", path)
		}
		return v + " (Claude Code)", nil
	}
	t.Cleanup(func() { anthClaudeCodePrefixes, anthRunVersion = prevPrefixes, prevRun })
}

func TestAnthropicGUILaunchReportsInstalledVersionNotFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PATH", filepath.Join(tmp, "empty-path"))
	installed := anthInstallCLI(t, filepath.Join(tmp, "prefix"), "claude")
	anthCLIVersions(t, []string{filepath.Join(tmp, "prefix")}, map[string]string{installed: "2.1.276"})
	if got := anthDetectClaudeCodeVersion(); got != "2.1.276" || got == anthClaudeCodeVersionFallback {
		t.Fatalf("detected %q", got)
	}
}

func TestAnthropicPathHitIsProbedFirst(t *testing.T) {
	tmp := t.TempDir()
	onPath := anthInstallCLI(t, filepath.Join(tmp, "path"), "claude-code")
	stale := anthInstallCLI(t, filepath.Join(tmp, "prefix"), "claude")
	t.Setenv("PATH", filepath.Join(tmp, "path"))
	anthCLIVersions(t, []string{filepath.Join(tmp, "prefix")}, map[string]string{onPath: "2.1.400", stale: "2.1.100"})
	if got := anthDetectClaudeCodeVersion(); got != "2.1.400" {
		t.Fatalf("PATH hit must win, got %q", got)
	}
}
