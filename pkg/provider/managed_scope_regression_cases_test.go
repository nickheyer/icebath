package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUserEnvOverridesShell(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "sandbox"))
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("FOO_TOKEN=from_user_env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOO_TOKEN", "from_shell")
	if _, err := LoadIcebathDotenv(context.Background(), DotenvLoadOptions{Home: h}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO_TOKEN"); got != "from_user_env" {
		t.Fatalf("FOO_TOKEN = %q", got)
	}
}
