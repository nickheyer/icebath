package pm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNPMArtifactsFollowTheUsersNPMRegistry(t *testing.T) {
	tmp := isolateHome(t)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToLower(key), "npm_config_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	digest := strings.Repeat("a", 64)
	lockURL := "https://registry.npmjs.org/npm/-/npm-10.9.2.tgz"
	pinned := func(rawURL, name string) string {
		source, err := PinnedSource(rawURL, filepath.Join(tmp, name), digest)
		if err != nil {
			t.Fatal(err)
		}
		return source.URL
	}
	if got := pinned(lockURL, "npm.tgz"); got != lockURL {
		t.Fatalf("default registry rewrote %s to %s", lockURL, got)
	}
	writeFile(t, filepath.Join(tmp, ".npmrc"), []byte("; corp\nregistry = https://npm.corp.example/npm/\n"), 0o666)
	if got := pinned(lockURL, "npm.tgz"); got != "https://npm.corp.example/npm/npm/-/npm-10.9.2.tgz" {
		t.Fatalf("npmrc registry gave %s", got)
	}
	t.Setenv("NPM_CONFIG_REGISTRY", "https://env.corp.example")
	if got := pinned(lockURL, "npm.tgz"); got != "https://env.corp.example/npm/-/npm-10.9.2.tgz" {
		t.Fatalf("environment registry gave %s", got)
	}
	other := "https://github.com/x/y/releases/download/v1/y.tgz"
	if got := pinned(other, "y.tgz"); got != other {
		t.Fatalf("non npm url rewritten to %s", got)
	}
}
