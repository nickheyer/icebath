package state

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Exported three message session the save helpers render
func saveSession() *pyjson.Object {
	return pyjson.ObjectOf("id", "20260814_abc123", "title", "Test Session", "model", "test-model", "started_at", 1755100000, "messages", []any{
		pyjson.ObjectOf("role", "user", "content", "Hello"), pyjson.ObjectOf("role", "assistant", "content", "Hi there"),
		pyjson.ObjectOf("role", "tool", "content", "tool output")})
}

// Whether an error stands for a Python ValueError
func isSaveValueError(err error) bool {
	var typed *pyint.Error
	return errors.As(err, &typed) && typed.Kind == "ValueError"
}

func TestNormalizeSaveFormatAliases(t *testing.T) {
	for token, want := range map[string]string{"markdown": "md", "MD": "md", "HTML": "html", "snapshot": "json"} {
		if got, err := NormalizeSaveFormat(token); err != nil || got != want {
			t.Fatalf("%q normalized to %q %v", token, got, err)
		}
	}
}

func TestNormalizeSaveFormatUnknownRaises(t *testing.T) {
	if _, err := NormalizeSaveFormat("pdf"); !isSaveValueError(err) {
		t.Fatalf("err %v", err)
	}
}

func TestNormalizeSaveFormatAllDeclaredFormatsNormalizeToThemselves(t *testing.T) {
	if len(SaveFormats) != 3 {
		t.Fatalf("save formats %v", SaveFormats)
	}
	for _, format := range SaveFormats {
		if got, err := NormalizeSaveFormat(format); err != nil || got != format {
			t.Fatalf("%q normalized to %q %v", format, got, err)
		}
	}
}

func TestRenderSessionForSaveJSONRoundTrips(t *testing.T) {
	out, err := RenderSessionForSave(saveSession(), "json")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(out), &data); err != nil || data["id"] != "20260814_abc123" || len(data["messages"].([]any)) != 3 {
		t.Fatalf("json %v %s", err, out)
	}
}

func TestRenderSessionForSaveMarkdownContainsMessages(t *testing.T) {
	out, err := RenderSessionForSave(saveSession(), "md")
	if err != nil || !strings.Contains(out, "Hello") || !strings.Contains(out, "Hi there") {
		t.Fatalf("markdown %v %s", err, out)
	}
}

func TestRenderSessionForSaveHTMLIsStandaloneDocument(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("html save panicked: %v", p)
		}
	}()
	out, err := RenderSessionForSave(saveSession(), "html")
	if err != nil || !strings.HasPrefix(strings.ToLower(strings.TrimLeft(out, " \t\r\n")), "<!doctype html") || !strings.Contains(out, "Hello") {
		t.Fatalf("html %v %s", err, out)
	}
}

func TestRenderSessionForSaveUnknownFormatRaises(t *testing.T) {
	if _, err := RenderSessionForSave(saveSession(), "pdf"); !isSaveValueError(err) {
		t.Fatalf("err %v", err)
	}
}

func TestDefaultSaveFilenameHostileSessionIDSanitized(t *testing.T) {
	name := DefaultSaveFilename("../../etc/passwd", "json")
	if strings.Contains(name, "/") || strings.Contains(strings.ReplaceAll(name, "etcpasswd", ""), "..") {
		t.Fatalf("file name %q", name)
	}
}
