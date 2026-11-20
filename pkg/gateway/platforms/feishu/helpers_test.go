package feishu

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustObject(t *testing.T, text string) (*pyjson.Object, any) {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object), v
}
