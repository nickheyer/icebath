package memory

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClonedMemoryProviderUnreadableConfigMeansNoNotice(t *testing.T) {
	bodies := map[string][]byte{
		"invalid_date":  []byte("memory:\n  provider: honcho\nwhen: 2024-02-30\n"),
		"yaml_error":    []byte("memory: [unclosed\n"),
		"unicode_error": []byte("memory:\n  provider: \xff\n"),
		"bad_int_tag":   []byte("memory:\n  provider: honcho\ncount: !!int abc\n"),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "honcho"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(dir, "config.yaml"), body)
			if got := ClonedMemoryProvider(dir); got != "" {
				t.Fatalf("ClonedMemoryProvider = %q, want none", got)
			}
		})
	}
}

func TestClonedMemoryProviderMissingConfigMeansNoNotice(t *testing.T) {
	if got := ClonedMemoryProvider(t.TempDir()); got != "" {
		t.Fatalf("ClonedMemoryProvider = %q, want none", got)
	}
}

func TestClonedMemoryProviderConfiguredWithCopiedStateIsNamed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "honcho"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "config.yaml"), []byte("memory:\n  provider: honcho\n"))
	if got := ClonedMemoryProvider(dir); got != "honcho" {
		t.Fatalf("ClonedMemoryProvider = %q, want honcho", got)
	}
}

func cloneActiveProviderConfig(t *testing.T, source, profile string) {
	t.Helper()
	provider := ExternalMemoryProvider(loadProfileConfigDict(source))
	if _, err := CloneMemoryProviderConfig(source, profile, provider); err != nil {
		t.Fatal(err)
	}
}

func TestCloneCopiesOnlyTheActiveMemoryProvidersConfig(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".icebath")
	profile := filepath.Join(source, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "config.yaml"), []byte("memory:\n  provider: hindsight\n"))
	payload := `{"mode": "local_embedded", "bank_id": "icebath", "apiKey": "hs-secret"}`
	writeTestFile(t, filepath.Join(source, "hindsight", "config.json"), []byte(payload))
	writeTestFile(t, filepath.Join(source, "mem0.json"), []byte(`{"agent_id": "icebath"}`))

	cloneActiveProviderConfig(t, source, profile)

	cloned := filepath.Join(profile, "hindsight", "config.json")
	data, err := os.ReadFile(cloned)
	if err != nil || string(data) != payload {
		t.Fatalf("cloned config = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(cloned)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("cloned mode = %o, want 600", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(profile, "mem0.json")); !os.IsNotExist(err) {
		t.Fatalf("mem0.json travelled into the clone: %v", err)
	}
}

func TestCloneCopiesFlatProviderJSONOwnerOnly(t *testing.T) {
	source, profile := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "config.yaml"), []byte("memory:\n  provider: mem0\n"))
	writeTestFile(t, filepath.Join(source, "mem0.json"), []byte(`{"agent_id": "icebath"}`))
	copied, err := CloneMemoryProviderConfig(source, profile, ExternalMemoryProvider(loadProfileConfigDict(source)))
	if err != nil || !copied {
		t.Fatalf("copied = %v, %v", copied, err)
	}
	st, err := os.Stat(filepath.Join(profile, "mem0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if copied, _ := CloneMemoryProviderConfig(source, profile, ""); copied {
		t.Fatal("an empty provider must copy nothing")
	}
}

func TestCloneIgnoresUnsafeMemoryProviderNames(t *testing.T) {
	for _, provider := range []string{"../outside", "a/b", "..", "hind sight"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, ".icebath")
			profile := filepath.Join(source, "profiles", "coder")
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(source, "config.yaml"), []byte("memory:\n  provider: '"+provider+"'\n"))
			writeTestFile(t, filepath.Join(root, "outside", "config.json"), []byte("{}"))
			writeTestFile(t, filepath.Join(source, "a", "b", "config.json"), []byte("{}"))

			cloneActiveProviderConfig(t, source, profile)

			for _, p := range []string{filepath.Join(profile, "a"), filepath.Join(filepath.Dir(profile), "outside"), filepath.Join(profile, "hind sight")} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("%s exists after clone: %v", p, err)
				}
			}
		})
	}
}

func TestExternalMemoryProviderSkipsCoreSentinels(t *testing.T) {
	for _, name := range []string{"", "default", "builtin", "built-in", "none", " None "} {
		cfg := map[string]any{"memory": map[string]any{"provider": name}}
		if got := ExternalMemoryProvider(cfg); got != "" {
			t.Fatalf("ExternalMemoryProvider(%q) = %q, want none", name, got)
		}
	}
	if got := ExternalMemoryProvider(map[string]any{"memory": map[string]any{"provider": " honcho "}}); got != "honcho" {
		t.Fatalf("ExternalMemoryProvider = %q, want honcho", got)
	}
	if got := ExternalMemoryProvider(map[string]any{"memory": "honcho"}); got != "" {
		t.Fatalf("non mapping memory section gave %q", got)
	}
}
