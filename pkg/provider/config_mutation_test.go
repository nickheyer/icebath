package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestCurrentProfileScopeBindsTheContextHomeAndSecrets(t *testing.T) {
	resetLaunchPolicy(t)
	root := t.TempDir()
	process := filepath.Join(root, ".icebath")
	named := filepath.Join(process, "profiles", "b")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{process, named} {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(named, ".env"), []byte("B_ONLY_KEY=from-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, process)
	ctx := context.Background()
	sctx := currentProfileScope(ctx)
	if home.Override(sctx) != "" || MultiplexProcess() {
		t.Fatalf("process: override %q", home.Override(sctx))
	}
	if scope := SecretScopeFrom(sctx); scope == nil || scope.ProfileHome != process {
		t.Fatalf("process secret scope %+v", scope)
	}
	sctx = currentProfileScope(home.WithOverride(ctx, named))
	if home.Dir(sctx) != named {
		t.Fatalf("named: dir %q", home.Dir(sctx))
	}
	if !MultiplexProcess() {
		t.Fatal("serving a second profile must activate multi profile hosting")
	}
	if scope := SecretScopeFrom(sctx); scope == nil || scope.ProfileHome != named || scope.Mapping["B_ONLY_KEY"] != "from-b" {
		t.Fatalf("named secret scope %+v", scope)
	}
	if nested := currentProfileScope(sctx); home.Dir(nested) != named {
		t.Fatalf("nested: dir %q", home.Dir(nested))
	}
}
