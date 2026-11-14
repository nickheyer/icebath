package provider

import (
	"errors"
	"testing"
)

func TestPreloadJiterNativeExtensionLoadsSdkParserDependency(t *testing.T) {
	if !PreloadJiterNativeExtension() {
		t.Fatal("preload reported failure")
	}
	jiterPreloadMu.Lock()
	loaded := jiterLoadedModules["jiter.jiter"]
	jiterPreloadMu.Unlock()
	if !loaded {
		t.Fatal("jiter.jiter not recorded as a loaded module")
	}
}

func TestPreloadJiterNativeExtensionIsBestEffort(t *testing.T) {
	jiterPreloadMu.Lock()
	prevLoaded, prevErr, prevImport := jiterPreloaded, jiterPreloadError, jiterImportModule
	jiterPreloaded = false
	jiterImportModule = func(name string) error {
		if name != "jiter.jiter" {
			t.Errorf("import of %q, want jiter.jiter first", name)
		}
		return &ModuleNotFoundError{Name: name}
	}
	jiterPreloadMu.Unlock()
	t.Cleanup(func() {
		jiterPreloadMu.Lock()
		jiterPreloaded, jiterPreloadError, jiterImportModule = prevLoaded, prevErr, prevImport
		jiterPreloadMu.Unlock()
	})
	if PreloadJiterNativeExtension() {
		t.Fatal("preload reported success through a failing import")
	}
	jiterPreloadMu.Lock()
	defer jiterPreloadMu.Unlock()
	var missing *ModuleNotFoundError
	if jiterPreloaded || !errors.As(jiterPreloadError, &missing) || missing.Name != "jiter.jiter" {
		t.Fatalf("preloaded=%v error=%v", jiterPreloaded, jiterPreloadError)
	}
}
