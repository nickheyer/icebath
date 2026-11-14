package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch home A with a local backend and routed profile B with a docker backend, multiplex on
func twoInboundHomes(t *testing.T) (string, string) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, ".icebath")
	b := filepath.Join(a, "profiles", "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv("ICEBATH_HOME", a)
	for dir, backend := range map[string]string{a: "local", b: "docker"} {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("terminal:\n  backend: "+backend+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return a, b
}

// File an adapter leaves under the cache kind directory of a home
func adapterCached(t *testing.T, homeDir, kind, name string) string {
	path := filepath.Join(homeDir, "cache", kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("\xff\xd8\xff\xe0 jpeg")
	if kind == "documents" {
		body = []byte("%PDF-1.4 x")
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRoutedTurnRehomesAdapterCachedAttachmentsABA(t *testing.T) {
	a, b := twoInboundHomes(t)
	doc := adapterCached(t, a, "documents", "doc_0123456789ab_report.pdf")
	img := adapterCached(t, a, "images", "img_1.jpg")
	event := &gateway.MessageEvent{
		Text: "[document 'report.pdf' saved at: " + doc + "]", MessageType: gateway.MessageTypeDocument,
		MediaURLs: []string{doc, img}, MediaTypes: []string{"application/pdf", "image/jpeg"},
	}
	const sandboxDoc = "/root/.icebath/cache/documents/doc_0123456789ab_report.pdf"
	ctx := gateway.ProfileRuntimeScope(context.Background(), b, false)
	rehomeInboundMedia(ctx, event)
	note := prependInboundDocumentNotes(ctx, event, "")
	if !strings.Contains(note, sandboxDoc) {
		t.Fatalf("note %q", note)
	}
	env := inboundToolsEnv(ctx)
	host := tools.FromAgentVisibleCachePath(env, sandboxDoc, "")
	if info, err := os.Stat(host); err != nil || !info.Mode().IsRegular() || host != filepath.Join(b, "cache", "documents", "doc_0123456789ab_report.pdf") {
		t.Fatalf("host %q err %v", host, err)
	}
	mounted := map[string]bool{}
	for _, m := range tools.GetCacheDirectoryMounts(env, "") {
		mounted[m.HostPath] = true
	}
	if !mounted[filepath.Dir(host)] || !mounted[filepath.Join(b, "cache", "images")] {
		t.Fatalf("mounts %v", mounted)
	}
	if !strings.Contains(event.Text, sandboxDoc) {
		t.Fatalf("baked note %q", event.Text)
	}
	want := []string{filepath.Join(b, "cache", "documents", "doc_0123456789ab_report.pdf"), filepath.Join(b, "cache", "images", "img_1.jpg")}
	if !reflect.DeepEqual(event.MediaURLs, want) {
		t.Fatalf("media urls %v", event.MediaURLs)
	}
	for _, gone := range []string{doc, img} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", gone)
		}
	}
	if info, err := os.Stat(filepath.Join(b, "cache", "images", "img_1.jpg")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("moved image %v", err)
	}

	docA := adapterCached(t, a, "documents", "doc_aaaaaaaaaaaa_own.pdf")
	own := &gateway.MessageEvent{MessageType: gateway.MessageTypeDocument, MediaURLs: []string{docA}, MediaTypes: []string{"application/pdf"}}
	rehomeInboundMedia(gateway.ProfileRuntimeScope(context.Background(), a, false), own)
	if !reflect.DeepEqual(own.MediaURLs, []string{docA}) {
		t.Fatalf("own %v", own.MediaURLs)
	}
	if _, err := os.Stat(docA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "cache", "documents", "doc_aaaaaaaaaaaa_own.pdf")); !os.IsNotExist(err) {
		t.Fatal("launch profile file bled into the routed profile")
	}
}
