package provider

import "testing"

func TestNewGeminiNativeClientExportNeedsAKey(t *testing.T) {
	if _, err := NewGeminiNativeClient(Route{BaseURL: "https://generativelanguage.googleapis.com/v1beta"}); err == nil {
		t.Fatal("keyless route accepted")
	}
	c, err := NewGeminiNativeClient(Route{APIKey: "k", BaseURL: "https://generativelanguage.googleapis.com/v1beta"})
	if err != nil || c.(*geminiNativeClient).Route().APIKey != "k" {
		t.Fatalf("client %v err %v", c, err)
	}
}
