package files

import (
	"testing"
)

func TestRecognizesEveryBareLocalFilesystemPathShape(t *testing.T) {
	for _, v := range []string{"/tmp/report.pdf", "///tmp/foo", "~/logs/desktop.log", `C:\Users\x\a.md`, "C:/Work/report.html", `\\server\share\a.md`} {
		if !LooksLikeLocalFilesystemPath(v) {
			t.Fatal(v)
		}
	}
}

func TestDoesNotClaimURLsRelativeNamesOrSchemeFulInput(t *testing.T) {
	for _, v := range []string{"", "hermes-support-slack.png", "https://example.com", "file:///tmp/report.pdf", "mailto:a@b.c", "//cdn.example.com/img.png"} {
		if LooksLikeLocalFilesystemPath(v) {
			t.Fatal(v)
		}
	}
}

func TestAbsolutizesProtocolRelativeURLsAsHttpsAndLeavesEverythingElseAlone(t *testing.T) {
	if AbsolutizeProtocolRelativeURL("//cdn.example.com/img.png") != "https://cdn.example.com/img.png" ||
		AbsolutizeProtocolRelativeURL("///tmp/foo") != "///tmp/foo" ||
		AbsolutizeProtocolRelativeURL("https://example.com") != "https://example.com" {
		t.Fatal("protocol relative")
	}
}
