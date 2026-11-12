package files

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const bridged = `\\wsl.localhost\Ubuntu\home\alex\file`

// Replaces the WSL bridge the read boundaries route through, recording each call
func spyBridge(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := localReadPath
	localReadPath = func(p string) string { calls = append(calls, p); return bridged }
	t.Cleanup(func() { localReadPath = orig })
	return &calls
}

func TestResolveMediaStreamFileHermesMediaResolveLocalFileBridgesTheAlreadyDecodedPathUnchanged(t *testing.T) {
	calls := spyBridge(t)
	result := ResolveMediaStreamFile("/home/alex/My Clips/clip.mp4")
	if !reflect.DeepEqual(*calls, []string{"/home/alex/My Clips/clip.mp4"}) || result != bridged {
		t.Fatal(*calls, result)
	}
}

func TestResolveMediaStreamFilePreservesALeadingSlashSoAbsolutePOSIXPathsStayAbsolute(t *testing.T) {
	calls := spyBridge(t)
	u, err := url.Parse("hermes-media://stream/" + url.PathEscape("/home/alex/clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	filePath, err := url.PathUnescape(strings.TrimLeft(u.EscapedPath(), "/"))
	if err != nil {
		t.Fatal(err)
	}
	ResolveMediaStreamFile(filePath)
	if !reflect.DeepEqual(*calls, []string{"/home/alex/clip.mp4"}) {
		t.Fatal(*calls)
	}
}

func TestResolveMediaStreamFileKeepsPercentSignBytesInFilenamesIntact(t *testing.T) {
	calls := spyBridge(t)
	ResolveMediaStreamFile("/home/alex/100% clip.mp4")
	if !reflect.DeepEqual(*calls, []string{"/home/alex/100% clip.mp4"}) {
		t.Fatal(*calls)
	}
}

func TestResolveMediaStreamFileToleratesNullishInput(t *testing.T) {
	calls := spyBridge(t)
	if result := ResolveMediaStreamFile(nil); result != bridged || !reflect.DeepEqual(*calls, []string{""}) {
		t.Fatal(*calls, result)
	}
}

func TestResolveIpcFileReadPathHermesReadFileDataUrlHermesReadFileTextBridgesTheSuppliedPath(t *testing.T) {
	calls := spyBridge(t)
	if result := ResolveIpcFileReadPath("/home/alex/notes.txt"); result != bridged || !reflect.DeepEqual(*calls, []string{"/home/alex/notes.txt"}) {
		t.Fatal(*calls, result)
	}
}

func TestResolveIpcFileReadPathCoercesANullishPathToAnEmptyStringBeforeBridging(t *testing.T) {
	calls := spyBridge(t)
	ResolveIpcFileReadPath(nil)
	if !reflect.DeepEqual(*calls, []string{""}) {
		t.Fatal(*calls)
	}
}

func TestResolvePreviewTargetPathPreviewFileTargetExpandsAndBridgesAPlainBackendTarget(t *testing.T) {
	calls := spyBridge(t)
	var expanded []string
	result := ResolvePreviewTargetPath("~/docs/readme.md", func(v string) string {
		expanded = append(expanded, v)
		return strings.Replace(v, "~", "/home/alex", 1)
	})
	if !reflect.DeepEqual(expanded, []string{"~/docs/readme.md"}) || !reflect.DeepEqual(*calls, []string{"/home/alex/docs/readme.md"}) || result != bridged {
		t.Fatal(expanded, *calls, result)
	}
}

func TestResolvePreviewTargetPathPassesFileURLsThroughTheBridgeWithoutExpanding(t *testing.T) {
	calls := spyBridge(t)
	expandedCalls := 0
	ResolvePreviewTargetPath("file:///home/alex/report.html", func(v string) string { expandedCalls++; return v })
	if expandedCalls != 0 || !reflect.DeepEqual(*calls, []string{"file:///home/alex/report.html"}) {
		t.Fatal(expandedCalls, *calls)
	}
}
