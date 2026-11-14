package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

func TestCloneAllDoesNotShareTheSourceWebappBrowserCredential(t *testing.T) {
	tmp := profileTestHome(t)
	var out strings.Builder
	t.Cleanup(manage.SwapOutput(&out, &out))
	def := filepath.Join(tmp, ".icebath")
	if err := os.WriteFile(filepath.Join(def, "config.yaml"), []byte("model: test"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := func(h string) string {
		t.Helper()
		tok, err := gateway.BrowserSessionToken(context.Background(), h, false)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	source := token(def)
	dir, err := manage.CreateProfile(context.Background(), "coder", manage.CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, gateway.WebappTokenPath)); !os.IsNotExist(err) {
		t.Fatalf("clone carries the source credential: %v", err)
	}
	if token(dir) == source {
		t.Fatal("clone shares the source credential")
	}
	if token(def) != source {
		t.Fatal("source credential changed")
	}
}
