//go:build windows

package pymimetypes

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

func writeClassKey(t *testing.T, ext, value string, expand bool) {
	root, path := registry.CLASSES_ROOT, ext
	k, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		root, path = registry.CURRENT_USER, `Software\Classes\`+ext
		if k, _, err = registry.CreateKey(root, path, registry.SET_VALUE); err != nil {
			t.Fatal(err)
		}
	}
	if expand {
		k.SetExpandStringValue("Content Type", value)
	} else {
		k.SetStringValue("Content Type", value)
	}
	k.Close()
	t.Cleanup(func() { registry.DeleteKey(root, path) })
}

func TestReadWindowsRegistryContentType(t *testing.T) {
	writeClassKey(t, ".icebathtest", "image/x-icebath-test", false)
	writeClassKey(t, ".icebathexpand", "image/x-expand", true)
	d := New()
	readWindowsRegistry(d)
	if typ, _, _ := d.GuessType(`C:\dir\a.icebathtest`, true); typ != "image/x-icebath-test" {
		t.Fatalf("registry type %q", typ)
	}
	if typ, _, _ := d.GuessType("a.icebathexpand", false); typ != "" {
		t.Fatalf("expand string accepted %q", typ)
	}
}
