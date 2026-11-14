package provider

import "testing"

func TestCodexCloudflareHeadersExportCarriesIcebathOriginator(t *testing.T) {
	headers := CodexCloudflareHeaders("", "https://chatgpt.com/backend-api/codex")
	if headers["originator"] != "icebath-agent" {
		t.Fatalf("codex headers = %v", headers)
	}
}

func TestApplyRequiredCodexHeadersExportMatchesTheUnexported(t *testing.T) {
	base := "https://chatgpt.com/backend-api/codex"
	got := ApplyRequiredCodexHeaders(map[string]string{"X-Custom": "1"}, "tok", base)
	want := applyRequiredCodexHeaders(map[string]string{"X-Custom": "1"}, "tok", base)
	if len(got) != len(want) || got["X-Custom"] != "1" {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q want %q", k, got[k], v)
		}
	}
}
