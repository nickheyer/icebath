package acp

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
)

func fileURI(p string) string {
	if runtime.GOOS == "windows" {
		return "file:///" + filepath.ToSlash(p)
	}
	return "file://" + p
}

func TestAcpImageBlocksConvertToOpenaiMultimodalContent(t *testing.T) {
	content, err := ContentBlocksToOpenAIUserContent([]pyacp.ContentBlock{
		&pyacp.TextContentBlock{Text: "What is in this image?"},
		&pyacp.ImageContentBlock{Data: "aGVsbG8=", MimeType: "image/png"},
	})
	want := []map[string]any{
		{"type": "text", "text": "What is in this image?"},
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
	}
	if err != nil || !reflect.DeepEqual(content, want) {
		t.Fatalf("content %#v err %v", content, err)
	}
}

func TestTextOnlyAcpBlocksStayStringForLegacyPromptPath(t *testing.T) {
	content, err := ContentBlocksToOpenAIUserContent([]pyacp.ContentBlock{&pyacp.TextContentBlock{Text: "/help"}})
	if err != nil || content != "/help" {
		t.Fatalf("content %#v", content)
	}
}

func TestAcpResourceLinkFileIsInlinedAsText(t *testing.T) {
	attached := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(attached, []byte("# Notes\n\nAttached file body"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := fileURI(attached)
	content, err := ContentBlocksToOpenAIUserContent([]pyacp.ContentBlock{
		&pyacp.TextContentBlock{Text: "Please read this file"},
		&pyacp.ResourceContentBlock{Name: "notes.md", Title: pyacp.Ptr("Project notes"), URI: uri, MimeType: pyacp.Ptr("text/markdown")},
	})
	want := "Please read this file\n[Attached file: Project notes (notes.md)]\nURI: " + uri + "\n\n# Notes\n\nAttached file body"
	if err != nil || content != want {
		t.Fatalf("content %#v", content)
	}
}

func TestNativeDrivePathAndFileUriReferToSameAttachment(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	path := filepath.Join(t.TempDir(), "notes with spaces.md")
	if err := os.WriteFile(path, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _, _ := pathFromFileURI(path)
	b, _, _ := pathFromFileURI("file:///" + filepath.ToSlash(filepath.Dir(path)) + "/notes%20with%20spaces.md")
	if filepath.Clean(a) != filepath.Clean(b) || filepath.Clean(a) != path {
		t.Fatalf("a %q b %q want %q", a, b, path)
	}
}
